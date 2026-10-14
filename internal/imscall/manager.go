package imscall

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"

	"vocat/internal/cscall"
	"vocat/internal/sipgw"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

// Controller 抽象设备级 IMS 呼叫控制与媒体能力，由 vowifi runtime Manager 实现。
type Controller interface {
	Calls(deviceID string) ([]vowifi.Call, error)
	DialCall(ctx context.Context, deviceID, number string) (vowifi.Call, error)
	AnswerCall(ctx context.Context, deviceID, callID string) (vowifi.Call, error)
	HangupCall(ctx context.Context, deviceID, callID string) error
	CallMedia(ctx context.Context, deviceID, callID string) (vowifi.CallMedia, error)
}

// callState 描述桥接通话状态。
type callState int

const (
	// stateIdle 空闲。
	stateIdle callState = iota
	// stateDialing 外呼建立中（已拨号，等待接通）。
	stateDialing
	// stateRinging 来电振铃中（等待软电话接听）。
	stateRinging
	// stateConnected 已接通。
	stateConnected
)

// Manager 负责管理某台设备的 IMS 通话，并把它以 B2BUA 方式桥接到软电话。
type Manager struct {
	deviceID   string
	controller Controller
	registrar  *sipgw.Registrar
	logger     *slog.Logger

	mu           sync.Mutex
	state        callState
	number       string // 对端号码
	sipCallID    string
	imsCallID    string
	answeredByUs bool // 来电由本桥接听（用于区分网页端接听）
	currentCall  *imsCall

	monitorCtx    context.Context
	monitorCancel context.CancelFunc
}

// imsCall 保存一次通话相关的资源。
type imsCall struct {
	bridge       *MediaBridge
	clientReq    *sip.Request
	clientTx     sip.ClientTransaction
	serverTx     sip.ServerTransaction
	serverReq    *sip.Request
	cancelFunc   context.CancelFunc
	isOutbound   bool
	earlyBridged bool // 已通过 183 早期媒体起桥
	finalSent    bool // INVITE 服务端事务已送最终响应
	answered200  bool // 外呼已向软电话发送 200 OK
}

// NewManager 创建 IMS 呼叫管理器。
func NewManager(deviceID string, controller Controller, registrar *sipgw.Registrar, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		deviceID:      deviceID,
		controller:    controller,
		registrar:     registrar,
		logger:        logger.With("device_id", deviceID),
		state:         stateIdle,
		monitorCtx:    ctx,
		monitorCancel: cancel,
	}
	go manager.monitorLoop()
	return manager
}

// Stop 停止管理器：挂断当前通话并释放资源。
func (manager *Manager) Stop() {
	if manager.monitorCancel != nil {
		manager.monitorCancel()
	}
	manager.endCall(0, "", true, true)
}

// monitorLoop 每秒轮询 IMS 通话列表，检测对端挂断与他端（网页）接听。
func (manager *Manager) monitorLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-manager.monitorCtx.Done():
			return
		case <-ticker.C:
		}
		manager.mu.Lock()
		state := manager.state
		imsCallID := manager.imsCallID
		answeredByUs := manager.answeredByUs
		call := manager.currentCall
		manager.mu.Unlock()
		if state == stateIdle || imsCallID == "" || call == nil {
			continue
		}
		// 外呼建立期由拨号循环全权处理，避免与最终响应竞争。
		if call.isOutbound && state != stateConnected {
			continue
		}
		calls, err := manager.controller.Calls(manager.deviceID)
		if err != nil {
			continue
		}
		current := findCall(calls, imsCallID)
		if current == nil {
			manager.logger.Info("IMS 通话已消失，结束桥接")
			manager.endCall(0, "", true, false)
			continue
		}
		switch current.State {
		case "ended", "failed":
			manager.logger.Info("对端已挂断或通话结束",
				"state", current.State, "sip_code", current.SIPCode)
			manager.endCall(0, "", true, false)
		case "active":
			// 来电振铃中被其他端（网页）接听：取消对软电话的呼叫。
			if state == stateRinging && !call.isOutbound && !answeredByUs {
				manager.logger.Info("来电已被其他端接听，取消对软电话的呼叫")
				manager.endCall(0, "", true, false)
			}
		}
	}
}

// HandleOutboundInvite 处理软电话发起的外呼 INVITE。
func (manager *Manager) HandleOutboundInvite(deviceID string, req *sip.Request, tx sip.ServerTransaction) {
	to := req.To()
	if to == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request - Missing To", nil))
		return
	}
	callee := to.Address.User
	callID := ""
	if header := req.CallID(); header != nil {
		callID = header.Value()
	}
	to.Params.Add("tag", uuid.NewString()[:8])

	manager.logger.Info("收到软电话外呼请求", "callee", callee)

	manager.mu.Lock()
	if manager.state != stateIdle {
		manager.mu.Unlock()
		manager.logger.Warn("外呼失败，当前已有通话进行中")
		_ = tx.Respond(sip.NewResponseFromRequest(req, 486, "Busy Here", nil))
		return
	}
	manager.state = stateDialing
	manager.sipCallID = callID
	manager.number = callee
	manager.answeredByUs = false
	manager.mu.Unlock()

	bridge, err := NewMediaBridge(manager.deviceID, manager.logger)
	if err != nil {
		manager.logger.Error("初始化媒体桥失败", "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		manager.endCall(0, "", false, false)
		return
	}
	call := &imsCall{bridge: bridge, serverTx: tx, serverReq: req, isOutbound: true}
	manager.mu.Lock()
	manager.currentCall = call
	manager.mu.Unlock()

	if body := req.Body(); len(body) > 0 {
		if info, err := cscall.ParseSDP(body); err == nil {
			bridge.SetClientAddr(info.ConnectionIP, info.MediaPort)
		}
	}

	// 设备上已有其他通话（如网页端通话）时直接回忙，保证单设备单通话。
	if manager.deviceBusy() {
		manager.logger.Warn("外呼失败，设备已有其他通话进行中")
		manager.endCall(486, "Busy Here", false, false)
		return
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 15*time.Second)
	dialed, err := manager.controller.DialCall(dialCtx, manager.deviceID, callee)
	dialCancel()
	if err != nil {
		manager.logger.Error("IMS 拨号失败", "err", err)
		manager.endCall(503, "Service Unavailable - Dial Failed", false, false)
		return
	}
	manager.mu.Lock()
	manager.imsCallID = dialed.ID
	manager.mu.Unlock()

	_ = tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil))

	localIP := manager.registrar.GetExternalIP()
	if localIP == "" {
		localIP = detectLocalIP(hostOf(req.Source()))
	}
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	manager.runOutboundSetup(call, localIP)
}

// runOutboundSetup 外呼建立循环：等待早期媒体/接通/终结，最长 60 秒。
func (manager *Manager) runOutboundSetup(call *imsCall, localIP string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	call.cancelFunc = cancel
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		manager.mu.Lock()
		st := manager.state
		imsCallID := manager.imsCallID
		manager.mu.Unlock()
		if st == stateIdle {
			// 已被 CANCEL 等路径处理（最终响应已由对应路径补发）。
			return
		}
		select {
		case <-ctx.Done():
			// 仅真正超时（60s 未接通）才补发 408；手动取消由 CANCEL/BYE 等路径处理。
			if ctx.Err() == context.DeadlineExceeded {
				manager.logger.Warn("外呼超时，对方未接听")
				manager.endCall(408, "Request Timeout", false, true)
			}
			return
		case <-ticker.C:
		}
		calls, err := manager.controller.Calls(manager.deviceID)
		if err != nil {
			continue
		}
		current := findCall(calls, imsCallID)
		if current == nil {
			manager.logger.Warn("外呼通话已消失")
			manager.endCall(480, "Temporarily Unavailable", false, false)
			return
		}
		switch current.State {
		case "ended", "failed":
			code, reason := mapTermination(current.SIPCode)
			manager.logger.Info("外呼被对端终结", "sip_code", current.SIPCode, "mapped", code)
			manager.endCall(code, reason, false, false)
			return
		case "early_media", "ringing":
			// 早期媒体就绪：回 183 让软电话播放回铃音并提前起桥。
			if current.MediaReady && !call.earlyBridged {
				if manager.startEarlyMedia(call, localIP) {
					call.earlyBridged = true
				}
			}
		case "active":
			manager.mu.Lock()
			manager.state = stateConnected
			number := manager.number
			manager.mu.Unlock()
			if !call.earlyBridged && !manager.attachMedia(call) {
				manager.endCall(500, "Internal Server Error", false, true)
				return
			}
			sdpBody := cscall.BuildSDP(localIP, call.bridge.LocalPort(), "VoCat IMS Call")
			contact := sip.NewHeader("Contact", "<"+(&sip.Uri{User: number, Host: localIP}).String()+">")
			manager.sendFinal(call, 200, "OK", []byte(sdpBody), contact)
			manager.mu.Lock()
			call.answered200 = true
			manager.mu.Unlock()
			manager.logger.Info("外呼已接通，媒体桥运行中", "callee", number)
			return
		}
	}
}

// startEarlyMedia 挂接媒体、起桥并向软电话发送 183 Session Progress。
func (manager *Manager) startEarlyMedia(call *imsCall, localIP string) bool {
	if !manager.attachMedia(call) {
		return false
	}
	sdpBody := cscall.BuildSDP(localIP, call.bridge.LocalPort(), "VoCat IMS Call")
	resp := sip.NewResponseFromRequest(call.serverReq, 183, "Session Progress", []byte(sdpBody))
	resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	if err := call.serverTx.Respond(resp); err != nil {
		manager.logger.Warn("发送 183 早期媒体失败", "err", err)
		return false
	}
	manager.logger.Info("已向软电话发送 183（早期媒体）")
	return true
}

// attachMedia 获取 IMS 通话媒体并启动媒体桥，幂等。
func (manager *Manager) attachMedia(call *imsCall) bool {
	manager.mu.Lock()
	imsCallID := manager.imsCallID
	manager.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	media, err := manager.controller.CallMedia(ctx, manager.deviceID, imsCallID)
	cancel()
	if err != nil {
		manager.logger.Warn("获取 IMS 通话媒体失败", "err", err)
		return false
	}
	call.bridge.AttachMedia(media)
	call.bridge.Start(context.Background())
	return true
}

// HandleIncomingCall 处理 IMS 来电（由 onIncomingCall 回调驱动）。
func (manager *Manager) HandleIncomingCall(ctx context.Context, incoming ims.ReceivedCall) {
	caller := strings.TrimSpace(incoming.Caller)
	if caller == "" {
		caller = "Unknown"
	}
	manager.mu.Lock()
	if manager.state != stateIdle {
		manager.mu.Unlock()
		manager.logger.Warn("来电时本桥已有通话，直接挂断", "caller", caller)
		manager.hangupIMS(incoming.CallID)
		return
	}
	manager.state = stateRinging
	manager.number = caller
	manager.imsCallID = incoming.CallID
	manager.sipCallID = uuid.NewString()
	manager.answeredByUs = false
	sipCallID := manager.sipCallID
	manager.mu.Unlock()

	go manager.runIncoming(ctx, incoming.CallID, sipCallID)
}

// runIncoming 来电流程：等软电话上线 → INVITE → 接听 → 起桥。
func (manager *Manager) runIncoming(ctx context.Context, imsCallID, sipCallID string) {
	// 等待软电话在线（最多 30 秒）。
	var user *sipgw.RegisteredUser
	for attempt := 0; attempt < 150; attempt++ {
		user = manager.registrar.GetUserByDevice(manager.deviceID)
		if user != nil {
			break
		}
		if attempt == 0 {
			manager.logger.Info("来电到达，等待软电话上线", "caller", manager.number)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
		manager.mu.Lock()
		stillRinging := manager.state == stateRinging && manager.sipCallID == sipCallID
		manager.mu.Unlock()
		if !stillRinging {
			manager.logger.Info("等待软电话期间来电已结束")
			return
		}
	}
	if user == nil {
		manager.logger.Warn("等待 30 秒后仍无软电话上线，挂断来电")
		manager.endCall(0, "", false, true)
		return
	}
	if latest := manager.registrar.GetUserByDevice(manager.deviceID); latest != nil {
		user = latest
	}

	bridge, err := NewMediaBridge(manager.deviceID, manager.logger)
	if err != nil {
		manager.logger.Error("初始化媒体桥失败", "err", err)
		manager.endCall(0, "", false, true)
		return
	}
	call := &imsCall{bridge: bridge}
	manager.mu.Lock()
	if manager.state != stateRinging || manager.sipCallID != sipCallID {
		manager.mu.Unlock()
		bridge.Stop()
		return
	}
	manager.currentCall = call
	caller := manager.number
	manager.mu.Unlock()

	localIP := detectLocalIP(user.ContactAddr.IP.String())
	if localIP == "" {
		localIP = manager.registrar.GetExternalIP()
	}
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	sdpBody := cscall.BuildSDP(localIP, bridge.LocalPort(), "VoCat IMS Call")

	from := &sip.Uri{User: caller, Host: "imscall.vocat"}
	to := &sip.Uri{User: user.Username, Host: localIP}
	contact := &sip.Uri{User: caller, Host: localIP}

	request := sip.NewRequest(sip.INVITE, sip.Uri{User: user.Username, Host: user.ContactAddr.IP.String(), Port: user.ContactAddr.Port})
	request.SetDestination(user.ContactAddr.String())
	if user.Transport != "" {
		request.SetTransport(user.Transport)
	}
	request.AppendHeader(sip.NewHeader("From", "<"+from.String()+fmt.Sprintf(">;tag=%s", uuid.NewString()[:8])))
	request.AppendHeader(sip.NewHeader("To", "<"+to.String()+">"))
	request.AppendHeader(sip.NewHeader("Call-ID", sipCallID))
	request.AppendHeader(sip.NewHeader("CSeq", "1 INVITE"))
	request.AppendHeader(sip.NewHeader("Contact", "<"+contact.String()+">"))
	request.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	request.SetBody([]byte(sdpBody))

	tx, err := manager.registrar.GetClient().TransactionRequest(context.Background(), request)
	if err != nil {
		manager.logger.Error("发送 SIP INVITE 失败", "err", err)
		manager.endCall(0, "", false, true)
		return
	}
	call.clientReq = request
	call.clientTx = tx

	waitCtx, cancel := context.WithCancel(context.Background())
	call.cancelFunc = cancel
	manager.logger.Info("已向软电话发送 INVITE，等待接听", "username", user.Username)
	manager.waitClientAnswer(waitCtx, call, sipCallID, imsCallID)
}

// waitClientAnswer 等待软电话对来电 INVITE 的响应。
func (manager *Manager) waitClientAnswer(ctx context.Context, call *imsCall, sipCallID, imsCallID string) {
	for {
		select {
		case <-ctx.Done():
			return
		case response, ok := <-call.clientTx.Responses():
			if !ok {
				manager.mu.Lock()
				stillActive := manager.state != stateIdle && manager.sipCallID == sipCallID
				manager.mu.Unlock()
				if stillActive {
					manager.logger.Warn("SIP 事务已关闭")
					manager.endCall(0, "", false, true)
				}
				return
			}
			code := response.StatusCode
			switch {
			case code >= 200 && code < 300:
				manager.handleClientAnswer(call, sipCallID, imsCallID, response)
				return
			case code >= 300:
				manager.logger.Info("软电话拒绝接听", "code", code)
				manager.endCall(0, "", false, true)
				return
			}
		}
	}
}

// handleClientAnswer 软电话接听来电：回 ACK、接听 IMS 侧并起桥。
func (manager *Manager) handleClientAnswer(call *imsCall, sipCallID, imsCallID string, response *sip.Response) {
	manager.mu.Lock()
	if manager.state != stateRinging || manager.sipCallID != sipCallID {
		manager.mu.Unlock()
		return
	}
	manager.state = stateConnected
	manager.answeredByUs = true
	manager.mu.Unlock()

	manager.logger.Info("软电话已接听，接听 IMS 通话并建立媒体")

	if info, err := cscall.ParseSDP(response.Body()); err == nil {
		call.bridge.SetClientAddr(info.ConnectionIP, info.MediaPort)
	} else {
		manager.logger.Warn("解析软电话 SDP 失败，回退首包学习地址", "err", err)
	}

	manager.sendAck(call, response)

	answerCtx, answerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err := manager.controller.AnswerCall(answerCtx, manager.deviceID, imsCallID)
	answerCancel()
	if err != nil {
		manager.logger.Error("接听 IMS 通话失败", "err", err)
		manager.endCall(0, "", true, true)
		return
	}
	if !manager.attachMedia(call) {
		manager.endCall(0, "", true, true)
		return
	}
	manager.logger.Info("来电双向语音建立完成")
}

// sendAck 针对软电话的 200 OK 回送 ACK（与 cscall 相同的构造方式）。
func (manager *Manager) sendAck(call *imsCall, response *sip.Response) {
	var recipient sip.Uri
	if contact := response.Contact(); contact != nil {
		recipient = sip.Uri{
			Scheme: contact.Address.Scheme,
			User:   contact.Address.User,
			Host:   contact.Address.Host,
			Port:   contact.Address.Port,
		}
	} else {
		recipient = sip.Uri{
			Scheme: call.clientReq.Recipient.Scheme,
			User:   call.clientReq.Recipient.User,
			Host:   call.clientReq.Recipient.Host,
			Port:   call.clientReq.Recipient.Port,
		}
	}
	ack := sip.NewRequest(sip.ACK, recipient)
	ack.SipVersion = call.clientReq.SipVersion

	origVia := call.clientReq.Via()
	viaHop := &sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "UDP",
		Host:            origVia.Host,
		Port:            origVia.Port,
		Params:          sip.NewParams(),
	}
	viaHop.Params.Add("branch", sip.GenerateBranch())
	ack.AppendHeader(viaHop)

	if len(call.clientReq.GetHeaders("Route")) > 0 {
		sip.CopyHeaders("Route", call.clientReq, ack)
	}
	if header := call.clientReq.From(); header != nil {
		ack.AppendHeader(sip.HeaderClone(header))
	}
	if header := response.To(); header != nil {
		ack.AppendHeader(sip.HeaderClone(header))
	}
	if header := call.clientReq.CallID(); header != nil {
		ack.AppendHeader(sip.HeaderClone(header))
	}
	if header := call.clientReq.CSeq(); header != nil {
		cloned := sip.HeaderClone(header).(*sip.CSeqHeader)
		cloned.MethodName = sip.ACK
		ack.AppendHeader(cloned)
	}
	maxForwards := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxForwards)
	ack.SetBody(nil)
	ack.SetTransport(call.clientReq.Transport())
	ack.SetSource(call.clientReq.Source())
	ack.SetDestination(call.clientReq.Destination())

	if err := manager.registrar.GetClient().WriteRequest(ack, sipgo.ClientRequestBuild); err != nil {
		manager.logger.Error("发送 ACK 失败", "err", err)
	}
}

// HandleClientCancel 处理软电话取消外呼（CANCEL）。
func (manager *Manager) HandleClientCancel(callID string) bool {
	manager.mu.Lock()
	match := manager.sipCallID == callID && manager.state != stateIdle &&
		manager.currentCall != nil && manager.currentCall.isOutbound
	manager.mu.Unlock()
	if !match {
		return false
	}
	manager.logger.Info("软电话取消外呼 (CANCEL)")
	manager.endCall(487, "Request Terminated", false, true)
	return true
}

// HandleClientBye 处理软电话主动挂断（BYE）。
func (manager *Manager) HandleClientBye(callID string) bool {
	manager.mu.Lock()
	match := manager.sipCallID == callID && manager.state != stateIdle
	manager.mu.Unlock()
	if !match {
		return false
	}
	manager.logger.Info("软电话主动挂断 (BYE)")
	manager.endCall(0, "", false, true)
	return true
}

// endCall 结束通话并复位状态（幂等）。
//
// finalCode > 0 时向软电话 INVITE 的服务端事务补发最终响应；
// notifyClient 为 true 时按通话方向与阶段向软电话补发 CANCEL/BYE；
// hangupIMS 为 true 时挂断 IMS 侧通话。
func (manager *Manager) endCall(finalCode int, finalReason string, notifyClient bool, hangupIMS bool) {
	manager.mu.Lock()
	if manager.state == stateIdle {
		manager.mu.Unlock()
		return
	}
	stateSnapshot := manager.state
	imsCallID := manager.imsCallID
	answeredByUs := manager.answeredByUs
	call := manager.currentCall
	manager.state = stateIdle
	manager.number = ""
	manager.sipCallID = ""
	manager.imsCallID = ""
	manager.answeredByUs = false
	manager.currentCall = nil
	manager.mu.Unlock()

	manager.logger.Info("结束通话并清理资源")

	if hangupIMS && imsCallID != "" {
		manager.hangupIMS(imsCallID)
	}
	if call == nil {
		return
	}
	if call.cancelFunc != nil {
		call.cancelFunc()
	}
	if call.bridge != nil {
		call.bridge.Stop()
	}
	if finalCode > 0 {
		manager.sendFinal(call, finalCode, finalReason, nil)
	}
	if !notifyClient {
		return
	}
	client := manager.registrar.GetClient()
	if call.isOutbound {
		// 外呼方向：已接通时向软电话发送 BYE。
		if call.answered200 && call.serverReq != nil {
			bye := sip.NewRequest(sip.BYE, call.serverReq.From().Address)
			sip.CopyHeaders("To", call.serverReq, bye)
			sip.CopyHeaders("From", call.serverReq, bye)
			sip.CopyHeaders("Call-ID", call.serverReq, bye)
			bye.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d BYE", call.serverReq.CSeq().SeqNo+1)))
			_ = client.WriteRequest(bye)
		}
		return
	}
	if call.clientReq == nil {
		return
	}
	if stateSnapshot == stateRinging && !answeredByUs {
		// 来电方向、软电话未接听：取消未完成的 INVITE。
		cancel := sip.NewRequest(sip.CANCEL, call.clientReq.Recipient)
		sip.CopyHeaders("To", call.clientReq, cancel)
		sip.CopyHeaders("From", call.clientReq, cancel)
		sip.CopyHeaders("Call-ID", call.clientReq, cancel)
		sip.CopyHeaders("Via", call.clientReq, cancel)
		cancel.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d CANCEL", call.clientReq.CSeq().SeqNo)))
		_ = client.WriteRequest(cancel)
		return
	}
	bye := sip.NewRequest(sip.BYE, call.clientReq.Recipient)
	sip.CopyHeaders("To", call.clientReq, bye)
	sip.CopyHeaders("From", call.clientReq, bye)
	sip.CopyHeaders("Call-ID", call.clientReq, bye)
	bye.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d BYE", call.clientReq.CSeq().SeqNo+1)))
	_ = client.WriteRequest(bye)
}

// sendFinal 幂等地向 INVITE 服务端事务发送最终响应。
func (manager *Manager) sendFinal(call *imsCall, code int, reason string, body []byte, headers ...sip.Header) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if call == nil || call.serverTx == nil || call.finalSent {
		return
	}
	call.finalSent = true
	resp := sip.NewResponseFromRequest(call.serverReq, code, reason, body)
	if len(body) > 0 {
		resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	for _, header := range headers {
		resp.AppendHeader(header)
	}
	_ = call.serverTx.Respond(resp)
}

// hangupIMS 挂断 IMS 侧通话（忽略错误，仅记录）。
func (manager *Manager) hangupIMS(imsCallID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.controller.HangupCall(ctx, manager.deviceID, imsCallID); err != nil {
		manager.logger.Warn("挂断 IMS 通话失败", "err", err)
	}
}

// deviceBusy 判断设备上是否已有其他进行中的通话（如网页端通话）。
func (manager *Manager) deviceBusy() bool {
	calls, err := manager.controller.Calls(manager.deviceID)
	if err != nil {
		return false
	}
	for _, call := range calls {
		if call.State != "ended" && call.State != "failed" {
			return true
		}
	}
	return false
}

// detectLocalIP 通过向目标地址拨 UDP 探测本机出口 IP。
func detectLocalIP(targetIP string) string {
	if targetIP == "" {
		return ""
	}
	conn, err := net.Dial("udp", targetIP+":1")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// hostOf 从 "host[:port]" 形式的地址中提取主机部分。
func hostOf(address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

// findCall 按 ID 查找通话。
func findCall(calls []vowifi.Call, id string) *vowifi.Call {
	for index := range calls {
		if calls[index].ID == id {
			return &calls[index]
		}
	}
	return nil
}

// mapTermination 把 IMS 侧的终结 SIP 状态码映射为回给软电话的响应。
func mapTermination(sipCode int) (int, string) {
	switch sipCode {
	case 403:
		return 403, "Forbidden"
	case 404:
		return 404, "Not Found"
	case 408:
		return 408, "Request Timeout"
	case 480:
		return 480, "Temporarily Unavailable"
	case 486:
		return 486, "Busy Here"
	case 487:
		return 487, "Request Terminated"
	case 603:
		return 603, "Decline"
	default:
		if sipCode >= 400 && sipCode < 700 {
			return sipCode, "Call Terminated"
		}
		return 480, "Temporarily Unavailable"
	}
}

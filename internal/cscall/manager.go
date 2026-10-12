package cscall

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

	"vocat/internal/sipgw"
)

// Manager 负责管理某个设备的 CS 域呼叫，并把它桥接到 SIP/RTP。
type Manager struct {
	deviceID   string
	audioDev   string
	controller Controller
	registrar  *sipgw.Registrar
	logger     *slog.Logger

	mu               sync.Mutex
	state            CallState
	callerID         string
	sipCallID        string
	controllerCallID string
	currentCall      *csCall

	monitorCtx    context.Context
	monitorCancel context.CancelFunc
}

// csCall 保存一次通话相关的资源。
type csCall struct {
	audio      *AudioBridge
	clientAddr *net.UDPAddr
	clientReq  *sip.Request
	clientTx   sip.ClientTransaction
	serverTx   sip.ServerTransaction
	serverReq  *sip.Request
	cancelFunc context.CancelFunc
	isOutbound bool
}

// NewManager 创建 CS 呼叫管理器。
func NewManager(deviceID, audioDev string, controller Controller, registrar *sipgw.Registrar, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		deviceID:      deviceID,
		audioDev:      audioDev,
		controller:    controller,
		registrar:     registrar,
		logger:        logger.With("device_id", deviceID),
		state:         CallStateIdle,
		monitorCtx:    ctx,
		monitorCancel: cancel,
	}
	if controller != nil {
		if err := controller.Start(ctx); err != nil {
			manager.logger.Error("CS 呼叫控制器启动失败", "err", err)
		}
		go manager.monitorControllerEvents()
		go manager.monitorPCMReady()
	}
	return manager
}

// Stop 停止 CS 呼叫管理器。
func (manager *Manager) Stop() {
	if manager.monitorCancel != nil {
		manager.monitorCancel()
	}
	if manager.controller != nil {
		manager.controller.Stop()
	}
}

// HasCall 判断指定 Call-ID 是否正在进行。
func (manager *Manager) HasCall(callID string) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.sipCallID == callID && manager.state != CallStateIdle
}

func (manager *Manager) monitorControllerEvents() {
	ch := manager.controller.Events()
	for {
		select {
		case <-manager.monitorCtx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			manager.handleControllerEvent(event)
		}
	}
}

func (manager *Manager) handleControllerEvent(event Event) {
	switch event.Type {
	case EventIncoming:
		manager.onIncoming(event.CallID, event.Number)
	case EventHangup:
		manager.onHangup()
	}
}

func (manager *Manager) monitorPCMReady() {
	ch := manager.controller.PCMReady()
	for {
		select {
		case <-manager.monitorCtx.Done():
			return
		case ready, ok := <-ch:
			if !ok {
				return
			}
			manager.mu.Lock()
			if manager.currentCall != nil && manager.currentCall.audio != nil {
				manager.currentCall.audio.SetPCMReady(ready)
			}
			manager.mu.Unlock()
		}
	}
}

func (manager *Manager) beginIncomingCall(callID, number string) (sipCallID string, shouldStart bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.state != CallStateIdle && manager.state != CallStateRinging {
		return "", false
	}
	if strings.TrimSpace(number) == "" {
		number = "Unknown"
	}
	if manager.state == CallStateIdle {
		manager.state = CallStateRinging
		manager.callerID = number
		manager.controllerCallID = callID
		manager.sipCallID = uuid.NewString()
		return manager.sipCallID, number != "Unknown"
	}
	if manager.callerID == "Unknown" && number != "Unknown" {
		manager.callerID = number
		if callID != "" {
			manager.controllerCallID = callID
		}
		return manager.sipCallID, true
	}
	return "", false
}

func (manager *Manager) onIncoming(callID, number string) {
	sipCallID, ok := manager.beginIncomingCall(callID, number)
	if ok {
		go manager.initiateSIPCall(sipCallID)
		return
	}
	if number == "" || number == "Unknown" {
		if sipCallID == "" {
			manager.mu.Lock()
			sipCallID = manager.sipCallID
			manager.mu.Unlock()
		}
		if sipCallID == "" {
			return
		}
		go func(callID string) {
			// 有些模组先在 RING 后送 +CLIP，给号码 3 秒机会补全。
			time.Sleep(3 * time.Second)
			manager.mu.Lock()
			pending := manager.state == CallStateRinging && manager.sipCallID == callID && manager.callerID == "Unknown"
			manager.mu.Unlock()
			if pending {
				manager.logger.Warn("3 秒未收到来电号码，使用 Unknown 发起呼叫")
				manager.initiateSIPCall(callID)
			}
		}(sipCallID)
	}
}

// onHangup 处理 NO CARRIER 等远端挂断。
func (manager *Manager) onHangup() {
	manager.mu.Lock()
	if manager.state == CallStateIdle {
		manager.mu.Unlock()
		return
	}
	manager.mu.Unlock()
	manager.logger.Info("对端已挂断或通话中断")
	manager.endCallAndHangup(true, false)
}

// initiateSIPCall 构建并向软电话发起 SIP INVITE（来电方向）。
func (manager *Manager) initiateSIPCall(callID string) {
	manager.mu.Lock()
	if manager.state != CallStateRinging || manager.sipCallID != callID {
		manager.mu.Unlock()
		return
	}
	caller := manager.callerID
	manager.mu.Unlock()

	// 等待软电话在线（最多 30 秒）。
	var user *sipgw.RegisteredUser
	for attempt := 0; attempt < 150; attempt++ {
		user = manager.registrar.GetUserByDevice(manager.deviceID)
		if user != nil {
			break
		}
		if attempt == 0 {
			manager.logger.Info("来电到达，等待软电话上线", "caller", caller)
		}
		time.Sleep(200 * time.Millisecond)

		manager.mu.Lock()
		stillRinging := manager.state == CallStateRinging && manager.sipCallID == callID
		manager.mu.Unlock()
		if !stillRinging {
			manager.logger.Info("等待软电话期间来电已结束")
			return
		}
	}
	if user == nil {
		manager.logger.Warn("等待 30 秒后仍无软电话上线，挂断来电")
		manager.endCallAndHangup(false, true)
		return
	}
	if latest := manager.registrar.GetUserByDevice(manager.deviceID); latest != nil {
		user = latest
	}

	bridge, err := NewAudioBridge(manager.audioDev, manager.deviceID, manager.logger)
	if err != nil {
		manager.logger.Error("初始化音频桥失败", "err", err)
		manager.endCallAndHangup(false, true)
		return
	}

	call := &csCall{audio: bridge}
	manager.mu.Lock()
	manager.currentCall = call
	manager.mu.Unlock()

	localIP := manager.detectLocalIP(user.ContactAddr.IP.String())
	if localIP == "" {
		localIP = manager.registrar.GetExternalIP()
	}
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	sdpBody := buildSDP(localIP, bridge.LocalPort())

	from := &sip.Uri{User: caller, Host: "cscall.vocat"}
	to := &sip.Uri{User: user.Username, Host: localIP}
	contact := &sip.Uri{User: caller, Host: localIP}

	request := sip.NewRequest(sip.INVITE, sip.Uri{User: user.Username, Host: user.ContactAddr.IP.String(), Port: user.ContactAddr.Port})
	request.SetDestination(user.ContactAddr.String())
	if user.Transport != "" {
		request.SetTransport(user.Transport)
	}
	request.AppendHeader(sip.NewHeader("From", "<"+from.String()+fmt.Sprintf(">;tag=%s", uuid.NewString()[:8])))
	request.AppendHeader(sip.NewHeader("To", "<"+to.String()+">"))
	request.AppendHeader(sip.NewHeader("Call-ID", callID))
	request.AppendHeader(sip.NewHeader("CSeq", "1 INVITE"))
	request.AppendHeader(sip.NewHeader("Contact", "<"+contact.String()+">"))
	request.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	request.SetBody([]byte(sdpBody))

	tx, err := manager.registrar.GetClient().TransactionRequest(context.Background(), request)
	if err != nil {
		manager.logger.Error("发送 SIP INVITE 失败", "err", err)
		bridge.Stop()
		manager.endCallAndHangup(false, true)
		return
	}
	call.clientReq = request
	call.clientTx = tx

	ctx, cancel := context.WithCancel(context.Background())
	call.cancelFunc = cancel
	manager.logger.Info("已向软电话发送 INVITE，等待接听", "username", user.Username)

	go manager.waitClientResponse(ctx, call, callID)
}

func (manager *Manager) detectLocalIP(targetIP string) string {
	conn, err := net.Dial("udp", targetIP+":1")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func (manager *Manager) waitClientResponse(ctx context.Context, call *csCall, callID string) {
	for {
		select {
		case <-ctx.Done():
			return
		case response, ok := <-call.clientTx.Responses():
			if !ok {
				manager.logger.Warn("SIP 事务已关闭")
				manager.endCallAndHangup(true, true)
				return
			}
			code := response.StatusCode
			switch {
			case code >= 200 && code < 300:
				manager.handleClientAnswer(call, callID, response)
				return
			case code >= 300:
				manager.logger.Info("软电话拒绝接听", "code", code)
				manager.endCallAndHangup(false, true)
				return
			}
		}
	}
}

func (manager *Manager) handleClientAnswer(call *csCall, callID string, response *sip.Response) {
	manager.mu.Lock()
	if manager.state != CallStateRinging || manager.sipCallID != callID {
		manager.mu.Unlock()
		return
	}
	manager.state = CallStateConnected
	manager.mu.Unlock()

	manager.logger.Info("软电话已接听，开始建立媒体通道")

	if info, err := parseSDP(response.Body()); err == nil {
		call.clientAddr = &net.UDPAddr{IP: net.ParseIP(info.ConnectionIP), Port: info.MediaPort}
		call.audio.SetClientAddr(info.ConnectionIP, info.MediaPort)
	} else {
		manager.logger.Warn("解析协商 SDP 失败，回退到首包学习地址", "err", err)
	}

	manager.sendAck(call, response)

	if err := call.audio.Start(); err != nil {
		manager.logger.Error("音频桥启动失败", "err", err)
		manager.endCallAndHangup(true, true)
		return
	}

	manager.mu.Lock()
	controllerCallID := manager.controllerCallID
	manager.mu.Unlock()
	if manager.controller == nil {
		manager.endCallAndHangup(true, false)
		return
	}
	if err := manager.controller.Answer(context.Background(), controllerCallID); err != nil {
		manager.logger.Error("接听指令失败", "err", err)
		manager.endCallAndHangup(true, true)
		return
	}
	// 模组侧不一定会可靠上报 +QPCMV，接听后主动置为就绪。
	call.audio.SetPCMReady(true)
	manager.logger.Info("双向语音建立完成")
}

func (manager *Manager) sendAck(call *csCall, response *sip.Response) {
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

// HandleOutboundInvite 处理软电话发起的外呼 INVITE。
func (manager *Manager) HandleOutboundInvite(deviceID string, request *sip.Request, tx sip.ServerTransaction) {
	to := request.To()
	if to == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(request, 400, "Bad Request - Missing To", nil))
		return
	}
	callee := to.Address.User
	callID := request.CallID().Value()

	toTag := uuid.NewString()[:8]
	to.Params.Add("tag", toTag)

	manager.logger.Info("收到软电话外呼请求", "callee", callee)

	manager.mu.Lock()
	if manager.state != CallStateIdle {
		manager.mu.Unlock()
		manager.logger.Warn("外呼失败，当前已有通话进行中")
		_ = tx.Respond(sip.NewResponseFromRequest(request, 486, "Busy Here", nil))
		return
	}
	manager.state = CallStateDialing
	manager.sipCallID = callID
	manager.callerID = callee
	manager.mu.Unlock()

	localIP := manager.registrar.GetExternalIP()
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	bridge, err := NewAudioBridge(manager.audioDev, manager.deviceID, manager.logger)
	if err != nil {
		manager.logger.Error("初始化音频桥失败", "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(request, 500, "Internal Server Error", nil))
		manager.mu.Lock()
		manager.state = CallStateIdle
		manager.mu.Unlock()
		return
	}
	call := &csCall{audio: bridge, serverTx: tx, serverReq: request, isOutbound: true}
	manager.mu.Lock()
	manager.currentCall = call
	manager.mu.Unlock()

	if body := request.Body(); len(body) > 0 {
		if info, err := parseSDP(body); err == nil {
			bridge.SetClientAddr(info.ConnectionIP, info.MediaPort)
		}
	}

	if manager.controller == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(request, 503, "Service Unavailable - Call Control Missing", nil))
		manager.mu.Lock()
		manager.state = CallStateIdle
		manager.currentCall = nil
		manager.mu.Unlock()
		bridge.Stop()
		return
	}

	ref, err := manager.controller.Dial(context.Background(), callee)
	if err != nil {
		manager.logger.Error("模组拨号失败", "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(request, 503, "Service Unavailable - Dial Failed", nil))
		manager.mu.Lock()
		manager.state = CallStateIdle
		manager.currentCall = nil
		manager.mu.Unlock()
		bridge.Stop()
		return
	}
	manager.mu.Lock()
	manager.controllerCallID = ref.ID
	manager.mu.Unlock()

	_ = tx.Respond(sip.NewResponseFromRequest(request, 180, "Ringing", nil))
	manager.logger.Debug("已向软电话发送 180 Ringing")

	connectCtx, connectCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer connectCancel()
	call.cancelFunc = connectCancel
	connected := false
	for {
		if connectCtx.Err() != nil {
			manager.logger.Warn("外呼超时，对方未接听")
			_ = tx.Respond(sip.NewResponseFromRequest(request, 408, "Request Timeout", nil))
			manager.endCallAndHangup(false, true)
			return
		}
		manager.mu.Lock()
		idle := manager.state == CallStateIdle
		manager.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(500 * time.Millisecond)
		calls, err := manager.controller.GetCalls(connectCtx)
		if err != nil {
			continue
		}
		if hasConnectedCall(calls, manager.controllerCallID) {
			connected = true
			break
		}
	}
	if !connected {
		return
	}

	manager.logger.Info("对方已接听，建立媒体通道")
	manager.mu.Lock()
	manager.state = CallStateConnected
	manager.mu.Unlock()

	sdpBody := buildSDP(localIP, bridge.LocalPort())
	response200 := sip.NewResponseFromRequest(request, 200, "OK", []byte(sdpBody))
	response200.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	contact := &sip.Uri{User: callee, Host: localIP}
	response200.AppendHeader(sip.NewHeader("Contact", "<"+contact.String()+">"))
	_ = tx.Respond(response200)

	if err := bridge.Start(); err != nil {
		manager.logger.Error("音频桥启动失败", "err", err)
		manager.endCallAndHangup(false, true)
		return
	}
	bridge.SetPCMReady(true)
	manager.logger.Info("外呼双向语音建立完成", "callee", callee)
}

// HandleClientCancel 处理软电话拒接（CANCEL）。
func (manager *Manager) HandleClientCancel(callID string) bool {
	manager.mu.Lock()
	if manager.sipCallID != callID || manager.state != CallStateRinging {
		manager.mu.Unlock()
		return false
	}
	manager.mu.Unlock()
	manager.logger.Info("软电话拒接呼叫 (CANCEL)")
	manager.endCallAndHangup(false, true)
	return true
}

// HandleClientBye 处理软电话主动挂断（BYE）。
func (manager *Manager) HandleClientBye(callID string) bool {
	manager.mu.Lock()
	if manager.sipCallID != callID || manager.state == CallStateIdle {
		manager.mu.Unlock()
		return false
	}
	manager.mu.Unlock()
	manager.logger.Info("软电话主动挂断 (BYE)")
	manager.endCallAndHangup(false, true)
	return true
}

func (manager *Manager) endCallAndHangup(sendClientSignal bool, sendATH bool) {
	manager.mu.Lock()
	if manager.state == CallStateIdle {
		manager.mu.Unlock()
		return
	}
	stateSnapshot := manager.state
	controllerCallID := manager.controllerCallID
	manager.state = CallStateIdle
	call := manager.currentCall
	manager.currentCall = nil
	manager.mu.Unlock()

	manager.logger.Info("挂断并清理通话")

	if manager.controller != nil {
		_ = manager.controller.Hangup(context.Background(), controllerCallID, HangupOptions{SendModemSignal: sendATH})
	}
	if call == nil {
		return
	}
	if call.cancelFunc != nil {
		call.cancelFunc()
	}
	if call.audio != nil {
		call.audio.Stop()
	}
	if !sendClientSignal {
		return
	}
	client := manager.registrar.GetClient()
	if call.isOutbound {
		if call.serverReq != nil && stateSnapshot == CallStateConnected {
			bye := sip.NewRequest(sip.BYE, call.serverReq.From().Address)
			sip.CopyHeaders("To", call.serverReq, bye)
			sip.CopyHeaders("From", call.serverReq, bye)
			sip.CopyHeaders("Call-ID", call.serverReq, bye)
			bye.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d BYE", call.serverReq.CSeq().SeqNo+1)))
			_ = client.WriteRequest(bye)
		}
		return
	}
	if call.clientReq == nil || call.clientTx == nil {
		return
	}
	if stateSnapshot == CallStateRinging {
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

// containsCLCCActive 检查 AT+CLCC 返回中是否有已接通的语音通话。
// +CLCC: idx,dir,stat,mode,mpty[,"number",type]
// dir=0(MO 外呼), stat=0(active 已接通), mode=0(voice 语音)。
func containsCLCCActive(response string) bool {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		index := strings.Index(line, ":")
		if index < 0 {
			continue
		}
		fields := strings.Split(line[index+1:], ",")
		if len(fields) < 4 {
			continue
		}
		dir := strings.TrimSpace(fields[1])
		stat := strings.TrimSpace(fields[2])
		mode := strings.TrimSpace(fields[3])
		if dir == "0" && stat == "0" && mode == "0" {
			return true
		}
	}
	return false
}

func hasConnectedCall(calls []CallInfo, callID string) bool {
	for _, call := range calls {
		if call.State != CallStateConnected {
			continue
		}
		if callID == "" || call.ID == "" || call.ID == callID {
			return true
		}
	}
	return false
}

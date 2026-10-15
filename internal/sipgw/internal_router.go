package sipgw

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// RouteInternalInvite 处理同一 VoCat 网内两个软电话之间的 INVITE 路由。
// 主叫 ServerTransaction（callerTx）已在 registrar.handleInvite 中建立，这里
// 构造到被叫的 ClientTransaction（calleeTx），把两条事务的 100/180/183/200/4xx
// /6xx、ACK、BYE、CANCEL 信令双向桥接。RTP 媒体不走 VoCat，两端直接 P2P 对接。
// 本函数同步阻塞，直到通话结束（BYE、CANCEL、超时、或任意一方挂断）。
func RouteInternalInvite(
	ctx context.Context,
	logger *slog.Logger,
	client *sipgo.Client,
	registrar *Registrar,
	callerUser *RegisteredUser,
	calleeUser *RegisteredUser,
	req *sip.Request,
	callerTx sip.ServerTransaction,
) {
	callID := ""
	if h := req.GetHeader("Call-ID"); h != nil {
		callID = h.Value()
	}
	logger.Info("网内 INVITE 路由",
		"from", callerUser.Username, "to", calleeUser.Username,
		"call_id", callID, "callee_contact", calleeUser.Source)

	// 构造到被叫 Contact 的 INVITE。
	forward := sip.NewRequest(sip.INVITE, sip.Uri{
		Scheme: "sip",
		User:   calleeUser.Username,
		Host:   calleeUser.ContactAddr.IP.String(),
		Port:   calleeUser.ContactAddr.Port,
	})
	forward.SetDestination(calleeUser.ContactAddr.String())
	// 传输方式智能选择：
	// - sipgo UDP MTU 阈值是 1300 字节（1500-200），包体接近或超过时发送会失败
	// - 默认跟随被叫注册时的 transport（多数 Linphone 只监听 UDP，强制 TCP 会 connection refused）
	// - 包体超过 1200 字节时优先尝试 TCP（部分 Linphone 未开 TCP，失败则回 503，
	//   此时需在被叫 Linphone 账号设置里把传输协议改为 TCP 或自动）
	packetBytes := len(forward.String())
	transport := calleeUser.Transport
	if packetBytes > 1200 && !strings.EqualFold(transport, "TCP") {
		transport = "TCP"
	}
	forward.SetTransport(transport)
	// 只复制必要头：Via 和 Max-Forwards 让 sipgo 自动生成（避免旧 Via 链膨胀），
	// Route/Record-Route/Service-Route 等运营商头必须丢掉（内部网内路由不走运营商路径）。
	sip.CopyHeaders("From", req, forward)
	sip.CopyHeaders("To", req, forward)
	sip.CopyHeaders("Call-ID", req, forward)
	sip.CopyHeaders("CSeq", req, forward)
	sip.CopyHeaders("Contact", req, forward)
	sip.CopyHeaders("Content-Type", req, forward)
	// 给被叫显示主叫用户名（如 1002），而非完整 sip:1002@vocat.local
	if fh := forward.From(); fh != nil {
		fh.DisplayName = callerUser.Username
	}
	forward.SetBody(req.Body())

	// 调试日志：包大小 + 实际选择的传输方式。
	// sipgo UDP MTU 阈值是 1300 字节，超过会 transaction transport error。
	logger.Info("网内 INVITE 构造完成", "call_id", callID, "packet_bytes", packetBytes,
		"body_bytes", len(req.Body()), "transport", transport, "callee_registered_transport", calleeUser.Transport)

	fwdCtx, fwdCancel := context.WithTimeout(ctx, 30*time.Second)
	defer fwdCancel()

	calleeTx, err := client.TransactionRequest(fwdCtx, forward)
	if err != nil {
		logger.Warn("网内 INVITE 转发失败", "call_id", callID, "err", err)
		if strings.EqualFold(transport, "TCP") && !strings.EqualFold(calleeUser.Transport, "TCP") {
			logger.Warn("被叫 Linphone 未启用 TCP 且包体超过 UDP MTU 限制，请在被叫 Linphone 账号设置的传输协议改为 TCP 或自动",
				"call_id", callID, "packet_bytes", packetBytes, "callee", calleeUser.Username)
		}
		_ = callerTx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
		return
	}

	// 100 Trying 先回主叫，让 Linphone 知道请求已受理。
	_ = callerTx.Respond(sip.NewResponseFromRequest(req, 100, "Trying", nil))

	// 桥接主叫侧 CANCEL → 终止被叫侧事务。
	cancelled := make(chan struct{}, 1)
	if stx, ok := callerTx.(*sip.ServerTx); ok {
		stx.OnCancel(func(_ *sip.Request) {
			select {
			case cancelled <- struct{}{}:
			default:
			}
			calleeTx.Terminate()
		})
	}

	// 跟踪被叫是否已回最终响应（200 或 4xx+），用于判断后续 ACK/BYE 流程。
	var calleeAnswered bool

	// 主循环：桥接被叫响应 → 主叫，以及主叫 ACK/BYE → 被叫。
	// 用 select 同时监听：被叫响应流、主叫 ACK、主叫 BYE、cancelled 信号、事务结束。
	for {
		select {
		case resp, ok := <-calleeTx.Responses():
			if !ok {
				// 被叫侧事务已终结（calleeTx.Terminate 被调用）。
				// 如果还没给主叫回最终响应，回 487。
				if !calleeAnswered {
					_ = callerTx.Respond(sip.NewResponseFromRequest(req, 487, "Request Terminated", nil))
				}
				return
			}
			reason := resp.Reason
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				// 200 OK：被叫接听。透传回主叫，进入 ACK/BYE 阶段。
				calleeAnswered = true
				logger.Info("网内 INVITE 被叫接听，透传 200 给主叫", "call_id", callID)
				callerOK := sip.NewResponseFromRequest(req, resp.StatusCode, reason, resp.Body())
				// 复用被叫的 To tag 作为主叫侧 dialog 标识：
				// 之后被叫挂断时，VoCat 给主叫转发 BYE 必须用同一 tag，主叫才认账。
				if to := callerOK.To(); to != nil {
					if calleeTo := resp.To(); calleeTo != nil {
						if tag, ok := calleeTo.Params.Get("tag"); ok {
							to.Params.Add("tag", tag)
						}
					}
				}
				if err := callerTx.Respond(callerOK); err != nil {
					logger.Warn("透传 200 给主叫失败", "call_id", callID, "err", err)
					calleeTx.Terminate()
					return
				}
				// 等主叫 ACK；通话建立后双方的 BYE 由 registrar.handleBye 拦截转发。
				waitAckAndBye(ctx, logger, client, registrar, callerUser, calleeUser,
					req, forward, resp, callerOK, callerTx, calleeTx, callID)
				return

			case resp.StatusCode >= 300:
				// 3xx/4xx/5xx/6xx：被叫拒绝或失败。透传回主叫，结束。
				calleeAnswered = true
				logger.Info("网内 INVITE 被叫拒绝", "call_id", callID, "code", resp.StatusCode)
				_ = callerTx.Respond(sip.NewResponseFromRequest(req, resp.StatusCode, reason, resp.Body()))
				calleeTx.Terminate()
				return

			default:
				// 1xx（180 Ringing / 183 Session Progress）：透传回主叫。
				logger.Info("网内 INVITE 透传临时响应", "call_id", callID, "code", resp.StatusCode)
				_ = callerTx.Respond(sip.NewResponseFromRequest(req, resp.StatusCode, reason, resp.Body()))
			}

		case <-cancelled:
			// 主叫已 CANCEL；等被叫侧事务收尾，或发 BYE 清场，然后返回。
			logger.Info("网内 INVITE 主叫已 CANCEL，等待被叫侧收尾", "call_id", callID)
			// 给主叫回 487 Request Terminated（如果还没回最终响应）。
			if !calleeAnswered {
				_ = callerTx.Respond(sip.NewResponseFromRequest(req, 487, "Request Terminated", nil))
			}
			// 等被叫侧事务自行结束，最多 5 秒。
			select {
			case <-calleeTx.Done():
			case <-time.After(5 * time.Second):
			}
			return

		case <-callerTx.Done():
			// 主叫侧事务已终结（极少见，可能是网络异常）。
			logger.Warn("网内 INVITE 主叫事务提前终结", "call_id", callID)
			calleeTx.Terminate()
			return

		case <-ctx.Done():
			logger.Warn("网内 INVITE 路由上下文取消", "call_id", callID, "err", ctx.Err())
			calleeTx.Terminate()
			return
		}
	}
}

// waitAckAndBye 在被叫已回 200 OK 后，处理主叫 ACK 并等待通话结束。
// 通话建立后注册网内会话，任意一方的 BYE 由 registrar.handleBye 按 Call-ID 拦截，
// 转发给对侧后关闭会话（sess.done）唤醒本函数返回。
func waitAckAndBye(
	ctx context.Context,
	logger *slog.Logger,
	client *sipgo.Client,
	registrar *Registrar,
	callerUser *RegisteredUser,
	calleeUser *RegisteredUser,
	callerReq *sip.Request,
	calleeReq *sip.Request,
	callee200 *sip.Response,
	callerOK *sip.Response,
	callerTx sip.ServerTransaction,
	calleeTx sip.ClientTransaction,
	callID string,
) {
	// 构造到被叫的 ACK（基于 200 OK 的 Contact）。
	ack := buildAck(calleeReq, callee200)

	// 等待主叫 ACK，最多 30 秒。
	ackTimer := time.NewTimer(30 * time.Second)
	defer ackTimer.Stop()

	ackSent := false
	for !ackSent {
		select {
		case ackReq, ok := <-callerTx.Acks():
			if !ok {
				logger.Warn("网内 INVITE 主叫 ACK 通道关闭", "call_id", callID)
				calleeTx.Terminate()
				return
			}
			// 用我们构造的 ACK 发给被叫（主叫 ACK 不直接转发，因为 Request-URI 是主叫视角的）。
			_ = ackReq
			if err := client.WriteRequest(ack, sipgo.ClientRequestBuild); err != nil {
				logger.Warn("网内 INVITE 发送被叫 ACK 失败", "call_id", callID, "err", err)
			} else {
				logger.Info("网内 INVITE 已发送被叫 ACK", "call_id", callID)
			}
			ackSent = true

		case <-ackTimer.C:
			logger.Warn("网内 INVITE 等待主叫 ACK 超时", "call_id", callID)
			calleeTx.Terminate()
			return

		case <-callerTx.Done():
			logger.Warn("网内 INVITE 主叫侧事务提前终结，补发 ACK", "call_id", callID)
			_ = client.WriteRequest(ack, sipgo.ClientRequestBuild)
			calleeTx.Terminate()
			return

		case <-ctx.Done():
			calleeTx.Terminate()
			return
		}
	}

	// ACK 已送达被叫，通话正式建立。注册网内会话：
	// 任意一方挂断时 registrar.handleBye 按 Call-ID 拦截 BYE，转发给对侧。
	sess := &internalSession{
		callID:     callID,
		callerUser: callerUser,
		calleeUser: calleeUser,
		callerReq:  callerReq,
		callerOK:   callerOK,
		calleeReq:  calleeReq,
		calleeOK:   callee200,
		client:     client,
		logger:     logger,
		done:       make(chan struct{}),
	}
	registrar.registerInternalSession(sess)
	defer registrar.unregisterInternalSession(callID)

	logger.Info("网内通话建立，等待挂断", "call_id", callID,
		"caller", callerUser.Username, "callee", calleeUser.Username)

	// 等待结束信号：任一方 BYE（sess.done）、事务异常终结、或兜底超时。
	select {
	case <-sess.done:
		logger.Info("网内通话已结束（一方挂断，BYE 已转发对侧）", "call_id", callID)
	case <-calleeTx.Done():
		logger.Info("网内通话被叫侧事务终结", "call_id", callID)
	case <-callerTx.Done():
		logger.Info("网内通话主叫侧事务终结", "call_id", callID)
	case <-time.After(4 * time.Hour):
		logger.Info("网内通话兜底超时结束", "call_id", callID)
	case <-ctx.Done():
		logger.Info("网内通话上下文取消", "call_id", callID)
	}
}

// buildAck 构造 ACK 请求，基于 INVITE 请求和 200 OK 响应。
func buildAck(invite *sip.Request, okResp *sip.Response) *sip.Request {
	var recipient sip.Uri
	if contact := okResp.Contact(); contact != nil {
		recipient = sip.Uri{
			Scheme: contact.Address.Scheme,
			User:   contact.Address.User,
			Host:   contact.Address.Host,
			Port:   contact.Address.Port,
		}
	} else {
		recipient = sip.Uri{
			Scheme: invite.Recipient.Scheme,
			User:   invite.Recipient.User,
			Host:   invite.Recipient.Host,
			Port:   invite.Recipient.Port,
		}
	}
	ack := sip.NewRequest(sip.ACK, recipient)
	ack.SipVersion = invite.SipVersion

	// Via：branch 必须新生成。
	origVia := invite.Via()
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

	if header := invite.From(); header != nil {
		ack.AppendHeader(sip.HeaderClone(header))
	}
	if header := okResp.To(); header != nil {
		ack.AppendHeader(sip.HeaderClone(header))
	}
	if header := invite.CallID(); header != nil {
		ack.AppendHeader(sip.HeaderClone(header))
	}
	if header := invite.CSeq(); header != nil {
		cloned := sip.HeaderClone(header).(*sip.CSeqHeader)
		cloned.MethodName = sip.ACK
		ack.AppendHeader(cloned)
	}
	maxForwards := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxForwards)
	ack.SetBody(nil)
	ack.SetTransport(invite.Transport())
	return ack
}

// buildBye 构造 BYE 请求。
func buildBye(dialogReq *sip.Request, dialogResp *sip.Response) *sip.Request {
	var recipient sip.Uri
	if contact := dialogResp.Contact(); contact != nil {
		recipient = sip.Uri{
			Scheme: contact.Address.Scheme,
			User:   contact.Address.User,
			Host:   contact.Address.Host,
			Port:   contact.Address.Port,
		}
	} else {
		recipient = sip.Uri{
			Scheme: dialogReq.Recipient.Scheme,
			User:   dialogReq.Recipient.User,
			Host:   dialogReq.Recipient.Host,
			Port:   dialogReq.Recipient.Port,
		}
	}
	bye := sip.NewRequest(sip.BYE, recipient)
	bye.SipVersion = dialogReq.SipVersion

	origVia := dialogReq.Via()
	viaHop := &sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "UDP",
		Host:            origVia.Host,
		Port:            origVia.Port,
		Params:          sip.NewParams(),
	}
	viaHop.Params.Add("branch", sip.GenerateBranch())
	bye.AppendHeader(viaHop)

	if header := dialogReq.From(); header != nil {
		bye.AppendHeader(sip.HeaderClone(header))
	}
	if header := dialogResp.To(); header != nil {
		bye.AppendHeader(sip.HeaderClone(header))
	}
	if header := dialogReq.CallID(); header != nil {
		bye.AppendHeader(sip.HeaderClone(header))
	}
	if header := dialogReq.CSeq(); header != nil {
		cloned := sip.HeaderClone(header).(*sip.CSeqHeader)
		cloned.MethodName = sip.BYE
		cloned.SeqNo++
		bye.AppendHeader(cloned)
	}
	maxForwards := sip.MaxForwardsHeader(70)
	bye.AppendHeader(&maxForwards)
	bye.SetBody(nil)
	bye.SetTransport(dialogReq.Transport())
	return bye
}

// internalSession 记录一条进行中的网内通话，供 registrar.handleBye 按 Call-ID 拦截 BYE。
// SIP 中 BYE 是独立事务，不会走 INVITE 的事务事件流，必须用会话表把两者关联起来。
type internalSession struct {
	callID     string
	callerUser *RegisteredUser
	calleeUser *RegisteredUser
	callerReq  *sip.Request  // 主叫 INVITE（主叫侧 dialog 的 UAC 请求）
	callerOK   *sip.Response // VoCat 发给主叫的 200 OK（To 复用了被叫 tag）
	calleeReq  *sip.Request  // VoCat 转发给被叫的 INVITE
	calleeOK   *sip.Response // 被叫的 200 OK（To 带被叫 tag）
	client     *sipgo.Client
	logger     *slog.Logger
	done       chan struct{} // 对侧 BYE 转发完成后关闭，唤醒 waitAckAndBye
}

// forwardByeToPeer 一方挂断后，给对侧构造并发送 BYE，让对方 Linphone 立即结束通话。
// bye 是挂断方发来的 BYE 请求，通过其 From 用户名判断挂断方向。
func (s *internalSession) forwardByeToPeer(bye *sip.Request) {
	fromUser := ""
	if from := bye.From(); from != nil {
		fromUser = from.Address.User
	}

	var target *sip.Request
	var peerName string
	if fromUser == s.callerUser.Username {
		// 主叫挂断 → 给被叫发 BYE（被叫侧 dialog：calleeReq + calleeOK）。
		target = buildBye(s.calleeReq, s.calleeOK)
		target.SetDestination(s.calleeUser.ContactAddr.String())
		if s.calleeUser.Transport != "" {
			target.SetTransport(s.calleeUser.Transport)
		}
		peerName = s.calleeUser.Username
	} else {
		// 被叫挂断 → 给主叫发 BYE（主叫侧 dialog：callerReq + callerOK）。
		target = buildByeToCaller(s.callerReq, s.callerOK)
		target.SetDestination(s.callerUser.ContactAddr.String())
		if s.callerUser.Transport != "" {
			target.SetTransport(s.callerUser.Transport)
		}
		peerName = s.callerUser.Username
	}

	// 等对侧回 200，最多 8 秒；超时不影响挂断已生效（BYE 已送达）。
	sendCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := s.client.TransactionRequest(sendCtx, target); err != nil {
		s.logger.Warn("网内通话对侧 BYE 转发失败", "call_id", s.callID, "peer", peerName, "err", err)
	} else {
		s.logger.Info("网内通话对侧 BYE 转发完成，对侧已挂断", "call_id", s.callID, "peer", peerName)
	}
	close(s.done)
}

// buildByeToCaller 构造发给主叫的 BYE（被叫先挂断时）。
// 主叫侧 dialog：callerReq（主叫 INVITE）+ callerOK（VoCat 透传的 200，To 带被叫 tag）。
// UAS→UAC 方向的 in-dialog 请求：From = dialog 的 To（被叫侧），To = dialog 的 From（主叫侧）。
// Via 不手工构造，交给 client.TransactionRequest 按实际连接自动生成。
func buildByeToCaller(callerReq *sip.Request, callerOK *sip.Response) *sip.Request {
	// Request-URI：主叫 Contact；缺失时退回主叫 INVITE 的目标。
	recipient := callerReq.Recipient
	if contact := callerReq.Contact(); contact != nil {
		recipient = sip.Uri{
			Scheme: contact.Address.Scheme,
			User:   contact.Address.User,
			Host:   contact.Address.Host,
			Port:   contact.Address.Port,
		}
	}
	bye := sip.NewRequest(sip.BYE, recipient)
	bye.SipVersion = callerReq.SipVersion

	// From：被叫侧标识（主叫 dialog 的 To + 被叫 tag）。
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip"}}
	if to := callerOK.To(); to != nil {
		from.Address.User = to.Address.User
		from.Address.Host = to.Address.Host
		from.Address.Port = to.Address.Port
		if tag, ok := to.Params.Get("tag"); ok {
			from.Params.Add("tag", tag)
		}
	}
	bye.AppendHeader(from)

	// To：主叫侧标识（主叫 INVITE 的 From + 主叫 tag）。
	to := &sip.ToHeader{}
	if fromH := callerReq.From(); fromH != nil {
		to.Address = fromH.Address
		to.Params = sip.NewParams()
		if tag, ok := fromH.Params.Get("tag"); ok {
			to.Params.Add("tag", tag)
		}
	}
	bye.AppendHeader(to)

	if h := callerReq.CallID(); h != nil {
		bye.AppendHeader(sip.HeaderClone(h))
	}
	if h := callerOK.CSeq(); h != nil {
		cloned := sip.HeaderClone(h).(*sip.CSeqHeader)
		cloned.MethodName = sip.BYE
		cloned.SeqNo++
		bye.AppendHeader(cloned)
	}
	maxForwards := sip.MaxForwardsHeader(70)
	bye.AppendHeader(&maxForwards)
	bye.SetBody(nil)
	return bye
}

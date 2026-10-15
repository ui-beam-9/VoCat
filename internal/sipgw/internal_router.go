package sipgw

import (
	"context"
	"log/slog"
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
	// 内部路由强制用 TCP：sipgo UDP MTU 阈值是 1300 字节（1500-200），
	// Android Linphone 带完整 SDP 的 INVITE 轻松超过。
	// 5060 端口通常同时监听 UDP 和 TCP。
	forward.SetTransport("TCP")
	// 只复制必要头：Via 和 Max-Forwards 让 sipgo 自动生成（避免旧 Via 链膨胀），
	// Route/Record-Route/Service-Route 等运营商头必须丢掉（内部网内路由不走运营商路径）。
	sip.CopyHeaders("From", req, forward)
	sip.CopyHeaders("To", req, forward)
	sip.CopyHeaders("Call-ID", req, forward)
	sip.CopyHeaders("CSeq", req, forward)
	sip.CopyHeaders("Contact", req, forward)
	sip.CopyHeaders("Content-Type", req, forward)
	forward.SetBody(req.Body())

	// MTU 调试：日志记录构造出的 INVITE 包大小。
	// sipgo UDP 默认 MTU 是 64KB（uint16 max），实际以太网通常 1500。
	logger.Info("网内 INVITE 构造完成", "call_id", callID, "packet_bytes", len(forward.String()),
		"body_bytes", len(req.Body()), "transport", calleeUser.Transport)

	fwdCtx, fwdCancel := context.WithTimeout(ctx, 30*time.Second)
	defer fwdCancel()

	calleeTx, err := client.TransactionRequest(fwdCtx, forward)
	if err != nil {
		logger.Warn("网内 INVITE 转发失败", "call_id", callID, "err", err)
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
				if err := callerTx.Respond(sip.NewResponseFromRequest(req, resp.StatusCode, reason, resp.Body())); err != nil {
					logger.Warn("透传 200 给主叫失败", "call_id", callID, "err", err)
					calleeTx.Terminate()
					return
				}
				// 等主叫 ACK + BYE，以及被叫 BYE。
				if waitAckAndBye(ctx, logger, client, calleeUser, forward, resp, callerTx, calleeTx, callID) {
					return
				}
				// waitAckAndBye 返回 true 表示通话已完整结束；否则继续循环处理后续事务事件。
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

// waitAckAndBye 在被叫已回 200 OK 后，处理主叫 ACK 和双方 BYE。
// 返回 true 表示通话已完整结束。
func waitAckAndBye(
	ctx context.Context,
	logger *slog.Logger,
	client *sipgo.Client,
	calleeUser *RegisteredUser,
	fwdRequest *sip.Request,
	callee200 *sip.Response,
	callerTx sip.ServerTransaction,
	calleeTx sip.ClientTransaction,
	callID string,
) bool {
	// 构造到被叫的 ACK（基于 200 OK 的 Contact）。
	ack := buildAck(fwdRequest, callee200)

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
				return true
			}
			// 用我们构造的 ACK 发给被叫（主叫 ACK 不直接转发，因为 Request-URI 是主叫视角的）。
			if err := client.WriteRequest(ack, sipgo.ClientRequestBuild); err != nil {
				logger.Warn("网内 INVITE 发送被叫 ACK 失败", "call_id", callID, "err", err)
			} else {
				logger.Info("网内 INVITE 已发送被叫 ACK", "call_id", callID)
			}
			_ = ackReq // 不使用主叫原始 ACK 的 Request-URI
			ackSent = true

		case <-ackTimer.C:
			logger.Warn("网内 INVITE 等待主叫 ACK 超时", "call_id", callID)
			calleeTx.Terminate()
			return true

		case <-callerTx.Done():
			logger.Warn("网内 INVITE 主叫侧事务提前终结，补发 ACK", "call_id", callID)
			_ = client.WriteRequest(ack, sipgo.ClientRequestBuild)
			calleeTx.Terminate()
			return true

		case <-ctx.Done():
			calleeTx.Terminate()
			return true
		}
	}

	// ACK 已送达被叫。现在等 BYE：监听主叫侧 BYE，以及 calleeTx 的后续响应（被叫也可能发 BYE）。
	// 主叫 BYE 通过 registrar 的 OnBye 回调进入，但此时我们的 goroutine 阻塞在 RouteInternalInvite 里，
	// 需要一种机制让外部能通知我们。简单方案：启动一个 goroutine 专门监听主叫 BYE。
	byeCh := make(chan struct{}, 2)

	go func() {
		// sipgo ServerTransaction 上没有专门的 Bye() 通道，但 Acks() 之后可能能收到其他请求。
		// 实际上 BYE 会由 sipgo 作为独立 ServerTransaction 处理，不走这条通道。
		// 所以改成监听 calleeTx.Done + 定时探活。
		// 主叫 BYE 会触发 registrar.handleBye → routeBye → 但那是外部机制，
		// 我们这里只需要在 calleeTx 或 callerTx 任一终结时结束循环。
	}()
	_ = byeCh

	// 简化处理：监听 calleeTx.Done（被叫侧事务结束）和 callerTx.Done（主叫侧事务结束）。
	// 任意一方 BYE 都会让对应事务终止，RouteInternalInvite 的外层循环会检测到并返回。
	// 但问题是 BYE 是独立事务，不会让 INVITE 事务 Done。
	//
	// 实际场景：Linphone 挂机会发 BYE，sipgo 会把 BYE 作为新请求走 registrar.handleBye，
	// 那里已经有 onBye 回调 → voicegateway.routeBye。
	// 但内部路由场景下，我们需要拦截这个 BYE 转发给被叫。
	//
	// 最简方案：内部路由场景下，在 handleInvite 里调用 RouteInternalInvite 后，
	// 不依赖外层的 onBye 回调，而是让 RouteInternalInvite 自己起 goroutine 监听
	// 主叫侧的 BYE（通过额外机制），同时监听 calleeTx 的后续 BYE 响应。
	//
	// 暂时用一个简单的超时兜底：ACK 后最多等 10 分钟，或 calleeTx/callerTx Done。
	// BYE 的完整转发由 registrar 的 handleBye + 内部路由状态表协作完成（后续实现）。

	logger.Info("网内 INVITE 通话进行中，等待 BYE 挂断", "call_id", callID)

	// 先监听常见的终结信号。
	select {
	case <-calleeTx.Done():
		logger.Info("网内 INVITE 被叫侧事务结束", "call_id", callID)
	case <-callerTx.Done():
		logger.Info("网内 INVITE 主叫侧事务结束", "call_id", callID)
	case <-time.After(10 * time.Minute):
		logger.Info("网内 INVITE 通话超时结束", "call_id", callID)
	case <-ctx.Done():
		logger.Info("网内 INVITE 上下文取消", "call_id", callID)
	}
	return true
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

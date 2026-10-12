package ims

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vocat/internal/vowifi"
)

var (
	ErrCallNotFound = errors.New("ims: call not found")
	ErrCallState    = errors.New("ims: call is not in the required state")
)

const terminalCallRetention = 30 * time.Second

// 通话兜底超时（运营商不回传终态信令时，由本端主动结束，避免网页一直显示通话中）：
//   - outgoingSetupTimeout：呼出后从 INVITE 发出起算，超时仍未收到 200 OK 则 CANCEL，
//     覆盖对方拒接/失败但最终响应（486/603/480）被网络丢弃，呼叫卡在拨号/振铃/early media；
//   - earlyMediaNoRTPTimeout：183 已协商媒体但对端从未发送 RTP；
//   - earlyMediaSilenceTimeout：early media 曾收到 RTP（忙音/提示音）后音频停止；
//   - mediaSilenceTimeout：接通后对端音频停止（BYE 丢失）。
const (
	mediaSilenceTimeout      = 30 * time.Second
	earlyMediaSilenceTimeout = 15 * time.Second
	earlyMediaNoRTPTimeout   = 20 * time.Second
	outgoingSetupTimeout     = 60 * time.Second
)

const (
	mmtelServiceURN = "urn:urn-7:3gpp-service.ims.icsi.mmtel"
	mmtelFeatureTag = "urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel"
)

type imsCall struct {
	public         vowifi.Call
	callID         string
	target         string
	from           string
	to             string
	branch         string
	cseq           uint32
	inviteTarget   string
	invite         *sipRequest
	respond        func([]byte) error
	responses      chan *sipResponse
	remoteTag      string
	routes         []string
	terminated     bool
	media          *rtpMedia
	pracked        map[string]bool
	sessionExpires int
	sessionCancel  context.CancelFunc
	watchdogOnce   sync.Once
	stateSince     time.Time
}

func (session *Session) Calls() []vowifi.Call {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	now := time.Now().UTC()
	calls := make([]vowifi.Call, 0, len(session.calls))
	for id, call := range session.calls {
		if call.public.EndedAt != nil && now.Sub(*call.public.EndedAt) > terminalCallRetention {
			delete(session.calls, id)
			continue
		}
		calls = append(calls, call.public)
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].StartedAt.Before(calls[j].StartedAt) })
	return calls
}

func (session *Session) DialCall(ctx context.Context, number string) (vowifi.Call, error) {
	number = strings.TrimSpace(number)
	if !validCallNumber(number) {
		return vowifi.Call{}, errors.New("ims: invalid dial number")
	}
	callToken, err := randomHex(18)
	if err != nil {
		return vowifi.Call{}, err
	}
	branch, err := randomHex(12)
	if err != nil {
		return vowifi.Call{}, err
	}
	callID := callToken + "@" + addressHost(session.conn.LocalAddr())
	carrierProfile := vowifi.ResolveCarrierProfile(session.request.Identity)
	target := callTargetURI(number, session.identity.domain, carrierProfile)
	session.mu.Lock()
	cseq := session.cseq
	session.cseq++
	routes := append([]string(nil), session.evidence.ServiceRoute...)
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	fromIdentity, preferredIdentity, identitySource := session.callOriginatingIdentitiesLocked(carrierProfile)
	session.mu.Unlock()
	media, err := newRTPMedia(session.localMediaIP())
	if err != nil {
		return vowifi.Call{}, err
	}
	body := media.offerSDP(session.localMediaIP())
	transportUpper := strings.ToUpper(session.transport)
	from := "<" + fromIdentity + ">;tag=" + session.fromTag
	to := "<" + target + ">"
	lines := []string{
		"INVITE " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", transportUpper, session.conn.LocalAddr().String(), branch),
		"Max-Forwards: 70",
	}
	lines = append(lines, securityHeaders...)
	if len(routes) == 0 {
		lines = append(lines, "Route: <sip:"+session.endpoint.address()+";transport="+session.transport+";lr>")
	} else {
		for _, route := range routes {
			lines = append(lines, "Route: "+route)
		}
	}
	lines = append(lines,
		"From: "+from,
		"To: "+to,
		"Call-ID: "+callID,
		fmt.Sprintf("CSeq: %d INVITE", cseq),
		session.dialogContactHeader(),
		"P-Preferred-Identity: <"+preferredIdentity+">",
		"P-Preferred-Service: "+mmtelServiceURN,
		`Accept-Contact: *;+g.3gpp.icsi-ref="`+mmtelFeatureTag+`"`,
	)
	lines = session.appendPAccessNetworkInfoHeader(lines)
	lines = append(lines,
		"User-Agent: "+session.imsUserAgent(),
		"Allow: INVITE, ACK, CANCEL, BYE, OPTIONS, MESSAGE, PRACK, UPDATE, INFO",
		"Supported: 100rel, timer, replaces",
		"Session-Expires: 1800;refresher=uac",
		"Min-SE: 90",
		"Accept: application/sdp",
		"Content-Type: application/sdp",
		"Content-Length: "+strconv.Itoa(len(body)), "", "",
	)
	request := append([]byte(strings.Join(lines, "\r\n")), body...)
	responses := make(chan *sipResponse, 8)
	key := sipTransactionKey{callID: callID, cseq: cseq, method: "INVITE"}
	session.transactionsMu.Lock()
	if _, duplicate := session.transactions[key]; duplicate {
		session.transactionsMu.Unlock()
		_ = media.Close()
		return vowifi.Call{}, errors.New("ims: duplicate call transaction")
	}
	session.transactions[key] = responses
	session.transactionsMu.Unlock()
	call := &imsCall{
		public: vowifi.Call{ID: callID, Number: number, Direction: "outgoing", State: "dialing", StartedAt: time.Now().UTC()},
		callID: callID, target: target, inviteTarget: target, from: from, to: to, branch: branch, cseq: cseq, responses: responses,
		routes: routes, media: media, pracked: make(map[string]bool), stateSince: time.Now().UTC(),
	}
	session.callMu.Lock()
	session.calls[callID] = call
	session.callMu.Unlock()
	if session.provider != nil && session.provider.config.Logger != nil {
		session.provider.config.Logger.Info("IMS call started",
			"category", "call",
			"device_id", session.request.DeviceID,
			"direction", "outgoing",
			"identity_source", identitySource,
			"target_scheme", strings.ToLower(strings.TrimSuffix(strings.SplitN(target, ":", 2)[0], ":")),
		)
	}
	session.writeMu.Lock()
	_, err = session.conn.Write(request)
	session.writeMu.Unlock()
	if err != nil {
		_ = media.Close()
		session.transactionsMu.Lock()
		delete(session.transactions, key)
		session.transactionsMu.Unlock()
		session.callMu.Lock()
		delete(session.calls, callID)
		session.callMu.Unlock()
		return vowifi.Call{}, fmt.Errorf("ims: send SIP INVITE: %w", err)
	}
	go session.watchOutgoingCall(call, key)
	// 呼出即启动兜底监控：即使对端只回 180 Ringing（无 SDP）、或拒接最终响应被丢弃，
	// 也能靠建立总超时主动 CANCEL，而不是死等 SIP 事务超时。
	session.startCallWatchdog(call)
	return call.public, nil
}

func (session *Session) watchOutgoingCall(call *imsCall, key sipTransactionKey) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	defer func() {
		session.transactionsMu.Lock()
		delete(session.transactions, key)
		session.transactionsMu.Unlock()
	}()
	for {
		select {
		case <-session.refreshContext.Done():
			return
		case <-timer.C:
			if session.callWasTerminated(call.callID) {
				session.finishCall(call.callID, "ended", 0, "")
				return
			}
			session.finishCall(call.callID, "failed", 0, "SIP INVITE transaction timed out")
			return
		case response := <-call.responses:
			if response == nil {
				continue
			}
			diagnostic := callResponseDiagnostic(response)
			session.logCallResponse(response, diagnostic)
			if response.StatusCode < 200 {
				session.setCallDiagnostic(call.callID, response.StatusCode, diagnostic)
				session.updateCallDialogFromResponse(call, response)
				if len(response.Body) > 0 {
					if mediaErr := call.media.configureRemote(response.Body); mediaErr == nil {
						session.setCallMediaReady(call.callID)
						session.setCallState(call.callID, "early_media")
						session.startCallWatchdog(call)
					}
				} else if response.StatusCode >= 180 {
					// 183(带 SDP) 与 180 可能乱序到达：一旦进入 early_media 就不再回退为 ringing，
					// 否则振铃期媒体静默兜底失效（对端拒接后 RTP 停止无法被检测）。
					session.advanceCallToRinging(call.callID)
				}
				if reliableProvisional(response) {
					go session.sendPRACK(call, response)
				}
				continue
			}
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				session.updateCallDialogFromResponse(call, response)
				// 2xx 响应可能不携带 SDP：媒体往往已在 183 Session Progress 中协商。
				// 仅在响应确实带有 SDP 时才重新协商，否则空 SDP 会被误判为失败并立即挂断。
				if len(response.Body) > 0 {
					if mediaErr := call.media.configureRemote(response.Body); mediaErr != nil {
						_ = session.sendACK(call)
						session.finishCall(call.callID, "failed", response.StatusCode, mediaErr.Error())
						go func() {
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							_ = session.sendDialogRequest(ctx, call, "BYE")
						}()
						return
					}
				}
				_ = session.sendACK(call)
				session.setCallMediaReady(call.callID)
				session.setCallState(call.callID, "active")
				session.startSessionTimer(call, response.value("Session-Expires"))
				session.startCallWatchdog(call)
			} else {
				if ackErr := session.sendRejectedInviteACK(call, response); ackErr != nil && session.provider != nil && session.provider.config.Logger != nil {
					session.provider.config.Logger.Warn("IMS rejected INVITE ACK failed",
						"category", "call",
						"device_id", session.request.DeviceID,
						"carrier_profile", vowifi.ResolveCarrierProfile(session.request.Identity).ID,
						"sip_status", response.StatusCode,
						"error", safeSIPDiagnostic(ackErr.Error()),
					)
				}
				if session.callWasTerminated(call.callID) {
					// CANCEL normally causes the pending INVITE transaction to finish
					// with 487 Request Terminated.  It is the expected response to our
					// local hang-up, not a new network rejection.
					session.finishCall(call.callID, "ended", response.StatusCode, diagnostic)
				} else {
					session.finishCall(call.callID, "failed", response.StatusCode, diagnostic)
				}
			}
			return
		}
	}
}

func (session *Session) AnswerCall(_ context.Context, id string) (vowifi.Call, error) {
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallNotFound
	}
	if call.public.Direction != "incoming" || call.public.State != "ringing" || call.invite == nil || call.respond == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallState
	}
	request, respond := call.invite, call.respond
	session.callMu.Unlock()
	response, err := buildSIPResponseWithBody(
		request,
		200,
		session.fromTag,
		call.media.answerSDP(session.localMediaIP()),
		session.dialogContactHeader(),
	)
	if err != nil {
		return vowifi.Call{}, err
	}
	if err := respond(response); err != nil {
		return vowifi.Call{}, err
	}
	session.setCallState(id, "active")
	session.startSessionTimer(call, request.value("Session-Expires"))
	if call.media.ready() {
		session.setCallMediaReady(id)
	}
	session.startCallWatchdog(call)
	session.callMu.Lock()
	result := call.public
	session.callMu.Unlock()
	return result, nil
}

func (session *Session) HangupCall(ctx context.Context, id string) error {
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return ErrCallNotFound
	}
	state := call.public.State
	direction := call.public.Direction
	request, respond := call.invite, call.respond
	call.terminated = true
	session.callMu.Unlock()
	if direction == "incoming" && state == "ringing" && request != nil && respond != nil {
		response, err := buildSIPResponseWithBody(request, 486, session.fromTag, nil)
		if err != nil {
			return err
		}
		if err := respond(response); err != nil {
			return err
		}
		session.finishCall(id, "ended", 0, "")
		return nil
	}
	method := "BYE"
	if direction == "outgoing" && (state == "dialing" || state == "ringing" || state == "early_media") {
		method = "CANCEL"
	}
	err := session.sendDialogRequest(ctx, call, method)
	// A remote endpoint may already have removed the dialog and answer BYE with
	// 481. The local call must still leave the active list after a hang-up.
	session.finishCall(id, "ended", 0, "")
	return err
}

// SendDTMF 通过 SIP INFO（application/dtmf-relay）向对端发送单个按键音，
// 用于在接通后响应「请按 1/2/3 选择业务」这类交互式语音菜单。
func (session *Session) SendDTMF(ctx context.Context, id, digit string) error {
	digit = strings.TrimSpace(digit)
	if len(digit) != 1 || !strings.ContainsRune("0123456789*#", rune(digit[0])) {
		return errors.New("ims: invalid DTMF digit")
	}
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return ErrCallNotFound
	}
	state := call.public.State
	session.callMu.Unlock()
	if state != "active" {
		return ErrCallState
	}
	body := []byte("Signal=" + digit + "\r\nDuration=160\r\n")
	return session.sendDialogBody(ctx, call, "INFO", "application/dtmf-relay", body, nil)
}

// SetCallHold 通过 re-INVITE 把通话置为保持（SDP a=sendonly）或恢复（a=sendrecv）。
func (session *Session) SetCallHold(ctx context.Context, id string, hold bool) (vowifi.Call, error) {
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallNotFound
	}
	if call.public.State != "active" || call.media == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallState
	}
	sessionExpires := call.sessionExpires
	session.callMu.Unlock()

	body := call.media.holdSDP(session.localMediaIP(), hold)
	extra := []string(nil)
	if sessionExpires > 0 {
		extra = append(extra, fmt.Sprintf("Session-Expires: %d;refresher=uac", sessionExpires))
	}
	session.mu.Lock()
	cseq := session.cseq
	session.cseq++
	session.mu.Unlock()
	request := session.buildDialogMessage(call, "INVITE", cseq, "application/sdp", extra, body)
	response, err := session.exchangeRuntime(ctx, request, sipTransactionKey{callID: call.callID, cseq: cseq, method: "INVITE"})
	if err != nil {
		return vowifi.Call{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return vowifi.Call{}, fmt.Errorf("ims: SIP re-INVITE rejected with %d", response.StatusCode)
	}
	// 2xx 的 re-INVITE 必须用相同 CSeq 回 ACK，否则对端会持续重传最终响应。
	ack := session.buildDialogMessage(call, "ACK", cseq, "", nil, nil)
	session.writeMu.Lock()
	_, _ = session.conn.Write(ack)
	session.writeMu.Unlock()

	session.callMu.Lock()
	if current := session.calls[id]; current != nil {
		current.public.Held = hold
		result := current.public
		session.callMu.Unlock()
		return result, nil
	}
	session.callMu.Unlock()
	return vowifi.Call{}, ErrCallNotFound
}

// sendDialogBody 发送带正文的对话内请求并等待 2xx 响应。
func (session *Session) sendDialogBody(
	ctx context.Context,
	call *imsCall,
	method string,
	contentType string,
	body []byte,
	extraHeaders []string,
) error {
	session.mu.Lock()
	cseq := session.cseq
	session.cseq++
	session.mu.Unlock()
	request := session.buildDialogMessage(call, method, cseq, contentType, extraHeaders, body)
	response, err := session.exchangeRuntime(ctx, request, sipTransactionKey{callID: call.callID, cseq: cseq, method: method})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("ims: SIP %s rejected with %d", method, response.StatusCode)
	}
	return nil
}

func (session *Session) callWasTerminated(id string) bool {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	call := session.calls[id]
	return call != nil && call.terminated
}

func (session *Session) handleCallRequest(request *sipRequest, respond func([]byte) error) bool {
	switch request.Method {
	case "INVITE":
		callID := strings.TrimSpace(request.value("Call-ID"))
		if callID == "" {
			return true
		}
		session.callMu.Lock()
		existing := session.calls[callID]
		session.callMu.Unlock()
		if existing != nil && existing.public.State == "active" {
			return session.handleDialogOffer(request, respond, existing)
		}
		number := identityNumber(request.value("From"))
		target := headerURI(request.value("Contact"))
		if target == "" {
			target = request.URI
		}
		media, err := newRTPMedia(session.localMediaIP())
		if err != nil {
			if response, buildErr := buildSIPResponseWithBody(request, 488, session.fromTag, nil); buildErr == nil {
				_ = respond(response)
			}
			return true
		}
		if len(request.Body) > 0 {
			if err := media.configureRemote(request.Body); err != nil {
				_ = media.Close()
				if response, buildErr := buildSIPResponseWithBody(request, 488, session.fromTag, nil); buildErr == nil {
					_ = respond(response)
				}
				return true
			}
		}
		call := &imsCall{
			public: vowifi.Call{ID: callID, Number: number, Direction: "incoming", State: "ringing", StartedAt: time.Now().UTC()},
			callID: callID, target: target, inviteTarget: request.URI, from: request.value("To") + ";tag=" + session.fromTag,
			to: request.value("From"), invite: request, respond: respond, routes: request.values("Record-Route"), media: media,
			pracked: make(map[string]bool), stateSince: time.Now().UTC(),
		}
		session.callMu.Lock()
		session.calls[callID] = call
		session.callMu.Unlock()
		if session.provider != nil && session.provider.config.Logger != nil {
			session.provider.config.Logger.Info("IMS incoming call received",
				"category", "call",
				"device_id", session.request.DeviceID,
				"caller", call.public.Number,
				"call_id", call.public.ID,
			)
		}
		if session.provider != nil && session.provider.config.OnIncomingCall != nil {
			calledNumber := identityNumber(request.value("To"))
			if calledNumber == "" {
				calledNumber = session.identity.public
			}
			receivedCall := ReceivedCall{
				DeviceID:  session.request.DeviceID,
				IMSI:      session.request.Identity.IMSI,
				CallID:    callID,
				Caller:    number,
				Called:    calledNumber,
				Timestamp: time.Now().UTC(),
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = session.provider.config.OnIncomingCall(ctx, receivedCall)
			}()
		}
		response, err := buildSIPResponseWithBody(request, 180, session.fromTag, nil)
		if err == nil {
			_ = respond(response)
		}
		return true
	case "PRACK":
		response, err := buildSIPResponseWithBody(request, 200, session.fromTag, nil)
		if err == nil {
			_ = respond(response)
		}
		return true
	case "UPDATE":
		callID := strings.TrimSpace(request.value("Call-ID"))
		session.callMu.Lock()
		call := session.calls[callID]
		session.callMu.Unlock()
		if call == nil {
			return false
		}
		return session.handleDialogOffer(request, respond, call)
	case "ACK":
		callID := strings.TrimSpace(request.value("Call-ID"))
		session.callMu.Lock()
		call := session.calls[callID]
		session.callMu.Unlock()
		if call != nil && call.media != nil && !call.media.ready() && len(request.Body) > 0 {
			if err := call.media.configureRemote(request.Body); err != nil {
				session.finishCall(callID, "failed", 0, err.Error())
			} else {
				session.setCallMediaReady(callID)
			}
		}
		return true
	case "CANCEL", "BYE":
		callID := strings.TrimSpace(request.value("Call-ID"))
		session.callMu.Lock()
		call := session.calls[callID]
		session.callMu.Unlock()
		if call == nil {
			// 对端挂断（BYE/CANCEL）但本地没有对应通话：记录以便排查"网页仍显示通话中"。
			if session.provider != nil && session.provider.config.Logger != nil {
				session.provider.config.Logger.Warn("IMS inbound call teardown did not match a call",
					"category", "call",
					"event", "call.remote_teardown_unmatched",
					"device_id", session.request.DeviceID,
					"method", request.Method,
					"call_id", callID,
				)
			}
			return false
		}
		response, err := buildSIPResponseWithBody(request, 200, session.fromTag, nil)
		if err == nil {
			_ = respond(response)
		}
		if request.Method == "CANCEL" {
			if call.invite != nil && call.respond != nil {
				if terminated, buildErr := buildSIPResponseWithBody(call.invite, 487, session.fromTag, nil); buildErr == nil {
					_ = call.respond(terminated)
				}
			}
		}
		session.finishCall(callID, "ended", 0, "")
		if session.provider != nil && session.provider.config.Logger != nil {
			session.provider.config.Logger.Info("IMS call ended by remote",
				"category", "call",
				"event", "call.remote_teardown",
				"device_id", session.request.DeviceID,
				"method", request.Method,
				"call_id", callID,
			)
		}
		return true
	default:
		return false
	}
}

func (session *Session) sendACK(call *imsCall) error {
	request := session.buildDialogRequest(call, "ACK", call.cseq)
	session.writeMu.Lock()
	_, err := session.conn.Write(request)
	session.writeMu.Unlock()
	return err
}

// sendRejectedInviteACK acknowledges a non-2xx final response using the
// original INVITE transaction branch and request URI. Unlike a 2xx ACK this is
// part of the INVITE transaction; sending a dialog-style ACK with a new branch
// leaves the P-CSCF retransmitting the rejection and leaking transaction state.
func (session *Session) sendRejectedInviteACK(call *imsCall, response *sipResponse) error {
	if call == nil || response == nil || response.StatusCode < 300 {
		return nil
	}
	if session == nil || session.conn == nil {
		return errors.New("ims: SIP connection unavailable for rejected INVITE ACK")
	}
	target := call.inviteTarget
	if target == "" {
		target = call.target
	}
	to := strings.TrimSpace(response.value("To"))
	if to == "" {
		to = call.to
	}
	lines := []string{
		"ACK " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", strings.ToUpper(session.transport), session.conn.LocalAddr().String(), call.branch),
		"Max-Forwards: 70",
	}
	session.mu.Lock()
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	session.mu.Unlock()
	lines = append(lines, securityHeaders...)
	for _, route := range call.routes {
		lines = append(lines, "Route: "+route)
	}
	lines = append(lines,
		"From: "+call.from,
		"To: "+to,
		"Call-ID: "+call.callID,
		fmt.Sprintf("CSeq: %d ACK", call.cseq),
	)
	lines = session.appendPAccessNetworkInfoHeader(lines)
	lines = append(lines,
		"User-Agent: "+session.imsUserAgent(),
		"Content-Length: 0", "", "",
	)
	session.writeMu.Lock()
	_, err := session.conn.Write([]byte(strings.Join(lines, "\r\n")))
	session.writeMu.Unlock()
	return err
}

func reliableProvisional(response *sipResponse) bool {
	if response == nil || strings.TrimSpace(response.value("RSeq")) == "" {
		return false
	}
	for _, token := range strings.Split(strings.ToLower(response.value("Require")), ",") {
		if strings.TrimSpace(token) == "100rel" {
			return true
		}
	}
	return false
}

func (session *Session) updateCallDialogFromResponse(call *imsCall, response *sipResponse) {
	if call == nil || response == nil {
		return
	}
	session.callMu.Lock()
	defer session.callMu.Unlock()
	call.to = response.value("To")
	call.remoteTag = headerParameter(call.to, "tag")
	if contact := headerURI(response.value("Contact")); contact != "" {
		call.target = contact
	}
	if recordRoutes := response.values("Record-Route"); len(recordRoutes) > 0 {
		call.routes = reverseStrings(recordRoutes)
	}
}

func (session *Session) sendPRACK(call *imsCall, response *sipResponse) {
	rseq := strings.TrimSpace(response.value("RSeq"))
	inviteCSeq := strings.TrimSpace(response.value("CSeq"))
	if rseq == "" || inviteCSeq == "" {
		return
	}
	key := rseq + "|" + inviteCSeq
	session.callMu.Lock()
	if call.pracked == nil {
		call.pracked = make(map[string]bool)
	}
	if call.pracked[key] || call.public.EndedAt != nil {
		session.callMu.Unlock()
		return
	}
	call.pracked[key] = true
	target, to, from := call.target, call.to, call.from
	routes := append([]string(nil), call.routes...)
	session.callMu.Unlock()

	session.mu.Lock()
	cseq := session.cseq
	session.cseq++
	session.mu.Unlock()
	branch, _ := randomHex(12)
	lines := []string{
		"PRACK " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", strings.ToUpper(session.transport), session.conn.LocalAddr().String(), branch),
		"Max-Forwards: 70",
	}
	session.mu.Lock()
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	session.mu.Unlock()
	lines = append(lines, securityHeaders...)
	for _, route := range routes {
		lines = append(lines, "Route: "+route)
	}
	lines = append(lines,
		"From: "+from, "To: "+to, "Call-ID: "+call.callID,
		fmt.Sprintf("CSeq: %d PRACK", cseq), "RAck: "+rseq+" "+inviteCSeq,
	)
	lines = session.appendPAccessNetworkInfoHeader(lines)
	lines = append(lines,
		"User-Agent: "+session.imsUserAgent(),
		"Content-Length: 0", "", "",
	)
	ctx, cancel := context.WithTimeout(session.refreshContext, 10*time.Second)
	defer cancel()
	result, err := session.exchangeRuntime(ctx, []byte(strings.Join(lines, "\r\n")), sipTransactionKey{callID: call.callID, cseq: cseq, method: "PRACK"})
	if err != nil || result.StatusCode < 200 || result.StatusCode >= 300 {
		reason := "reliable provisional response could not be acknowledged"
		if err != nil {
			reason = err.Error()
		}
		session.finishCall(call.callID, "failed", 0, reason)
	}
}

func (session *Session) handleDialogOffer(request *sipRequest, respond func([]byte) error, call *imsCall) bool {
	var body []byte
	if len(request.Body) > 0 {
		if err := call.media.configureRemote(request.Body); err != nil {
			if response, buildErr := buildSIPResponseWithBody(request, 488, session.fromTag, nil); buildErr == nil {
				_ = respond(response)
			}
			return true
		}
		body = call.media.answerSDP(session.localMediaIP())
		session.setCallMediaReady(call.callID)
	}
	extraHeaders := []string(nil)
	if request.Method == "INVITE" {
		extraHeaders = append(extraHeaders, session.dialogContactHeader())
	}
	response, err := buildSIPResponseWithBody(request, 200, session.fromTag, body, extraHeaders...)
	if err == nil {
		_ = respond(response)
		session.startSessionTimer(call, request.value("Session-Expires"))
	}
	return true
}

func (session *Session) startSessionTimer(call *imsCall, header string) {
	value := strings.TrimSpace(strings.Split(header, ";")[0])
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 90 || seconds > 86400 || call == nil {
		return
	}
	ctx, cancel := context.WithCancel(session.refreshContext)
	session.callMu.Lock()
	if call.sessionCancel != nil {
		call.sessionCancel()
	}
	call.sessionExpires = seconds
	call.sessionCancel = cancel
	session.callMu.Unlock()
	go func() {
		interval := time.Duration(seconds) * time.Second / 2
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				refreshContext, cancelRefresh := context.WithTimeout(ctx, 15*time.Second)
				refreshErr := session.sendDialogRequest(refreshContext, call, "UPDATE")
				cancelRefresh()
				if refreshErr != nil {
					session.finishCall(call.callID, "failed", 0, "SIP session refresh failed")
					return
				}
				timer.Reset(interval)
			}
		}
	}()
}

func (session *Session) sendDialogRequest(ctx context.Context, call *imsCall, method string) error {
	cseq := call.cseq
	if method == "BYE" || method == "UPDATE" {
		session.mu.Lock()
		cseq = session.cseq
		session.cseq++
		session.mu.Unlock()
	}
	request := session.buildDialogRequest(call, method, cseq)
	if method == "ACK" {
		session.writeMu.Lock()
		_, err := session.conn.Write(request)
		session.writeMu.Unlock()
		return err
	}
	response, err := session.exchangeRuntime(ctx, request, sipTransactionKey{callID: call.callID, cseq: cseq, method: method})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("ims: SIP %s rejected with %d", method, response.StatusCode)
	}
	return nil
}

func (session *Session) buildDialogRequest(call *imsCall, method string, cseq uint32) []byte {
	return session.buildDialogMessage(call, method, cseq, "", nil, nil)
}

// buildDialogMessage 构造对话内请求，支持附带正文（INFO 的 DTMF、re-INVITE 的 SDP）。
func (session *Session) buildDialogMessage(
	call *imsCall,
	method string,
	cseq uint32,
	contentType string,
	extraHeaders []string,
	body []byte,
) []byte {
	branch, _ := randomHex(12)
	if method == "CANCEL" {
		branch = call.branch
	}
	target := call.target
	if method == "CANCEL" && call.inviteTarget != "" {
		target = call.inviteTarget
	}
	to := call.to
	if to == "" {
		to = "<" + call.target + ">"
	}
	lines := []string{
		method + " " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", strings.ToUpper(session.transport), session.conn.LocalAddr().String(), branch),
		"Max-Forwards: 70",
	}
	session.mu.Lock()
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	session.mu.Unlock()
	lines = append(lines, securityHeaders...)
	for _, route := range call.routes {
		lines = append(lines, "Route: "+route)
	}
	lines = append(lines,
		"From: "+call.from,
		"To: "+to,
		"Call-ID: "+call.callID,
		fmt.Sprintf("CSeq: %d %s", cseq, method),
		"Supported: 100rel, timer",
		"User-Agent: "+session.imsUserAgent(),
	)
	if method != "CANCEL" {
		lines = session.appendPAccessNetworkInfoHeader(lines)
	}
	if method == "UPDATE" || (method == "INVITE" && len(body) > 0) {
		lines = append(lines, session.dialogContactHeader())
	}
	if method == "UPDATE" && call.sessionExpires > 0 {
		lines = append(lines, fmt.Sprintf("Session-Expires: %d;refresher=uac", call.sessionExpires))
	}
	for _, header := range extraHeaders {
		if strings.TrimSpace(header) != "" {
			lines = append(lines, header)
		}
	}
	if contentType != "" {
		lines = append(lines, "Content-Type: "+contentType)
	}
	lines = append(lines, "Content-Length: "+strconv.Itoa(len(body)), "", "")
	message := []byte(strings.Join(lines, "\r\n"))
	if len(body) == 0 {
		return message
	}
	return append(message, body...)
}

func (session *Session) localMediaIP() net.IP {
	var localAddress net.Addr
	if session.conn != nil {
		localAddress = session.conn.LocalAddr()
	}
	return addressIP(localAddress)
}

func buildSIPResponseWithBody(request *sipRequest, status int, tag string, body []byte, extraHeaders ...string) ([]byte, error) {
	reasons := map[int]string{180: "Ringing", 200: "OK", 486: "Busy Here", 487: "Request Terminated", 488: "Not Acceptable Here"}
	reason := reasons[status]
	if reason == "" {
		return nil, errors.New("ims: unsupported call response status")
	}
	via := request.values("Via")
	from, to := request.value("From"), request.value("To")
	callID, cseq := request.value("Call-ID"), request.value("CSeq")
	if len(via) == 0 || from == "" || to == "" || callID == "" || cseq == "" {
		return nil, errors.New("ims: call request omitted a mandatory response header")
	}
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + tag
	}
	lines := []string{fmt.Sprintf("SIP/2.0 %d %s", status, reason)}
	for _, value := range via {
		lines = append(lines, "Via: "+value)
	}
	lines = append(lines, "From: "+from, "To: "+to, "Call-ID: "+callID, "CSeq: "+cseq)
	for _, header := range extraHeaders {
		if strings.TrimSpace(header) != "" {
			lines = append(lines, header)
		}
	}
	if value := strings.TrimSpace(request.value("Session-Expires")); value != "" && status >= 200 && status < 300 && (request.Method == "INVITE" || request.Method == "UPDATE") {
		lines = append(lines, "Supported: timer", "Session-Expires: "+value)
	}
	if len(body) > 0 {
		lines = append(lines, "Content-Type: application/sdp")
	}
	lines = append(lines, "Content-Length: "+strconv.Itoa(len(body)), "", "")
	return append([]byte(strings.Join(lines, "\r\n")), body...), nil
}

func (session *Session) dialogContactHeader() string {
	if session == nil || session.conn == nil || strings.TrimSpace(session.identity.user) == "" {
		return ""
	}
	if session.imsRegisterOptions().ContactFormat == vowifi.IMSContactFormatGSMA {
		contact := "Contact: <sip:" + session.contactAddress() + `>;+g.3gpp.icsi-ref="` + mmtelFeatureTag + `"`
		if strings.TrimSpace(session.instanceID) != "" {
			contact += `;+sip.instance="<` + session.instanceID + `>"`
		}
		return contact
	}
	contact := "Contact: <sip:" + session.identity.user + "@" + session.contactAddress() + ";transport=" + session.transport + ">"
	if strings.TrimSpace(session.instanceID) != "" {
		contact += `;+sip.instance="<` + session.instanceID + `>"`
	}
	return contact + `;audio;+g.3gpp.icsi-ref="` + mmtelFeatureTag + `"`
}

func callTargetURI(number, domain string, profile vowifi.CarrierProfile) string {
	domain = strings.TrimSpace(domain)
	if profile.IMSDialURIScheme == "sip" {
		target := "sip:" + number + "@" + domain
		if profile.IMSUserEqPhone {
			target += ";user=phone"
		}
		return target
	}
	if strings.HasPrefix(number, "+") {
		return "tel:" + number
	}
	return "tel:" + number + ";phone-context=" + domain
}

// callOriginatingIdentitiesLocked selects only a number that IMS explicitly
// associated with this registration. 3GPP originating sessions use that
// public identity in both From and P-Preferred-Identity; some TAS deployments
// accept an IMSI IMPU at the P-CSCF and then terminate the session immediately.
// The fallback deliberately remains the registered IMPU and never derives a
// telephone number from IMSI digits.
func (session *Session) callOriginatingIdentitiesLocked(profile vowifi.CarrierProfile) (from, preferred, source string) {
	if number, numberSource, ok := vowifi.ExtractAssociatedMSISDN(session.evidence); ok {
		from = "sip:" + number + "@" + session.identity.domain
		if profile.IMSUserEqPhone {
			from += ";user=phone"
		}
		return from, "tel:" + number, numberSource
	}
	return session.identity.public, session.identity.public, "registered_impu"
}

func (session *Session) pAccessNetworkInfo() string {
	if session == nil {
		return ""
	}
	if session.paniResolved {
		return session.pani
	}
	return resolveSessionPAccessNetworkInfo(session.request.Identity, session.imsLogger())
}

func (session *Session) appendPAccessNetworkInfoHeader(lines []string) []string {
	if pani := session.pAccessNetworkInfo(); pani != "" {
		return append(lines, "P-Access-Network-Info: "+pani)
	}
	return lines
}

func callResponseDiagnostic(response *sipResponse) string {
	if response == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if reason := safeSIPDiagnostic(response.Reason); reason != "" {
		parts = append(parts, reason)
	}
	for _, name := range []string{"Reason", "Warning"} {
		for _, value := range response.values(name) {
			if value = safeSIPDiagnostic(value); value != "" {
				parts = append(parts, name+": "+value)
			}
		}
	}
	return safeSIPDiagnostic(strings.Join(parts, "; "))
}

func (session *Session) logCallResponse(response *sipResponse, diagnostic string) {
	if session == nil || session.provider == nil || session.provider.config.Logger == nil || response == nil {
		return
	}
	session.provider.config.Logger.Info("IMS call response",
		"category", "call",
		"device_id", session.request.DeviceID,
		"carrier_profile", vowifi.ResolveCarrierProfile(session.request.Identity).ID,
		"sip_status", response.StatusCode,
		"diagnostic", diagnostic,
		"content_type", safeSIPDiagnostic(response.value("Content-Type")),
		"body_bytes", len(response.Body),
	)
}

// advanceCallToRinging 仅把 dialing 状态的呼叫推进为 ringing；已进入 early_media 的呼叫
// 不因后续无 SDP 的 1xx（如乱序到达的 180 Ringing）而降级，保持振铃期媒体检测生效。
func (session *Session) advanceCallToRinging(id string) {
	session.callMu.Lock()
	if call := session.calls[id]; call != nil && call.public.State == "dialing" {
		call.public.State = "ringing"
		call.stateSince = time.Now().UTC()
	}
	session.callMu.Unlock()
}

func (session *Session) setCallState(id, state string) {
	session.callMu.Lock()
	if call := session.calls[id]; call != nil {
		if call.public.State != state {
			call.stateSince = time.Now().UTC()
		}
		call.public.State = state
		if state == "active" && call.public.AnsweredAt == nil {
			now := time.Now().UTC()
			call.public.AnsweredAt = &now
		}
		if state != "ended" && state != "failed" {
			call.public.EndedAt = nil
		}
	}
	session.callMu.Unlock()
}

func (session *Session) setCallDiagnostic(id string, code int, reason string) {
	session.callMu.Lock()
	if call := session.calls[id]; call != nil {
		call.public.SIPCode = code
		call.public.Reason = safeSIPDiagnostic(reason)
	}
	session.callMu.Unlock()
}

func (session *Session) setCallMediaReady(id string) {
	session.callMu.Lock()
	if call := session.calls[id]; call != nil && call.media != nil {
		call.public.MediaReady = call.media.ready()
		call.public.Codec = call.media.Codec()
	}
	session.callMu.Unlock()
}

func (session *Session) CallMedia(_ context.Context, id string) (vowifi.CallMedia, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	call := session.calls[id]
	if call == nil {
		return nil, ErrCallNotFound
	}
	if call.public.State != "active" || call.media == nil || !call.media.ready() {
		return nil, ErrCallState
	}
	return call.media, nil
}

// startCallWatchdog 启动一次通话的兜底监控（每通电话只启动一次）。
func (session *Session) startCallWatchdog(call *imsCall) {
	if call == nil || session == nil || session.refreshContext == nil {
		return
	}
	call.watchdogOnce.Do(func() {
		go session.watchCallMedia(call)
	})
}

// watchCallMedia 监控通话存活，覆盖运营商不回传终态信令（拒接最终响应/BYE 丢失）的场景：
//   - 呼出未接通（dialing/ringing/early_media）超过 outgoingSetupTimeout 仍未收到 200 OK：主动 CANCEL；
//   - early_media 已协商媒体但对端从未发送 RTP，超过 earlyMediaNoRTPTimeout：主动 CANCEL；
//   - early_media/active 曾收到 RTP 后音频停止：分别按 15s/30s 主动 CANCEL/BYE。
//
// 本地保持（Held）或对端 SDP 不发送音频（recvonly/inactive）时跳过媒体类判定，避免误挂断。
// 来电振铃（incoming/ringing）不启动本监控，不受呼出建立超时影响。
func (session *Session) watchCallMedia(call *imsCall) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-session.refreshContext.Done():
			return
		case <-ticker.C:
		}
		session.callMu.Lock()
		current := session.calls[call.callID]
		if current == nil || current.public.EndedAt != nil || current.public.Held {
			session.callMu.Unlock()
			return
		}
		state := current.public.State
		direction := current.public.Direction
		media := current.media
		stateSince := current.stateSince
		startedAt := current.public.StartedAt
		session.callMu.Unlock()
		if state != "dialing" && state != "ringing" && state != "early_media" && state != "active" {
			return
		}

		// 逐类判定是否需要主动结束；event 非空表示触发。
		event, reason := "", ""
		method := "BYE"
		if direction == "outgoing" && state != "active" && time.Since(startedAt) > outgoingSetupTimeout {
			// 呼叫建立总超时：对方拒接/失败的最终响应被网络丢弃时的最终兜底（与真实手机行为一致）。
			event = "call.setup_timeout"
			reason = "outgoing call setup timed out without final response"
			method = "CANCEL"
		} else if state == "early_media" || state == "active" {
			if media != nil && media.ready() && media.RemoteSendsAudio() {
				last := media.LastInbound()
				switch {
				case state == "early_media" && last.IsZero() && time.Since(stateSince) > earlyMediaNoRTPTimeout:
					// 183 已协商媒体但对端一直不发 RTP，媒体通道实际未建立。
					event = "call.early_media_no_rtp"
					reason = "early media negotiated but no inbound RTP"
					method = "CANCEL"
				case !last.IsZero() && time.Since(last) > mediaTimeoutForState(state):
					// 曾收到对端音频（忙音/提示音/通话语音）后音频停止。
					event = "call.media_silence"
					reason = "remote audio stopped"
					if state == "early_media" {
						method = "CANCEL"
					}
				}
			}
		}
		if event == "" {
			continue
		}

		if session.provider != nil && session.provider.config.Logger != nil {
			session.provider.config.Logger.Info("IMS call ended by watchdog",
				"category", "call",
				"event", event,
				"device_id", session.request.DeviceID,
				"call_id", call.callID,
				"state", state,
			)
		}
		// 标记为本地主动结束：CANCEL 后迟到的 487 Request Terminated 会被识别为本地挂断，
		// 而不是覆盖成网络拒接（failed）。
		session.callMu.Lock()
		if existing := session.calls[call.callID]; existing != nil {
			existing.terminated = true
		}
		session.callMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = session.sendDialogRequest(ctx, current, method)
		cancel()
		session.finishCall(call.callID, "ended", 0, reason)
		return
	}
}

func mediaTimeoutForState(state string) time.Duration {
	if state == "early_media" {
		return earlyMediaSilenceTimeout
	}
	return mediaSilenceTimeout
}

func (session *Session) finishCall(id, state string, code int, reason string) {
	now := time.Now().UTC()
	var media *rtpMedia
	session.callMu.Lock()
	if call := session.calls[id]; call != nil {
		media = call.media
		call.public.State = state
		if code != 0 {
			call.public.SIPCode = code
		}
		if reason = safeSIPDiagnostic(reason); reason != "" {
			call.public.Reason = reason
		}
		call.public.EndedAt = &now
		if call.sessionCancel != nil {
			call.sessionCancel()
			call.sessionCancel = nil
		}
	}
	session.callMu.Unlock()
	if media != nil {
		_ = media.Close()
	}
}

func validCallNumber(value string) bool {
	if len(value) < 2 || len(value) > 32 {
		return false
	}
	for index, character := range value {
		if character >= '0' && character <= '9' || index == 0 && character == '+' || character == '*' || character == '#' {
			continue
		}
		return false
	}
	return true
}

func identityNumber(value string) string {
	value = strings.TrimSpace(value)
	if start := strings.Index(value, "<"); start >= 0 {
		if end := strings.Index(value[start:], ">"); end > 0 {
			value = value[start+1 : start+end]
		}
	}
	value = strings.TrimPrefix(value, "sip:")
	value = strings.TrimPrefix(value, "tel:")
	if at := strings.Index(value, "@"); at >= 0 {
		value = value[:at]
	}
	return strings.TrimSpace(value)
}

func headerParameter(value, name string) string {
	needle := ";" + strings.ToLower(name) + "="
	lower := strings.ToLower(value)
	index := strings.Index(lower, needle)
	if index < 0 {
		return ""
	}
	value = value[index+len(needle):]
	if end := strings.IndexAny(value, ";,> \t"); end >= 0 {
		value = value[:end]
	}
	return strings.Trim(value, `"`)
}

func headerURI(value string) string {
	value = strings.TrimSpace(value)
	if start := strings.Index(value, "<"); start >= 0 {
		if end := strings.Index(value[start+1:], ">"); end >= 0 {
			return strings.TrimSpace(value[start+1 : start+1+end])
		}
	}
	if end := strings.Index(value, ";"); end >= 0 {
		value = value[:end]
	}
	if strings.HasPrefix(strings.ToLower(value), "sip:") || strings.HasPrefix(strings.ToLower(value), "tel:") {
		return strings.TrimSpace(value)
	}
	return ""
}

func reverseStrings(values []string) []string {
	result := append([]string(nil), values...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

var _ vowifi.CallController = (*Session)(nil)
var _ vowifi.CallMediaController = (*Session)(nil)

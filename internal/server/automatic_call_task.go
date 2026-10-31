package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

const (
	// automaticCallPollInterval 是自动任务等待通话状态变化的默认采样周期。
	automaticCallPollInterval = time.Second
	// automaticCallSetupWindow 是拨号后等待「真实呼出」（振铃/早期媒体/接通）的最长时间。
	// 略大于 IMS 层的 outgoingSetupTimeout（60 秒），让 IMS 自身的兜底 CANCEL 与终态优先发生。
	automaticCallSetupWindow = 70 * time.Second
	// automaticCallHangupWindow 是主动挂断后等待终态落定的最长时间：
	// 通话记录依赖 EndedAt/AnsweredAt，必须等终态写入后再落库。
	automaticCallHangupWindow = 5 * time.Second
	// automaticCallSettleInterval 是挂断后等待终态的采样周期。
	automaticCallSettleInterval = 200 * time.Millisecond
	// automaticCallCleanupTimeout 是挂断收尾的独立超时，避免占用任务总预算。
	automaticCallCleanupTimeout = 15 * time.Second
	// automaticCallObserveFailureLimit 是连续采样失败多少次后判定运行时不可用。
	automaticCallObserveFailureLimit = 5
	// automaticCallCellularPollInterval 是 cellular 呼叫的采样周期。
	// 比 VoWiFi 慢一些，减少与既有 CLCC 轮询/AT 队列的竞争。
	automaticCallCellularPollInterval = 2 * time.Second
	// automaticCallSetupBudget 是 call 任务在通话时长之外预留的准备/建立/收尾预算。
	automaticCallSetupBudget = 3 * time.Minute
	// automaticCallTaskMaxRuntime 是 call 任务总运行时间的上限。
	automaticCallTaskMaxRuntime = 20 * time.Minute
)

// automaticCallObservation 是一次通话状态采样。
type automaticCallObservation struct {
	Found    bool
	State    string
	Placed   bool
	Answered bool
	Finished bool
	Reason   string
	SIPCode  int
}

// automaticCallOutcome 是一次自动任务通话的最终结果。
type automaticCallOutcome struct {
	Placed          bool
	Answered        bool
	Finished        bool
	State           string
	Reason          string
	SIPCode         int
	Elapsed         time.Duration
	AnsweredElapsed time.Duration
	HangupIssued    bool
	HangupError     error
	SetupTimedOut   bool
}

// automaticCallPlan 描述一次自动任务通话的观测方式与主动挂断方式。
// 零值窗口/周期使用上面的默认常量，测试可注入更短的值。
type automaticCallPlan struct {
	duration     time.Duration
	setupWindow  time.Duration
	pollInterval time.Duration
	settleWindow time.Duration
	// observe 采样当前通话状态；返回 error 表示设备或 IMS 运行时当前不可用。
	observe func(ctx context.Context) (automaticCallObservation, error)
	// hangup 主动结束通话。
	hangup func(ctx context.Context) error
}

func (plan automaticCallPlan) setup() time.Duration {
	if plan.setupWindow > 0 {
		return plan.setupWindow
	}
	return automaticCallSetupWindow
}

func (plan automaticCallPlan) interval() time.Duration {
	if plan.pollInterval > 0 {
		return plan.pollInterval
	}
	return automaticCallPollInterval
}

func (plan automaticCallPlan) settle() time.Duration {
	if plan.settleWindow > 0 {
		return plan.settleWindow
	}
	return automaticCallHangupWindow
}

// automaticTaskRuntimeLimit 返回单个任务运行允许占用的最长时间。
// call 任务需要覆盖「建立通话 + 保持设定时长 + 收尾」，否则 600 秒的通话会被
// 固定的 8 分钟总超时截断。
func automaticTaskRuntimeLimit(task store.AutomaticTask) time.Duration {
	limit := automaticTaskMaxRuntime
	if task.TaskType != "call" {
		return limit
	}
	var payload automaticTaskPayload
	_ = json.Unmarshal(task.Payload, &payload)
	duration := time.Duration(payload.DurationSeconds) * time.Second
	if duration < 0 {
		duration = 0
	}
	if callLimit := duration + automaticCallSetupBudget + 2*time.Minute; callLimit > limit {
		limit = callLimit
	}
	if limit > automaticCallTaskMaxRuntime {
		limit = automaticCallTaskMaxRuntime
	}
	return limit
}

// executeAutomaticCall 完成一次真实的自动通话：拨号、等待网络真实呼出、
// 保持设定时长后主动挂断，并把结果写入通话记录。
//
// 只有真正进入振铃/接通才算成功；环境还原（defer）必须等到本函数返回之后，
// 否则 VoWiFi 生命周期操作会拆掉正在建立的 IMS 会话，导致「假成功、无呼出」。
func (s *Server) executeAutomaticCall(ctx context.Context, task store.AutomaticTask, payload automaticTaskPayload, progress automaticTaskProgress) (string, error) {
	config, err := s.store.Device(ctx, task.DeviceID)
	if err != nil {
		return "", err
	}
	_, physicalID, present := s.physicalForConfig(config)
	if !present {
		return "", errors.New("configured device is offline")
	}
	number := strings.TrimSpace(payload.Phone)
	duration := time.Duration(payload.DurationSeconds) * time.Second
	if duration < time.Second || duration > maxCallDuration {
		// 任务配置非法（含历史脏数据）：重试也不会变好，直接判失败。
		return "", automaticTaskExecutionError{
			err:       fmt.Errorf("automatic hang-up duration must be between 1 and %d seconds", int(maxCallDuration/time.Second)),
			retryable: false,
		}
	}
	progress("正在拨打 " + number)
	// 在拨号前锁定传输方式，保证拨号与后续等待/挂断走同一条链路。
	transport := s.callTransport(config.ID)
	callID, err := s.dialAutomaticCall(ctx, config, physicalID, number)
	if err != nil {
		return "", err
	}
	if transport == "vowifi" {
		return s.runAutomaticVoWiFiCall(ctx, config.ID, callID, number, duration, progress)
	}
	return s.runAutomaticCellularCall(ctx, physicalID, number, duration, progress)
}

// dialAutomaticCall 通过统一的拨号接口发起呼叫。
// duration_seconds 固定为 0：自动任务自己负责保持时长与挂断，避免出现两个计时器
// 互相干扰，也避免任务收尾与环境还原早于通话结束。
func (s *Server) dialAutomaticCall(ctx context.Context, config store.Device, physicalID, number string) (string, error) {
	body, _ := json.Marshal(map[string]any{"number": number, "duration_seconds": 0})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/devices/calls/dial", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.handleCallAction(recorder, request, config, physicalID, "dial")
	if recorder.Code < 200 || recorder.Code >= 300 {
		return "", fmt.Errorf("dial failed (HTTP %d): %s", recorder.Code, compactAutomaticResponse(recorder.Body.Bytes()))
	}
	var response struct {
		Data struct {
			CallID string `json:"call_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	return strings.TrimSpace(response.Data.CallID), nil
}

// waitForAutomaticCall 等待通话真实呼出、保持设定时长、主动挂断并确认终态。
func waitForAutomaticCall(ctx context.Context, plan automaticCallPlan, progress automaticTaskProgress) (automaticCallOutcome, error) {
	dialedAt := time.Now()
	setupWindow := plan.setup()
	ticker := time.NewTicker(plan.interval())
	defer ticker.Stop()
	outcome := automaticCallOutcome{}
	observeFailures := 0
	announced := false
	for {
		elapsed := time.Since(dialedAt)
		observation, observeErr := plan.observe(ctx)
		if observeErr != nil {
			observeFailures++
			// 拨号刚完成时 modem/IMS 可能暂时读不到通话状态，容忍少量采样失败；
			// 但放弃观测前必须尽力挂断，避免把已拨出的电话留在通话中。
			if observeFailures >= automaticCallObserveFailureLimit {
				outcome.HangupError = hangupWithCleanupTimeout(ctx, plan)
				outcome.Elapsed = time.Since(dialedAt)
				return outcome, observeErr
			}
		} else {
			observeFailures = 0
			applyAutomaticCallObservation(&outcome, observation, dialedAt)
			if observation.Finished {
				outcome.Finished = true
				outcome.Elapsed = time.Since(dialedAt)
				return outcome, nil
			}
			if outcome.Placed && !announced {
				announced = true
				remaining := plan.duration - elapsed
				if remaining < 0 {
					remaining = 0
				}
				progress(fmt.Sprintf("对方已振铃，%d 秒后自动挂断", int(remaining/time.Second)))
			}
		}
		if !outcome.Placed && elapsed >= setupWindow {
			// 建立超时：网络上没有出现振铃/接通，不能算成功。
			outcome.SetupTimedOut = true
			outcome.HangupError = hangupWithCleanupTimeout(ctx, plan)
			outcome.Elapsed = time.Since(dialedAt)
			return outcome, nil
		}
		if outcome.Placed && elapsed >= plan.duration {
			progress("正在自动挂断")
			outcome.HangupIssued = true
			// 挂断用独立超时，避免 SIP 事务等待吃掉任务剩余预算。
			outcome.HangupError = hangupWithCleanupTimeout(ctx, plan)
			settleAutomaticCall(ctx, dialedAt, plan, &outcome)
			if !outcome.Finished {
				// 迟到的 2xx 可能把通话重新拉回 active，或对端未确认挂断：
				// 再补一次挂断，仍无法确认终态就不能谎报成功。
				if retryErr := hangupWithCleanupTimeout(ctx, plan); retryErr != nil && outcome.HangupError == nil {
					outcome.HangupError = retryErr
				}
				settleAutomaticCall(ctx, dialedAt, plan, &outcome)
			}
			outcome.Elapsed = time.Since(dialedAt)
			return outcome, nil
		}
		select {
		case <-ctx.Done():
			outcome.HangupError = hangupWithCleanupTimeout(ctx, plan)
			outcome.Elapsed = time.Since(dialedAt)
			return outcome, ctx.Err()
		case <-ticker.C:
		}
	}
}

func applyAutomaticCallObservation(outcome *automaticCallOutcome, observation automaticCallObservation, dialedAt time.Time) {
	outcome.State = observation.State
	outcome.Reason = observation.Reason
	outcome.SIPCode = observation.SIPCode
	if observation.Placed {
		outcome.Placed = true
	}
	if observation.Answered {
		if !outcome.Answered {
			outcome.AnsweredElapsed = time.Since(dialedAt)
		}
		outcome.Answered = true
	}
}

// settleAutomaticCall 在主动挂断后等待终态写入，保证通话记录能读到 EndedAt/AnsweredAt。
func settleAutomaticCall(ctx context.Context, dialedAt time.Time, plan automaticCallPlan, outcome *automaticCallOutcome) {
	window := plan.settle()
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), window)
	defer cancel()
	wait := automaticCallSettleInterval
	if window < wait {
		wait = window
	}
	deadline := time.Now().Add(window)
	for {
		observation, err := plan.observe(settleCtx)
		if err == nil {
			applyAutomaticCallObservation(outcome, observation, dialedAt)
			if observation.Finished {
				outcome.Finished = true
				return
			}
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-settleCtx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// hangupWithCleanupTimeout 用独立超时执行挂断：任务超时/取消后也能尽力收尾，
// 正常挂断时也不会被 SIP 事务等待吃掉任务预算。
func hangupWithCleanupTimeout(ctx context.Context, plan automaticCallPlan) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), automaticCallCleanupTimeout)
	defer cancel()
	return plan.hangup(cleanup)
}

func (s *Server) runAutomaticVoWiFiCall(ctx context.Context, deviceID, callID, number string, duration time.Duration, progress automaticTaskProgress) (string, error) {
	controller, ok := s.vowifi.(VoWiFiCallController)
	if !ok {
		return "", errors.New("VoWiFi voice calling is unavailable")
	}
	targetID := callID
	var latest vowifi.Call
	plan := automaticCallPlan{
		duration: duration,
		observe: func(context.Context) (automaticCallObservation, error) {
			call, found, err := findAutomaticVoWiFiCall(controller, deviceID, targetID, number)
			if err != nil {
				return automaticCallObservation{}, err
			}
			if !found {
				return automaticCallObservation{State: "unknown"}, nil
			}
			latest = call
			if targetID == "" {
				targetID = call.ID
			}
			state := strings.TrimSpace(call.State)
			return automaticCallObservation{
				Found: true,
				State: state,
				// AnsweredAt 不信任采样时机：接通后立刻结束（秒接秒挂）时，
				// 唯一一次采样可能已经是 ended，但通话确实发生过。
				Placed:   state == "ringing" || state == "early_media" || state == "active" || call.AnsweredAt != nil,
				Answered: state == "active" || call.AnsweredAt != nil,
				Finished: state == "ended" || state == "failed",
				Reason:   call.Reason,
				SIPCode:  call.SIPCode,
			}, nil
		},
		hangup: func(hangupCtx context.Context) error {
			if targetID == "" {
				return errors.New("no outgoing call to hang up")
			}
			return controller.HangupCall(hangupCtx, deviceID, targetID)
		},
	}
	outcome, waitErr := waitForAutomaticCall(ctx, plan, progress)
	if strings.TrimSpace(latest.ID) != "" {
		s.recordAutomaticCallHistory(ctx, deviceID, latest)
	}
	if waitErr != nil {
		// 已经拿到 Call-ID（INVITE 已发出）就无法排除通话仍在进行，
		// 重试会重复呼叫同一被叫。
		return "", automaticCallWaitError(number, outcome, targetID != "", waitErr)
	}
	return automaticCallResult(number, outcome)
}

func (s *Server) runAutomaticCellularCall(ctx context.Context, physicalID, number string, duration time.Duration, progress automaticTaskProgress) (string, error) {
	seenCall := false
	plan := automaticCallPlan{
		duration:     duration,
		pollInterval: automaticCallCellularPollInterval,
		observe: func(observeCtx context.Context) (automaticCallObservation, error) {
			response, err := s.devices.ExecuteAT(observeCtx, physicalID, "AT+CLCC")
			if err != nil {
				return automaticCallObservation{}, err
			}
			state, answered, placed, found := outgoingCellularCallState(parseCLCC(response))
			if !found {
				if seenCall {
					// 已经出现过本次呼出的通话，随后从 CLCC 消失即表示通话结束。
					return automaticCallObservation{State: "ended", Finished: true}, nil
				}
				return automaticCallObservation{State: "dialing"}, nil
			}
			seenCall = true
			return automaticCallObservation{State: state, Placed: placed, Answered: answered}, nil
		},
		hangup: func(hangupCtx context.Context) error {
			response, err := s.devices.ExecuteAT(hangupCtx, physicalID, "ATH")
			if err != nil {
				return err
			}
			if !strings.EqualFold(strings.TrimSpace(response.Final), "OK") {
				return fmt.Errorf("modem did not accept hang-up (final %q)", strings.TrimSpace(response.Final))
			}
			return nil
		},
	}
	outcome, waitErr := waitForAutomaticCall(ctx, plan, progress)
	if waitErr != nil {
		// ATD 已被 modem 接受，无法排除通话仍在进行，重试会重复呼叫。
		return "", automaticCallWaitError(number, outcome, true, waitErr)
	}
	return automaticCallResult(number, outcome)
}

// automaticCallWaitError 把等待过程中的错误映射为任务错误。
// 一旦确认「呼叫已发出」或「已真实呼出」，重试就可能造成重复呼叫，必须不可重试。
func automaticCallWaitError(number string, outcome automaticCallOutcome, callIssued bool, waitErr error) error {
	message := fmt.Errorf("拨打 %s 未能完成：%w", number, waitErr)
	if outcome.Placed || callIssued {
		return automaticTaskExecutionError{err: message, retryable: false}
	}
	return message
}

// automaticCallResult 把通话结果转换为任务输出或错误。
func automaticCallResult(number string, outcome automaticCallOutcome) (string, error) {
	if !outcome.Placed {
		return "", automaticCallNotPlacedError(number, outcome)
	}
	if !outcome.Finished {
		return "", automaticTaskExecutionError{
			err:       fmt.Errorf("已呼出 %s，但未能确认挂断，通话可能仍在进行", number),
			retryable: false,
		}
	}
	text := automaticCallSuccessText(number, outcome)
	if outcome.HangupError != nil {
		// 终态已确认（本地 dialog 已结束），但挂断信令本身报错：如实提示，不判失败。
		text = fmt.Sprintf("%s（挂断信令异常：%v）", text, outcome.HangupError)
	}
	return text, nil
}

// findAutomaticVoWiFiCall 按 Call-ID 查找本次呼出的通话；
// 个别运营商/流程下拨号响应拿不到 Call-ID 时，回退为「最近一次同号码呼出」。
func findAutomaticVoWiFiCall(controller VoWiFiCallController, deviceID, callID, number string) (vowifi.Call, bool, error) {
	calls, err := controller.Calls(deviceID)
	if err != nil {
		return vowifi.Call{}, false, err
	}
	if callID != "" {
		for _, call := range calls {
			if call.ID == callID {
				return call, true, nil
			}
		}
		return vowifi.Call{}, false, nil
	}
	number = strings.TrimSpace(number)
	matched := false
	var latest vowifi.Call
	for _, call := range calls {
		if call.Direction != "outgoing" || strings.TrimSpace(call.Number) != number {
			continue
		}
		if !matched || call.StartedAt.After(latest.StartedAt) {
			latest = call
			matched = true
		}
	}
	return latest, matched, nil
}

// outgoingCellularCallState 从 AT+CLCC 结果中找出本次主叫通话的状态。
// 3GPP 27.007：direction 0 为主叫（MO）；state 0/1 已接通，2 拨号中（尚未振铃），3 振铃中。
// 拨号中不算「真实呼出」，否则会在网络尚未寻呼被叫时误判成功。
func outgoingCellularCallState(calls []map[string]any) (state string, answered, placed, found bool) {
	for _, call := range calls {
		if automaticCallIntField(call, "direction") != 0 {
			continue
		}
		switch automaticCallIntField(call, "state") {
		case 0, 1:
			return "active", true, true, true
		case 2:
			return "dialing", false, false, true
		case 3:
			return "ringing", false, true, true
		}
	}
	return "", false, false, false
}

func automaticCallIntField(call map[string]any, key string) int {
	switch value := call[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(value))
		return parsed
	}
	return 0
}

// recordAutomaticCallHistory 把自动任务通话写入通话记录。
// 通话记录器依赖 2 秒轮询当前 IMS 会话，任务收尾会很快还原环境并销毁会话，
// 因此自动任务必须自己落库（AppendCallHistory 按 Call-ID 去重）。
func (s *Server) recordAutomaticCallHistory(ctx context.Context, deviceID string, call vowifi.Call) {
	entry, ok := callHistoryEntryFromCall(deviceID, call, time.Now().UTC())
	if !ok {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := s.store.AppendCallHistory(recordCtx, entry); err != nil {
		s.logger.Warn("记录自动任务通话失败", "device_id", deviceID, "call_id", call.ID, "error", err)
	}
}

func automaticCallNotPlacedError(number string, outcome automaticCallOutcome) error {
	if outcome.SetupTimedOut {
		return fmt.Errorf("拨打 %s 未取得网络振铃响应（%d 秒内未真实呼出）", number, int(automaticCallSetupWindow/time.Second))
	}
	detail := strings.TrimSpace(outcome.Reason)
	if outcome.SIPCode != 0 && detail != "" {
		return fmt.Errorf("拨打 %s 未真实呼出（SIP %d %s）", number, outcome.SIPCode, detail)
	}
	if outcome.SIPCode != 0 {
		return fmt.Errorf("拨打 %s 未真实呼出（SIP %d）", number, outcome.SIPCode)
	}
	if detail != "" {
		return fmt.Errorf("拨打 %s 未真实呼出（%s）", number, detail)
	}
	return fmt.Errorf("拨打 %s 未真实呼出", number)
}

func automaticCallSuccessText(number string, outcome automaticCallOutcome) string {
	seconds := int(outcome.Elapsed / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	if outcome.Answered {
		talk := int((outcome.Elapsed - outcome.AnsweredElapsed) / time.Second)
		if talk < 0 {
			talk = 0
		}
		if outcome.HangupIssued {
			return fmt.Sprintf("已拨打 %s，已接通，通话 %d 秒后自动挂断", number, talk)
		}
		return fmt.Sprintf("已拨打 %s，已接通，通话 %d 秒后对方挂断", number, talk)
	}
	if outcome.Finished {
		return fmt.Sprintf("已拨打 %s，振铃 %d 秒后通话结束", number, seconds)
	}
	return fmt.Sprintf("已拨打 %s，振铃 %d 秒后自动挂断", number, seconds)
}

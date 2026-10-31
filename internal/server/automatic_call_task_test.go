package server

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// fakeAutomaticCallVoWiFi 在 fakeVoWiFiController 之上补齐通话能力，
// 用状态机推进通话，并记录拨号/挂断/环境还原的先后顺序。
type fakeAutomaticCallVoWiFi struct {
	fakeVoWiFiController
	mu         sync.Mutex
	calls      []vowifi.Call
	events     []string
	nextState  func(call vowifi.Call) vowifi.Call
	dialErr    error
	hangupErr  error
	hangupEnds bool
}

func (controller *fakeAutomaticCallVoWiFi) record(event string) {
	controller.mu.Lock()
	controller.events = append(controller.events, event)
	controller.mu.Unlock()
}

func (controller *fakeAutomaticCallVoWiFi) eventList() []string {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return append([]string(nil), controller.events...)
}

func (controller *fakeAutomaticCallVoWiFi) Calls(string) ([]vowifi.Call, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.nextState != nil && len(controller.calls) > 0 {
		controller.calls[0] = controller.nextState(controller.calls[0])
	}
	return append([]vowifi.Call(nil), controller.calls...), nil
}

func (controller *fakeAutomaticCallVoWiFi) DialCall(_ context.Context, _ string, number string) (vowifi.Call, error) {
	if controller.dialErr != nil {
		return vowifi.Call{}, controller.dialErr
	}
	call := vowifi.Call{
		ID: "call-1", Number: number, Direction: "outgoing",
		State: "dialing", StartedAt: time.Now().UTC(),
	}
	controller.mu.Lock()
	controller.calls = []vowifi.Call{call}
	controller.events = append(controller.events, "dial")
	controller.mu.Unlock()
	return call, nil
}

func (*fakeAutomaticCallVoWiFi) AnswerCall(context.Context, string, string) (vowifi.Call, error) {
	return vowifi.Call{}, nil
}

func (controller *fakeAutomaticCallVoWiFi) HangupCall(_ context.Context, _, id string) error {
	controller.mu.Lock()
	controller.events = append(controller.events, "hangup")
	// 真实 IMS 的 HangupCall 即使信令发送失败也会本地 finishCall，
	// hangupEnds 模拟这一行为，用于验证「终态已确认时不谎报失败」。
	ends := controller.hangupEnds || controller.hangupErr == nil
	if ends {
		now := time.Now().UTC()
		for index := range controller.calls {
			if controller.calls[index].ID == id {
				controller.calls[index].State = "ended"
				controller.calls[index].EndedAt = &now
			}
		}
	}
	err := controller.hangupErr
	controller.mu.Unlock()
	return err
}

func (*fakeAutomaticCallVoWiFi) SendDTMF(context.Context, string, string, string) error { return nil }

func (*fakeAutomaticCallVoWiFi) SetCallHold(_ context.Context, _ string, id string, hold bool) (vowifi.Call, error) {
	return vowifi.Call{ID: id, State: "active", Held: hold}, nil
}

func (controller *fakeAutomaticCallVoWiFi) RequestEnabled(id string, enabled bool) (vowifi.State, error) {
	controller.record("enable:" + strconv.FormatBool(enabled))
	return controller.fakeVoWiFiController.RequestEnabled(id, enabled)
}

func (controller *fakeAutomaticCallVoWiFi) RequestReconnect(id string) (vowifi.State, error) {
	controller.record("reconnect")
	return controller.fakeVoWiFiController.RequestReconnect(id)
}

func newAutomaticCallVoWiFi() *fakeAutomaticCallVoWiFi {
	return &fakeAutomaticCallVoWiFi{fakeVoWiFiController: fakeVoWiFiController{
		state: vowifi.State{
			DeviceID: "dev1", Enabled: true, Active: true, IMSReady: true, ICCID: "test-card",
		},
	}}
}

// newAutomaticCallTaskServer 构造一台已上线、IMS 就绪的 VoWiFi 设备。
func newAutomaticCallTaskServer(t *testing.T, controller *fakeAutomaticCallVoWiFi) *Server {
	t.Helper()
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	entry := device.Device{
		ID: "dev1", Discovered: true,
		Snapshot: &device.Snapshot{DeviceID: "dev1", ICCID: "test-card"},
	}
	server := &Server{
		store:               database,
		logger:              regionTestLogger(),
		maxRequestBodyBytes: 4096,
		devices:             fakeDeviceController{entry: entry},
		vowifi:              controller,
	}
	if err := database.UpsertDevice(context.Background(), store.Device{ID: "dev1", Name: "VoWiFi"}); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	return server
}

func automaticCallTaskFixture() store.AutomaticTask {
	return store.AutomaticTask{
		DeviceID: "dev1", ProfileICCID: "test-card", TaskType: "call", Environment: "vowifi",
		Payload: []byte(`{"phone":"+447700900123","duration_seconds":1}`),
	}
}

func eventIndex(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}

// 回归用例：拨号后必须先真实呼出并挂断，环境还原（VoWiFi 重连/启停）只能发生在挂断之后。
// 修复前 executeAutomaticCall 提交 INVITE 就返回，defer 立即重连 VoWiFi 把通话拆掉，
// 任务却显示成功。
func TestAutomaticCallTaskHangsUpBeforeRestoringEnvironment(t *testing.T) {
	controller := newAutomaticCallVoWiFi()
	controller.nextState = func(call vowifi.Call) vowifi.Call {
		if call.State == "dialing" {
			call.State = "ringing"
		}
		return call
	}
	server := newAutomaticCallTaskServer(t, controller)

	output, err := server.executeAutomaticTask(context.Background(), automaticCallTaskFixture(), func(string) {})
	if err != nil {
		t.Fatalf("executeAutomaticTask error = %v", err)
	}
	if !strings.Contains(output, "已拨打 +447700900123") || !strings.Contains(output, "振铃") {
		t.Fatalf("output = %q, want 已拨打 + 振铃", output)
	}

	events := controller.eventList()
	hangupAt := eventIndex(events, "hangup")
	restoreAt := eventIndex(events, "reconnect")
	if restoreAt < 0 {
		restoreAt = eventIndex(events, "enable:false")
	}
	if hangupAt < 0 {
		t.Fatalf("events = %v, want a hang-up", events)
	}
	if restoreAt < 0 {
		t.Fatalf("events = %v, want environment restore", events)
	}
	if hangupAt > restoreAt {
		t.Fatalf("events = %v, environment restored before hang-up", events)
	}

	entries, err := server.store.ListCallHistory(context.Background(), "dev1", 10)
	if err != nil {
		t.Fatalf("ListCallHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("call history = %d entries, want 1", len(entries))
	}
	if entries[0].Number != "+447700900123" || entries[0].Direction != "outgoing" || entries[0].Outcome != "missed" {
		t.Fatalf("call history entry = %+v", entries[0])
	}
}

func TestAutomaticCallTaskRecordsAnsweredCall(t *testing.T) {
	controller := newAutomaticCallVoWiFi()
	controller.nextState = func(call vowifi.Call) vowifi.Call {
		switch call.State {
		case "dialing":
			call.State = "ringing"
		case "ringing":
			call.State = "active"
			answeredAt := time.Now().UTC()
			call.AnsweredAt = &answeredAt
		}
		return call
	}
	server := newAutomaticCallTaskServer(t, controller)

	output, err := server.executeAutomaticTask(context.Background(), automaticCallTaskFixture(), func(string) {})
	if err != nil {
		t.Fatalf("executeAutomaticTask error = %v", err)
	}
	if !strings.Contains(output, "已接通") {
		t.Fatalf("output = %q, want 已接通", output)
	}
	entries, err := server.store.ListCallHistory(context.Background(), "dev1", 10)
	if err != nil {
		t.Fatalf("ListCallHistory: %v", err)
	}
	if len(entries) != 1 || entries[0].Outcome != "answered" {
		t.Fatalf("call history = %+v, want one answered entry", entries)
	}
}

// 秒接秒挂：两次采样之间完成「接通 + 结束」，唯一一次采样就是 ended。
// 只要 AnsweredAt 存在就必须算真实呼出，不能误判成未呼出。
func TestAutomaticCallTaskSucceedsWhenAnsweredAndEndedBeforeSampling(t *testing.T) {
	controller := newAutomaticCallVoWiFi()
	controller.nextState = func(call vowifi.Call) vowifi.Call {
		answeredAt := time.Now().UTC()
		now := time.Now().UTC().Add(time.Millisecond)
		return vowifi.Call{
			ID: call.ID, Number: call.Number, Direction: "outgoing", State: "ended",
			StartedAt: call.StartedAt, AnsweredAt: &answeredAt, EndedAt: &now,
		}
	}
	server := newAutomaticCallTaskServer(t, controller)

	output, err := server.executeAutomaticTask(context.Background(), automaticCallTaskFixture(), func(string) {})
	if err != nil {
		t.Fatalf("executeAutomaticTask error = %v (output %q), want success", err, output)
	}
	entries, listErr := server.store.ListCallHistory(context.Background(), "dev1", 10)
	if listErr != nil {
		t.Fatalf("ListCallHistory: %v", listErr)
	}
	if len(entries) != 1 || entries[0].Outcome != "answered" {
		t.Fatalf("call history = %+v, want one answered entry", entries)
	}
}

// 回归用例：网络没有让通话真实呼出（例如 487）时必须失败，不能显示成功。
func TestAutomaticCallTaskFailsWhenCallIsNeverPlaced(t *testing.T) {
	controller := newAutomaticCallVoWiFi()
	controller.nextState = func(call vowifi.Call) vowifi.Call {
		call.State, call.SIPCode, call.Reason = "failed", 487, "Request Terminated"
		return call
	}
	server := newAutomaticCallTaskServer(t, controller)

	output, err := server.executeAutomaticTask(context.Background(), automaticCallTaskFixture(), func(string) {})
	if err == nil {
		t.Fatalf("executeAutomaticTask succeeded with output %q, want failure", output)
	}
	if !strings.Contains(err.Error(), "487") {
		t.Fatalf("error = %v, want SIP 487 detail", err)
	}
	var executionError automaticTaskExecutionError
	if errors.As(err, &executionError) {
		t.Fatal("未真实呼出的失败应当允许重试")
	}
	entries, listErr := server.store.ListCallHistory(context.Background(), "dev1", 10)
	if listErr != nil {
		t.Fatalf("ListCallHistory: %v", listErr)
	}
	if len(entries) != 1 {
		t.Fatalf("call history = %d entries, want 1 recorded attempt", len(entries))
	}
}

// 挂断信令报错但本地 dialog 已结束：如实提示，不把已经结束的通话判成任务失败。
func TestAutomaticCallTaskReportsHangupSignallingErrorButSucceeds(t *testing.T) {
	controller := newAutomaticCallVoWiFi()
	controller.nextState = func(call vowifi.Call) vowifi.Call {
		if call.State == "dialing" {
			call.State = "ringing"
		}
		return call
	}
	controller.hangupErr = errors.New("ims: CANCEL send failed")
	controller.hangupEnds = true
	server := newAutomaticCallTaskServer(t, controller)

	output, err := server.executeAutomaticTask(context.Background(), automaticCallTaskFixture(), func(string) {})
	if err != nil {
		t.Fatalf("executeAutomaticTask error = %v, want success", err)
	}
	if !strings.Contains(output, "挂断信令异常") {
		t.Fatalf("output = %q, want 挂断信令异常 hint", output)
	}
}

func TestWaitForAutomaticCallTimesOutWhenNeverPlaced(t *testing.T) {
	hungUp := false
	plan := automaticCallPlan{
		duration:     30 * time.Second,
		setupWindow:  50 * time.Millisecond,
		pollInterval: 10 * time.Millisecond,
		observe: func(context.Context) (automaticCallObservation, error) {
			return automaticCallObservation{State: "dialing"}, nil
		},
		hangup: func(context.Context) error {
			hungUp = true
			return nil
		},
	}
	outcome, err := waitForAutomaticCall(context.Background(), plan, func(string) {})
	if err != nil {
		t.Fatalf("waitForAutomaticCall error = %v", err)
	}
	if !outcome.SetupTimedOut || outcome.Placed {
		t.Fatalf("outcome = %+v, want setup timeout without placed call", outcome)
	}
	if !hungUp {
		t.Fatal("建立超时后应当主动挂断")
	}
}

func TestWaitForAutomaticCallStopsWaitingWhenRemoteEndsCall(t *testing.T) {
	var observed int
	plan := automaticCallPlan{
		duration:     30 * time.Second,
		pollInterval: 10 * time.Millisecond,
		observe: func(context.Context) (automaticCallObservation, error) {
			// 第一次振铃，第二次对方挂断。
			if observed == 0 {
				observed++
				return automaticCallObservation{State: "ringing", Placed: true}, nil
			}
			return automaticCallObservation{State: "ended", Finished: true, SIPCode: 200}, nil
		},
		hangup: func(context.Context) error {
			t.Error("对方挂断后不应再主动挂断")
			return nil
		},
	}
	outcome, err := waitForAutomaticCall(context.Background(), plan, func(string) {})
	if err != nil {
		t.Fatalf("waitForAutomaticCall error = %v", err)
	}
	if !outcome.Placed || !outcome.Finished {
		t.Fatalf("outcome = %+v, want placed and finished", outcome)
	}
}

// 连续采样失败的退出路径也必须尽力挂断，不能把已拨出的电话留在通话中。
func TestWaitForAutomaticCallHangsUpWhenObservationFails(t *testing.T) {
	hungUp := false
	plan := automaticCallPlan{
		duration:     30 * time.Second,
		pollInterval: 5 * time.Millisecond,
		observe: func(context.Context) (automaticCallObservation, error) {
			return automaticCallObservation{}, errors.New("ims runtime unavailable")
		},
		hangup: func(context.Context) error {
			hungUp = true
			return nil
		},
	}
	if _, err := waitForAutomaticCall(context.Background(), plan, func(string) {}); err == nil {
		t.Fatal("采样持续失败时应当返回错误")
	}
	if !hungUp {
		t.Fatal("放弃观测前应当尽力挂断")
	}
}

// 迟到的 2xx 可能把已挂断的通话重新拉回 active：必须再挂一次，而不能直接报成功。
func TestWaitForAutomaticCallRetriesHangupWhenCallResurrects(t *testing.T) {
	hangupCount := 0
	plan := automaticCallPlan{
		duration:     20 * time.Millisecond,
		pollInterval: 5 * time.Millisecond,
		settleWindow: 40 * time.Millisecond,
		observe: func(context.Context) (automaticCallObservation, error) {
			if hangupCount >= 2 {
				return automaticCallObservation{State: "ended", Finished: true}, nil
			}
			return automaticCallObservation{State: "active", Placed: true, Answered: true}, nil
		},
		hangup: func(context.Context) error {
			hangupCount++
			return nil
		},
	}
	outcome, err := waitForAutomaticCall(context.Background(), plan, func(string) {})
	if err != nil {
		t.Fatalf("waitForAutomaticCall error = %v", err)
	}
	if hangupCount != 2 {
		t.Fatalf("hangup count = %d, want 2", hangupCount)
	}
	if !outcome.Finished {
		t.Fatalf("outcome = %+v, want finished", outcome)
	}
}

// 无法确认挂断时不能报成功，且不能重试（会重复呼叫）。
func TestAutomaticCallResultRefusesUnconfirmedHangup(t *testing.T) {
	text, err := automaticCallResult("+447700900123", automaticCallOutcome{Placed: true, Finished: false})
	if err == nil {
		t.Fatalf("unconfirmed hang-up returned success text %q", text)
	}
	var executionError automaticTaskExecutionError
	if !errors.As(err, &executionError) || executionError.retryable {
		t.Fatalf("error = %v, want non-retryable automaticTaskExecutionError", err)
	}
	if !strings.Contains(err.Error(), "未能确认挂断") {
		t.Fatalf("error = %v, want 未能确认挂断", err)
	}
}

// 已发出呼叫/已真实呼出时的等待错误不能重试，避免重复呼叫同一被叫。
func TestAutomaticCallWaitErrorRetrySafety(t *testing.T) {
	cause := errors.New("runtime unavailable")
	var executionError automaticTaskExecutionError

	placed := automaticCallWaitError("+447700900123", automaticCallOutcome{Placed: true}, false, cause)
	if !errors.As(placed, &executionError) || executionError.retryable {
		t.Fatalf("已呼出错误 = %v, want non-retryable", placed)
	}
	issued := automaticCallWaitError("+447700900123", automaticCallOutcome{}, true, cause)
	if !errors.As(issued, &executionError) || executionError.retryable {
		t.Fatalf("已发出呼叫错误 = %v, want non-retryable", issued)
	}
	notPlaced := automaticCallWaitError("+447700900123", automaticCallOutcome{}, false, cause)
	if errors.As(notPlaced, &executionError) {
		t.Fatalf("未呼出错误 = %v, want retryable", notPlaced)
	}
}

func TestAutomaticTaskRuntimeLimitCoversCallDuration(t *testing.T) {
	longCall := automaticTaskRuntimeLimit(store.AutomaticTask{
		TaskType: "call", Payload: []byte(`{"duration_seconds":600}`),
	})
	if longCall != 15*time.Minute {
		t.Fatalf("600s call runtime limit = %v, want 15m", longCall)
	}
	shortCall := automaticTaskRuntimeLimit(store.AutomaticTask{
		TaskType: "call", Payload: []byte(`{"duration_seconds":1}`),
	})
	if shortCall != automaticTaskMaxRuntime {
		t.Fatalf("short call runtime limit = %v, want %v", shortCall, automaticTaskMaxRuntime)
	}
	if limit := automaticTaskRuntimeLimit(store.AutomaticTask{TaskType: "sms"}); limit != automaticTaskMaxRuntime {
		t.Fatalf("SMS runtime limit = %v, want %v", limit, automaticTaskMaxRuntime)
	}
	if longCall > automaticCallTaskMaxRuntime {
		t.Fatalf("call runtime limit %v exceeds cap %v", longCall, automaticCallTaskMaxRuntime)
	}
}

// 自动挂断时长超出 IMS 上限（或为 0）属于配置错误，不应重试。
func TestAutomaticCallRejectsInvalidDuration(t *testing.T) {
	server := newAutomaticCallTaskServer(t, newAutomaticCallVoWiFi())
	for _, seconds := range []int{0, 601} {
		task := automaticCallTaskFixture()
		task.Payload = []byte(`{"phone":"+447700900123","duration_seconds":` + strconv.Itoa(seconds) + `}`)
		if _, err := server.executeAutomaticTask(context.Background(), task, func(string) {}); err == nil {
			t.Fatalf("duration=%d accepted", seconds)
		} else {
			var executionError automaticTaskExecutionError
			if !errors.As(err, &executionError) || executionError.retryable {
				t.Fatalf("duration=%d error = %v, want non-retryable", seconds, err)
			}
		}
	}
}

func TestAutomaticCallSuccessTextDistinguishesWhoEndedCall(t *testing.T) {
	base := automaticCallOutcome{
		Placed: true, Answered: true, Finished: true,
		Elapsed: 12 * time.Second, AnsweredElapsed: 2 * time.Second,
	}
	local := base
	local.HangupIssued = true
	if text := automaticCallSuccessText("+447700900123", local); !strings.Contains(text, "通话 10 秒后自动挂断") {
		t.Fatalf("local hang-up text = %q", text)
	}
	remote := base
	if text := automaticCallSuccessText("+447700900123", remote); !strings.Contains(text, "对方挂断") {
		t.Fatalf("remote hang-up text = %q", text)
	}
	if text := automaticCallSuccessText("+447700900123", automaticCallOutcome{Placed: true, Finished: true, Elapsed: 5 * time.Second}); !strings.Contains(text, "振铃 5 秒后通话结束") {
		t.Fatalf("unanswered text = %q", text)
	}
}

func TestOutgoingCellularCallState(t *testing.T) {
	for _, test := range []struct {
		line     string
		state    string
		answered bool
		placed   bool
		found    bool
	}{
		// 拨号中（尚未振铃）找到通话但不算真实呼出。
		{`+CLCC: 1,0,2,0,0,"+447700900123",145`, "dialing", false, false, true},
		{`+CLCC: 1,0,3,0,0,"+447700900123",145`, "ringing", false, true, true},
		{`+CLCC: 1,0,0,0,0,"+447700900123",145`, "active", true, true, true},
		{`+CLCC: 1,0,1,0,0,"+447700900123",145`, "active", true, true, true},
		// 来电（direction 1）不算本次主叫。
		{`+CLCC: 1,1,4,0,0,"+447700900123",129`, "", false, false, false},
	} {
		state, answered, placed, found := outgoingCellularCallState(parseCLCC(modem.Response{Lines: []string{test.line}}))
		if state != test.state || answered != test.answered || placed != test.placed || found != test.found {
			t.Fatalf("%s => state=%q answered=%v placed=%v found=%v", test.line, state, answered, placed, found)
		}
	}
}

// cellular 端到端：拨号中不算真实呼出，必须等到 alerting 才允许按设定时长挂断。
func TestRunAutomaticCellularCallWaitsForRinging(t *testing.T) {
	var mu sync.Mutex
	var atEvents []string
	clccCount := 0
	controller := fakeDeviceController{entry: device.Device{ID: "dev1", Discovered: true}}
	controller.atHandler = func(command string) (modem.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		switch command {
		case "AT+CLCC":
			clccCount++
			atEvents = append(atEvents, "clcc:"+strconv.Itoa(clccCount))
			switch clccCount {
			case 1, 2:
				return modem.Response{Lines: []string{`+CLCC: 1,0,2,0,0,"+447700900123",145`}, Final: "OK"}, nil
			case 3:
				return modem.Response{Lines: []string{`+CLCC: 1,0,3,0,0,"+447700900123",145`}, Final: "OK"}, nil
			}
			// 已挂断：CLCC 不再包含通话。
			return modem.Response{Final: "OK"}, nil
		case "ATH":
			atEvents = append(atEvents, "ATH")
			return modem.Response{Final: "OK"}, nil
		}
		return modem.Response{Final: "OK"}, nil
	}
	server := &Server{logger: regionTestLogger(), devices: controller, maxRequestBodyBytes: 4096}

	output, err := server.runAutomaticCellularCall(context.Background(), "dev1", "+447700900123", time.Second, func(string) {})
	if err != nil {
		t.Fatalf("runAutomaticCellularCall error = %v", err)
	}
	if !strings.Contains(output, "振铃") {
		t.Fatalf("output = %q, want 振铃", output)
	}
	mu.Lock()
	events := append([]string(nil), atEvents...)
	mu.Unlock()
	hangupAt := eventIndex(events, "ATH")
	ringingAt := eventIndex(events, "clcc:3")
	if ringingAt < 0 {
		t.Fatalf("events = %v, want alerting CLCC", events)
	}
	if hangupAt < 0 || hangupAt < ringingAt {
		t.Fatalf("events = %v, ATH before alerting (dialing must not count as placed)", events)
	}
}

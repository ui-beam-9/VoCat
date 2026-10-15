package imscall

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"vocat/internal/sipgw"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

// --- 测试替身 ---

type fakeController struct {
	mu    sync.Mutex
	calls []vowifi.Call
	// callsAfterDial 拨号成功后才生效的通话列表，模拟 IMS 侧拨号后的通话演进。
	callsAfterDial []vowifi.Call
	dialErr        error
	media          vowifi.CallMedia
	mediaErr       error
	dialed         string
	hangups        []string
	answers        []string
}

func (f *fakeController) Calls(string) ([]vowifi.Call, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vowifi.Call(nil), f.calls...), nil
}

func (f *fakeController) DialCall(_ context.Context, _ string, number string) (vowifi.Call, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dialErr != nil {
		return vowifi.Call{}, f.dialErr
	}
	f.dialed = number
	if f.callsAfterDial != nil {
		f.calls = f.callsAfterDial
	}
	return vowifi.Call{ID: "ims-1", Number: number, Direction: "outgoing", State: "dialing"}, nil
}

func (f *fakeController) AnswerCall(_ context.Context, _ string, id string) (vowifi.Call, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, id)
	return vowifi.Call{ID: id, State: "active"}, nil
}

func (f *fakeController) HangupCall(_ context.Context, _ string, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangups = append(f.hangups, id)
	return nil
}

func (f *fakeController) CallMedia(_ context.Context, _, _ string) (vowifi.CallMedia, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mediaErr != nil {
		return nil, f.mediaErr
	}
	if f.media == nil {
		return nil, errors.New("无媒体")
	}
	return f.media, nil
}

func (f *fakeController) setCalls(calls []vowifi.Call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = calls
}

func (f *fakeController) hangupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hangups)
}

type fakeMedia struct{}

func (fakeMedia) Codec() string { return "PCMA" }
func (fakeMedia) ReadPCM(ctx context.Context) ([]int16, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (fakeMedia) WritePCM([]int16) error { return nil }

type fakeServerTx struct {
	mu        sync.Mutex
	responses []*sip.Response
}

func (f *fakeServerTx) Respond(res *sip.Response) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, res)
	return nil
}
func (f *fakeServerTx) Acks() <-chan *sip.Request          { return make(chan *sip.Request) }
func (f *fakeServerTx) OnCancel(sip.FnTxCancel) bool       { return false }
func (f *fakeServerTx) Terminate()                         {}
func (f *fakeServerTx) OnTerminate(sip.FnTxTerminate) bool { return false }
func (f *fakeServerTx) Done() <-chan struct{}              { return make(chan struct{}) }
func (f *fakeServerTx) Err() error                         { return nil }

func (f *fakeServerTx) last() *sip.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.responses) == 0 {
		return nil
	}
	return f.responses[len(f.responses)-1]
}

func (f *fakeServerTx) lastCode() int {
	res := f.last()
	if res == nil {
		return 0
	}
	return res.StatusCode
}

// newInvite 构造一个最小的外呼 INVITE 请求。
func newInvite(callee, callID string) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: callee, Host: "vocat.local"})
	req.AppendHeader(sip.NewHeader("Via", "SIP/2.0/UDP 192.168.1.10:5060;branch=z9hG4bK-test"))
	req.AppendHeader(sip.NewHeader("To", "<sip:"+callee+"@vocat.local>"))
	req.AppendHeader(sip.NewHeader("From", "<sip:1001@192.168.1.10>;tag=abc"))
	req.AppendHeader(sip.NewHeader("Call-ID", callID))
	req.AppendHeader(sip.NewHeader("CSeq", "1 INVITE"))
	return req
}

func newTestManager(ctrl Controller) *Manager {
	registrar, _ := sipgw.NewRegistrar(sipgw.DefaultConfig(), nil)
	return NewManager("dev-1", ctrl, registrar, nil)
}

// TestOutboundBusy 本桥已有通话时新外呼直接回 486。
func TestOutboundBusy(t *testing.T) {
	ctrl := &fakeController{}
	manager := newTestManager(ctrl)
	defer manager.Stop()

	manager.mu.Lock()
	manager.state = stateConnected
	manager.mu.Unlock()

	tx := &fakeServerTx{}
	manager.HandleOutboundInvite("dev-1", newInvite("10086", "call-busy"), tx)
	if code := tx.lastCode(); code != 486 {
		t.Fatalf("忙线应回 486，得到 %d", code)
	}
}

// TestOutboundDeviceBusy 设备上已有其他通话（如网页端）时回 486。
func TestOutboundDeviceBusy(t *testing.T) {
	ctrl := &fakeController{
		calls: []vowifi.Call{{ID: "web-1", State: "active"}},
	}
	manager := newTestManager(ctrl)
	defer manager.Stop()

	tx := &fakeServerTx{}
	manager.HandleOutboundInvite("dev-1", newInvite("10086", "call-devbusy"), tx)
	if code := tx.lastCode(); code != 486 {
		t.Fatalf("设备忙应回 486，得到 %d", code)
	}
	if ctrl.dialed != "" {
		t.Fatal("设备忙时不应发起 IMS 拨号")
	}
}

// TestOutboundDialError  IMS 拨号失败回 503。
func TestOutboundDialError(t *testing.T) {
	ctrl := &fakeController{dialErr: errors.New("IMS 未注册")}
	manager := newTestManager(ctrl)
	defer manager.Stop()

	tx := &fakeServerTx{}
	manager.HandleOutboundInvite("dev-1", newInvite("10086", "call-dialerr"), tx)
	if code := tx.lastCode(); code != 503 {
		t.Fatalf("拨号失败应回 503，得到 %d", code)
	}
	manager.mu.Lock()
	st := manager.state
	manager.mu.Unlock()
	if st != stateIdle {
		t.Fatal("失败后状态应复位为空闲")
	}
}

// TestOutboundAnswered 外呼被接通：回 200 OK（带 SDP）并起桥，BYE 后挂断 IMS 侧。
func TestOutboundAnswered(t *testing.T) {
	ctrl := &fakeController{
		media: fakeMedia{},
		// 拨号后 IMS 侧立即进入 active 且媒体就绪。
		callsAfterDial: []vowifi.Call{{ID: "ims-1", Number: "10086", State: "active", MediaReady: true}},
	}
	manager := newTestManager(ctrl)
	defer manager.Stop()

	tx := &fakeServerTx{}
	manager.HandleOutboundInvite("dev-1", newInvite("10086", "call-ok"), tx)
	if code := tx.lastCode(); code != 200 {
		t.Fatalf("接通后应回 200，得到 %d", code)
	}
	if body := tx.last().Body(); len(body) == 0 {
		t.Fatal("200 OK 应携带 SDP")
	}
	manager.mu.Lock()
	st := manager.state
	manager.mu.Unlock()
	if st != stateConnected {
		t.Fatal("接通后状态应为已连接")
	}

	if !manager.HandleClientBye("call-ok") {
		t.Fatal("BYE 应命中当前通话")
	}
	if ctrl.hangupCount() != 1 {
		t.Fatalf("BYE 后应挂断 IMS 侧 1 次，得到 %d", ctrl.hangupCount())
	}
	manager.mu.Lock()
	st = manager.state
	manager.mu.Unlock()
	if st != stateIdle {
		t.Fatal("BYE 后状态应复位为空闲")
	}
}

// TestOutboundRejected 对端拒接：IMS 侧 486 映射回 486。
func TestOutboundRejected(t *testing.T) {
	ctrl := &fakeController{
		callsAfterDial: []vowifi.Call{{ID: "ims-1", Number: "10086", State: "ended", SIPCode: 486}},
	}
	manager := newTestManager(ctrl)
	defer manager.Stop()

	tx := &fakeServerTx{}
	manager.HandleOutboundInvite("dev-1", newInvite("10086", "call-rej"), tx)
	if code := tx.lastCode(); code != 486 {
		t.Fatalf("对端 486 应映射为 486，得到 %d", code)
	}
}

// TestOutboundCancel 外呼建立中被软电话 CANCEL：回 487 并挂断 IMS 侧。
func TestOutboundCancel(t *testing.T) {
	ctrl := &fakeController{
		media: fakeMedia{},
		// IMS 侧一直处于 dialing，保证拨号循环持续等待。
		callsAfterDial: []vowifi.Call{{ID: "ims-1", Number: "10086", State: "dialing"}},
	}
	manager := newTestManager(ctrl)
	defer manager.Stop()

	tx := &fakeServerTx{}
	done := make(chan struct{})
	go func() {
		manager.HandleOutboundInvite("dev-1", newInvite("10086", "call-cancel"), tx)
		close(done)
	}()

	// 等待拨号循环进入等待状态后再取消。
	time.Sleep(1200 * time.Millisecond)
	if !manager.HandleClientCancel("call-cancel") {
		t.Fatal("CANCEL 应命中当前外呼")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("CANCEL 后拨号循环应及时退出")
	}
	if code := tx.lastCode(); code != 487 {
		t.Fatalf("CANCEL 后应回 487，得到 %d", code)
	}
	if ctrl.hangupCount() != 1 {
		t.Fatalf("CANCEL 后应挂断 IMS 侧 1 次，得到 %d", ctrl.hangupCount())
	}
}

// TestIncomingBusy 来电时本桥已有通话：直接挂断 IMS 来电。
func TestIncomingBusy(t *testing.T) {
	ctrl := &fakeController{}
	manager := newTestManager(ctrl)
	defer manager.Stop()

	manager.mu.Lock()
	manager.state = stateConnected
	manager.mu.Unlock()

	manager.HandleIncomingCall(context.Background(), ims.ReceivedCall{
		DeviceID: "dev-1",
		CallID:   "ims-in-1",
		Caller:   "+8613800138000",
	})
	if ctrl.hangupCount() != 1 {
		t.Fatalf("忙时来电应直接挂断 IMS 侧 1 次，得到 %d", ctrl.hangupCount())
	}
}

// TestIncomingNoSoftphoneResetsWithoutHangup 软电话未注册时等待超时只复位桥接状态：
// 不挂断 IMS 侧（来电保持振铃，网页仍可接听）。同时必须复位状态，否则会遗留
// state!=idle 且 currentCall==nil 的卡死状态：monitorLoop 会永久跳过，之后所有外呼
// 都会被判忙拒绝，且只有重启服务才能恢复。
func TestIncomingNoSoftphoneResetsWithoutHangup(t *testing.T) {
	ctrl := &fakeController{dialErr: errors.New("IMS 未注册")}
	manager := newTestManager(ctrl)
	manager.softphoneWait = 50 * time.Millisecond
	defer manager.Stop()

	manager.HandleIncomingCall(context.Background(), ims.ReceivedCall{
		DeviceID: "dev-1",
		CallID:   "ims-in-abort",
		Caller:   "+8613800138000",
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		manager.mu.Lock()
		st := manager.state
		manager.mu.Unlock()
		if st == stateIdle || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	manager.mu.Lock()
	st := manager.state
	manager.mu.Unlock()
	if st != stateIdle {
		t.Fatal("等待软电话上线超时后状态应复位为空闲")
	}
	if ctrl.hangupCount() != 0 {
		t.Fatalf("软电话未上线时不应挂断 IMS 侧，得到 %d 次", ctrl.hangupCount())
	}

	// 复位后必须能正常发起外呼（不应再被判忙）。
	tx := &fakeServerTx{}
	manager.HandleOutboundInvite("dev-1", newInvite("10086", "call-after-abort"), tx)
	if code := tx.lastCode(); code != 503 {
		t.Fatalf("复位后外呼不应被判忙，期望 503（拨号失败），得到 %d", code)
	}
}

// TestMapTermination 终结码映射。
func TestMapTermination(t *testing.T) {
	cases := map[int]int{
		486: 486,
		603: 603,
		480: 480,
		408: 408,
		487: 487,
		404: 404,
		0:   480,
		500: 500,
	}
	for sipCode, want := range cases {
		if got, _ := mapTermination(sipCode); got != want {
			t.Fatalf("mapTermination(%d) = %d，期望 %d", sipCode, got, want)
		}
	}
}

// TestDeviceBusy ended/failed 不算忙，其余状态算忙。
func TestDeviceBusy(t *testing.T) {
	ctrl := &fakeController{}
	manager := newTestManager(ctrl)
	defer manager.Stop()

	ctrl.setCalls([]vowifi.Call{{ID: "a", State: "ended"}, {ID: "b", State: "failed"}})
	if manager.deviceBusy() {
		t.Fatal("ended/failed 不应判定为设备忙")
	}
	ctrl.setCalls([]vowifi.Call{{ID: "c", State: "ringing"}})
	if !manager.deviceBusy() {
		t.Fatal("ringing 应判定为设备忙")
	}
}

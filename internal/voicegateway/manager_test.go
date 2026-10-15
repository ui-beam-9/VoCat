package voicegateway

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"vocat/internal/cscall"
	"vocat/internal/imscall"
	"vocat/internal/modem"
	"vocat/internal/sipgw"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type fakeStore struct {
	devices  map[string]store.Device
	settings map[string]store.AppSetting
}

func newFakeStore() *fakeStore {
	return &fakeStore{devices: map[string]store.Device{}, settings: map[string]store.AppSetting{}}
}

func (f *fakeStore) Device(_ context.Context, id string) (store.Device, error) {
	device, ok := f.devices[id]
	if !ok {
		return store.Device{}, store.ErrNotFound
	}
	return device, nil
}

func (f *fakeStore) ListDevices(context.Context) ([]store.Device, error) {
	values := make([]store.Device, 0, len(f.devices))
	for _, device := range f.devices {
		values = append(values, device)
	}
	return values, nil
}

func (f *fakeStore) AppSetting(_ context.Context, key string) (store.AppSetting, error) {
	setting, ok := f.settings[key]
	if !ok {
		return store.AppSetting{}, store.ErrNotFound
	}
	return setting, nil
}

func (f *fakeStore) UpsertAppSetting(_ context.Context, value store.AppSetting) error {
	f.settings[value.Key] = value
	return nil
}

type fakeDevices struct{}

func (fakeDevices) ExecuteAT(context.Context, string, string) (modem.Response, error) {
	return modem.Response{}, nil
}

func (fakeDevices) SubscribeCallURCs(context.Context, string) (<-chan string, error) {
	ch := make(chan string)
	return ch, nil
}

func newTestManager(t *testing.T, backend *fakeStore) *Manager {
	t.Helper()
	manager, err := New(Options{Devices: fakeDevices{}, Store: backend})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return manager
}

// TestMergeSecrets 验证空密码与掩码都沿用已保存的密码。
func TestMergeSecrets(t *testing.T) {
	current := sipgw.Config{Users: []sipgw.User{{Username: "linphone", Password: "secret"}}}
	incoming := sipgw.Config{Users: []sipgw.User{{Username: "linphone", Password: ""}}}
	merged := mergeSecrets(incoming, current)
	if merged.Users[0].Password != "secret" {
		t.Fatalf("空密码应沿用旧值，得到 %q", merged.Users[0].Password)
	}

	masked := sipgw.Config{Users: []sipgw.User{{Username: "linphone", Password: store.SecretMask}}}
	if got := mergeSecrets(masked, current).Users[0].Password; got != "secret" {
		t.Fatalf("掩码应沿用旧值，得到 %q", got)
	}

	replaced := sipgw.Config{Users: []sipgw.User{{Username: "linphone", Password: "new"}}}
	if got := mergeSecrets(replaced, current).Users[0].Password; got != "new" {
		t.Fatalf("新密码应覆盖，得到 %q", got)
	}
}

// TestSavePersistsAndRedacts 验证保存后能读回，且对外展示时密码被掩码。
func TestSavePersistsAndRedacts(t *testing.T) {
	backend := newFakeStore()
	manager := newTestManager(t, backend)

	saved, err := manager.Save(context.Background(), sipgw.Config{
		Enabled: false,
		Listen:  "0.0.0.0:5060",
		Users:   []sipgw.User{{Username: "linphone", Password: "hunter2", DeviceID: "dev-1"}},
	})
	if err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if saved.Users[0].Password != "hunter2" {
		t.Fatalf("保存结果应保留明文密码，得到 %q", saved.Users[0].Password)
	}

	stored, present, err := manager.Stored(context.Background())
	if err != nil || !present {
		t.Fatalf("Stored 失败: present=%v err=%v", present, err)
	}
	if stored.Users[0].Password != "hunter2" {
		t.Fatalf("存储中应为明文密码，得到 %q", stored.Users[0].Password)
	}

	redacted, err := manager.Redacted(context.Background())
	if err != nil {
		t.Fatalf("Redacted 失败: %v", err)
	}
	if redacted.Users[0].Password != store.SecretMask {
		t.Fatalf("对外配置应为掩码，得到 %q", redacted.Users[0].Password)
	}
}

// TestSaveRejectsInvalidConfig 验证非法配置会被拒绝。
func TestSaveRejectsInvalidConfig(t *testing.T) {
	manager := newTestManager(t, newFakeStore())
	if _, err := manager.Save(context.Background(), sipgw.Config{
		Enabled: true,
		Listen:  "bad-listen",
	}); err == nil {
		t.Fatal("非法监听地址应被拒绝")
	}
}

// TestStatusReportsDeviceAudio 验证状态里包含绑定设备的音频设备信息。
func TestStatusReportsDeviceAudio(t *testing.T) {
	backend := newFakeStore()
	backend.devices["dev-1"] = store.Device{ID: "dev-1", AudioDevice: "hw:2,0"}
	manager := newTestManager(t, backend)

	if _, err := manager.Save(context.Background(), sipgw.Config{
		Enabled: false,
		Listen:  "0.0.0.0:5060",
		Users:   []sipgw.User{{Username: "linphone", Password: "p", DeviceID: "dev-1"}},
	}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	status := manager.Status(context.Background())
	if status.Running {
		t.Fatal("enabled=false 时不应处于运行状态")
	}
	if len(status.Devices) != 1 || status.Devices[0].AudioDevice != "hw:2,0" {
		t.Fatalf("设备状态不符合预期: %+v", status.Devices)
	}
}

// fakeVoWiFi 是可编程的 VoWiFi 能力替身，用于后端选择测试。
type fakeVoWiFi struct {
	callsErr error
}

func (f *fakeVoWiFi) Calls(string) ([]vowifi.Call, error) {
	if f.callsErr != nil {
		return nil, f.callsErr
	}
	return nil, nil
}
func (f *fakeVoWiFi) DialCall(context.Context, string, string) (vowifi.Call, error) {
	return vowifi.Call{}, errors.New("未实现")
}
func (f *fakeVoWiFi) AnswerCall(context.Context, string, string) (vowifi.Call, error) {
	return vowifi.Call{}, errors.New("未实现")
}
func (f *fakeVoWiFi) HangupCall(context.Context, string, string) error { return nil }
func (f *fakeVoWiFi) CallMedia(context.Context, string, string) (vowifi.CallMedia, error) {
	return nil, errors.New("未实现")
}

// TestResolveBackend 覆盖 auto/ims/cs 三种配置下的后端选择。
// 关键点：后端选择只看「系统有没有 VoWiFi 能力」（vowifi 是否注入），
// 不探测 VoWiFi 运行时连接状态（Calls 是否成功）。
func TestResolveBackend(t *testing.T) {
	cases := []struct {
		name     string
		backend  string
		vowifi   VoWiFi
		expected string
	}{
		{"auto 未注入 VoWiFi 回退 CS", sipgw.BackendAuto, nil, sipgw.BackendCS},
		{"auto 有 VoWiFi 能力选 IMS", sipgw.BackendAuto, &fakeVoWiFi{}, sipgw.BackendIMS},
		{"auto 有 VoWiFi 能力但 Calls 失败仍选 IMS（不探测运行时状态）", sipgw.BackendAuto, &fakeVoWiFi{callsErr: errors.New("未注册")}, sipgw.BackendIMS},
		{"强制 IMS", sipgw.BackendIMS, &fakeVoWiFi{callsErr: errors.New("未注册")}, sipgw.BackendIMS},
		{"强制 IMS 但未注入回退 CS", sipgw.BackendIMS, nil, sipgw.BackendCS},
		{"强制 CS", sipgw.BackendCS, &fakeVoWiFi{}, sipgw.BackendCS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager := &Manager{
				cfg:    sipgw.Config{Backend: tc.backend},
				vowifi: tc.vowifi,
				logger: slog.Default(),
			}
			if got := manager.resolveBackend("dev-1"); got != tc.expected {
				t.Fatalf("resolveBackend = %q，期望 %q", got, tc.expected)
			}
		})
	}
}

// newBridgeTestManager 构造用于 handlerForCallLocked 测试的协调器。
func newBridgeTestManager(backend string, vowifi VoWiFi, devices map[string]store.Device) *Manager {
	backendStore := newFakeStore()
	for id, device := range devices {
		backendStore.devices[id] = device
	}
	return &Manager{
		devices: fakeDevices{},
		store:   backendStore,
		vowifi:  vowifi,
		logger:  slog.Default(),
		cfg: sipgw.Config{
			Backend: backend,
			Users:   []sipgw.User{{Username: "u", Password: "p", DeviceID: "dev-1"}},
		},
		bridges: make(map[string]*bridge),
	}
}

// TestHandlerForCallAutoCreatesIMSBridge 验证 auto + 有 VoWiFi 能力 → 建 IMS 桥。
func TestHandlerForCallAutoCreatesIMSBridge(t *testing.T) {
	manager := newBridgeTestManager(sipgw.BackendAuto, &fakeVoWiFi{}, nil)
	t.Cleanup(manager.Close)

	handler, err := manager.handlerForCallLocked("dev-1")
	if err != nil {
		t.Fatalf("期望成功建桥，得到 err=%v", err)
	}
	if _, ok := handler.(*imscall.Manager); !ok {
		t.Fatalf("期望 IMS 桥，得到 %T", handler)
	}
}

// TestHandlerForCallForceCSWithSoundCard 验证强制 CS + 配声卡 → 建 CS 桥。
func TestHandlerForCallForceCSWithSoundCard(t *testing.T) {
	devices := map[string]store.Device{"dev-1": {ID: "dev-1", AudioDevice: "hw:0,0"}}
	manager := newBridgeTestManager(sipgw.BackendCS, &fakeVoWiFi{}, devices)
	t.Cleanup(manager.Close)

	handler, err := manager.handlerForCallLocked("dev-1")
	if err != nil {
		t.Fatalf("期望成功建桥，得到 err=%v", err)
	}
	if _, ok := handler.(*cscall.Manager); !ok {
		t.Fatalf("期望 CS 桥，得到 %T", handler)
	}
}

// TestHandlerForCallForceCSErrorWithoutSoundCard 验证强制 CS + 无声卡 → 返回错误（480 场景）。
func TestHandlerForCallForceCSErrorWithoutSoundCard(t *testing.T) {
	devices := map[string]store.Device{"dev-1": {ID: "dev-1"}}
	manager := newBridgeTestManager(sipgw.BackendCS, &fakeVoWiFi{}, devices)
	t.Cleanup(manager.Close)

	if _, err := manager.handlerForCallLocked("dev-1"); err == nil {
		t.Fatal("期望强制 CS 但无声卡时返回错误，得到 nil")
	}
	if len(manager.bridges) != 0 {
		t.Fatalf("期望 bridges 为空，得到 %d 个", len(manager.bridges))
	}
}

// TestHandlerForCallBackendSwitchAtRuntime 验证运行时配置变更（IMS→CS→IMS）时桥会被正确重建。
func TestHandlerForCallBackendSwitchAtRuntime(t *testing.T) {
	devices := map[string]store.Device{"dev-1": {ID: "dev-1", AudioDevice: "hw:0,0"}}
	manager := newBridgeTestManager(sipgw.BackendAuto, &fakeVoWiFi{}, devices) // auto = IMS
	t.Cleanup(manager.Close)

	first, err := manager.handlerForCallLocked("dev-1")
	if err != nil {
		t.Fatalf("首次期望成功建桥，得到 err=%v", err)
	}
	if _, ok := first.(*imscall.Manager); !ok {
		t.Fatalf("首次期望 IMS 桥，得到 %T", first)
	}

	// 用户把配置改成强制 CS。
	manager.cfg.Backend = sipgw.BackendCS
	second, err := manager.handlerForCallLocked("dev-1")
	if err != nil {
		t.Fatalf("切 CS 期望成功建桥，得到 err=%v", err)
	}
	if _, ok := second.(*cscall.Manager); !ok {
		t.Fatalf("第二次期望 CS 桥，得到 %T", second)
	}
	if len(manager.bridges) != 1 {
		t.Fatalf("期望旧桥已释放只保留一个桥，得到 %d 个", len(manager.bridges))
	}

	// 再切回 auto（= IMS），后端应再次切换。
	manager.cfg.Backend = sipgw.BackendAuto
	third, err := manager.handlerForCallLocked("dev-1")
	if err != nil {
		t.Fatalf("切回 auto 期望成功建桥，得到 err=%v", err)
	}
	if _, ok := third.(*imscall.Manager); !ok {
		t.Fatalf("第三次期望 IMS 桥，得到 %T", third)
	}
}

// Package voicegateway 协调 SIP 网关与各设备的语音桥接。
//
// 它负责：读取/保存语音网关配置、按配置启动 SIP 注册服务、为每台绑定了
// 软电话账号的设备创建通话管理器（IMS 或 CS 后端），并把软电话的呼叫信令
// 路由到对应设备。
package voicegateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"

	"vocat/internal/cscall"
	"vocat/internal/imscall"
	"vocat/internal/modem"
	"vocat/internal/sipgw"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

// SettingKey 是语音网关配置在 app_settings 中的键名。
const SettingKey = "voice_gateway"

// DeviceAT 是设备层提供的 AT 执行与通话 URC 订阅能力。
type DeviceAT interface {
	ExecuteAT(ctx context.Context, id, command string) (modem.Response, error)
	SubscribeCallURCs(ctx context.Context, id string) (<-chan string, error)
}

// Store 是本包需要的存储能力。
type Store interface {
	Device(ctx context.Context, id string) (store.Device, error)
	ListDevices(ctx context.Context) ([]store.Device, error)
	AppSetting(ctx context.Context, key string) (store.AppSetting, error)
	UpsertAppSetting(ctx context.Context, value store.AppSetting) error
}

// VoWiFi 是本包需要的设备级 IMS 呼叫控制与媒体能力（由 vowifi runtime Manager 实现）。
type VoWiFi interface {
	Calls(string) ([]vowifi.Call, error)
	DialCall(context.Context, string, string) (vowifi.Call, error)
	AnswerCall(context.Context, string, string) (vowifi.Call, error)
	HangupCall(context.Context, string, string) error
	CallMedia(context.Context, string, string) (vowifi.CallMedia, error)
}

// Options 构造协调器所需依赖。
type Options struct {
	Devices DeviceAT
	Store   Store
	// VoWiFi 可选；为空时 IMS 桥接后端不可用，仅能使用 CS 后端。
	VoWiFi VoWiFi
	Logger *slog.Logger
}

// DeviceStatus 描述单台设备的桥接状态。
type DeviceStatus struct {
	DeviceID    string `json:"device_id"`
	AudioDevice string `json:"audio_device"`
	Backend     string `json:"backend,omitempty"`
	Active      bool   `json:"active"`
	Detail      string `json:"detail,omitempty"`
}

// Status 描述语音网关运行状态。
type Status struct {
	Enabled    bool           `json:"enabled"`
	Running    bool           `json:"running"`
	Listen     string         `json:"listen"`
	Realm      string         `json:"realm"`
	ExternalIP string         `json:"external_ip"`
	UserCount  int            `json:"user_count"`
	Registered int            `json:"registered"`
	Devices    []DeviceStatus `json:"devices"`
	LastError  string         `json:"last_error,omitempty"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
}

// callHandler 抽象单设备的呼叫处理能力（cscall.Manager 或 imscall.Manager）。
type callHandler interface {
	HandleOutboundInvite(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	HandleClientCancel(callID string) bool
	HandleClientBye(callID string) bool
	Stop()
}

type bridge struct {
	handler callHandler
	backend string // sipgw.BackendIMS 或 sipgw.BackendCS
}

// Manager 协调 SIP 网关与逐设备通话桥。
type Manager struct {
	devices DeviceAT
	store   Store
	vowifi  VoWiFi
	logger  *slog.Logger

	mu        sync.Mutex
	cfg       sipgw.Config
	registrar *sipgw.Registrar
	bridges   map[string]*bridge
	cancel    context.CancelFunc
	running   bool
	lastError string
	startedAt time.Time
}

// New 创建协调器。
func New(options Options) (*Manager, error) {
	if options.Devices == nil {
		return nil, errors.New("voicegateway: device controller is required")
	}
	if options.Store == nil {
		return nil, errors.New("voicegateway: store is required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		devices: options.Devices,
		store:   options.Store,
		vowifi:  options.VoWiFi,
		logger:  logger,
		cfg:     sipgw.DefaultConfig(),
		bridges: make(map[string]*bridge),
	}, nil
}

// Apply 应用一份新配置：停掉旧实例，按需启动新实例。
func (manager *Manager) Apply(ctx context.Context, incoming sipgw.Config) error {
	incoming = incoming.Normalize()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.applyLocked(ctx, incoming)
}

func (manager *Manager) applyLocked(ctx context.Context, incoming sipgw.Config) error {
	manager.stopLocked()
	manager.cfg = incoming
	manager.lastError = ""
	if !incoming.Enabled || len(incoming.Users) == 0 {
		return nil
	}
	if err := incoming.Validate(); err != nil {
		manager.lastError = err.Error()
		return err
	}
	registrar, err := sipgw.NewRegistrar(incoming, manager.logger)
	if err != nil {
		manager.lastError = err.Error()
		return err
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	manager.cancel = cancel
	manager.registrar = registrar

	registrar.SetOnInvite(manager.routeInvite)
	registrar.SetOnCancel(manager.routeCancel)
	registrar.SetOnBye(manager.routeBye)
	registrar.SetOnAck(func(string, *sip.Request, sip.ServerTransaction) {})
	registrar.SetOnPrack(func(string, *sip.Request, sip.ServerTransaction) {})

	if err := registrar.Start(rootCtx); err != nil {
		manager.lastError = err.Error()
		cancel()
		manager.registrar = nil
		manager.cancel = nil
		return err
	}

	started := 0
	for _, user := range incoming.Users {
		if user.DeviceID == "" {
			continue
		}
		if _, exists := manager.bridges[user.DeviceID]; exists {
			continue
		}
		if err := manager.startBridgeLocked(rootCtx, user.DeviceID); err != nil {
			manager.logger.Warn("初始化设备语音桥失败",
				"device_id", user.DeviceID, "err", err)
			continue
		}
		started++
	}
	manager.running = true
	manager.startedAt = time.Now().UTC()
	manager.logger.Info("语音网关已应用配置",
		"listen", incoming.Listen, "users", len(incoming.Users), "bridges", started)
	return nil
}

func (manager *Manager) startBridgeLocked(ctx context.Context, deviceID string) error {
	backend := manager.resolveBackend(deviceID)
	if backend == sipgw.BackendIMS {
		callManager := imscall.NewManager(deviceID, manager.vowifi, manager.registrar, manager.logger)
		manager.bridges[deviceID] = &bridge{handler: callManager, backend: sipgw.BackendIMS}
		return nil
	}

	device, err := manager.store.Device(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("读取设备配置失败: %w", err)
	}
	audioDevice := strings.TrimSpace(device.AudioDevice)
	if audioDevice == "" {
		return errors.New("设备未配置 audio_device（ALSA 声卡），已跳过语音桥")
	}
	adapter := &deviceATAdapter{deviceID: deviceID, devices: manager.devices}
	controller := cscall.NewATController(adapter, manager.logger)
	callManager := cscall.NewManager(deviceID, audioDevice, controller, manager.registrar, manager.logger)
	manager.bridges[deviceID] = &bridge{handler: callManager, backend: sipgw.BackendCS}
	return nil
}

// resolveBackend 按配置与系统能力确定设备的桥接后端。
//
// 后端选择只看**系统有没有 VoWiFi 能力**（manager.vowifi 是否注入），
// 不探测 VoWiFi 运行时连接状态（Phase 在哪、Calls 是否成功）——那是 VoWiFi
// 独立 lifecycle 的事，桥接后端只管"系统有能力就选 IMS"。VoWiFi 连不上时
// DialCall 会在真正拨号处返回错误，由 IMS handler 回 503 并记录明确原因，
// 而不是在这里提前 fallback CS 导致无声卡设备根本建不起桥。
func (manager *Manager) resolveBackend(deviceID string) string {
	configured := manager.cfg.Backend
	if configured == sipgw.BackendCS {
		return sipgw.BackendCS
	}
	if manager.vowifi == nil {
		if configured == sipgw.BackendIMS {
			manager.logger.Warn("未注入 VoWiFi 能力，IMS 桥接后端不可用，回退 CS", "device_id", deviceID)
		}
		return sipgw.BackendCS
	}
	// IMS 或 auto：系统有 VoWiFi 能力就选 IMS。
	return sipgw.BackendIMS
}

// handlerForCallLocked 返回该设备当前应使用的呼叫处理器。
// 运行时若桥后端与 resolveBackend 最新判定不一致（典型场景：用户保存了新的
// 桥接后端配置），停掉旧桥并按最新判定重建。调用方需持有 manager.mu。
func (manager *Manager) handlerForCallLocked(deviceID string) (callHandler, error) {
	desired := manager.resolveBackend(deviceID)
	item := manager.bridges[deviceID]
	if item != nil && item.backend == desired {
		return item.handler, nil
	}
	if item != nil {
		manager.logger.Info("设备语音桥后端切换",
			"device_id", deviceID, "old_backend", item.backend, "new_backend", desired)
		item.handler.Stop()
		delete(manager.bridges, deviceID)
	}
	// startBridgeLocked 仅将 ctx 用于读取设备配置（CS 桥），与网关生命周期无关。
	if err := manager.startBridgeLocked(context.Background(), deviceID); err != nil {
		return nil, err
	}
	return manager.bridges[deviceID].handler, nil
}

func (manager *Manager) stopLocked() {
	for deviceID, item := range manager.bridges {
		item.handler.Stop()
		delete(manager.bridges, deviceID)
	}
	if manager.registrar != nil {
		_ = manager.registrar.Stop()
		manager.registrar = nil
	}
	if manager.cancel != nil {
		manager.cancel()
		manager.cancel = nil
	}
	manager.running = false
	manager.startedAt = time.Time{}
}

// Close 停止协调器持有的所有资源。
func (manager *Manager) Close() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
}

// --- 呼叫信令路由 ---

func (manager *Manager) routeInvite(deviceID string, req *sip.Request, tx sip.ServerTransaction) {
	manager.mu.Lock()
	handler, err := manager.handlerForCallLocked(deviceID)
	manager.mu.Unlock()
	if err != nil {
		manager.logger.Warn("外呼 INVITE 无法路由：该设备没有可用语音桥，回 480",
			"device_id", deviceID, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 480, "Temporarily Unavailable", nil))
		return
	}
	handler.HandleOutboundInvite(deviceID, req, tx)
}

func (manager *Manager) routeCancel(deviceID string, req *sip.Request, _ sip.ServerTransaction) {
	manager.mu.Lock()
	item := manager.bridges[deviceID]
	manager.mu.Unlock()
	if item == nil || req.CallID() == nil {
		return
	}
	item.handler.HandleClientCancel(req.CallID().Value())
}

func (manager *Manager) routeBye(deviceID string, req *sip.Request, _ sip.ServerTransaction) {
	manager.mu.Lock()
	item := manager.bridges[deviceID]
	manager.mu.Unlock()
	if item == nil || req.CallID() == nil {
		return
	}
	item.handler.HandleClientBye(req.CallID().Value())
}

// HandleIncomingCall 把 IMS 来电转给对应设备的 IMS 桥（CS 后端设备忽略，走自身 URC）。
// auto 模式下桥可能之前被定型为 CS，而来电本身说明 IMS 已就绪，这里通过
// handlerForCallLocked 重新判定并在需要时重建 IMS 桥。
func (manager *Manager) HandleIncomingCall(ctx context.Context, call ims.ReceivedCall) {
	manager.mu.Lock()
	handler, err := manager.handlerForCallLocked(call.DeviceID)
	manager.mu.Unlock()
	if err != nil {
		manager.logger.Warn("IMS 来电无法路由：该设备没有可用语音桥",
			"device_id", call.DeviceID, "err", err)
		return
	}
	imsManager, ok := handler.(*imscall.Manager)
	if !ok {
		// 强制 CS 的设备：来电仍由 modem 自身 URC 处理，忽略。
		return
	}
	imsManager.HandleIncomingCall(ctx, call)
}

// --- 配置读写 ---

// Stored 读取已保存的配置。第二个返回值为 false 表示尚未保存过。
func (manager *Manager) Stored(ctx context.Context) (sipgw.Config, bool, error) {
	setting, err := manager.store.AppSetting(ctx, SettingKey)
	if errors.Is(err, store.ErrNotFound) {
		return sipgw.DefaultConfig(), false, nil
	}
	if err != nil {
		return sipgw.Config{}, false, err
	}
	var cfg sipgw.Config
	if len(setting.Value) > 0 {
		if err := json.Unmarshal(setting.Value, &cfg); err != nil {
			return sipgw.DefaultConfig(), true, fmt.Errorf("解析已保存的语音网关配置失败: %w", err)
		}
	}
	return cfg.Normalize(), true, nil
}

// Redacted 返回把密码替换为掩码的配置，供前端展示。
func (manager *Manager) Redacted(ctx context.Context) (sipgw.Config, error) {
	cfg, _, err := manager.Stored(ctx)
	if err != nil {
		return sipgw.Config{}, err
	}
	for index := range cfg.Users {
		if strings.TrimSpace(cfg.Users[index].Password) != "" {
			cfg.Users[index].Password = store.SecretMask
		}
	}
	return cfg, nil
}

// Save 合并敏感字段、校验并保存配置，随后立即应用。
func (manager *Manager) Save(ctx context.Context, incoming sipgw.Config) (sipgw.Config, error) {
	current, _, err := manager.Stored(ctx)
	if err != nil {
		return sipgw.Config{}, err
	}
	merged := mergeSecrets(incoming, current).Normalize()
	if err := merged.Validate(); err != nil {
		return sipgw.Config{}, err
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return sipgw.Config{}, fmt.Errorf("编码语音网关配置失败: %w", err)
	}
	if err := manager.store.UpsertAppSetting(ctx, store.AppSetting{
		Key:   SettingKey,
		Value: encoded,
	}); err != nil {
		return sipgw.Config{}, err
	}
	if err := manager.Apply(ctx, merged); err != nil {
		return merged, err
	}
	return merged, nil
}

// ApplyStored 在进程启动时加载并应用已保存的配置。
func (manager *Manager) ApplyStored(ctx context.Context) error {
	cfg, present, err := manager.Stored(ctx)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return manager.Apply(ctx, cfg)
}

// Status 返回当前运行状态。
func (manager *Manager) Status(ctx context.Context) Status {
	manager.mu.Lock()
	cfg := manager.cfg
	registrar := manager.registrar
	running := manager.running
	lastError := manager.lastError
	startedAt := manager.startedAt
	active := make(map[string]bool, len(manager.bridges))
	backends := make(map[string]string, len(manager.bridges))
	for deviceID, item := range manager.bridges {
		active[deviceID] = true
		backends[deviceID] = item.backend
	}
	manager.mu.Unlock()

	status := Status{
		Enabled:    cfg.Enabled,
		Running:    running,
		Listen:     cfg.Listen,
		Realm:      cfg.Realm,
		ExternalIP: cfg.ExternalIP,
		UserCount:  len(cfg.Users),
		LastError:  lastError,
		Devices:    []DeviceStatus{},
	}
	if registrar != nil {
		status.Registered = registrar.CountRegistered()
		if status.ExternalIP == "" {
			status.ExternalIP = registrar.GetExternalIP()
		}
	}
	if !startedAt.IsZero() {
		at := startedAt
		status.StartedAt = &at
	}
	// 逐账号列出绑定设备及其桥接状态。
	for _, user := range cfg.Users {
		if user.DeviceID == "" {
			continue
		}
		entry := DeviceStatus{DeviceID: user.DeviceID, Active: active[user.DeviceID], Backend: backends[user.DeviceID]}
		if device, err := manager.store.Device(ctx, user.DeviceID); err == nil {
			entry.AudioDevice = strings.TrimSpace(device.AudioDevice)
			if entry.AudioDevice == "" && entry.Backend != sipgw.BackendIMS {
				entry.Detail = "设备未配置 audio_device"
			}
		} else {
			entry.Detail = "设备不存在"
		}
		status.Devices = append(status.Devices, entry)
	}
	return status
}

// mergeSecrets 在提交配置中密码为空或为掩码时，沿用已保存的密码。
func mergeSecrets(incoming, current sipgw.Config) sipgw.Config {
	existing := make(map[string]string, len(current.Users))
	for _, user := range current.Users {
		existing[strings.ToLower(user.Username)] = user.Password
	}
	for index := range incoming.Users {
		password := strings.TrimSpace(incoming.Users[index].Password)
		if password == "" || password == store.SecretMask {
			if previous, ok := existing[strings.ToLower(strings.TrimSpace(incoming.Users[index].Username))]; ok {
				incoming.Users[index].Password = previous
			}
		}
	}
	return incoming
}

// deviceATAdapter 把 DeviceAT 绑定到具体设备，供 cscall.ATModem 使用。
type deviceATAdapter struct {
	deviceID string
	devices  DeviceAT
}

func (adapter *deviceATAdapter) DeviceID() string { return adapter.deviceID }

func (adapter *deviceATAdapter) ExecuteAT(ctx context.Context, command string) (modem.Response, error) {
	return adapter.devices.ExecuteAT(ctx, adapter.deviceID, command)
}

func (adapter *deviceATAdapter) SubscribeCallURCs(ctx context.Context) (<-chan string, error) {
	return adapter.devices.SubscribeCallURCs(ctx, adapter.deviceID)
}

func (adapter *deviceATAdapter) PCMReadyTimeout() time.Duration { return 5 * time.Second }

// Package voicegateway 协调 SIP 网关与各设备的 CS 域语音桥接。
//
// 它负责：读取/保存语音网关配置、按配置启动 SIP 注册服务、为每台配置了
// ALSA 音频设备的模组创建通话管理器，并把软电话的呼叫信令路由到对应设备。
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
	"vocat/internal/modem"
	"vocat/internal/sipgw"
	"vocat/internal/store"
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

// Options 构造协调器所需依赖。
type Options struct {
	Devices DeviceAT
	Store   Store
	Logger  *slog.Logger
}

// DeviceStatus 描述单台设备的桥接状态。
type DeviceStatus struct {
	DeviceID    string `json:"device_id"`
	AudioDevice string `json:"audio_device"`
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

type bridge struct {
	manager *cscall.Manager
}

// Manager 协调 SIP 网关与逐设备通话桥。
type Manager struct {
	devices DeviceAT
	store   Store
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
	manager.bridges[deviceID] = &bridge{manager: callManager}
	return nil
}

func (manager *Manager) stopLocked() {
	for deviceID, item := range manager.bridges {
		item.manager.Stop()
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
	item := manager.bridges[deviceID]
	manager.mu.Unlock()
	if item == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 480, "Temporarily Unavailable", nil))
		return
	}
	item.manager.HandleOutboundInvite(deviceID, req, tx)
}

func (manager *Manager) routeCancel(deviceID string, req *sip.Request, _ sip.ServerTransaction) {
	manager.mu.Lock()
	item := manager.bridges[deviceID]
	manager.mu.Unlock()
	if item == nil || req.CallID() == nil {
		return
	}
	item.manager.HandleClientCancel(req.CallID().Value())
}

func (manager *Manager) routeBye(deviceID string, req *sip.Request, _ sip.ServerTransaction) {
	manager.mu.Lock()
	item := manager.bridges[deviceID]
	manager.mu.Unlock()
	if item == nil || req.CallID() == nil {
		return
	}
	item.manager.HandleClientBye(req.CallID().Value())
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
	for deviceID := range manager.bridges {
		active[deviceID] = true
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
		entry := DeviceStatus{DeviceID: user.DeviceID, Active: active[user.DeviceID]}
		if device, err := manager.store.Device(ctx, user.DeviceID); err == nil {
			entry.AudioDevice = strings.TrimSpace(device.AudioDevice)
			if entry.AudioDevice == "" {
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

package cscall

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ATController 是基于 AT 串口指令的呼叫控制器实现。
type ATController struct {
	modem  ATModem
	logger *slog.Logger

	events   chan Event
	pcmReady chan bool

	startOnce sync.Once
	cancel    context.CancelFunc
}

// NewATController 创建 AT 呼叫控制器。
func NewATController(deviceModem ATModem, logger *slog.Logger) *ATController {
	if logger == nil {
		logger = slog.Default()
	}
	return &ATController{
		modem:    deviceModem,
		logger:   logger,
		events:   make(chan Event, 8),
		pcmReady: make(chan bool, 8),
	}
}

// Start 启动控制器：开启 UAC 语音模式并开始监听通话相关 URC。
func (controller *ATController) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	controller.cancel = cancel

	// 开启 UAC 语音模式，使模组语音通道输出到 USB 声卡（音频桥必需）。
	go func() {
		enableCtx, enableCancel := context.WithTimeout(runCtx, 3*time.Second)
		defer enableCancel()
		if _, err := controller.modem.ExecuteAT(enableCtx, "AT+QPCMV=1,2"); err != nil {
			controller.logger.Warn("开启模组 UAC 语音模式失败（将依赖后续呼叫时重试）",
				"device_id", controller.modem.DeviceID(), "err", err)
		}
	}()

	urcs, err := controller.modem.SubscribeCallURCs(runCtx)
	if err != nil {
		cancel()
		return err
	}
	go controller.consumeURCs(runCtx, urcs)
	return nil
}

// Stop 停止控制器。
func (controller *ATController) Stop() {
	if controller.cancel != nil {
		controller.cancel()
	}
}

func (controller *ATController) consumeURCs(ctx context.Context, urcs <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-urcs:
			if !ok {
				return
			}
			controller.handleURC(line)
		}
	}
}

func (controller *ATController) handleURC(line string) {
	upper := strings.ToUpper(strings.TrimSpace(line))
	switch {
	case upper == "RING" || upper == "CALL READY":
		controller.emit(Event{Type: EventIncoming, CallID: "at", Number: "Unknown"})
	case strings.HasPrefix(upper, "+CLIP:"):
		number := parseCLIPNumber(line)
		controller.emit(Event{Type: EventIncoming, CallID: "at", Number: number})
	case strings.HasPrefix(upper, "+QPCMV:"):
		controller.emitPCM(qpcmvReady(line))
	case upper == "NO CARRIER" || upper == "BUSY" || upper == "NO ANSWER":
		controller.emit(Event{Type: EventHangup, CallID: "at"})
	}
}

// Dial 通过 ATD 发起外呼。
func (controller *ATController) Dial(ctx context.Context, number string) (CallRef, error) {
	if _, err := controller.modem.ExecuteAT(ctx, "ATD"+number+";"); err != nil {
		return CallRef{}, err
	}
	return CallRef{ID: "at", Number: number}, nil
}

// Answer 接听来电。
func (controller *ATController) Answer(ctx context.Context, callID string) error {
	_, err := controller.modem.ExecuteAT(ctx, "ATA")
	return err
}

// Hangup 挂断。SendModemSignal 为 true 时先发 ATH，最后总是清理 QPCMV。
func (controller *ATController) Hangup(ctx context.Context, callID string, opts HangupOptions) error {
	var hangupErr error
	if opts.SendModemSignal {
		if _, err := controller.modem.ExecuteAT(ctx, "ATH"); err != nil {
			hangupErr = err
		}
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if _, err := controller.modem.ExecuteAT(cleanupCtx, "AT+QPCMV=0"); err != nil {
		controller.logger.Debug("清理 QPCMV 失败", "device_id", controller.modem.DeviceID(), "err", err)
	}
	return hangupErr
}

// GetCalls 通过 AT+CLCC 查询当前活跃语音呼叫。
func (controller *ATController) GetCalls(ctx context.Context) ([]CallInfo, error) {
	response, err := controller.modem.ExecuteAT(ctx, "AT+CLCC")
	if err != nil {
		return nil, err
	}
	if containsCLCCActive(response.Text()) {
		return []CallInfo{{ID: "at", Direction: "out", State: CallStateConnected}}, nil
	}
	return nil, nil
}

// Events 返回呼叫事件通道。
func (controller *ATController) Events() <-chan Event { return controller.events }

// PCMReady 返回 PCM 就绪状态通道。
func (controller *ATController) PCMReady() <-chan bool { return controller.pcmReady }

func (controller *ATController) emit(event Event) {
	select {
	case controller.events <- event:
	default:
	}
}

func (controller *ATController) emitPCM(ready bool) {
	select {
	case controller.pcmReady <- ready:
	default:
	}
}

// parseCLIPNumber 从 +CLIP: "+8613800138000",145,... 中提取号码。
func parseCLIPNumber(line string) string {
	body := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, `"`) {
		if end := strings.Index(body[1:], `"`); end >= 0 {
			return body[1 : 1+end]
		}
	}
	if index := strings.Index(body, ","); index >= 0 {
		return strings.Trim(body[:index], `" `)
	}
	return strings.Trim(body, `" `)
}

// qpcmvReady 解析 +QPCMV: <state>，state==1 表示就绪。
func qpcmvReady(line string) bool {
	body := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
	fields := strings.FieldsFunc(body, func(r rune) bool { return r == ',' || r == ' ' })
	if len(fields) == 0 {
		return false
	}
	return strings.TrimSpace(fields[0]) == "1"
}

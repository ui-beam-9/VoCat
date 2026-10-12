// Package cscall 实现 CS 域（电路域）语音呼叫到 SIP/RTP 的桥接。
//
// 它把 4G 模组的电路域呼叫（ATD/ATA/ATH/CLCC + USB 声卡 PCM）与软电话
// （Linphone 等）的 SIP/RTP 语音对接起来：来电时向软电话发 INVITE，
// 外呼时用软电话的 INVITE 触发模组拨号，通话中把两侧音频双向桥接。
package cscall

import (
	"context"
	"time"

	"vocat/internal/modem"
)

// CallState 描述 CS 呼叫状态。
type CallState int

const (
	// CallStateIdle 空闲。
	CallStateIdle CallState = iota
	// CallStateRinging 来电振铃中（或外呼等待接通中）。
	CallStateRinging
	// CallStateDialing 外呼正在拨号。
	CallStateDialing
	// CallStateConnected 已接通。
	CallStateConnected
)

// EventType 描述呼叫事件类型。
type EventType string

const (
	// EventIncoming 收到来电。
	EventIncoming EventType = "incoming"
	// EventDialing 正在拨号外呼。
	EventDialing EventType = "dialing"
	// EventRinging 对方振铃。
	EventRinging EventType = "ringing"
	// EventConnected 已接通。
	EventConnected EventType = "connected"
	// EventHangup 已挂断。
	EventHangup EventType = "hangup"
)

// Event 表示一次呼叫状态变化。
type Event struct {
	Type   EventType
	CallID string
	Number string
}

// CallRef 标识一次已发起的呼叫。
type CallRef struct {
	ID     string
	Number string
}

// CallInfo 描述一个活跃呼叫。
type CallInfo struct {
	ID        string
	Number    string
	Direction string // "in" 来电 / "out" 去电
	State     CallState
}

// HangupOptions 控制挂断行为。
type HangupOptions struct {
	// SendModemSignal 为 true 时向模组发送 ATH 挂断信令。
	SendModemSignal bool
}

// Controller 是电路域（CS）呼叫控制器的核心接口。
type Controller interface {
	Start(ctx context.Context) error
	Stop()
	Dial(ctx context.Context, number string) (CallRef, error)
	Answer(ctx context.Context, callID string) error
	Hangup(ctx context.Context, callID string, opts HangupOptions) error
	GetCalls(ctx context.Context) ([]CallInfo, error)
	Events() <-chan Event
	PCMReady() <-chan bool
}

// ATModem 是 CS 通话控制面对底层 AT 模组的最小依赖，
// 由 device.Manager 通过适配器实现，避免本包反向依赖设备层。
type ATModem interface {
	// DeviceID 返回绑定的设备 ID。
	DeviceID() string
	// ExecuteAT 执行一条 AT 命令。
	ExecuteAT(ctx context.Context, command string) (modem.Response, error)
	// SubscribeCallURCs 订阅通话相关 URC（RING/+CLIP/+QPCMV/NO CARRIER），
	// 返回的通道在 ctx 取消时关闭。
	SubscribeCallURCs(ctx context.Context) (<-chan string, error)
	// PCMReadyTimeout 返回等待 PCM 就绪的兜底时长。
	PCMReadyTimeout() time.Duration
}

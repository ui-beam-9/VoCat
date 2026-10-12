package device

import (
	"context"
	"strings"
	"time"

	"vocat/internal/modem"
)

// callURCDrainer 是支持非侵入式 URC 轮询的会话能力。
type callURCDrainer interface {
	DrainURCs(context.Context, time.Duration, func(string) bool) (string, bool, error)
}

// callURCWait 是单次轮询 URC 的等待窗口。取值需要在“及时性”和“占用设备
// 操作锁”之间折中：越短越及时，但会越频繁地争用 opMu。
const callURCWait = 400 * time.Millisecond

// isCallURC 判断一行输出是否属于 CS 通话相关 URC。
func isCallURC(line string) bool {
	upper := strings.ToUpper(strings.TrimSpace(line))
	switch upper {
	case "RING", "BUSY", "NO ANSWER", "NO CARRIER":
		return true
	}
	for _, prefix := range []string{"+CLIP:", "+CRING:", "+QPCMV:"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// SubscribeCallURCs 订阅某台设备的通话相关 URC（RING/+CLIP/+QPCMV/NO CARRIER）。
//
// 它在后台循环轮询 URC 队列与串口，把匹配的行转发到返回的通道；ctx 取消或
// 设备会话关闭时通道会关闭。该订阅与普通 AT 命令通过设备操作锁串行化，
// 单次最多占用 callURCWait，避免影响其它功能。
func (manager *Manager) SubscribeCallURCs(ctx context.Context, id string) (<-chan string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	state, err := manager.lookup(id)
	if err != nil {
		return nil, err
	}
	output := make(chan string, 32)
	go func() {
		defer close(output)
		backoff := 0
		for {
			if ctx.Err() != nil {
				return
			}
			line, found, err := manager.pollCallURC(ctx, id, state)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// 会话暂不可用：短暂退避后重试。
				backoff++
				if backoff > 25 {
					backoff = 25
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(backoff) * 200 * time.Millisecond):
				}
				continue
			}
			backoff = 0
			if !found {
				continue
			}
			select {
			case output <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	return output, nil
}

func (manager *Manager) pollCallURC(ctx context.Context, id string, state *managedDevice) (string, bool, error) {
	state.opMu.Lock()
	defer state.opMu.Unlock()
	if err := manager.validateActive(id, state); err != nil {
		return "", false, err
	}
	client, err := manager.clientLocked(ctx, state, manager.candidateFor(state))
	if err != nil {
		return "", false, err
	}
	drainer, ok := client.(callURCDrainer)
	if !ok {
		return "", false, modem.ErrUnsupportedPlatform
	}
	line, found, err := drainer.DrainURCs(ctx, callURCWait, isCallURC)
	if err != nil {
		return "", false, err
	}
	return line, found, nil
}

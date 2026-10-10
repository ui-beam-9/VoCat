package server

import (
	"context"
	"strings"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// voWiFiCallRecordInterval 是服务端扫描 VoWiFi 通话终态的周期。
const voWiFiCallRecordInterval = 2 * time.Second

// StartVoWiFiCallRecorder 在服务端持续记录 VoWiFi 通话结果，
// 因此通话记录不依赖浏览器是否打开通话页。
func (s *Server) StartVoWiFiCallRecorder(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	controller, ok := s.vowifi.(VoWiFiCallController)
	if !ok {
		return
	}
	ticker := time.NewTicker(voWiFiCallRecordInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.recordVoWiFiCalls(ctx, controller)
		}
	}
}

func (s *Server) recordVoWiFiCalls(ctx context.Context, controller VoWiFiCallController) {
	devices, err := s.store.ListDevices(ctx)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, config := range devices {
		calls, err := controller.Calls(config.ID)
		if err != nil {
			continue
		}
		for _, call := range calls {
			if !isTerminalCallState(call.State) {
				continue
			}
			entry, ok := callHistoryEntryFromCall(config.ID, call, now)
			if !ok {
				continue
			}
			// AppendCallHistory 按 Call-ID 去重，重复上报只会命中已有记录。
			if _, err := s.store.AppendCallHistory(ctx, entry); err != nil {
				s.logger.Warn("记录 VoWiFi 通话失败",
					"device_id", config.ID,
					"call_id", call.ID,
					"error", err,
				)
			}
		}
	}
}

func isTerminalCallState(state string) bool {
	return state == "ended" || state == "failed"
}

// callHistoryEntryFromCall 把一通已结束的 IMS 通话映射为通话记录。
// outcome：failed / answered / missed（未接通，由前端按呼叫方向显示为“未接”或“未接通”）。
func callHistoryEntryFromCall(deviceID string, call vowifi.Call, now time.Time) (store.CallHistoryEntry, bool) {
	number := strings.TrimSpace(call.Number)
	if number == "" {
		return store.CallHistoryEntry{}, false
	}
	direction := call.Direction
	if direction != "incoming" && direction != "outgoing" {
		direction = "outgoing"
	}
	startedAt := call.StartedAt
	if startedAt.IsZero() {
		startedAt = now
	}
	endedAt := now
	if call.EndedAt != nil {
		endedAt = *call.EndedAt
	}
	outcome := "missed"
	duration := 0
	switch {
	case call.State == "failed":
		outcome = "failed"
	case call.AnsweredAt != nil:
		outcome = "answered"
		if call.AnsweredAt.Before(endedAt) {
			duration = int(endedAt.Sub(*call.AnsweredAt).Seconds())
		}
	}
	if duration < 0 {
		duration = 0
	}
	return store.CallHistoryEntry{
		CallID:          call.ID,
		DeviceID:        deviceID,
		Number:          number,
		Direction:       direction,
		Outcome:         outcome,
		StartedAt:       startedAt,
		EndedAt:         endedAt,
		DurationSeconds: duration,
	}, true
}

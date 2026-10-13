package server

import (
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func TestCallHistoryEntryFromCallOutcome(t *testing.T) {
	now := time.Now().UTC()
	answeredAt := now.Add(-time.Minute)
	base := vowifi.Call{
		ID:        "call-1",
		Number:    "10086",
		Direction: "incoming",
		State:     "ended",
		StartedAt: now.Add(-2 * time.Minute),
	}

	// 本地拒接：呼入振铃期挂断，IMS 层回 486 并记入 SIPCode。
	rejected := base
	rejected.SIPCode = 486
	entry, ok := callHistoryEntryFromCall("dev-1", rejected, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "rejected" {
		t.Fatalf("outcome = %q, want rejected", entry.Outcome)
	}

	// 对方取消（CANCEL）：无 SIPCode，仍为未接。
	missed := base
	entry, ok = callHistoryEntryFromCall("dev-1", missed, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "missed" {
		t.Fatalf("outcome = %q, want missed", entry.Outcome)
	}

	// 已接通优先于 SIPCode 判定。
	answered := base
	answered.AnsweredAt = &answeredAt
	answered.SIPCode = 486
	entry, ok = callHistoryEntryFromCall("dev-1", answered, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "answered" {
		t.Fatalf("outcome = %q, want answered", entry.Outcome)
	}

	// 呼出被对端拒接（486 Busy Here）：同样判定为拒接，前端按方向显示「对方已拒接」。
	outgoing := base
	outgoing.Direction = "outgoing"
	outgoing.State = "failed"
	outgoing.SIPCode = 486
	entry, ok = callHistoryEntryFromCall("dev-1", outgoing, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "rejected" {
		t.Fatalf("outcome = %q, want rejected", entry.Outcome)
	}

	// 呼出被对端拒接（603 Decline）：同样判定为拒接。
	outgoing603 := base
	outgoing603.Direction = "outgoing"
	outgoing603.State = "failed"
	outgoing603.SIPCode = 603
	entry, ok = callHistoryEntryFromCall("dev-1", outgoing603, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "rejected" {
		t.Fatalf("outcome = %q, want rejected", entry.Outcome)
	}

	// 呼出无人接听（480 Temporarily Unavailable）：未接通。
	outgoing480 := base
	outgoing480.Direction = "outgoing"
	outgoing480.State = "failed"
	outgoing480.SIPCode = 480
	entry, ok = callHistoryEntryFromCall("dev-1", outgoing480, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "missed" {
		t.Fatalf("outcome = %q, want missed", entry.Outcome)
	}

	// 呼出空号（404 Not Found）：未接通。
	outgoing404 := base
	outgoing404.Direction = "outgoing"
	outgoing404.State = "failed"
	outgoing404.SIPCode = 404
	entry, ok = callHistoryEntryFromCall("dev-1", outgoing404, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "missed" {
		t.Fatalf("outcome = %q, want missed", entry.Outcome)
	}

	// 呼出网络失败（事务超时，无 SIPCode）：未接通。
	outgoingTimeout := base
	outgoingTimeout.Direction = "outgoing"
	outgoingTimeout.State = "failed"
	entry, ok = callHistoryEntryFromCall("dev-1", outgoingTimeout, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "missed" {
		t.Fatalf("outcome = %q, want missed", entry.Outcome)
	}
}

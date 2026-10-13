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

	// 呼出方向不判定拒接。
	outgoing := base
	outgoing.Direction = "outgoing"
	outgoing.SIPCode = 486
	entry, ok = callHistoryEntryFromCall("dev-1", outgoing, now)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Outcome != "missed" {
		t.Fatalf("outcome = %q, want missed", entry.Outcome)
	}
}

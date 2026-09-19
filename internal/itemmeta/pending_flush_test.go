package itemmeta

import (
	"encoding/json"
	"testing"
)

func TestSetPendingFlushMarksAndClears(t *testing.T) {
	marked, err := SetPendingFlush(`{"sendId":"send-1"}`, true)
	if err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(marked), &m); err != nil {
		t.Fatalf("marked meta is not valid JSON: %v", err)
	}
	if m["pendingFlush"] != true || m["sendId"] != "send-1" {
		t.Fatalf("marked meta = %q, want the marker beside the existing keys", marked)
	}

	cleared, err := SetPendingFlush(marked, false)
	if err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	// A fresh map: unmarshalling into the previous one would MERGE, and
	// the stale marker would hide the very removal under test.
	m = nil
	if err := json.Unmarshal([]byte(cleared), &m); err != nil {
		t.Fatalf("cleared meta is not valid JSON: %v", err)
	}
	// Presence is the marker's whole meaning: a confirmed row must be
	// byte-identical to the same row imported from a session file, which
	// never carried the key at all.
	if _, present := m["pendingFlush"]; present {
		t.Fatalf("cleared meta = %q, want the key removed", cleared)
	}
	if m["sendId"] != "send-1" {
		t.Fatalf("cleared meta = %q, want unrelated keys preserved", cleared)
	}
}

func TestSetPendingFlushClearsToAbsentMeta(t *testing.T) {
	marked, err := SetPendingFlush("", true)
	if err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	cleared, err := SetPendingFlush(marked, false)
	if err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	if cleared != "" {
		t.Fatalf("cleared meta = %q, want the absent-meta shape a row with no metadata stores", cleared)
	}
}

func TestSetPendingFlushClearIsIdempotent(t *testing.T) {
	cleared, err := SetPendingFlush(`{"sendId":"send-1"}`, false)
	if err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	if cleared != `{"sendId":"send-1"}` {
		t.Fatalf("clearing an unmarked meta = %q, want it unchanged", cleared)
	}
}

func TestSetPendingFlushPreservesLargeNumbers(t *testing.T) {
	marked, err := SetPendingFlush(`{"counter":9007199254740993}`, true)
	if err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	cleared, err := SetPendingFlush(marked, false)
	if err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	if cleared != `{"counter":9007199254740993}` {
		t.Fatalf("round trip = %q, want the counter intact", cleared)
	}
}

func TestSetPendingFlushRejectsMalformed(t *testing.T) {
	for _, pending := range []bool{true, false} {
		if _, err := SetPendingFlush(`{broken`, pending); err == nil {
			t.Fatalf("SetPendingFlush(malformed, %v) = nil error, want a decode failure", pending)
		}
	}
}

func TestMarkPromotedAtInterruptClearsPendingFlush(t *testing.T) {
	marked, err := SetPendingFlush(`{"sendId":"send-1"}`, true)
	if err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	promoted, err := MarkPromotedAtInterrupt(marked)
	if err != nil {
		t.Fatalf("MarkPromotedAtInterrupt: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(promoted), &m); err != nil {
		t.Fatalf("promoted meta is not valid JSON: %v", err)
	}
	if _, present := m["pendingFlush"]; present {
		t.Fatalf("promoted meta = %q, want the pending marker removed", promoted)
	}
	if m[promotedAtInterruptKey] != true || m["sendId"] != "send-1" {
		t.Fatalf("promoted meta = %q, want the promotion marker beside the existing keys", promoted)
	}
}

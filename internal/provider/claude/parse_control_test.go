package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseRateLimitEvent_FiveHourUtilization pins the wire round-trip
// for `rate_limit_event` envelopes carrying a `five_hour` window with
// utilization. The previous parser hardcoded UsedPercent based on
// `status` ("allowed" → 0, otherwise → 100), discarding the wire's
// `utilization` field — that meant the UI ring would always read 0%
// while healthy and pin to 100% the moment Claude warned. This test
// guards against regressing to that shape.
func TestParseRateLimitEvent_FiveHourUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1776981600,"rateLimitType":"five_hour","utilization":0.42,"isUsingOverage":false}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventRateLimits {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventRateLimits)
	}

	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.Provider != string(provider.Claude) {
		t.Errorf("Provider: got %q, want %q", snap.Provider, provider.Claude)
	}
	if len(snap.Limits) != 1 {
		t.Fatalf("Limits len: got %d, want 1", len(snap.Limits))
	}
	got := snap.Limits[0]
	if got.LimitID != "five_hour" {
		t.Errorf("LimitID: got %q, want %q", got.LimitID, "five_hour")
	}
	if got.UsedPercent != 42 {
		t.Errorf("UsedPercent: got %v, want 42 (utilization 0.42 × 100)", got.UsedPercent)
	}
	if got.WindowMins != 300 {
		t.Errorf("WindowMins: got %d, want 300 (five_hour)", got.WindowMins)
	}
	if got.ResetsAt != 1776981600 {
		t.Errorf("ResetsAt: got %d, want 1776981600", got.ResetsAt)
	}
}

// TestParseRateLimitEvent_SevenDayUtilization mirrors the five-hour
// case for the seven-day window. seven_day → 10080 minutes.
func TestParseRateLimitEvent_SevenDayUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1776981600,"rateLimitType":"seven_day","utilization":0.51,"isUsingOverage":false}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(snap.Limits) != 1 {
		t.Fatalf("Limits len: got %d, want 1", len(snap.Limits))
	}
	got := snap.Limits[0]
	if got.LimitID != "seven_day" {
		t.Errorf("LimitID: got %q, want %q", got.LimitID, "seven_day")
	}
	if got.UsedPercent != 51 {
		t.Errorf("UsedPercent: got %v, want 51", got.UsedPercent)
	}
	if got.WindowMins != 10080 {
		t.Errorf("WindowMins: got %d, want 10080 (seven_day)", got.WindowMins)
	}
}

// TestParseRateLimitEvent_MissingUtilization covers the wire shape
// observed in some "allowed" events (e.g. fixtures
// local_agent_outlives.ndjson) where Claude omits `utilization`
// entirely and surfaces overage metadata instead. Go's zero-value
// gives UsedPercent=0 — the right empty-ring rendering.
func TestParseRateLimitEvent_MissingUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1777920000,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := snap.Limits[0]
	if got.UsedPercent != 0 {
		t.Errorf("UsedPercent: got %v, want 0 (utilization absent)", got.UsedPercent)
	}
	if got.WindowMins != 300 {
		t.Errorf("WindowMins: got %d, want 300", got.WindowMins)
	}
	if got.ResetsAt != 1777920000 {
		t.Errorf("ResetsAt: got %d, want 1777920000", got.ResetsAt)
	}
}

// TestParseRateLimitEvent_FullUtilization pins the upper bound: 1.0
// utilization renders as 100. Without the parser fix, anything except
// `status:"allowed"` already hit 100 by accident — make sure the
// utilization path produces it correctly.
func TestParseRateLimitEvent_FullUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"overloaded","resetsAt":1776981600,"rateLimitType":"five_hour","utilization":1.0}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Limits[0].UsedPercent != 100 {
		t.Errorf("UsedPercent: got %v, want 100", snap.Limits[0].UsedPercent)
	}
}

// TestParseRateLimitEvent_UnknownRateLimitType — when the wire emits a
// rateLimitType we don't recognise, WindowMins falls back to 0 and
// the LimitID is preserved. This keeps the parser future-proof if
// Claude ships a third window length.
func TestParseRateLimitEvent_UnknownRateLimitType(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1776981600,"rateLimitType":"thirty_day","utilization":0.1}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := snap.Limits[0]
	if got.LimitID != "thirty_day" {
		t.Errorf("LimitID: got %q, want %q", got.LimitID, "thirty_day")
	}
	if got.WindowMins != 0 {
		t.Errorf("WindowMins: got %d, want 0 (unknown type → fallback)", got.WindowMins)
	}
	if got.UsedPercent != 10 {
		t.Errorf("UsedPercent: got %v, want 10", got.UsedPercent)
	}
}

// TestParseRateLimitEvent_MissingInfo — the envelope without a
// `rate_limit_info` payload is dropped silently. This matches the
// existing parser contract (return nil events, no error).
func TestParseRateLimitEvent_MissingInfo(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

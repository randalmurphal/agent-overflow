package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func decodeSessionInfo(t *testing.T, evt provider.ProviderEvent) provider.SessionInfo {
	t.Helper()
	if evt.Kind != provider.EventInit {
		t.Fatalf("kind = %q, want %q", evt.Kind, provider.EventInit)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(evt.Meta, &info); err != nil {
		t.Fatalf("decode session info: %v", err)
	}
	return info
}

func turnCompleteFastMode(t *testing.T, evt provider.ProviderEvent) *provider.FastModeStatus {
	t.Helper()
	meta, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta)
	if !ok {
		t.Fatalf("TurnComplete payload type = %T, want *provider.WireTurnCompleteMeta", evt.TurnComplete)
	}
	return meta.FastMode
}

func TestParseResult_FastModeStateAndReason(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"result","subtype":"success","is_error":false,`+
			`"fast_mode_state":"cooldown","fast_mode_disabled_reason":"network_error"}`))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	status := turnCompleteFastMode(t, events[0])
	if status == nil {
		t.Fatal("FastMode = nil, want a status")
	}
	if status.State != "cooldown" || status.DisabledReason != "network_error" {
		t.Fatalf("FastMode = %+v, want {cooldown network_error}", *status)
	}
}

// A CLI old enough to report `fast_mode_state` but not
// `fast_mode_disabled_reason` (2.1.105) must still surface the state. The
// missing reason is silence, not an empty answer that would be rendered
// as a denial.
func TestParseResult_FastModeStateWithoutReasonIsVersionTolerant(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"result","subtype":"success","is_error":false,"fast_mode_state":"on"}`))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	status := turnCompleteFastMode(t, events[0])
	if status == nil {
		t.Fatal("FastMode = nil, want a status carrying state only")
	}
	if status.State != "on" || status.DisabledReason != "" {
		t.Fatalf("FastMode = %+v, want {on \"\"}", *status)
	}
}

// Neither key present is NOT "fast mode is off" — it is no signal at all,
// and the event must carry nil so nothing downstream can read a denial
// into it.
func TestParseResult_FastModeAbsentIsNil(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(`{"type":"result","subtype":"success","is_error":false}`))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if status := turnCompleteFastMode(t, events[0]); status != nil {
		t.Fatalf("FastMode = %+v, want nil", *status)
	}
}

func TestParseSystemInit_FastModeStatus(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-4-7",`+
			`"fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required"}`))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	info := decodeSessionInfo(t, events[0])
	if info.FastMode == nil {
		t.Fatal("SessionInfo.FastMode = nil, want a status")
	}
	if info.FastMode.State != "off" || info.FastMode.DisabledReason != "sdk_opt_in_required" {
		t.Fatalf("FastMode = %+v, want {off sdk_opt_in_required}", *info.FastMode)
	}
}

func TestParseSystemInit_FastModeAbsentIsNil(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-4-7"}`))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if info := decodeSessionInfo(t, events[0]); info.FastMode != nil {
		t.Fatalf("SessionInfo.FastMode = %+v, want nil", *info.FastMode)
	}
}

// The 2.1.105 capture is the version-tolerance anchor: it carries
// fast_mode_state on `result` and has no fast_mode_disabled_reason key
// anywhere. Parsing it must not invent a reason.
func TestParseResult_RealOutputFixtureCarriesStateWithoutReason(t *testing.T) {
	file, err := os.Open("testdata/real_output.ndjson")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()

	parser := NewParser()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	var seen int
	for scanner.Scan() {
		line := scanner.Bytes()
		if !strings.Contains(string(line), `"type":"result"`) {
			continue
		}
		events, err := parser.ParseLine(testThread, append([]byte(nil), line...))
		if err != nil {
			t.Fatalf("parse result line: %v", err)
		}
		status := turnCompleteFastMode(t, events[0])
		if status == nil {
			t.Fatal("FastMode = nil, want the fixture's fast_mode_state")
		}
		if status.State == "" {
			t.Fatalf("FastMode.State empty, want the fixture's value")
		}
		if status.DisabledReason != "" {
			t.Fatalf("FastMode.DisabledReason = %q, want empty (2.1.105 has no reason key)", status.DisabledReason)
		}
		seen++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if seen == 0 {
		t.Fatal("fixture produced no result envelopes")
	}
}

func TestExtractFastModeStatus_BoundsOversizedFields(t *testing.T) {
	long := strings.Repeat("x", maxFastModeFieldRunes*3)
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"result","subtype":"success","fast_mode_state":"`+long+`"}`))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	status := turnCompleteFastMode(t, events[0])
	if status == nil {
		t.Fatal("FastMode = nil, want a bounded status")
	}
	if got := len([]rune(status.State)); got != maxFastModeFieldRunes {
		t.Fatalf("State rune length = %d, want %d", got, maxFastModeFieldRunes)
	}
}

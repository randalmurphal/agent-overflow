package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestParseInvalidJSON(t *testing.T) {
	_, err := ParseLine(testThread, []byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseUnknownType(t *testing.T) {
	line := []byte(`{"type":"some_future_event","data":{}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown type, got %d", len(events))
	}
}

func TestParseUserTypeSkipped(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for user type, got %d", len(events))
	}
}

// TestParseRealCLIFixture validates the parser against real Claude CLI
// output. The fixture predates `--include-partial-messages`, so text
// reaches triage via `stream_event` deltas now rather than via the
// coalesced `assistant` envelope — the `EventTextDelta` check that
// previously ran here has moved to the unit tests in
// `partial_messages_test.go`. This test's bar is "the parser handles
// every line without error and recognises the session bookends."
func TestParseRealCLIFixture(t *testing.T) {
	f, err := os.Open("testdata/real_output.ndjson")
	if err != nil {
		t.Skipf("skipping real fixture test: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lineNum int
	var foundInit, foundResult bool

	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		events, err := ParseLine(testThread, line)
		if err != nil {
			t.Errorf("line %d: parse error: %v", lineNum, err)
			continue
		}

		for _, evt := range events {
			switch evt.Kind {
			case provider.EventInit:
				foundInit = true
				var info provider.SessionInfo
				if err := json.Unmarshal(evt.Meta, &info); err != nil {
					t.Errorf("line %d: unmarshal session info: %v", lineNum, err)
				}
				if info.SessionID == "" {
					t.Errorf("line %d: init event has empty session ID", lineNum)
				}
				if info.Model == "" {
					t.Errorf("line %d: init event has empty model", lineNum)
				}
			case provider.EventTurnComplete:
				foundResult = true
			}
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	if !foundInit {
		t.Error("fixture missing system/init event")
	}
	if !foundResult {
		t.Error("fixture missing result/turn_complete event")
	}

	t.Logf("processed %d lines from real fixture: init=%v result=%v",
		lineNum, foundInit, foundResult)
}

// TestParseLineWarnsOnceForUnknownEnvelopeType pins the area guide's rule
// that no NDJSON line is dropped silently: an envelope type ParseLine has no
// case for is ignored (never fatal, never a dropped read loop) but logged —
// once per type per parser lifetime, no matter how often it repeats.
func TestParseLineWarnsOnceForUnknownEnvelopeType(t *testing.T) {
	p := NewParser()

	out := captureLog(t, func() {
		for range 5 {
			events, err := p.ParseLine(testThread, []byte(`{"type":"quantum_flux","payload":{}}`))
			if err != nil {
				t.Fatalf("unknown envelope must not error: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("unknown envelope emitted %d events, want 0", len(events))
			}
		}
	})

	if got := strings.Count(out, "quantum_flux"); got != 1 {
		t.Fatalf("logged the unknown type %d times, want exactly 1:\n%s", got, out)
	}
	if !strings.Contains(out, "unknown NDJSON envelope type") {
		t.Fatalf("log line does not name the drop:\n%s", out)
	}

	// A DIFFERENT unknown type is its own first sighting.
	out = captureLog(t, func() {
		if _, err := p.ParseLine(testThread, []byte(`{"type":"tachyon_pulse"}`)); err != nil {
			t.Fatalf("unknown envelope must not error: %v", err)
		}
		// ...while the first one stays quiet.
		if _, err := p.ParseLine(testThread, []byte(`{"type":"quantum_flux"}`)); err != nil {
			t.Fatalf("unknown envelope must not error: %v", err)
		}
	})
	if !strings.Contains(out, "tachyon_pulse") {
		t.Fatalf("second unknown type was not logged:\n%s", out)
	}
	if strings.Contains(out, "quantum_flux") {
		t.Fatalf("already-reported type logged again:\n%s", out)
	}
}

// TestParseLineUnknownEnvelopeWarningIsBounded pins the cap: a stream
// inventing a fresh type per line must not grow the dedup set for the
// session's lifetime, and the suppression notice itself is logged once.
func TestParseLineUnknownEnvelopeWarningIsBounded(t *testing.T) {
	p := NewParser()

	out := captureLog(t, func() {
		for i := range maxUnknownEnvelopeTypes + 10 {
			line := fmt.Appendf(nil, `{"type":"drift_%d"}`, i)
			if _, err := p.ParseLine(testThread, line); err != nil {
				t.Fatalf("unknown envelope must not error: %v", err)
			}
		}
	})

	if got := len(p.unknownEnvelopeTypes); got != maxUnknownEnvelopeTypes {
		t.Fatalf("dedup set holds %d entries, want it capped at %d", got, maxUnknownEnvelopeTypes)
	}
	if got := strings.Count(out, "suppressing further drift warnings"); got != 1 {
		t.Fatalf("suppression notice logged %d times, want exactly 1", got)
	}
}

// TestParseLineUnknownEnvelopeOnNilParserIsSilent pins that the one-shot
// stateless path stays usable: a nil parser has no lifetime to dedupe
// against, so it must ignore the envelope without erroring or logging.
func TestParseLineUnknownEnvelopeOnNilParserIsSilent(t *testing.T) {
	var p *Parser
	out := captureLog(t, func() {
		events, err := p.ParseLine(testThread, []byte(`{"type":"quantum_flux"}`))
		if err != nil {
			t.Fatalf("unknown envelope must not error: %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("unknown envelope emitted %d events, want 0", len(events))
		}
	})
	if out != "" {
		t.Fatalf("nil parser logged %q, want silence", out)
	}
}

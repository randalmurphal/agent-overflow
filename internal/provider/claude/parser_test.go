package claude

import (
	"bufio"
	"encoding/json"
	"os"
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

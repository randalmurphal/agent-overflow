package scenario_test

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// TestLibraryAgainstRealParsers feeds every emit line of every shipped
// scenario through the app's actual wire parsers — the same code a live
// harness session runs. A scenario whose frames the parser rejects (or
// silently drops in an unexpected way) would make the harness lie about
// app behaviour, which is exactly the failure mode this test blocks.
//
// External test package: the scenario package itself must stay
// import-light (ao-mockprovider links it); provider imports live only
// in this test binary.
func TestLibraryAgainstRealParsers(t *testing.T) {
	entries, err := scenario.Library()
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	vars := scenario.Vars{
		"SESSION_ID": "sess-test",
		"THREAD_ID":  "thread-test",
		"TURN":       "1",
		"TURN_ID":    "turn-1",
		"REQUEST_ID": "7",
		"CWD":        "/tmp/workspace",
		"ITER":       "1",
	}
	for _, entry := range entries {
		_, s, err := scenario.LoadLibrary(entry.Name)
		if err != nil {
			t.Fatalf("LoadLibrary(%s): %v", entry.Name, err)
		}
		t.Run(entry.Name, func(t *testing.T) {
			switch s.Provider {
			case scenario.ProviderClaude:
				checkClaudeScenario(t, s, vars)
			case scenario.ProviderCodex:
				checkCodexScenario(t, s, vars)
			}
		})
	}
}

// collectEmitLines flattens every emit line in the scenario, including
// approval branches, substituted against vars.
func collectEmitLines(s *scenario.Scenario, vars scenario.Vars) []string {
	var out []string
	var walk func(steps []scenario.Step)
	walk = func(steps []scenario.Step) {
		for _, step := range steps {
			if step.Emit != nil {
				for _, line := range step.Emit.Lines {
					out = append(out, vars.Substitute(line))
				}
			}
			if step.Approval != nil {
				walk(step.Approval.OnAllow)
				walk(step.Approval.OnDeny)
			}
			if step.Repeat != nil {
				walk(step.Repeat.Steps)
			}
		}
	}
	walk(s.OnStart)
	for _, turn := range s.Turns {
		walk(turn.Steps)
	}
	return out
}

func checkClaudeScenario(t *testing.T, s *scenario.Scenario, vars scenario.Vars) {
	t.Helper()
	parser := claude.NewParser()
	for _, line := range collectEmitLines(s, vars) {
		if _, err := parser.ParseLine("thread-test", []byte(line)); err != nil {
			t.Errorf("claude parser rejected line: %v\n  line: %s", err, line)
		}
	}
}

func checkCodexScenario(t *testing.T, s *scenario.Scenario, vars scenario.Vars) {
	t.Helper()
	for _, line := range collectEmitLines(s, vars) {
		var frame struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			ID      json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Errorf("codex line is not a JSON-RPC frame: %v\n  line: %s", err, line)
			continue
		}
		if len(frame.ID) != 0 {
			continue // request/response frames answer the app, not the classifier
		}
		if frame.Method == "" {
			t.Errorf("codex notification without method: %s", line)
			continue
		}
		// The classifier must not panic and must recognise lifecycle
		// methods; per-method event expectations are asserted below.
		events := codex.ClassifyNotification("thread-test", frame.Method, frame.Params)
		switch frame.Method {
		case "turn/completed":
			if len(events) == 0 {
				t.Errorf("turn/completed produced no events: %s", line)
			}
		case "item/agentMessage/delta":
			if len(events) == 0 {
				t.Errorf("agentMessage delta produced no events: %s", line)
			}
		}
	}
}

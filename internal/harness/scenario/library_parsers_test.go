package scenario_test

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider"
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
	// The same environment the integrity check substitutes with — see
	// scenario.TestVars for why it must not be a second copy.
	vars := scenario.TestVars
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

// TestUsageLimitScenariosCarryTheUsageLimitReason asserts what the two
// usage-limit scenarios claim in their descriptions, which the blanket
// parse-every-line sweep above cannot: that the wire lines reach the app as a
// USAGE-LIMIT failure specifically, not as a generic provider error.
//
// The distinction is the whole point of the scenarios. provider.
// FailureReasonUsageLimit is what internal/workflowhost/quota.go keys on to
// PARK a workflow run (engine.OutcomeProviderUsageLimited) rather than fail it,
// so a wire shape that classified one step lower would send a run to its
// failure path with no test noticing.
//
// What these scenarios deliberately do NOT reach is internal/usagebackoff's
// durable per-account hold. That ledger is fed exclusively by
// claude.RateLimitedError, raised inside the out-of-band HTTPS probe of
// Anthropic's OAuth usage endpoint (claude.ProbeRateLimits, driven by
// app_claude_ratelimits.go). No line on either provider's stdio stream reaches
// Ledger.Note, so no scenario can arm a backoff hold.
func TestUsageLimitScenariosCarryTheUsageLimitReason(t *testing.T) {
	vars := scenario.TestVars

	t.Run("usage-limit-claude", func(t *testing.T) {
		_, s, err := scenario.LoadLibrary("usage-limit-claude")
		if err != nil {
			t.Fatalf("LoadLibrary: %v", err)
		}
		parser := claude.NewParser()
		var sawUsageLimit, sawSpentWindow bool
		for _, line := range collectEmitLines(s, vars) {
			events, err := parser.ParseLine("thread-test", []byte(line))
			if err != nil {
				t.Fatalf("parse %s: %v", line, err)
			}
			for _, event := range events {
				if event.Failure != nil && event.Failure.Reason == provider.FailureReasonUsageLimit {
					sawUsageLimit = true
				}
				if event.Kind == provider.EventRateLimits {
					var snapshot provider.RateLimitsSnapshot
					if err := json.Unmarshal(event.Meta, &snapshot); err != nil {
						t.Fatalf("decode rate limits meta: %v", err)
					}
					for _, limit := range snapshot.Limits {
						// A refused window is spent by definition, and the
						// reset time is the only thing that tells the user
						// when it clears.
						if limit.UsedPercent == 100 && limit.ResetsAt > 0 {
							sawSpentWindow = true
						}
					}
				}
			}
		}
		if !sawUsageLimit {
			t.Error("no event carried FailureReasonUsageLimit; a workflow run would fail instead of parking")
		}
		if !sawSpentWindow {
			t.Error("no rate-limit snapshot reported a spent window with a reset time")
		}
	})

	t.Run("usage-limit-codex", func(t *testing.T) {
		_, s, err := scenario.LoadLibrary("usage-limit-codex")
		if err != nil {
			t.Fatalf("LoadLibrary: %v", err)
		}
		var sawUsageLimit, sawFailedTurn bool
		for _, line := range collectEmitLines(s, vars) {
			var frame struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatalf("decode frame %s: %v", line, err)
			}
			for _, event := range codex.ClassifyNotification("thread-test", frame.Method, frame.Params) {
				if event.Failure != nil && event.Failure.Reason == provider.FailureReasonUsageLimit {
					sawUsageLimit = true
				}
				// The turn must still close through the wire lifecycle:
				// a synthesized completion would hide the failed status.
				if event.Kind != provider.EventTurnComplete {
					continue
				}
				if meta, ok := event.TurnComplete.(*provider.WireTurnCompleteMeta); ok &&
					meta.StopReason == "error" {
					sawFailedTurn = true
				}
			}
		}
		if !sawUsageLimit {
			t.Error("no event carried FailureReasonUsageLimit; a workflow run would fail instead of parking")
		}
		if !sawFailedTurn {
			t.Error("the turn did not close as failed through turn/completed")
		}
	})
}

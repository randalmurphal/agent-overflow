package claudetui

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func inheritedEffortOf(t *testing.T, h parserHarness, itemID string) (string, bool) {
	t.Helper()
	start := toolStartFor(*h.events, itemID)
	if start == nil {
		t.Fatalf("no tool start for %s", itemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(start.Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	effort, ok := meta["inherited_effort"].(string)
	return effort, ok
}

// The terminal's `/effort` never reaches the parser, so an Agent launch
// inherits the effort of the main-loop request that produced it: its
// `output_config.effort`. A request that names none leaves the effort
// unknown instead of the startup effort.
func TestAgentLaunchInheritsItsMainRequestEffort(t *testing.T) {
	h := legacyHarness(t)

	h.runMain(t, `{"model":"claude-opus-4-7","max_tokens":32000,"tools":[{"name":"Agent"}],"output_config":{"effort":"low"}}`,
		agentLaunchSSE("toolu_LOW", "first"))
	if effort, _ := inheritedEffortOf(t, h, "toolu_LOW"); effort != "low" {
		t.Fatalf("launch under a low request inherited %q, want low", effort)
	}

	h.runMain(t, `{"model":"claude-opus-4-7","max_tokens":32000,"tools":[{"name":"Agent"}]}`,
		agentLaunchSSE("toolu_AUTO", "second"))
	if effort, ok := inheritedEffortOf(t, h, "toolu_AUTO"); ok {
		t.Fatalf("launch under a request with no effort inherited %q, want none", effort)
	}
}

// legacyHarness is a parser harness for a CLI before 2.1.317, where a
// launch that names no effort runs at its launcher's, stamped at launch.
func legacyHarness(t *testing.T) parserHarness {
	h := newParserHarness(t)
	h.parser.SetCLIVersion("2.1.316")
	return h
}

// modelReportOf is the meta of the update that records the model the agent
// launched at launchID reports, nil when none was emitted.
func modelReportOf(t *testing.T, h parserHarness, launchID string) map[string]any {
	t.Helper()
	for _, event := range *h.events {
		if event.Kind != provider.EventToolStart || event.ItemID != launchID || event.ItemType != "" {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if _, ok := meta["subagent_model"]; ok {
			return meta
		}
	}
	return nil
}

// A subagent's request carries the effort that subagent runs at, which its
// launch records once, and leaves the session's effort alone.
func TestSubagentRequestEffortIsTheAgentsRunEffort(t *testing.T) {
	h := legacyHarness(t)
	h.runMain(t, `{"model":"claude-opus-4-7","max_tokens":32000,"tools":[{"name":"Agent"}],"output_config":{"effort":"high"}}`,
		agentLaunchSSE("toolu_A", "investigate the bug"))
	sub := `{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"}],"output_config":{"effort":"low"}}`
	if parent := h.runSubagentRequest(t, "aid-1", "investigate the bug", sub, subagentBashSSE("toolu_BASH")); parent == "" {
		t.Fatal("subagent did not resolve its parent")
	}
	if report := modelReportOf(t, h, "toolu_A"); report == nil || report["run_effort"] != "low" {
		t.Fatalf("model report = %v, want run_effort low", report)
	}
	h.runMain(t, `{"model":"claude-opus-4-7","max_tokens":32000,"tools":[{"name":"Agent"}],"output_config":{"effort":"high"}}`,
		agentLaunchSSE("toolu_B", "again"))
	if effort, _ := inheritedEffortOf(t, h, "toolu_B"); effort != "high" {
		t.Fatalf("launch after a subagent request inherited %q, want high", effort)
	}
}

// From 2.1.317 an agent on another model than the main loop's runs at that
// model's own effort: not the session's, which the launch does not claim.
// The agent's request says what it runs at.
func TestAgentOnAnotherModelRecordsItsRequestEffortOnly(t *testing.T) {
	h := newParserHarness(t)
	h.parser.SetCLIVersion("2.1.321")
	launch := agentLaunchSSE("toolu_A", "investigate the bug")
	launch[0] = strings.Replace(launch[0], `"model":"claude-haiku"`, `"model":"claude-sonnet-4-6"`, 1)
	h.runMain(t, `{"model":"claude-sonnet-4-6","max_tokens":32000,"tools":[{"name":"Agent"}],"output_config":{"effort":"high"}}`, launch)
	if effort, ok := inheritedEffortOf(t, h, "toolu_A"); ok {
		t.Fatalf("launch stamped %q before its agent's model is known", effort)
	}
	sub := `{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"}],"output_config":{"effort":"medium"}}`
	h.runSubagentRequest(t, "aid-1", "investigate the bug", sub, subagentBashSSE("toolu_BASH"))
	report := modelReportOf(t, h, "toolu_A")
	if report == nil || report["run_effort"] != "medium" {
		t.Fatalf("model report = %v, want run_effort medium", report)
	}
	if effort, ok := report["inherited_effort"]; ok {
		t.Fatalf("agent on Haiku under a Sonnet session inherited %v", effort)
	}
}

// The CLI clamps the session's effort to the main model: an xhigh session
// on Sonnet 4.6, which lacks xhigh, sends high. The launch records xhigh as
// the tier its high may stand for, which an Opus child would run at.
func TestClampedRequestEffortRecordsItsAlternatives(t *testing.T) {
	h := legacyHarness(t)
	// The API answers with the model the request named.
	launchOn := func(model, toolUseID, prompt string) []string {
		sse := agentLaunchSSE(toolUseID, prompt)
		sse[0] = strings.Replace(sse[0], `"model":"claude-haiku"`, `"model":"`+model+`"`, 1)
		return sse
	}
	h.runMain(t, `{"model":"claude-sonnet-4-6","max_tokens":32000,"tools":[{"name":"Agent"}],"output_config":{"effort":"high"}}`,
		launchOn("claude-sonnet-4-6", "toolu_SONNET", "first"))
	h.runMain(t, `{"model":"claude-opus-4-7","max_tokens":32000,"tools":[{"name":"Agent"}],"output_config":{"effort":"high"}}`,
		launchOn("claude-opus-4-7", "toolu_OPUS", "second"))
	for id, want := range map[string]string{"toolu_SONNET": `["xhigh"]`, "toolu_OPUS": ""} {
		start := toolStartFor(*h.events, id)
		if start == nil {
			t.Fatalf("no tool start for %s", id)
		}
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(start.Meta, &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if got := string(meta["inherited_effort_alternatives"]); got != want {
			t.Errorf("%s: inherited_effort_alternatives = %s, want %s", id, got, want)
		}
	}
}

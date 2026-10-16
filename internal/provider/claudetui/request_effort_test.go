package claudetui

import (
	"encoding/json"
	"strings"
	"testing"
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
	h := newParserHarness(t)

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

// A subagent's request carries the subagent's own effort, not the
// session's, so it leaves the session's effort alone.
func TestSubagentRequestEffortLeavesTheSessionEffort(t *testing.T) {
	h := newParserHarness(t)
	h.runMain(t, `{"model":"claude-opus-4-7","max_tokens":32000,"tools":[{"name":"Agent"}],"output_config":{"effort":"high"}}`,
		agentLaunchSSE("toolu_A", "investigate the bug"))
	if parent := h.runSubagent(t, "aid-1", "investigate the bug", subagentBashSSE("toolu_BASH")); parent == "" {
		t.Fatal("subagent did not resolve its parent")
	}
	sub := h.rec.beginSubagentRequest("toolu_A")
	if sub.requestEffort != nil {
		t.Fatalf("subagent request carries a request effort %q", *sub.requestEffort)
	}
	sub.end()
	h.runMain(t, `{"model":"claude-opus-4-7","max_tokens":32000,"tools":[{"name":"Agent"}],"output_config":{"effort":"high"}}`,
		agentLaunchSSE("toolu_B", "again"))
	if effort, _ := inheritedEffortOf(t, h, "toolu_B"); effort != "high" {
		t.Fatalf("launch after a subagent request inherited %q, want high", effort)
	}
}

// The CLI clamps the session's effort to the main model: an xhigh session
// on Sonnet 4.6, which lacks xhigh, sends high. The launch records xhigh as
// the tier its high may stand for, which an Opus child would run at.
func TestClampedRequestEffortRecordsItsAlternatives(t *testing.T) {
	h := newParserHarness(t)
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

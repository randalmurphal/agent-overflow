package claudetui

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// subagent_test.go is the subagent-nesting parity harness. It drives a main
// request that launches Agent tool calls, then the subagent requests those spawn,
// through the REAL claude.Parser — asserting the subagent's work nests under its
// Agent card (parent_tool_use_id) and that a subagent emits NO turn-complete.
// Both assertions go red without the fix: a subagent reconstructed as a main turn
// attributes its tools to the main thread and emits a spurious result that
// force-closes the Agent tool as "failed" (the reported bug).

// parserHarness reconstructs through one parser, collecting every ProviderEvent.
type parserHarness struct {
	rec    *reconstructor
	events *[]provider.ProviderEvent
}

func newParserHarness(t *testing.T) parserHarness {
	t.Helper()
	parser := claude.NewParser()
	out := &[]provider.ProviderEvent{}
	rec := newReconstructor(func(line json.RawMessage) {
		evs, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		*out = append(*out, evs...)
	})
	return parserHarness{rec: rec, events: out}
}

// runMain drives one main-loop request (registers any Agent launches at end()).
func (h parserHarness) runMain(t *testing.T, reqBody string, sse []string) {
	t.Helper()
	_, req := classifyRequest([]byte(reqBody))
	if req == nil {
		t.Fatalf("classifyRequest(%s) did not yield an agent request", reqBody)
	}
	ar := h.rec.beginAgentRequest(req)
	for _, s := range sse {
		ar.onSSE(json.RawMessage(s))
	}
	ar.end()
}

// runSubagent resolves a subagent's parent and drives its request. Returns the
// resolved parent tool_use_id ("" if unresolved — the request is then skipped,
// matching the gateway).
func (h parserHarness) runSubagent(t *testing.T, agentID, firstUser string, sse []string) string {
	t.Helper()
	parent := h.rec.resolveSubagentParent(agentID, firstUser)
	if parent == "" {
		return ""
	}
	ar := h.rec.beginSubagentRequest(parent)
	for _, s := range sse {
		ar.onSSE(json.RawMessage(s))
	}
	ar.end()
	return parent
}

// agentLaunchSSE is a main response that emits one Agent tool_use with the given
// id and prompt, then stops at tool_use (the model awaits the subagent).
func agentLaunchSSE(toolUseID, prompt string) []string {
	input, _ := json.Marshal(map[string]string{"prompt": prompt, "subagent_type": "general-purpose"})
	startBlock, _ := json.Marshal(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "tool_use", "id": toolUseID, "name": "Agent", "input": map[string]any{}},
	})
	deltaBlock, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)},
	})
	return []string{
		`{"type":"message_start","message":{"id":"msg_main","model":"claude-haiku","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
		string(startBlock),
		string(deltaBlock),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
}

// subagentBashSSE is a subagent response that runs one Bash tool then ends its
// turn (end_turn) — the case that, untagged, would emit a spurious turn-complete.
func subagentBashSSE(bashToolUseID string) []string {
	return []string{
		`{"type":"message_start","message":{"id":"msg_sub","model":"claude-haiku","role":"assistant","usage":{"input_tokens":30,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"running"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"` + bashToolUseID + `","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"echo hi\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":30,"output_tokens":8}}`,
		`{"type":"message_stop"}`,
	}
}

func toolStartFor(events []provider.ProviderEvent, itemID string) *provider.ProviderEvent {
	for i := range events {
		if events[i].Kind == provider.EventToolStart && events[i].ItemID == itemID {
			return &events[i]
		}
	}
	return nil
}

// TestReconstructSubagentNestsUnderParent is the core fix: a subagent's inner
// tool nests under the Agent that launched it, and the subagent emits NO
// turn-complete. Red without the fix (subagent → main turn): the Bash would have
// no parent and an end_turn result would fire and force-close the Agent tool.
func TestReconstructSubagentNestsUnderParent(t *testing.T) {
	const parentID = "toolu_AGENT"
	h := newParserHarness(t)

	h.runMain(t, agentReqBody, agentLaunchSSE(parentID, "investigate the bug"))

	// The Agent tool_use surfaced as a start with no parent (it IS the parent).
	if agentStart := toolStartFor(*h.events, parentID); agentStart == nil {
		t.Fatalf("expected EventToolStart for the Agent tool %q", parentID)
	} else if agentStart.ParentToolUseID != "" {
		t.Errorf("Agent tool start should have no parent, got %q", agentStart.ParentToolUseID)
	}

	firstUser := "<system-reminder>context</system-reminder>\n\n investigate the bug"
	got := h.runSubagent(t, "aid-1", firstUser, subagentBashSSE("toolu_BASH"))
	if got != parentID {
		t.Fatalf("subagent resolved parent = %q, want %q", got, parentID)
	}

	// The subagent's Bash nests under the Agent card.
	bash := toolStartFor(*h.events, "toolu_BASH")
	if bash == nil {
		t.Fatalf("expected EventToolStart for the subagent's Bash; kinds %v", kindsOf(*h.events))
	}
	if bash.ParentToolUseID != parentID {
		t.Errorf("subagent Bash ParentToolUseID = %q, want %q (subagent work attributed to main thread)",
			bash.ParentToolUseID, parentID)
	}

	// A subagent is not a top-level turn: it must emit NO turn-complete. A
	// spurious one is exactly what force-closes the Agent tool as failed.
	if tc := findKind(*h.events, provider.EventTurnComplete); len(tc) != 0 {
		t.Errorf("subagent emitted %d turn-complete(s), want 0", len(tc))
	}
}

// TestReconstructParallelSubagentsNestUnderDistinctParents proves the claim
// mechanism partitions parallel subagents: two Agents launched in one main
// message, two subagents, each nesting under its own Agent by content match.
func TestReconstructParallelSubagentsNestUnderDistinctParents(t *testing.T) {
	h := newParserHarness(t)

	// One main message launching two Agents (two tool_use blocks).
	inputA, _ := json.Marshal(map[string]string{"prompt": "task ALPHA", "subagent_type": "general-purpose"})
	inputB, _ := json.Marshal(map[string]string{"prompt": "task BETA", "subagent_type": "general-purpose"})
	h.runMain(t, agentReqBody, []string{
		`{"type":"message_start","message":{"id":"msg_main","model":"claude-haiku","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_A1","name":"Agent","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + jsonString(string(inputA)) + `}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_A2","name":"Agent","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":` + jsonString(string(inputB)) + `}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	})

	// Subagents arrive in the OPPOSITE order to the launches, to prove the match
	// is by content, not arrival order.
	if got := h.runSubagent(t, "aid-beta", "reminder\n\n task BETA now", subagentBashSSE("toolu_B2")); got != "toolu_A2" {
		t.Fatalf("beta subagent resolved %q, want toolu_A2", got)
	}
	if got := h.runSubagent(t, "aid-alpha", "reminder\n\n task ALPHA now", subagentBashSSE("toolu_B1")); got != "toolu_A1" {
		t.Fatalf("alpha subagent resolved %q, want toolu_A1", got)
	}

	if b1 := toolStartFor(*h.events, "toolu_B1"); b1 == nil || b1.ParentToolUseID != "toolu_A1" {
		t.Errorf("alpha Bash parent = %v, want toolu_A1", parentOrNil(b1))
	}
	if b2 := toolStartFor(*h.events, "toolu_B2"); b2 == nil || b2.ParentToolUseID != "toolu_A2" {
		t.Errorf("beta Bash parent = %v, want toolu_A2", parentOrNil(b2))
	}
}

// TestResolveSubagentParentMatchClaimCache unit-tests the join: content match,
// claim (a launch binds to exactly one subagent), agent-id cache (later requests
// skip the match), and the unresolved miss.
func TestResolveSubagentParentMatchClaimCache(t *testing.T) {
	rec := newReconstructor(func(json.RawMessage) {})
	// Two launches that share a prompt prefix but differ — and one identical pair
	// to exercise the claim path.
	rec.launches = []agentLaunch{
		{toolUseID: "A1", prompt: "do the thing"},
		{toolUseID: "A2", prompt: "do the thing"}, // identical prompt → claim disambiguates
		{toolUseID: "A3", prompt: "something else"},
	}

	// First subagent with the shared prompt claims the first matching launch.
	if got := rec.resolveSubagentParent("aid-1", "ctx do the thing ctx"); got != "A1" {
		t.Fatalf("first match = %q, want A1", got)
	}
	// A different subagent, same prompt, claims the SECOND launch (A1 is claimed).
	if got := rec.resolveSubagentParent("aid-2", "ctx do the thing ctx"); got != "A2" {
		t.Fatalf("second match = %q, want A2 (claim must skip the claimed A1)", got)
	}
	// aid-1 again resolves from cache to A1 without consuming another launch.
	if got := rec.resolveSubagentParent("aid-1", "irrelevant now"); got != "A1" {
		t.Fatalf("cached resolve = %q, want A1", got)
	}
	// A3 still matches its distinct prompt.
	if got := rec.resolveSubagentParent("aid-3", "x something else y"); got != "A3" {
		t.Fatalf("distinct match = %q, want A3", got)
	}
	// No matching launch → unresolved.
	if got := rec.resolveSubagentParent("aid-4", "nothing matches here"); got != "" {
		t.Fatalf("miss = %q, want empty", got)
	}
}

// jsonString quotes s as a JSON string literal (for embedding partial_json).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func parentOrNil(e *provider.ProviderEvent) string {
	if e == nil {
		return "<no start>"
	}
	return e.ParentToolUseID
}

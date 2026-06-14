package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestMainTextSurvivesInterleavedSubagentToolEvents reproduces the
// claude-tui fragmentation bug: when a backgrounded subagent runs
// concurrently with the main loop, the subagent's inner-tool start/complete
// events interleave between the main message's text deltas on the shared
// feed. The main text block (scope "") is ONE content block and must
// settle into ONE assistant_text item — the interleaved subagent-scoped
// events must not fragment it.
//
// Mirrors turn 17 of the captured repro (thread 4d82b192): main R4 text
// "Both backgrounded again …" streamed as three deltas, with each
// subagent's Bash start (parent=Agent-launch) + hook completion
// (parent="") landing between them.
func TestMainTextSurvivesInterleavedSubagentToolEvents(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	step := func(label string, evt provider.ProviderEvent) {
		t.Helper()
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}

	// Open a turn (index 0).
	step("turn start", provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now(),
	})

	agentStartMeta, _ := json.Marshal(map[string]any{"toolName": "Agent"})
	bashStartMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})

	// Two Agent launches in the MAIN message (scope ""). These legitimately
	// settle the preceding main text — that's the normal text→tool boundary.
	step("launch A", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentA",
		ItemType: "Agent", Meta: agentStartMeta, Timestamp: time.Now(),
	})
	step("launch B", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentB",
		ItemType: "Agent", Meta: agentStartMeta, Timestamp: time.Now(),
	})

	// --- R4 main text, ONE content block, streamed as three deltas with
	// subagent tool events interleaved between them. ---
	step("main start", provider.ProviderEvent{
		Kind: provider.EventContentBlockStart, ThreadID: "t1",
		Meta: json.RawMessage(`{"index":0,"blockType":"text"}`), Timestamp: time.Now(),
	})
	step("main d1", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: "Both back", Timestamp: time.Now(),
	})

	// Subagent A's assistant envelope stamps its model onto the parent Agent
	// card: a meta-only EventToolStart targeting agentA, with NO ParentToolUseID
	// (scope ""). This is the fragmenting event — parse_assistant.go emits it,
	// and if the handleToolStart gate doesn't recognize it as a meta-update it
	// runs settleStreamingBeforeTimelineBoundary, whose scope-"" branch calls
	// settleTurnStreaming and settles the live main text mid-stream.
	subModelMeta, _ := json.Marshal(map[string]any{"subagent_model": "claude-opus-4-8"})
	step("subA model stamp", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentA",
		Meta: subModelMeta, Timestamp: time.Now(),
	})

	// Subagent A's inner Bash: start carries parent=agentA (from the subagent
	// assistant envelope); the hook completion carries NO parent (inline
	// tool_result path sets none) — scope "".
	step("subA bash start", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bashA",
		ItemType: "Bash", ParentToolUseID: "agentA", Meta: bashStartMeta, Timestamp: time.Now(),
	})
	step("subA bash complete", provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bashA",
		Content: "cpu", Timestamp: time.Now(),
	})

	step("main d2", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: " grounded again", Timestamp: time.Now(),
	})

	step("subB model stamp", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentB",
		Meta: subModelMeta, Timestamp: time.Now(),
	})
	step("subB bash start", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bashB",
		ItemType: "Bash", ParentToolUseID: "agentB", Meta: bashStartMeta, Timestamp: time.Now(),
	})
	step("subB bash complete", provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bashB",
		Content: "os", Timestamp: time.Now(),
	})

	step("main d3", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: " arrives.", Timestamp: time.Now(),
	})
	step("main stop", provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"index":0,"blockType":"text"}`), Content: "Both back grounded again arrives.",
		ContentPresent: true, Timestamp: time.Now(),
	})
	router.WaitForPendingSettles()

	// The main text is ONE block → exactly ONE main-scope assistant_text
	// item carrying the whole reassembled text, NOT one item per delta.
	var mainText []store.Item
	for _, it := range findItemsByKind(t, st, "t1", itemKindAssistantText) {
		if it.ParentID == "" {
			mainText = append(mainText, it)
		}
	}
	if len(mainText) != 1 {
		var got []string
		for _, it := range mainText {
			got = append(got, it.ID+"="+it.Summary)
		}
		t.Fatalf("main-scope assistant_text items = %d, want 1 (fragmented):\n%v", len(mainText), got)
	}
	if want := "Both back grounded again arrives."; mainText[0].Summary != want {
		t.Fatalf("main text = %q, want %q", mainText[0].Summary, want)
	}

	// The fix must not lose the model stamp: the meta-only EventToolStart
	// still merges subagent_model onto the parent Agent card.
	agent, found, err := st.GetThreadItem("t1", "agentA")
	if err != nil || !found {
		t.Fatalf("agentA lookup: found=%v err=%v", found, err)
	}
	if !strings.Contains(agent.Meta, "claude-opus-4-8") {
		t.Fatalf("agentA meta missing subagent_model stamp: %s", agent.Meta)
	}
}

// TestMainTextSurvivesConsecutiveSubagentModelStamps isolates the back-to-back
// case: two subagent model-stamps land between the same pair of main deltas,
// with no other event between them. Both are scope-"" meta-only EventToolStarts.
// Neither may settle the live main block, and neither may mint a new segment —
// the main text must still reassemble into ONE item. Guards against a
// regression where consecutive meta-updates each advance the per-scope segment
// counter (the dual of the settle bug: fragmenting via the open path, not the
// settle path).
func TestMainTextSurvivesConsecutiveSubagentModelStamps(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	step := func(label string, evt provider.ProviderEvent) {
		t.Helper()
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}

	step("turn start", provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now(),
	})

	agentStartMeta, _ := json.Marshal(map[string]any{"toolName": "Agent"})
	step("launch A", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentA",
		ItemType: "Agent", Meta: agentStartMeta, Timestamp: time.Now(),
	})
	step("launch B", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentB",
		ItemType: "Agent", Meta: agentStartMeta, Timestamp: time.Now(),
	})

	step("main start", provider.ProviderEvent{
		Kind: provider.EventContentBlockStart, ThreadID: "t1",
		Meta: json.RawMessage(`{"index":0,"blockType":"text"}`), Timestamp: time.Now(),
	})
	step("main d1", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: "Both ", Timestamp: time.Now(),
	})

	// Two model-stamps back to back, nothing between them.
	stampA, _ := json.Marshal(map[string]any{"subagent_model": "claude-opus-4-8"})
	stampB, _ := json.Marshal(map[string]any{"subagent_model": "claude-haiku-4-5-20251001"})
	step("stamp A", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentA",
		Meta: stampA, Timestamp: time.Now(),
	})
	step("stamp B", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentB",
		Meta: stampB, Timestamp: time.Now(),
	})

	step("main d2", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: "done.", Timestamp: time.Now(),
	})
	step("main stop", provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"index":0,"blockType":"text"}`), Content: "Both done.",
		ContentPresent: true, Timestamp: time.Now(),
	})
	router.WaitForPendingSettles()

	var mainText []store.Item
	for _, it := range findItemsByKind(t, st, "t1", itemKindAssistantText) {
		if it.ParentID == "" {
			mainText = append(mainText, it)
		}
	}
	if len(mainText) != 1 {
		var got []string
		for _, it := range mainText {
			got = append(got, it.ID+"="+it.Summary)
		}
		t.Fatalf("main-scope assistant_text items = %d, want 1 (fragmented):\n%v", len(mainText), got)
	}
	if want := "Both done."; mainText[0].Summary != want {
		t.Fatalf("main text = %q, want %q", mainText[0].Summary, want)
	}
}

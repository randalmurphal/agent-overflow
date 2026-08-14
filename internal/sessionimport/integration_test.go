package sessionimport

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	claudesessions "agent-overflow/internal/provider/claude/sessionimport"
	"agent-overflow/internal/store"
)

// integration_test.go — the reader → writer → store round trip, driven
// through the orchestrator with hand-written provider homes.
//
// This is the file that keeps the two halves of the importer honest with
// each other. The readers and the writer were built against a documented
// contract without ever meeting; every assertion here is a claim one side
// makes that the other has to satisfy.

func importFixtureRow(t *testing.T, d Deps, row Row) ImportOutcome {
	t.Helper()
	outcome, err := ImportOne(context.Background(), d, row)
	if err != nil {
		t.Fatalf("ImportOne(%s): %v", row.ID, err)
	}
	// The drift alarm. A reader emitting a kind the writer has no row for
	// loses content silently in production; here it fails the fixture that
	// produced it.
	for _, warning := range outcome.Warnings {
		if warning.Code == "import.unmapped-event" || warning.Code == "import.untyped-block-stop" {
			t.Errorf("reader/writer drift on %s: %s", row.ID, warning.Message)
		}
	}
	return outcome
}

// TestImportKeepsAnOrphanCodexToolOutputAsAPlaceholder is the reader/writer
// contract the Codex orphan path depends on.
//
// A rollout's first line can be an output for a call this file never
// recorded — a fork's inherited prefix, a compaction that replaced the
// window, or a tail refresh that starts after the call. The reader
// deliberately emits a completion with no launch and an
// `import_unavailable: "exec-detail"` marker, expecting the placeholder row
// the frontend renders as "Not available from import". The writer used to
// hard-error on "no launch in this session" FIRST and fail the whole
// session, which is how a visible gap became a lost import.
func TestImportKeepsAnOrphanCodexToolOutputAsAPlaceholder(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.writeCodexRollout(t, codexThreadA,
		homes.codexMetaLine(codexThreadA, ""),
		// The very first record is an output whose call_id names nothing.
		codexLine(100, "response_item", map[string]any{
			"type": "function_call_output", "call_id": "call-from-a-previous-window",
			"output": map[string]any{"content": "ok"},
		}),
		codexLine(200, "event_msg", map[string]any{"type": "user_message", "message": "carry on"}),
		codexLine(300, "event_msg", map[string]any{
			"type": "agent_message", "message": "Done.", "phase": "final_answer",
		}),
	)
	d := homes.deps(st)

	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderCodex))
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(outcome.Threads))
	}
	items, err := st.ListItems(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	toolCalls := itemsByKind(items, "tool_call")
	if len(toolCalls) != 1 {
		t.Fatalf("tool_call rows = %d, want the placeholder: %s", len(toolCalls), itemKinds(items))
	}
	placeholder := toolCalls[0]
	if placeholder.ID != "call-from-a-previous-window" {
		t.Errorf("placeholder id = %q, want the call id the output named", placeholder.ID)
	}
	if placeholder.Status != "completed" {
		t.Errorf("placeholder status = %q, want completed (the file says how it ended)", placeholder.Status)
	}
	if !strings.Contains(placeholder.Meta, "exec-detail") {
		t.Errorf("placeholder meta = %q, want the import_unavailable reason", placeholder.Meta)
	}
	// The rest of the session imported: the gap costs one row, not the file.
	if got := len(itemsByKind(items, "user_text")); got != 1 {
		t.Errorf("user_text rows = %d, want 1: %s", got, itemKinds(items))
	}
	if got := len(itemsByKind(items, "assistant_text")); got != 1 {
		t.Errorf("assistant_text rows = %d, want 1: %s", got, itemKinds(items))
	}
}

// TestImportLeavesNoSessionRefWhenTheActiveClaudeBranchImportsNothing pins
// the two rules that are decided over the SURVIVING branches rather than
// over the transcript's leaf count.
//
// A trailing branch whose every row is metadata produces no thread. Keying
// the title and the resume ref on the leaf count there gave a sole survivor
// a "— branch 1" suffix and handed the ref to nobody. Both are wrong; and
// so is the obvious fix of handing the ref to the last SURVIVING thread —
// `claude --resume <id>` reopens the file's active branch, which is the one
// that imported nothing, so that thread would silently continue a different
// conversation. A thread with no ref is fully continuable:
// materializeImportedClaudeBranch cuts it a transcript at its own recorded
// leaf the first time it runs.
func TestImportLeavesNoSessionRefWhenTheActiveClaudeBranchImportsNothing(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeClaudeSession(t, claudeSessionA,
		homes.claudeUserRow("u1", "", "add a test", 0),
		homes.claudeAssistantRow("a1", "u1", "msg-1", []any{
			claudeTextBlock("Done."),
		}, 1_000, map[string]any{"input_tokens": 12, "output_tokens": 3}),
		// A root-level attachment row: admitted by the transcript type set,
		// a leaf of its own, and converted to nothing at all. It is the
		// file's LAST branch, so it is what a resume would land on.
		map[string]any{
			"type": "attachment", "uuid": "att1", "parentUuid": nil,
			"isSidechain": false, "timestamp": isoAt(2_000),
			"attachment": map[string]any{"type": "file_history", "content": "…"},
		},
	)
	d := homes.deps(st)

	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderClaude))
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want 1 (the attachment branch converts to nothing)", len(outcome.Threads))
	}
	thread, err := st.GetThread(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.SessionRef != "" {
		t.Errorf("SessionRef = %q, want empty — the active branch imported nothing, "+
			"so resuming the session id would continue a different conversation",
			thread.SessionRef)
	}
	if strings.Contains(thread.Title, "branch") {
		t.Errorf("title = %q, want no disambiguator on a sole surviving branch", thread.Title)
	}
	// The leaf is what makes the ref-less thread continuable.
	state, found, err := st.GetThreadImportState(thread.ID)
	if err != nil || !found {
		t.Fatalf("import state: found=%v err=%v", found, err)
	}
	if state.LeafUUID != "a1" {
		t.Errorf("leaf_uuid = %q, want the surviving branch's own leaf", state.LeafUUID)
	}
}

// TestWriterHandlesEveryKindTheReadersEmit is the anti-drift gate for the
// contract between the two halves.
//
// The list is the union of what the two readers can emit today, kept in
// sync by hand because neither reader may import this package (a provider
// package with a path to `store` is what internal/AGENTS.md's layering
// forbids). Refresh it with:
//
//	grep -rho 'provider\.Event[A-Za-z]*' \
//	    internal/provider/claude/sessionimport internal/provider/codex/rollout | sort -u
//
// The integration tests above are the automatic half: they fail on the
// same warning for every kind their fixtures actually exercise.
func TestWriterHandlesEveryKindTheReadersEmit(t *testing.T) {
	emitted := []provider.EventKind{
		// Claude reader.
		provider.EventCommandResult,
		provider.EventCompactBoundary,
		provider.EventError,
		provider.EventNotification,
		provider.EventTextDelta,
		provider.EventThinking,
		provider.EventToolComplete,
		provider.EventToolStart,
		provider.EventTurnComplete,
		provider.EventTurnStart,
		provider.EventUserText,
		// Codex reader adds these.
		provider.EventContentBlockStop,
		provider.EventDiff,
		provider.EventProposedPlan,
	}
	for _, kind := range emitted {
		t.Run(string(kind), func(t *testing.T) {
			st := newTestStore(t)
			thread := seedThread(t, st, testThreadID, "codex", "/repo")
			b, err := newBuilder(st, thread)
			if err != nil {
				t.Fatalf("new builder: %v", err)
			}
			// apply() is asked only whether it ROUTES the kind. A bare
			// event fails its own row builder for most kinds (no launch,
			// no payload); what must never happen is the default branch.
			_ = b.apply(importir.Event{
				ProviderEvent: provider.ProviderEvent{Kind: kind, Timestamp: at(0)},
				SourceUUID:    sourceUUID(1),
			})
			for _, warning := range b.warnings {
				if warning.Code == "import.unmapped-event" {
					t.Fatalf("the writer has no row mapping for %s", kind)
				}
			}
		})
	}
}

// scanOne scans and returns the single row for a provider, failing when
// the scan produced anything else.
func scanOne(t *testing.T, d Deps, providerName string) Row {
	t.Helper()
	result := scanFixture(t, d, Filter{Provider: providerName})
	if len(result.Rows) != 1 {
		t.Fatalf("%s scan rows = %s, want exactly one", providerName, rowIDs(result))
	}
	return result.Rows[0]
}

func itemsByKind(items []store.Item, kind string) []store.Item {
	out := make([]store.Item, 0, len(items))
	for _, item := range items {
		if item.Kind == kind {
			out = append(out, item)
		}
	}
	return out
}

func itemKinds(items []store.Item) string {
	kinds := make([]string, 0, len(items))
	for _, item := range items {
		kinds = append(kinds, item.Kind+"("+item.ID+")")
	}
	return strings.Join(kinds, ", ")
}

func TestImportClaudeSessionEndToEnd(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	d := homes.deps(st)

	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderClaude))
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(outcome.Threads))
	}
	thread, err := st.GetThread(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	if thread.ImportSource != ProviderClaude {
		t.Errorf("ImportSource = %q, want claude", thread.ImportSource)
	}
	if thread.SessionRef != claudeSessionA {
		t.Errorf("SessionRef = %q, want the transcript's session id", thread.SessionRef)
	}
	if thread.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want the model the transcript recorded", thread.Model)
	}
	if thread.WorkspacePath != homes.workspace {
		t.Errorf("WorkspacePath = %q, want %q", thread.WorkspacePath, homes.workspace)
	}

	items, err := st.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(itemsByKind(items, "user_text")) != 1 {
		t.Errorf("user_text rows = %d, want 1: %s", len(itemsByKind(items, "user_text")), itemKinds(items))
	}
	if got := len(itemsByKind(items, "assistant_text")); got != 2 {
		t.Errorf("assistant_text rows = %d, want 2: %s", got, itemKinds(items))
	}
	toolCalls := itemsByKind(items, "tool_call")
	if len(toolCalls) != 1 {
		t.Fatalf("tool_call rows = %d, want 1: %s", len(toolCalls), itemKinds(items))
	}
	// A tool_call is ONE row across its whole life, and a transcript that
	// recorded the result must not leave it spinning.
	if toolCalls[0].Status != "completed" || toolCalls[0].ToolName != "Read" {
		t.Errorf("tool_call = %+v, want a completed Read", toolCalls[0])
	}
	for _, item := range items {
		if !strings.Contains(item.Meta, "import_source_uuid") {
			t.Errorf("item %s carries no provenance: %q", item.ID, item.Meta)
		}
	}

	assertOriginalTimestamps(t, st, thread)
	assertCursorLandsOnTheLastRow(t, st, thread, items)
}

// TestImportCodexSessionEndToEnd is the drift gate that matters most: the
// Codex reader speaks the live Codex adapter's vocabulary, where an
// assistant message and a reasoning block arrive as CONTENT BLOCK STOPS
// carrying a `blockType`, never as deltas. A writer that treated those as
// framing imported every Codex session with no assistant text at all.
func TestImportCodexSessionEndToEnd(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)

	row := scanOne(t, d, ProviderCodex)
	outcome := importFixtureRow(t, d, row)
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want 1 (a rollout is one conversation)", len(outcome.Threads))
	}
	thread, err := st.GetThread(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.ImportSource != ProviderCodex || thread.SessionRef != codexThreadA {
		t.Errorf("provenance = %q/%q", thread.ImportSource, thread.SessionRef)
	}
	if thread.Model != "gpt-5.6-sol" || thread.ReasoningEffort != "high" {
		t.Errorf("model profile = %q/%q, want the turn_context values",
			thread.Model, thread.ReasoningEffort)
	}

	items, err := st.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if got := len(itemsByKind(items, "user_text")); got != 1 {
		t.Errorf("user_text rows = %d, want 1: %s", got, itemKinds(items))
	}
	assistant := itemsByKind(items, "assistant_text")
	if len(assistant) != 1 {
		t.Fatalf("assistant_text rows = %d, want 1: %s", len(assistant), itemKinds(items))
	}
	if assistant[0].Summary != "All tests pass." {
		t.Errorf("assistant summary = %q, want the agent message text", assistant[0].Summary)
	}
	toolCalls := itemsByKind(items, "tool_call")
	if len(toolCalls) != 1 || toolCalls[0].Status != "completed" {
		t.Fatalf("tool_call rows = %s, want one completed shell call", itemKinds(items))
	}

	assertOriginalTimestamps(t, st, thread)

	// Codex's cursor is a byte offset, and it must be the file position a
	// tail refresh resumes from — the END of the last line consumed, which
	// is what makes re-reading from it yield exactly the new lines.
	state, ok, err := st.GetThreadImportState(thread.ID)
	if err != nil || !ok {
		t.Fatalf("GetThreadImportState = ok:%v err:%v", ok, err)
	}
	stat, err := os.Stat(row.SourcePath)
	if err != nil {
		t.Fatalf("stat rollout: %v", err)
	}
	if state.LastSourceOffset != stat.Size() {
		t.Errorf("LastSourceOffset = %d, want the file size %d", state.LastSourceOffset, stat.Size())
	}
	if !strings.HasPrefix(state.LastSourceUUID, "line:") {
		t.Errorf("LastSourceUUID = %q, want a line coordinate", state.LastSourceUUID)
	}
	assertCursorLandsOnTheLastRow(t, st, thread, items)
}

// TestImportCodexKeepsReadableThinkingAndPlans covers the two remaining
// content shapes the reader emits through the live adapter's vocabulary.
func TestImportCodexKeepsReadableThinkingAndPlans(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.writeCodexRollout(t, codexThreadA,
		homes.codexMetaLine(codexThreadA, ""),
		codexLine(0, "turn_context", map[string]any{"turn_id": "turn-1", "model": "gpt-5.6-sol"}),
		codexLine(100, "event_msg", map[string]any{"type": "task_started", "turn_id": "turn-1"}),
		codexLine(200, "event_msg", map[string]any{"type": "user_message", "message": "plan it"}),
		codexLine(300, "event_msg", map[string]any{
			"type": "agent_reasoning", "text": "Weighing the two approaches.",
		}),
		codexLine(400, "event_msg", map[string]any{
			"type": "item_completed", "turn_id": "turn-1",
			"item": map[string]any{
				"id": "plan-1", "type": "plan", "text": "# Plan\n1. Do the thing\n",
			},
		}),
		codexLine(500, "event_msg", map[string]any{
			"type": "agent_message", "message": "Here is the plan.",
		}),
		codexLine(600, "event_msg", map[string]any{"type": "task_complete", "turn_id": "turn-1"}),
	)
	d := homes.deps(st)

	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderCodex))
	items, err := st.ListItems(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	thinking := itemsByKind(items, "thinking")
	if len(thinking) != 1 || !strings.Contains(thinking[0].Summary, "Weighing") {
		t.Errorf("thinking rows = %s, want the readable summary", itemKinds(items))
	}
	var plan *store.Item
	for i := range items {
		if items[i].ToolName == "plan" {
			plan = &items[i]
		}
	}
	if plan == nil {
		t.Fatalf("no proposed-plan row: %s", itemKinds(items))
	}
	if plan.Kind != "tool_call" || plan.Status != "completed" || plan.PayloadID == "" {
		t.Errorf("plan row = %+v, want a completed tool_call with a payload", *plan)
	}
}

// TestImportClaudeNestsSubagentRowsUnderTheirTask closes the loop on the
// only join available for Claude subagents (sidechain rows carry no
// parentToolUseID): the Task result names the agent, the agent names the
// file, and the writer turns that into Item.ParentID — which is what makes
// the existing store grouping work with no new store code.
func TestImportClaudeNestsSubagentRowsUnderTheirTask(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	const agentID = "agent-9"
	homes.writeClaudeSession(t, claudeSessionA,
		homes.claudeUserRow("u1", "", "investigate this", 0),
		homes.claudeAssistantRow("a1", "u1", "msg-1", []any{
			claudeToolUseBlock("toolu_task", "Task", map[string]any{
				"description": "investigate", "prompt": "look at the parser",
			}),
		}, 1_000, nil),
		homes.claudeToolResultRow("r1", "a1", "toolu_task", "found it", 4_000,
			map[string]any{"agentId": agentID}),
		claudeLastPromptRow("r1", "investigate this"),
	)
	homes.writeClaudeSubagent(t, claudeSessionA, agentID,
		map[string]any{
			"type": "user", "uuid": "s1", "parentUuid": nil, "isSidechain": true,
			"timestamp": isoAt(2_000), "cwd": homes.workspace,
			"message": map[string]any{"role": "user", "content": "look at the parser"},
		},
		map[string]any{
			"type": "assistant", "uuid": "s2", "parentUuid": "s1", "isSidechain": true,
			"timestamp": isoAt(3_000), "cwd": homes.workspace,
			"message": map[string]any{
				"role": "assistant", "id": "sub-msg-1", "model": "claude-sonnet-4-5",
				"content": []any{claudeTextBlock("The parser drops the last line.")},
			},
		},
	)
	d := homes.deps(st)

	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderClaude))
	threadID := outcome.Threads[0].ID

	descendants, err := st.ListSubagentDescendants(threadID, "toolu_task")
	if err != nil {
		t.Fatalf("list subagent descendants: %v", err)
	}
	if len(descendants) == 0 {
		items, _ := st.ListItems(threadID)
		t.Fatalf("no rows nested under the Task launch: %s", itemKinds(items))
	}
	for _, item := range descendants {
		if item.ParentID != "toolu_task" {
			t.Errorf("descendant %s parent = %q, want the Task launch", item.ID, item.ParentID)
		}
	}
	// The nested rows must stay OUT of the windowed timeline read — that
	// exclusion is what keeps one subagent-heavy turn from eating the
	// window budget, and it only works because ParentID is set.
	window, err := st.ListRecentItems(threadID, 0)
	if err != nil {
		t.Fatalf("list recent items: %v", err)
	}
	launchFound := false
	for _, item := range window.Items {
		if item.ParentID != "" {
			t.Errorf("subagent row %s leaked into the windowed timeline", item.ID)
		}
		if item.ID == "toolu_task" {
			launchFound = true
		}
	}
	if !launchFound {
		t.Error("the Task launch row is missing from the timeline")
	}
}

// TestImportClaudeMultiLeafCreatesAThreadPerBranch pins the whole reason
// Claude import enumerates leaves rather than picking the active chain.
func TestImportClaudeMultiLeafCreatesAThreadPerBranch(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeClaudeSession(t, claudeSessionA,
		homes.claudeUserRow("u1", "", "add a test", 0),
		homes.claudeAssistantRow("a1", "u1", "msg-1", []any{
			claudeTextBlock("First answer."),
			claudeToolUseBlock("toolu_shared", "Read", map[string]any{"file_path": "/tmp/shared.go"}),
		}, 1_000, nil),
		homes.claudeToolResultRow("r1", "a1", "toolu_shared", "shared prefix contents", 2_000, nil),
		homes.claudeUserRow("u2", "r1", "actually, do it differently", 3_000),
		homes.claudeAssistantRow("a2", "u2", "msg-2", []any{claudeTextBlock("Second answer.")}, 4_000, nil),
		// A second leaf hanging off the same completed tool prefix: the
		// user rewound and asked something else.
		homes.claudeUserRow("u3", "r1", "or maybe this instead", 5_000),
		homes.claudeAssistantRow("a3", "u3", "msg-3", []any{claudeTextBlock("Third answer.")}, 6_000, nil),
		claudeLastPromptRow("a2", "actually, do it differently"),
		claudeLastPromptRow("a3", "or maybe this instead"),
	)
	d := homes.deps(st)

	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderClaude))
	if len(outcome.Threads) != 2 {
		t.Fatalf("threads = %d, want one per leaf", len(outcome.Threads))
	}

	titles := map[string]bool{}
	refs := 0
	leaves := map[string]bool{}
	for _, created := range outcome.Threads {
		thread, err := st.GetThread(created.ID)
		if err != nil {
			t.Fatalf("get thread: %v", err)
		}
		if titles[thread.Title] {
			t.Errorf("two branches share the title %q", thread.Title)
		}
		titles[thread.Title] = true
		if thread.SessionRef != "" {
			refs++
		}
		state, ok, err := st.GetThreadImportState(thread.ID)
		if err != nil || !ok {
			t.Fatalf("GetThreadImportState = ok:%v err:%v", ok, err)
		}
		if state.LeafUUID == "" || leaves[state.LeafUUID] {
			t.Errorf("leaf uuid %q is missing or shared", state.LeafUUID)
		}
		leaves[state.LeafUUID] = true
		if state.SourceSessionID != claudeSessionA {
			t.Errorf("SourceSessionID = %q, want the shared session id", state.SourceSessionID)
		}
		shared, found, err := st.GetThreadItem(thread.ID, "toolu_shared")
		if err != nil || !found {
			t.Fatalf("shared tool row in branch %s: found=%v err=%v", thread.ID, found, err)
		}
		data, err := st.GetPayloadData(thread.ID, shared.PayloadID)
		if err != nil {
			t.Fatalf("shared tool payload in branch %s: %v", thread.ID, err)
		}
		if string(data) != "shared prefix contents" {
			t.Errorf("shared tool payload in branch %s = %q", thread.ID, data)
		}
	}
	// Exactly one branch may carry the resume reference: `claude --resume`
	// reopens the file's ACTIVE branch, so handing the ref to both would
	// make resuming the abandoned one silently continue the other.
	if refs != 1 {
		t.Errorf("threads carrying a session_ref = %d, want exactly 1", refs)
	}

	// Every branch is deduped by the shared source session id, so a second
	// scan offers nothing.
	if result := scanFixture(t, d, Filter{Provider: ProviderClaude}); len(result.Rows) != 0 {
		t.Errorf("rescan rows = %s, want none", rowIDs(result))
	}
}

func TestOptimizedClaudeBranchImportMatchesIndependentFullBranchBuilds(t *testing.T) {
	optimized := newTestStore(t)
	homes := newProviderHomes(t)
	sharedBlocks := make([]any, 0, 270)
	for i := 0; i < cap(sharedBlocks); i++ {
		sharedBlocks = append(sharedBlocks, claudeTextBlock("shared block "+strconv.Itoa(i)))
	}
	homes.writeClaudeSession(t, claudeSessionA,
		homes.claudeUserRow("u1", "", "shared prompt", 0),
		homes.claudeAssistantRow("a1", "u1", "msg-1", sharedBlocks, 1_000, map[string]any{
			"input_tokens": 120, "output_tokens": 270,
		}),
		homes.claudeUserRow("u2a", "a1", "branch A", 2_000),
		homes.claudeAssistantRow("a2a", "u2a", "msg-2a", []any{claudeTextBlock("answer A")}, 3_000, map[string]any{
			"input_tokens": 130, "output_tokens": 4,
		}),
		homes.claudeUserRow("u2b", "a1", "branch B", 4_000),
		homes.claudeAssistantRow("a2b", "u2b", "msg-2b", []any{claudeTextBlock("answer B")}, 5_000, map[string]any{
			"input_tokens": 140, "output_tokens": 5,
		}),
		claudeLastPromptRow("a2a", "branch A"),
		claudeLastPromptRow("a2b", "branch B"),
	)
	d := homes.deps(optimized)
	row := scanOne(t, d, ProviderClaude)
	outcome := importFixtureRow(t, d, row)
	if len(outcome.Threads) != 2 {
		t.Fatalf("optimized threads = %d, want 2", len(outcome.Threads))
	}

	baseline := newTestStore(t)
	baselineProject, err := resolveProject(baseline, row)
	if err != nil {
		t.Fatalf("resolve baseline project: %v", err)
	}
	loaded, err := claudesessions.LoadSession(row.SourcePath)
	if err != nil {
		t.Fatalf("load baseline session: %v", err)
	}
	defer loaded.Close()
	baselineThreads := make([]store.Thread, 0, len(loaded.Branches))
	for i := range loaded.Branches {
		branch, err := loaded.ConvertBranch(i)
		if err != nil {
			t.Fatalf("convert baseline branch %d: %v", i, err)
		}
		thread := newImportedThread(row, baselineProject, branch.Title, "", branch.Events, branch.LastActivityAt)
		if err := baseline.CreateThread(thread); err != nil {
			t.Fatalf("create baseline branch %d: %v", i, err)
		}
		batch, _, err := NewWriter(baseline, thread).Build(branch.Events)
		if err != nil {
			t.Fatalf("build baseline branch %d: %v", i, err)
		}
		if err := baseline.ApplyImportBatch(thread.ID, batch); err != nil {
			t.Fatalf("apply baseline branch %d: %v", i, err)
		}
		baselineThreads = append(baselineThreads, thread)
	}

	for i := range outcome.Threads {
		got, err := optimized.ListItems(outcome.Threads[i].ID)
		if err != nil {
			t.Fatalf("list optimized branch %d: %v", i, err)
		}
		want, err := baseline.ListItems(baselineThreads[i].ID)
		if err != nil {
			t.Fatalf("list baseline branch %d: %v", i, err)
		}
		if len(got) != len(want) {
			t.Fatalf("branch %d item count = %d, want %d", i, len(got), len(want))
		}
		for j := range got {
			got[j].ThreadID = ""
			want[j].ThreadID = ""
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("branch %d logical items differ after prefix reuse", i)
		}
		for _, item := range got {
			for _, payloadID := range []string{item.PayloadID, item.InputPayloadID} {
				if payloadID == "" {
					continue
				}
				gotData, err := optimized.GetPayloadData(outcome.Threads[i].ID, payloadID)
				if err != nil {
					t.Fatalf("read optimized payload %s: %v", payloadID, err)
				}
				wantData, err := baseline.GetPayloadData(baselineThreads[i].ID, payloadID)
				if err != nil {
					t.Fatalf("read baseline payload %s: %v", payloadID, err)
				}
				if !reflect.DeepEqual(gotData, wantData) {
					t.Fatalf("branch %d payload %s differs", i, payloadID)
				}
			}
		}

		gotTurns, err := optimized.ListRecentTurns(outcome.Threads[i].ID, 100)
		if err != nil {
			t.Fatalf("list optimized turns for branch %d: %v", i, err)
		}
		wantTurns, err := baseline.ListRecentTurns(baselineThreads[i].ID, 100)
		if err != nil {
			t.Fatalf("list baseline turns for branch %d: %v", i, err)
		}
		if len(gotTurns) != len(wantTurns) {
			t.Fatalf("branch %d turn count = %d, want %d", i, len(gotTurns), len(wantTurns))
		}
		for j := range gotTurns {
			if (gotTurns[j].CompletedAt == nil) != (wantTurns[j].CompletedAt == nil) ||
				(gotTurns[j].CompletedAt != nil && *gotTurns[j].CompletedAt != *wantTurns[j].CompletedAt) {
				var gotCompleted, wantCompleted int64
				if gotTurns[j].CompletedAt != nil {
					gotCompleted = *gotTurns[j].CompletedAt
				}
				if wantTurns[j].CompletedAt != nil {
					wantCompleted = *wantTurns[j].CompletedAt
				}
				t.Fatalf("branch %d turn %d completed_at differs: got=%d want=%d", i, j, gotCompleted, wantCompleted)
			}
			gotTurns[j].ThreadID, wantTurns[j].ThreadID = "", ""
			gotTurns[j].TurnID, wantTurns[j].TurnID = "", ""
			gotTurns[j].CompletedAt, wantTurns[j].CompletedAt = nil, nil
		}
		if !reflect.DeepEqual(gotTurns, wantTurns) {
			t.Fatalf("branch %d turns differ after prefix reuse: got=%+v want=%+v", i, gotTurns, wantTurns)
		}

		gotUsage, err := optimized.QueryUsage(store.UsageQuery{ThreadID: outcome.Threads[i].ID})
		if err != nil {
			t.Fatalf("query optimized usage for branch %d: %v", i, err)
		}
		wantUsage, err := baseline.QueryUsage(store.UsageQuery{ThreadID: baselineThreads[i].ID})
		if err != nil {
			t.Fatalf("query baseline usage for branch %d: %v", i, err)
		}
		if !reflect.DeepEqual(gotUsage, wantUsage) {
			t.Fatalf("branch %d usage differs after prefix reuse: got=%+v want=%+v", i, gotUsage, wantUsage)
		}
	}
}

// TestImportRollsBackTheWholeSessionOnFailure: a session that half-lands
// is worse than one that does not land at all, because the dedup set keys
// on the source session id and would hide the missing branches forever.
func TestImportRollsBackTheWholeSessionOnFailure(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeClaudeSession(t, claudeSessionA,
		homes.claudeUserRow("u1", "", "add a test", 0),
		homes.claudeAssistantRow("a1", "u1", "msg-1", []any{
			claudeTextBlock("Reading."),
			claudeToolUseBlock("toolu_1", "Read", map[string]any{"file_path": "/x"}),
		}, 1_000, map[string]any{"input_tokens": 12, "output_tokens": 3}),
		homes.claudeToolResultRow("r1", "a1", "toolu_1", "contents", 2_000, nil),
		// Leaf 2: a tool_result whose launch is on no chain at all. The
		// writer refuses a completion with no launch rather than shaping
		// half a timeline.
		homes.claudeToolResultRow("r2", "u1", "toolu_ghost", "orphan", 3_000, nil),
	)
	d := homes.deps(st)
	row := scanOne(t, d, ProviderClaude)

	if _, err := ImportOne(context.Background(), d, row); err == nil {
		t.Fatal("ImportOne accepted a transcript with an uncorrelated tool result")
	}

	threads, err := st.ListThreads()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("threads = %d, want none after a rolled-back import", len(threads))
	}
	usage, err := st.QueryUsage(store.UsageQuery{})
	if err != nil {
		t.Fatalf("query usage after rollback: %v", err)
	}
	if len(usage) != 0 {
		t.Fatalf("usage after rollback = %+v, want none", usage)
	}
	// The rollback is what keeps the session importable again.
	if result := scanFixture(t, d, Filter{Provider: ProviderClaude}); len(result.Rows) != 1 {
		t.Errorf("rescan rows = %s, want the session back", rowIDs(result))
	}
}

// TestImportIsolatesOneFailureFromTheNextSession is the per-session
// isolation an Import All depends on.
func TestImportIsolatesOneFailureFromTheNextSession(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeClaudeSession(t, claudeSessionA,
		homes.claudeUserRow("u1", "", "broken one", 0),
		homes.claudeToolResultRow("r1", "u1", "toolu_ghost", "orphan", 1_000, nil),
	)
	homes.claudeLinearSession(t, claudeSessionB)
	d := homes.deps(st)

	result := scanFixture(t, d, Filter{Provider: ProviderClaude})
	if len(result.Rows) != 2 {
		t.Fatalf("rows = %s, want both sessions", rowIDs(result))
	}
	failures, imported := 0, 0
	for _, row := range result.Rows {
		outcome, err := ImportOne(context.Background(), d, row)
		if err != nil {
			failures++
			continue
		}
		imported += len(outcome.Threads)
	}
	if failures != 1 || imported != 1 {
		t.Fatalf("failures/imported = %d/%d, want 1/1", failures, imported)
	}
}

// Synthetic Codex turns have no provider id to adopt, but their persisted
// turn_id still lives in the store's global key space. Keeping both sessions
// in one store is essential: a fixture that deletes the first thread before
// importing the second cannot detect a cross-session identity collision.
func TestImportKeepsSyntheticCodexTurnsDistinctAcrossSessions(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA, codexThreadB)
	for _, fixture := range []struct {
		id, prompt, answer string
	}{
		{codexThreadA, "question alpha", "answer alpha"},
		{codexThreadB, "question beta", "answer beta"},
	} {
		homes.writeCodexRollout(t, fixture.id,
			homes.codexMetaLine(fixture.id, ""),
			codexLine(100, "event_msg", map[string]any{
				"type": "user_message", "message": fixture.prompt,
			}),
			codexLine(200, "event_msg", map[string]any{
				"type": "agent_message", "message": fixture.answer, "phase": "final_answer",
			}),
		)
	}
	d := homes.deps(st)
	rows := scanFixture(t, d, Filter{Provider: ProviderCodex}).Rows
	if len(rows) != 2 {
		t.Fatalf("scan rows = %s, want two Codex sessions", rowIDs(ScanResult{Rows: rows}))
	}

	turnIDs := map[string]struct{}{}
	answers := map[string]string{codexThreadA: "answer alpha", codexThreadB: "answer beta"}
	for _, row := range rows {
		outcome := importFixtureRow(t, d, row)
		if len(outcome.Threads) != 1 {
			t.Fatalf("ImportOne(%s) threads = %d, want 1", row.ID, len(outcome.Threads))
		}
		thread := outcome.Threads[0]
		turns, err := st.ListRecentTurns(thread.ID, 10)
		if err != nil {
			t.Fatalf("list turns for %s: %v", row.ID, err)
		}
		if len(turns) != 1 {
			t.Fatalf("turns for %s = %+v, want one synthetic turn", row.ID, turns)
		}
		if _, duplicate := turnIDs[turns[0].TurnID]; duplicate {
			t.Fatalf("sessions shared synthetic turn id %q", turns[0].TurnID)
		}
		turnIDs[turns[0].TurnID] = struct{}{}
		if turns[0].ProviderTurnID != "" {
			t.Fatalf("provider turn id for inferred turn %s = %q, want empty (not a wire anchor)",
				row.SessionID, turns[0].ProviderTurnID)
		}

		items, err := st.ListItems(thread.ID)
		if err != nil {
			t.Fatalf("list items for %s: %v", row.ID, err)
		}
		wantAnswer := answers[row.SessionID]
		var found bool
		for _, item := range items {
			if item.Kind == "assistant_text" && item.Summary == wantAnswer {
				found = true
			}
			if item.ThreadID != thread.ID || item.TurnIndex != turns[0].TurnIndex {
				t.Fatalf("foreign history in %s: item=%+v turn=%+v", row.ID, item, turns[0])
			}
		}
		if !found {
			t.Fatalf("thread %s items = %+v, want its own answer %q", row.SessionID, items, wantAnswer)
		}
	}
}

// TestImportCreatesTheProjectForAWorkspaceThatIsGone: a moved or deleted
// repository must not cost the user their history. The project is created
// at the recorded path and resume fails exactly as it does for any moved
// repo today.
func TestImportCreatesTheProjectForAWorkspaceThatIsGone(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	d := homes.deps(st)
	row := scanOne(t, d, ProviderClaude)
	if err := os.RemoveAll(homes.workspace); err != nil {
		t.Fatalf("remove workspace: %v", err)
	}

	outcome := importFixtureRow(t, d, row)
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(outcome.Threads))
	}
	projects, err := st.ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 || projects[0].Path != homes.workspace {
		t.Fatalf("projects = %+v, want one at the recorded workspace", projects)
	}
	if outcome.Threads[0].ProjectID != projects[0].ID {
		t.Errorf("thread project = %q, want %q", outcome.Threads[0].ProjectID, projects[0].ID)
	}
}

func TestImportOneRefusesACancelledContext(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	d := homes.deps(st)
	row := scanOne(t, d, ProviderClaude)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ImportOne(ctx, d, row); err == nil {
		t.Fatal("ImportOne on a cancelled context returned no error")
	}
	threads, err := st.ListThreads()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("threads = %d, want none", len(threads))
	}
}

// TestImportedThreadIsReadOnArrival: an import replays history that
// already happened. Marking every imported thread unread would be the
// same mistake as bumping threads.updated_at.
func TestImportedThreadIsReadOnArrival(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	d := homes.deps(st)

	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderClaude))
	thread, err := st.GetThread(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.LastReadAt == nil {
		t.Fatal("imported thread has no read marker")
	}
	if thread.LatestTurnCompletedAt != nil && *thread.LastReadAt < *thread.LatestTurnCompletedAt {
		t.Errorf("read marker %d is behind the last turn %d",
			*thread.LastReadAt, *thread.LatestTurnCompletedAt)
	}
}

// assertOriginalTimestamps walks the whole chain — thread, items, turns,
// usage — and fails on anything stamped near now(). Every timestamp an
// import writes is the provider's own; a thread whose history claims it
// happened at import time is worse than no thread.
func assertOriginalTimestamps(t *testing.T, st *store.Store, thread store.Thread) {
	t.Helper()
	const window = 1_000_000 // the fixture clock's whole span
	inRange := func(label string, value int64) {
		t.Helper()
		if value < baseMillis || value > baseMillis+window {
			t.Errorf("%s = %d, outside the fixture clock [%d, %d] (restamped?)",
				label, value, baseMillis, baseMillis+window)
		}
	}
	inRange("thread.CreatedAt", thread.CreatedAt)
	inRange("thread.UpdatedAt", thread.UpdatedAt)

	items, err := st.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("imported thread has no items")
	}
	for _, item := range items {
		inRange("item "+item.ID+".CreatedAt", item.CreatedAt)
		inRange("item "+item.ID+".UpdatedAt", item.UpdatedAt)
	}

	turns, err := st.ListRecentTurns(thread.ID, 100)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) == 0 {
		t.Fatal("imported thread has no turns")
	}
	for _, turn := range turns {
		inRange("turn "+turn.TurnID+".StartedAt", turn.StartedAt)
		// No turn may reach SQLite unsettled: the boot sweep would rewrite
		// imported history as an interrupted crash.
		if turn.CompletedAt == nil {
			t.Fatalf("turn %s was imported with no completed_at", turn.TurnID)
		}
		inRange("turn "+turn.TurnID+".CompletedAt", *turn.CompletedAt)
	}

	usage, err := st.QueryUsageDetail(store.UsageQuery{
		ThreadID:   thread.ID,
		FromMillis: baseMillis,
		ToMillis:   baseMillis + window,
	})
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if len(usage) == 0 {
		t.Fatal("no usage_ledger rows dated inside the fixture clock")
	}
	for _, row := range usage {
		// Neither session file carries a wire-reported cost, so these must
		// be priced from the rate table at query time, not read as zero.
		if row.CostSource != "none" {
			t.Errorf("usage CostSource = %q, want none", row.CostSource)
		}
	}
}

// assertCursorLandsOnTheLastRow is the invariant WP8's refresh depends on:
// the recorded (turn, item) pair is the thread's real last position, so a
// thread that has not been touched since the import reads as unchanged.
func assertCursorLandsOnTheLastRow(t *testing.T, st *store.Store, thread store.Thread, items []store.Item) {
	t.Helper()
	state, ok, err := st.GetThreadImportState(thread.ID)
	if err != nil || !ok {
		t.Fatalf("GetThreadImportState = ok:%v err:%v", ok, err)
	}
	diverged, err := Diverged(st, state)
	if err != nil {
		t.Fatalf("Diverged: %v", err)
	}
	if diverged {
		t.Fatalf("a freshly imported thread reads as diverged (cursor %d/%d)",
			state.LastTurnIndex, state.LastItemIndex)
	}

	// One live row past the cursor is what a resumed thread looks like,
	// and it must be detected however small its item_index is — the last
	// imported row can sit at a HIGHER item index in an earlier turn.
	last := items[len(items)-1]
	if _, err := st.UpsertItem(store.Item{
		ID:        "live-row",
		ThreadID:  thread.ID,
		TurnIndex: last.TurnIndex + 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "resumed in AO",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}, nil); err != nil {
		t.Fatalf("upsert live row: %v", err)
	}
	diverged, err = Diverged(st, state)
	if err != nil {
		t.Fatalf("Diverged after a live row: %v", err)
	}
	if !diverged {
		t.Errorf("a thread resumed after import does not read as diverged (cursor %d/%d)",
			state.LastTurnIndex, state.LastItemIndex)
	}
}

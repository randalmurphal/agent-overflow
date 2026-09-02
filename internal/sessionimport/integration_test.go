package sessionimport

import (
	"context"
	"database/sql"
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

func TestImportCodexReviewJoinsChildActivityUnderOneSourcedResult(t *testing.T) {
	const (
		controlTurnID = "55555555-5555-4555-8555-555555555555"
		childThreadID = "66666666-6666-4666-8666-666666666666"
	)
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	rootPath := homes.writeCodexRollout(t, codexThreadA,
		homes.codexMetaLine(codexThreadA, ""),
		codexLine(100, "event_msg", map[string]any{
			"type": "entered_review_mode", "turn_id": codexThreadA,
			"item_id": "review-enter", "target": map[string]any{"type": "uncommittedChanges"},
			"user_facing_hint": "Review uncommitted changes",
		}),
		codexLine(200, "event_msg", map[string]any{
			"type": "task_started", "turn_id": controlTurnID,
			"started_at": 1.0, "model_context_window": 258400,
		}),
		codexLine(300, "event_msg", map[string]any{
			"type": "exited_review_mode", "turn_id": codexThreadA,
			"item_id": "review-exit", "review_output": map[string]any{
				"findings": []any{}, "overall_correctness": "patch is correct",
				"overall_explanation": "No issues found.",
			},
		}),
		codexLine(400, "event_msg", map[string]any{
			"type": "agent_message", "message": "No issues found.", "phase": nil,
		}),
		codexLine(500, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": codexThreadA,
			"started_at": 1.0, "completed_at": 2.0,
		}),
	)
	childPath := homes.writeCodexRollout(t, childThreadID,
		codexLine(0, "session_meta", map[string]any{
			"id": childThreadID, "parent_thread_id": codexThreadA,
			"thread_source": "subagent", "source": map[string]any{"subagent": "review"},
			"cwd": homes.workspace, "history_mode": "legacy",
		}),
		codexLine(100, "turn_context", map[string]any{
			"turn_id": controlTurnID, "cwd": homes.workspace,
			"model": "gpt-review", "effort": "high",
		}),
		codexLine(200, "event_msg", map[string]any{
			"type": "task_started", "turn_id": controlTurnID,
			"started_at": 1.0, "model_context_window": 258400,
		}),
		codexLine(300, "response_item", map[string]any{
			"type": "function_call", "call_id": "review-call", "name": "exec_command",
			"arguments": `{"cmd":"git diff"}`,
		}),
		codexLine(400, "response_item", map[string]any{
			"type": "function_call_output", "call_id": "review-call", "output": "diff output",
		}),
		codexLine(500, "event_msg", map[string]any{
			"type": "agent_message", "message": `{"findings":[]}`, "phase": nil,
		}),
		codexLine(600, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": controlTurnID,
			"started_at": 1.0, "completed_at": 2.0,
		}),
	)
	db, err := sql.Open("sqlite", homes.codexHome+"/state_5.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
INSERT INTO threads (id, rollout_path, created_at, updated_at, source, cwd, title,
                     first_user_message, archived, thread_source, preview, recency_at_ms,
                     created_at_ms, updated_at_ms, git_branch, model, reasoning_effort, tokens_used)
VALUES (?, ?, 1, 2, '{"subagent":"review"}', ?, 'Review', '', 0, 'subagent', '', 2,
        1000, 2000, 'main', 'gpt-review', 'high', 0)`, childThreadID, childPath, homes.workspace)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	d := homes.deps(st)
	row := scanOne(t, d, ProviderCodex)
	if row.SourcePath != rootPath {
		t.Fatalf("scan selected %q, want root rollout %q", row.SourcePath, rootPath)
	}
	outcome := importFixtureRow(t, d, row)
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want one root thread", len(outcome.Threads))
	}
	items, err := st.ListItems(outcome.Threads[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var launch, result *store.Item
	for i := range items {
		item := &items[i]
		if item.ToolName == "codex_review" {
			launch = item
		}
		if item.Kind == "command_result" {
			result = item
		}
		if item.Summary == `{"findings":[]}` {
			t.Fatalf("raw reviewer JSON leaked into root items: %+v", item)
		}
	}
	if launch == nil || launch.Status != "completed" || !strings.Contains(launch.Meta, `"model":"gpt-review"`) {
		t.Fatalf("review launch = %+v", launch)
	}
	if result == nil || result.Summary != "No issues found." || !strings.Contains(result.Meta, `"sourceKind":"review"`) {
		t.Fatalf("review result = %+v", result)
	}
	descendants, err := st.ListSubagentDescendants(outcome.Threads[0].ID, launch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(itemsByKind(descendants, "tool_call")) != 1 {
		t.Fatalf("review descendants = %+v, want one joined tool call", descendants)
	}
	for _, item := range descendants {
		if item.Summary == `{"findings":[]}` {
			t.Fatalf("raw reviewer JSON leaked into descendants: %+v", item)
		}
	}
}

// An inactive conversational leaf is not a substitute for an active branch
// containing only metadata. Importing the former and attaching no resume ref
// would turn one selected provider session into a different conversation.
func TestImportClaudeDoesNotSubstituteAnInactiveBranchForEmptyActiveHistory(t *testing.T) {
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

	row := scanOne(t, d, ProviderClaude)
	outcome := importFixtureRow(t, d, row)
	if len(outcome.Threads) != 0 {
		t.Fatalf("threads = %d, want none for metadata-only active history", len(outcome.Threads))
	}
	threads, err := st.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("stored threads = %d, want none", len(threads))
	}
	// No thread means no dedup marker: the source remains visible instead of
	// being silently consumed by an import that produced nothing.
	if result := scanFixture(t, d, Filter{Provider: ProviderClaude}); len(result.Rows) != 1 {
		t.Errorf("rescan rows = %s, want the empty session to remain available", rowIDs(result))
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
	opts := provider.SessionOptionsFromThread(thread, provider.AutoCompactDefaults{}, "", false)
	if opts.Model != "gpt-5.6-sol" || opts.ReasoningEffort != provider.EffortHigh {
		t.Fatalf("session start profile = %q/%q, want the imported rollout profile", opts.Model, opts.ReasoningEffort)
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

func TestImportCodexFallsBackToTheIndexedProfile(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.writeCodexRollout(t, codexThreadA,
		homes.codexMetaLine(codexThreadA, ""),
		codexLine(0, "event_msg", map[string]any{
			"type": "task_started", "turn_id": "turn-1",
		}),
		codexLine(100, "event_msg", map[string]any{
			"type": "user_message", "message": "legacy rollout without turn context",
		}),
		codexLine(200, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": "turn-1",
		}),
	)
	d := homes.deps(st)

	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderCodex))
	thread, err := st.GetThread(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Model != "gpt-5.6-sol" || thread.ReasoningEffort != "high" {
		t.Fatalf("indexed fallback profile = %q/%q", thread.Model, thread.ReasoningEffort)
	}
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

	row := scanOne(t, d, ProviderClaude)
	outcome := importFixtureRow(t, d, row)
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
	window, err := st.ListThreadSliceAround(threadID, "", 1000)
	if err != nil {
		t.Fatalf("list thread slice: %v", err)
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

// A Claude transcript can retain several alternative continuations after a
// rewind or retry. One selected provider session must still become one AO
// thread, and that thread must contain exactly the active root-to-leaf chain:
// shared ancestry plus the file-order-last continuation, never its sibling.
func TestImportClaudeMultiLeafSelectsOneCoherentActiveHistory(t *testing.T) {
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

	importRow := scanOne(t, d, ProviderClaude)
	outcome := importFixtureRow(t, d, importRow)
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want one active session thread", len(outcome.Threads))
	}
	thread, err := st.GetThread(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Title != importRow.Title || strings.Contains(thread.Title, " — ") {
		t.Errorf("title = %q, want provider session title %q without a branch suffix", thread.Title, importRow.Title)
	}
	if thread.SessionRef != claudeSessionA {
		t.Errorf("SessionRef = %q, want the active provider session", thread.SessionRef)
	}
	state, ok, err := st.GetThreadImportState(thread.ID)
	if err != nil || !ok {
		t.Fatalf("GetThreadImportState = ok:%v err:%v", ok, err)
	}
	if state.LeafUUID != "a3" || state.SourceSessionID != claudeSessionA {
		t.Errorf("import state = %+v, want active leaf a3 in session %s", state, claudeSessionA)
	}

	items, err := st.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var summaries []string
	for _, item := range items {
		if item.Kind == "user_text" || item.Kind == "assistant_text" {
			summaries = append(summaries, item.Summary)
		}
	}
	joined := strings.Join(summaries, "\n")
	for _, want := range []string{"add a test", "First answer.", "or maybe this instead", "Third answer."} {
		if !strings.Contains(joined, want) {
			t.Errorf("active history omitted %q: %q", want, joined)
		}
	}
	for _, excluded := range []string{"actually, do it differently", "Second answer."} {
		if strings.Contains(joined, excluded) {
			t.Errorf("inactive sibling %q leaked into active history: %q", excluded, joined)
		}
	}
	shared, found, err := st.GetThreadItem(thread.ID, "toolu_shared")
	if err != nil || !found {
		t.Fatalf("shared tool row: found=%v err=%v", found, err)
	}
	data, err := st.GetPayloadData(thread.ID, shared.PayloadID)
	if err != nil {
		t.Fatalf("shared tool payload: %v", err)
	}
	if string(data) != "shared prefix contents" {
		t.Errorf("shared tool payload = %q", data)
	}

	// The source session is consumed once even though its file retains several
	// leaves, so a second scan cannot offer or duplicate it.
	if result := scanFixture(t, d, Filter{Provider: ProviderClaude}); len(result.Rows) != 0 {
		t.Errorf("rescan rows = %s, want none", rowIDs(result))
	}
}

// An explicit provider fork is not the same thing as another leaf inside one
// transcript. It has a new resumable session id, so both conversations must
// import exactly once. Their shared prefix belongs in both coherent histories;
// only the post-fork continuations differ.
func TestImportClaudeExplicitForkKeepsBothCoherentHistoriesAndLineage(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeClaudeSession(t, claudeSessionA,
		homes.claudeUserRow("shared-u", "", "shared prompt", 0),
		homes.claudeAssistantRow("shared-a", "shared-u", "shared-msg", []any{
			claudeTextBlock("shared answer"),
		}, 1_000, nil),
		homes.claudeUserRow("parent-u", "shared-a", "parent continuation", 2_000),
		homes.claudeAssistantRow("parent-a", "parent-u", "parent-msg", []any{
			claudeTextBlock("parent answer"),
		}, 3_000, nil),
		claudeLastPromptRow("parent-a", "parent continuation"),
	)
	childRoot := homes.claudeUserRow("shared-u", "", "shared prompt", 0)
	withField(childRoot, "forkedFrom", map[string]any{
		"sessionId": claudeSessionA, "messageUuid": "shared-a",
	})
	homes.writeClaudeSession(t, claudeSessionB,
		childRoot,
		homes.claudeAssistantRow("shared-a", "shared-u", "shared-msg", []any{
			claudeTextBlock("shared answer"),
		}, 1_000, nil),
		homes.claudeUserRow("child-u", "shared-a", "child continuation", 2_500),
		homes.claudeAssistantRow("child-a", "child-u", "child-msg", []any{
			claudeTextBlock("child answer"),
		}, 3_500, nil),
		claudeLastPromptRow("child-a", "child continuation"),
	)
	d := homes.deps(st)

	scan := scanFixture(t, d, Filter{Provider: ProviderClaude})
	if len(scan.Rows) != 2 {
		t.Fatalf("scan rows = %s, want parent and explicit fork", rowIDs(scan))
	}
	rows := make(map[string]Row, len(scan.Rows))
	for _, row := range scan.Rows {
		rows[row.SessionID] = row
	}
	if rows[claudeSessionB].ParentSessionID != claudeSessionA {
		t.Fatalf("child parent session = %q, want %q",
			rows[claudeSessionB].ParentSessionID, claudeSessionA)
	}

	// Deliberately import child first to pin order-independent reconciliation.
	childOutcome := importFixtureRow(t, d, rows[claudeSessionB])
	if len(childOutcome.Threads) != 1 {
		t.Fatalf("child threads = %d, want 1", len(childOutcome.Threads))
	}
	childID := childOutcome.Threads[0].ID
	childBeforeParent, err := st.GetThread(childID)
	if err != nil {
		t.Fatalf("get child before parent: %v", err)
	}
	if childBeforeParent.ForkedFromThreadID != "" {
		t.Fatalf("child linked to absent parent %q", childBeforeParent.ForkedFromThreadID)
	}

	parentOutcome := importFixtureRow(t, d, rows[claudeSessionA])
	if len(parentOutcome.Threads) != 1 {
		t.Fatalf("parent threads = %d, want 1", len(parentOutcome.Threads))
	}
	parentID := parentOutcome.Threads[0].ID
	child, err := st.GetThread(childID)
	if err != nil {
		t.Fatalf("get reconciled child: %v", err)
	}
	if child.ForkedFromThreadID != parentID {
		t.Fatalf("child fork link = %q, want imported parent %q", child.ForkedFromThreadID, parentID)
	}
	state, ok, err := st.GetThreadImportState(childID)
	if err != nil || !ok {
		t.Fatalf("get child import state = ok:%v err:%v", ok, err)
	}
	if state.SourceSessionID != claudeSessionB || state.SourceParentSessionID != claudeSessionA {
		t.Fatalf("child provenance = %+v", state)
	}

	requireConversation := func(threadID string, included, excluded []string) {
		t.Helper()
		items, err := st.ListItems(threadID)
		if err != nil {
			t.Fatalf("list items for %s: %v", threadID, err)
		}
		var summaries []string
		for _, item := range items {
			if item.Kind == "user_text" || item.Kind == "assistant_text" {
				summaries = append(summaries, item.Summary)
			}
		}
		joined := strings.Join(summaries, "\n")
		for _, want := range included {
			if !strings.Contains(joined, want) {
				t.Errorf("thread %s omitted %q: %q", threadID, want, joined)
			}
		}
		for _, unwanted := range excluded {
			if strings.Contains(joined, unwanted) {
				t.Errorf("thread %s leaked %q: %q", threadID, unwanted, joined)
			}
		}
	}
	requireConversation(parentID,
		[]string{"shared prompt", "shared answer", "parent continuation", "parent answer"},
		[]string{"child continuation", "child answer"})
	requireConversation(childID,
		[]string{"shared prompt", "shared answer", "child continuation", "child answer"},
		[]string{"parent continuation", "parent answer"})

	threads, err := st.ListThreads()
	if err != nil {
		t.Fatalf("list imported threads: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("stored threads = %d, want exactly parent and child", len(threads))
	}
	if rescanned := scanFixture(t, d, Filter{Provider: ProviderClaude}); len(rescanned.Rows) != 0 {
		t.Fatalf("rescan rows = %s, want no duplicate candidates", rowIDs(rescanned))
	}
}

func TestImportClaudeProfilesTheActiveBranchFromItsOwnAncestry(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	shared := homes.claudeAssistantRow("a1", "u1", "msg-1", []any{claudeTextBlock("shared")}, 1_000, nil)
	shared["message"].(map[string]any)["model"] = "claude-shared"
	branchB := homes.claudeAssistantRow("a2b", "u2b", "msg-2b", []any{claudeTextBlock("branch B")}, 4_000, nil)
	branchB["message"].(map[string]any)["model"] = "claude-branch-b"
	homes.writeClaudeSession(t, claudeSessionA,
		homes.claudeUserRow("u1", "", "shared prompt", 0),
		shared,
		// Branch A records no newer assistant model, but it is inactive and
		// cannot contribute state to the imported thread.
		homes.claudeUserRow("u2a", "a1", "branch A", 2_000),
		homes.claudeUserRow("u2b", "a1", "branch B", 3_000),
		branchB,
		claudeLastPromptRow("u2a", "branch A"),
		claudeLastPromptRow("a2b", "branch B"),
	)
	d := homes.deps(st)
	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderClaude))
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(outcome.Threads))
	}
	thread, err := st.GetThread(outcome.Threads[0].ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Model != "claude-branch-b" {
		t.Fatalf("active branch model = %q, want claude-branch-b", thread.Model)
	}
}

// The one-session/one-thread rule is enforced by the write chokepoint as well
// as by today's Claude caller. A future provider loop must fail and roll back
// instead of silently restoring sidebar fan-out.
func TestSessionImporterRefusesASecondThreadForOneSession(t *testing.T) {
	st := newTestStore(t)
	proj := store.Project{
		ID: "project-one-thread", Path: t.TempDir(), Name: "One thread",
		CreatedAt: 1, UpdatedAt: 1,
	}
	if err := st.CreateProject(proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	row := Row{
		ID: RowKey(ProviderClaude, claudeSessionA), Provider: ProviderClaude,
		SessionID: claudeSessionA, SourcePath: "/copied/session.jsonl",
		ProjectPath: proj.Path, CreatedAt: 1, LastActivityAt: 2,
	}
	events := []importir.Event{{
		ProviderEvent: provider.ProviderEvent{
			Kind: provider.EventUserText, Content: "one history", Timestamp: time.UnixMilli(1),
		},
		SourceUUID: "u1",
	}}
	im := newSessionImporter(st, row, proj)
	if err := im.add(branchPlan{title: "one", events: events}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := im.add(branchPlan{title: "two", events: events}); err == nil ||
		!strings.Contains(err.Error(), "more than one thread") {
		t.Fatalf("second add error = %v, want the one-thread invariant", err)
	}
	threads, err := st.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("threads after refused fan-out = %d, want rollback to none", len(threads))
	}
}

func TestImportedClaudeActiveHistoryMatchesIndependentFullActiveBranchBuild(t *testing.T) {
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
	if len(outcome.Threads) != 1 {
		t.Fatalf("imported threads = %d, want 1", len(outcome.Threads))
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
	if len(loaded.Branches) != 2 {
		t.Fatalf("baseline branches = %d, want the two source alternatives", len(loaded.Branches))
	}
	branch, err := loaded.ConvertBranch(len(loaded.Branches) - 1)
	if err != nil {
		t.Fatalf("convert baseline active branch: %v", err)
	}
	baselineThread := newImportedThread(
		row, baselineProject, importedTitle(row.Title), row.SessionID,
		branch.Events, branch.Profile, branch.LastActivityAt,
	)
	if err := baseline.CreateThread(baselineThread); err != nil {
		t.Fatalf("create baseline active branch: %v", err)
	}
	batch, _, err := NewWriter(baseline, baselineThread).Build(branch.Events)
	if err != nil {
		t.Fatalf("build baseline active branch: %v", err)
	}
	if err := baseline.ApplyImportBatch(baselineThread.ID, batch); err != nil {
		t.Fatalf("apply baseline active branch: %v", err)
	}
	baselineThreads := []store.Thread{baselineThread}

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
			t.Fatalf("active branch %d logical items differ from the independent build", i)
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
			t.Fatalf("active branch %d turns differ from the independent build: got=%+v want=%+v", i, gotTurns, wantTurns)
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
			t.Fatalf("active branch %d usage differs from the independent build: got=%+v want=%+v", i, gotUsage, wantUsage)
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

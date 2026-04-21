package store

import (
	"testing"
)

func TestListThreadProposedPlans_OrderedDesc(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Three plans across two turns plus one non-plan item that must be
	// excluded by the payload_kind filter. Item.kind stays within the
	// CHECK-constrained set ('assistant_text' for the plans, 'tool_call'
	// for the diff stub); payload.kind is what the query filters on.
	seedPayloadItem(t, s, "t", "p1", 0, 0, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "p2", 1, 0, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "p3", 1, 2, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "diff", 1, 1, "tool_call", "diff", "{}")

	got, err := s.ListThreadProposedPlans("t")
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}

	ids := collectIDs(got)
	// Newest plan at turn 1 item 2 → p3; then p2 (turn 1 item 0); then p1
	// (turn 0 item 0). DESC, DESC.
	want := []string{"p3", "p2", "p1"}
	if !equalStringSlice(ids, want) {
		t.Errorf("ids: got %v, want %v", ids, want)
	}
}

func TestListThreadDiffPayloads_SelectsDiffAndToolResult(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// The query returns items whose payload.kind is 'diff' or
	// 'tool_result'; the frontend's selectAgentDiffEntries filters
	// tool_result rows further by meta. Other payload kinds must be
	// excluded.
	// item.kind must be one of the allowed CHECK values — diffs ride on
	// tool_call / tool_completion rows in production; payload.kind is
	// what the thread-aggregate query filters on.
	seedPayloadItem(t, s, "t", "d1", 0, 0, "tool_call", "diff", `{"insertions":1,"deletions":0}`)
	seedPayloadItem(t, s, "t", "tr1", 0, 1, "tool_completion", "tool_result",
		`{"inlineDiff":{"availability":"exact_patch","files":[{"insertions":2,"deletions":1}]}}`)
	seedPayloadItem(t, s, "t", "noise1", 0, 2, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "d2", 1, 0, "tool_call", "diff", `{"insertions":3,"deletions":2}`)

	got, err := s.ListThreadDiffPayloads("t")
	if err != nil {
		t.Fatalf("list diffs: %v", err)
	}

	ids := collectIDs(got)
	// Ordered by (turn, item). proposed_plan excluded.
	want := []string{"d1", "tr1", "d2"}
	if !equalStringSlice(ids, want) {
		t.Errorf("ids: got %v, want %v", ids, want)
	}
	// The payload meta must round-trip so the frontend summarizer can
	// parse it — this is what the DiffPanelDrawer cumulativeStats
	// calculation relies on.
	for _, item := range got {
		if item.PayloadMeta == "" {
			t.Errorf("payload meta empty on %s — frontend summarizer needs it", item.ID)
		}
	}
}

func TestListLiveBackgroundTasks_RunningOnly(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// A running background launch, a completed non-background item, and
	// a completed background launch with no recent completion row.
	seedBackgroundItem(t, s, "t", "run", 0, 0, "running", "", 1000)
	seedBackgroundItem(t, s, "t", "done-old", 0, 1, "completed", "", 1000)
	// Non-background item — must be excluded entirely.
	seedItem(t, s, "t", "regular", 1, 0, "")

	got, err := s.ListLiveBackgroundTasks("t", 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	ids := collectIDs(got)
	if !equalStringSlice(ids, []string{"run"}) {
		t.Errorf("ids: got %v, want [run]", ids)
	}
}

func TestListLiveBackgroundTasks_WithinCompletionCutoff(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Launch row is still "running" but a completion row pointing at it
	// was written at createdAt=5000. cutoff=4000 keeps it; cutoff=5001
	// drops it.
	seedBackgroundItem(t, s, "t", "launch", 0, 0, "running", "", 1000)
	// Two completions: one recent (within cutoff), one stale (outside).
	seedBackgroundItem(t, s, "t", "completion-new", 0, 1, "completed", "launch", 5000)
	seedBackgroundItem(t, s, "t", "completion-old", 0, 2, "completed", "launch", 3000)

	withCutoff4000, err := s.ListLiveBackgroundTasks("t", 4000)
	if err != nil {
		t.Fatalf("list 4000: %v", err)
	}
	// launch + the completion whose created_at >= 4000.
	if !equalStringSlice(collectIDs(withCutoff4000), []string{"launch", "completion-new"}) {
		t.Errorf("cutoff=4000 ids: got %v", collectIDs(withCutoff4000))
	}

	withCutoff5001, err := s.ListLiveBackgroundTasks("t", 5001)
	if err != nil {
		t.Fatalf("list 5001: %v", err)
	}
	// Only the running launch — both completion rows fall outside.
	if !equalStringSlice(collectIDs(withCutoff5001), []string{"launch"}) {
		t.Errorf("cutoff=5001 ids: got %v", collectIDs(withCutoff5001))
	}
}

func TestListLiveBackgroundTasks_IgnoresForeignCompletions(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// A completion row pointing at a non-background launch must not
	// appear in the tray query — the tray only shows background work.
	seedItem(t, s, "t", "fg-launch", 0, 0, "")
	seedBackgroundItem(t, s, "t", "fg-completion", 0, 1, "completed", "fg-launch", 10000)

	got, err := s.ListLiveBackgroundTasks("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no rows (completion targets non-background launch), got %d", len(got))
	}
}

func TestListLiveBackgroundTasks_IncludesRunningFromAnyTurn(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// A long-running background task launched in turn 0 must still be
	// returned even if the UI window has moved way past its origin.
	seedBackgroundItem(t, s, "t", "ancient-run", 0, 0, "running", "", 100)
	// Add filler so ancient-run is unambiguously in an "old" turn.
	for i := 1; i < 20; i++ {
		seedItem(t, s, "t", idForTurn(i), i, 0, "")
	}

	got, err := s.ListLiveBackgroundTasks("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"ancient-run"}) {
		t.Errorf("expected ancient-run regardless of turn depth, got %v", collectIDs(got))
	}
}

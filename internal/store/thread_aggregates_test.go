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

func TestListThreadProposedPlans_ExcludesUserAuthored(t *testing.T) {
	// Regression guard for the `role = 'assistant'` filter: a
	// user-authored item whose payload.kind happens to be
	// 'proposed_plan' (schema-allows it; can happen via thread imports
	// or forks) must not surface in the PlanSidebar — only plans the
	// assistant actually proposed.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "agent-plan", 0, 0, "assistant_text", "proposed_plan", "{}")
	seedUserPlan(t, s, "t", "user-plan", 0, 1, "proposed_plan", "{}")

	got, err := s.ListThreadProposedPlans("t")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := collectIDs(got)
	if !equalStringSlice(ids, []string{"agent-plan"}) {
		t.Errorf("plans: got %v, want [agent-plan] (user-authored must be filtered)", ids)
	}
}

func TestListThreadProposedPlans_EmptyThreadReturnsEmptySlice(t *testing.T) {
	// Stable JSON shape: an empty result is `[]`, never `null` — the
	// frontend binds the response directly as `Item[]` and a null here
	// would throw a type error at runtime.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	got, err := s.ListThreadProposedPlans("t")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("got %d rows, want 0", len(got))
	}
}

// seedUserPlan persists a user-authored item carrying a proposed_plan
// payload so the role-filter test above has a user row to assert
// against. Mirrors seedPayloadItem but overrides Kind/Role to match a
// real user-message insert.
func seedUserPlan(
	t *testing.T,
	s *Store,
	threadID, id string,
	turnIndex, itemIndex int,
	payloadKind, payloadMeta string,
) {
	t.Helper()
	payloadID := "p-" + id
	payload := Payload{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      payloadMeta,
		Data:      []byte("body-" + id),
		CreatedAt: int64(turnIndex*10 + itemIndex),
	}
	item := Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      "user_text",
		Role:      "user",
		Summary:   id,
		PayloadID: payloadID,
		CreatedAt: int64(turnIndex*10 + itemIndex),
	}
	if err := s.InsertItemWithPayload(item, payload); err != nil {
		t.Fatalf("seed user plan %s: %v", id, err)
	}
}

func TestListThreadDiffPayloads_ExcludesToolResultWithoutInlineDiff(t *testing.T) {
	// The SQL-level filter uses
	// `json_extract(meta, '$.inlineDiff.availability') = 'exact_patch'`
	// so a plain tool_result (bash / read-file / etc.) never ships to
	// the frontend. Verify both the exclusion and the positive branch
	// in the same scenario so a regression that broadens the filter
	// fails here.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Qualifies.
	seedPayloadItem(t, s, "t", "diff", 0, 0, "tool_call", "diff", `{}`)
	seedPayloadItem(t, s, "t", "tr-inline", 0, 1, "tool_completion", "tool_result",
		`{"inlineDiff":{"availability":"exact_patch"}}`)
	// Does NOT qualify: missing inlineDiff.
	seedPayloadItem(t, s, "t", "tr-bash", 0, 2, "tool_completion", "tool_result", `{}`)
	// Does NOT qualify: inlineDiff present but different availability value.
	seedPayloadItem(t, s, "t", "tr-out-of-band", 0, 3, "tool_completion", "tool_result",
		`{"inlineDiff":{"availability":"out_of_band"}}`)
	// Does NOT qualify: inlineDiff is a string, not an object (malformed
	// meta). json_extract returns NULL → the = comparison fails → row
	// excluded. Defensive: callers write meta, a bad writer shouldn't
	// poison the cumulative view.
	seedPayloadItem(t, s, "t", "tr-malformed", 0, 4, "tool_completion", "tool_result",
		`{"inlineDiff":"oops"}`)

	got, err := s.ListThreadDiffPayloads("t")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := collectIDs(got)
	if !equalStringSlice(ids, []string{"diff", "tr-inline"}) {
		t.Errorf("ids: got %v, want [diff tr-inline] (non-inline tool_results must be filtered)", ids)
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

	// Launch row is still "running" (the invariant — a background launch
	// never flips out of running; its sibling completion row carries the
	// final state). The launch+completion is treated as a pair for
	// retention: the launch is visible iff at least one of its
	// completions is still within the cutoff window.
	seedBackgroundItem(t, s, "t", "launch", 0, 0, "running", "", 1000)
	// Two completions: one recent (within cutoff), one stale (outside).
	seedBackgroundItem(t, s, "t", "completion-new", 0, 1, "completed", "launch", 5000)
	seedBackgroundItem(t, s, "t", "completion-old", 0, 2, "completed", "launch", 3000)

	withCutoff4000, err := s.ListLiveBackgroundTasks("t", 4000)
	if err != nil {
		t.Fatalf("list 4000: %v", err)
	}
	// launch + the completion whose created_at >= 4000. The fresh
	// completion keeps the launch visible.
	if !equalStringSlice(collectIDs(withCutoff4000), []string{"launch", "completion-new"}) {
		t.Errorf("cutoff=4000 ids: got %v", collectIDs(withCutoff4000))
	}

	withCutoff5001, err := s.ListLiveBackgroundTasks("t", 5001)
	if err != nil {
		t.Fatalf("list 5001: %v", err)
	}
	// Every completion has aged past the cutoff, so the launch also
	// ages out. Returning the launch alone would let the tray re-render
	// it as "running" forever.
	if len(withCutoff5001) != 0 {
		t.Errorf("cutoff=5001 ids: got %v, want [] (pair aged out together)", collectIDs(withCutoff5001))
	}
}

// TestListLiveBackgroundTasks_ExcludesLaunchWhenCompletionAgesOut is the
// regression guard for the two-completions-close-together race that leaves
// a launch stranded in the tray. When task A finishes and its completion
// row ages past the retention cutoff, the launch row — which by spec
// stays `status='running'` forever — must NOT leak through on its own.
// Otherwise a refresh triggered by ANOTHER event (e.g. task B completing
// >2 s later) re-queries and the stranded launch re-renders as "running"
// indefinitely.
func TestListLiveBackgroundTasks_ExcludesLaunchWhenCompletionAgesOut(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// L1 completed long ago (completion is stale) — pair must disappear.
	// L2 completed recently (completion is fresh) — pair stays visible.
	seedBackgroundItem(t, s, "t", "L1", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "t", "L2", 0, 1, "running", "", 200)
	seedBackgroundItem(t, s, "t", "C1", 0, 2, "completed", "L1", 1000)
	seedBackgroundItem(t, s, "t", "C2", 0, 3, "completed", "L2", 3000)

	got, err := s.ListLiveBackgroundTasks("t", 2000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Expected: L2 + C2 only. L1 paired with an expired C1 must NOT be
	// returned alone — that's exactly the bug where the tray would show
	// L1 as "running" forever.
	if !equalStringSlice(collectIDs(got), []string{"L2", "C2"}) {
		t.Errorf("ids: got %v, want [L2 C2] (L1 must age out with its stale completion)", collectIDs(got))
	}
}

// TestListLiveBackgroundTasks_LaunchWithNoCompletionAlwaysShown guards the
// happy path: a background task that has NOT yet completed has no
// completion row, so the tray must surface the launch regardless of how
// long it has been running.
func TestListLiveBackgroundTasks_LaunchWithNoCompletionAlwaysShown(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedBackgroundItem(t, s, "t", "long-runner", 0, 0, "running", "", 100)

	got, err := s.ListLiveBackgroundTasks("t", 999_999)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"long-runner"}) {
		t.Errorf("ids: got %v, want [long-runner]", collectIDs(got))
	}
}

// TestListLiveBackgroundTasks_CrossThreadIsolation guards the
// thread-scoping invariant across all three places the new query
// references thread_id (outer WHERE, NOT EXISTS subquery, EXISTS
// subquery, completion-of IN-list). A bug in any one binding would
// make launches from one thread leak into another thread's tray.
func TestListLiveBackgroundTasks_CrossThreadIsolation(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("ta", "claude")); err != nil {
		t.Fatalf("create thread ta: %v", err)
	}
	if err := s.CreateThread(makeThread("tb", "claude")); err != nil {
		t.Fatalf("create thread tb: %v", err)
	}

	// Thread A: running launch with a fresh completion. Thread B:
	// running launch (no completion) with an id that collides with
	// thread A's launch id so a missing thread_id predicate would
	// cross-link the two.
	seedBackgroundItem(t, s, "ta", "launch", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "ta", "completion", 0, 1, "completed", "launch", 5000)
	seedBackgroundItem(t, s, "tb", "launch", 0, 0, "running", "", 100)

	gotA, err := s.ListLiveBackgroundTasks("ta", 4000)
	if err != nil {
		t.Fatalf("list ta: %v", err)
	}
	if !equalStringSlice(collectIDs(gotA), []string{"launch", "completion"}) {
		t.Errorf("ta ids: got %v, want [launch completion]", collectIDs(gotA))
	}

	gotB, err := s.ListLiveBackgroundTasks("tb", 4000)
	if err != nil {
		t.Fatalf("list tb: %v", err)
	}
	// Thread B has no completion row, so the EXISTS/NOT-EXISTS branches
	// must evaluate only against thread B's rows — thread A's fresh
	// completion must not keep B's launch visible any more than it
	// already would be (launch has no completion → still visible),
	// and must not leak a `completion` row into B's result.
	if !equalStringSlice(collectIDs(gotB), []string{"launch"}) {
		t.Errorf("tb ids: got %v, want [launch]", collectIDs(gotB))
	}
}

// TestListLiveBackgroundTasks_ErroredCompletionKeepsPairVisible pins
// that a completion with status='errored' (or any non-'completed'
// terminal state) still gates launch visibility via its createdAt.
// The query is intentionally status-agnostic on the completion side;
// a future filter that excluded errored completions would orphan
// every failed background task's launch as "running".
func TestListLiveBackgroundTasks_ErroredCompletionKeepsPairVisible(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedBackgroundItem(t, s, "t", "launch", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "t", "err-completion", 0, 1, "errored", "launch", 5000)

	got, err := s.ListLiveBackgroundTasks("t", 4000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"launch", "err-completion"}) {
		t.Errorf("ids: got %v, want [launch err-completion]", collectIDs(got))
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

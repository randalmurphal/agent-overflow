package store

import (
	"strings"
	"testing"
)

func TestListThreadProposedPlans_ReturnsCurrentPlanOnly(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Three plans across two turns plus one non-plan item that must be
	// excluded by the payload_kind filter. The UI only presents the
	// current plan, so the aggregate query returns the highest plan
	// ordering row instead of the historical list.
	seedPayloadItem(t, s, "t", "p1", 0, 0, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "p2", 1, 0, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "p3", 1, 2, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "diff", 1, 1, "tool_call", "diff", "{}")
	for _, id := range []string{"p1", "p2", "p3"} {
		if _, err := s.EnsureProposedPlanState("t", id, 100); err != nil {
			t.Fatalf("ensure %s: %v", id, err)
		}
	}

	got, err := s.ListThreadProposedPlans("t")
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}

	if len(got) != 1 || got[0].ID != "p3" {
		t.Errorf("plans: got %v, want only current plan p3", collectIDs(got))
	}
}

func TestProposedPlanStateAllowsSameItemIDAcrossThreads(t *testing.T) {
	s := newTestStore(t)
	for _, threadID := range []string{"t1", "t2"} {
		if err := s.CreateThread(makeThread(threadID, "claude")); err != nil {
			t.Fatalf("create thread %s: %v", threadID, err)
		}
		seedPayloadItem(t, s, threadID, "plan:0", 0, 0, "assistant_text", "proposed_plan", "{}")
		if _, err := s.EnsureProposedPlanState(threadID, "plan:0", 100); err != nil {
			t.Fatalf("ensure plan state for %s: %v", threadID, err)
		}
	}
}

func TestListThreadProposedPlans_DecoratesVersionAndCommentCounts(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "p1", 0, 0, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "p2", 1, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("t", "p1", 100); err != nil {
		t.Fatalf("ensure p1: %v", err)
	}
	if _, err := s.EnsureProposedPlanStateWithParent("t", "p2", "p1", 200); err != nil {
		t.Fatalf("ensure p2: %v", err)
	}
	if _, err := s.CreateProposedPlanComment(ProposedPlanComment{
		ID: "c1", ThreadID: "t", PlanItemID: "p2", StartLine: 1, EndLine: 1, Body: "tighten this", CreatedAt: 300, UpdatedAt: 300,
	}); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	got, err := s.ListThreadProposedPlans("t")
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("plans = %d, want 1 current plan", len(got))
	}
	if got[0].ID != "p2" {
		t.Fatalf("latest id = %q, want p2", got[0].ID)
	}
	if got[0].Meta == "" {
		t.Fatal("latest plan meta is empty")
	}
	if !strings.Contains(got[0].Meta, `"planVersion":2`) {
		t.Fatalf("latest plan meta = %s, want version 2", got[0].Meta)
	}
	if !strings.Contains(got[0].Meta, `"draft":1`) {
		t.Fatalf("latest plan meta = %s, want one draft comment", got[0].Meta)
	}
	if !strings.Contains(got[0].Meta, `"planRevisionParentItemId":"p1"`) {
		t.Fatalf("latest plan meta = %s, want p1 parent", got[0].Meta)
	}
}

func TestEnsureProposedPlanStateWithParentUsesExplicitRevisionSource(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "p1", 0, 0, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "p2", 1, 0, "assistant_text", "proposed_plan", "{}")
	seedPayloadItem(t, s, "t", "p3", 2, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("t", "p1", 100); err != nil {
		t.Fatalf("ensure p1: %v", err)
	}
	if _, err := s.EnsureProposedPlanState("t", "p2", 200); err != nil {
		t.Fatalf("ensure p2: %v", err)
	}
	state, err := s.EnsureProposedPlanStateWithParent("t", "p3", "p1", 300)
	if err != nil {
		t.Fatalf("ensure p3: %v", err)
	}
	if state.RevisionParentItemID != "p1" {
		t.Fatalf("parent = %q, want explicit p1", state.RevisionParentItemID)
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
	if _, err := s.EnsureProposedPlanState("t", "agent-plan", 100); err != nil {
		t.Fatalf("ensure agent plan: %v", err)
	}

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

func TestMarkLiveBackgroundToolCallsInactiveClearsOnlyLiveTopLevelLaunches(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedBackgroundItem(t, s, "t", "run", 0, 0, "running", "", 1000)
	seedBackgroundItem(t, s, "t", "completed-launch", 0, 1, "running", "", 1000)
	seedBackgroundItem(t, s, "t", "completed-row", 0, 2, "completed", "completed-launch", 2000)
	if err := s.InsertItem(Item{
		ID:           "child",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    3,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      "child",
		IsBackground: true,
		ParentID:     "parent",
		Meta:         `{"live_background_active":true}`,
		CreatedAt:    1000,
	}); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	count, err := s.MarkLiveBackgroundToolCallsInactive("t", 3000)
	if err != nil {
		t.Fatalf("mark inactive: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows affected = %d, want 1", count)
	}

	run, ok, err := s.GetItem("run")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !ok {
		t.Fatal("run item missing")
	}
	if run.Status != "running" {
		t.Fatalf("run status = %q, want running", run.Status)
	}
	if !strings.Contains(run.Meta, `"live_background_active":false`) {
		t.Fatalf("run meta = %q, want live_background_active=false", run.Meta)
	}

	completedLaunch, _, err := s.GetItem("completed-launch")
	if err != nil {
		t.Fatalf("get completed launch: %v", err)
	}
	if completedLaunch.Status != "running" {
		t.Fatalf("completed launch status = %q, want running", completedLaunch.Status)
	}
	child, _, err := s.GetItem("child")
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if child.Status != "running" {
		t.Fatalf("child status = %q, want running", child.Status)
	}

	live, err := s.ListLiveBackgroundTasks("t", 0)
	if err != nil {
		t.Fatalf("list live background tasks: %v", err)
	}
	if ids := collectIDs(live); !equalStringSlice(ids, []string{"completed-launch", "completed-row"}) {
		t.Fatalf("live ids = %v, want completed launch pair only", ids)
	}
}

func TestCountLiveRunningBackgroundToolCallsIncludesStashedLaunches(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedBackgroundItem(t, s, "t", "exited", 0, 0, "running", "", 100)
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "t",
		TaskID:    "task-x",
		ToolUseID: "exited",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 500,
	}); err != nil {
		t.Fatalf("upsert stash: %v", err)
	}

	count, err := s.CountLiveRunningBackgroundToolCalls("t")
	if err != nil {
		t.Fatalf("count live background tasks: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestListLiveBackgroundTasks_ExcludesInactiveCodexSubagent(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "codex")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if err := s.InsertItem(Item{
		ID:           "spawn-inactive",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      "spawn-inactive",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"live_background_active":false}`,
		CreatedAt:    1000,
	}); err != nil {
		t.Fatalf("seed inactive spawn: %v", err)
	}

	got, err := s.ListLiveBackgroundTasks("t", 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("inactive Codex subagent should be excluded, got %+v", got)
	}
}

func TestListLiveCodexSubagentLaunches_ReturnsCompletedActiveSpawn(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "codex")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	activeMeta := `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"],"agentsStates":{"child-1":{"status":"running"}}}}`
	if err := s.InsertItem(Item{
		ID:           "spawn-active",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         activeMeta,
		CreatedAt:    1000,
	}); err != nil {
		t.Fatalf("seed active spawn: %v", err)
	}

	got, err := s.ListLiveCodexSubagentLaunches("t")
	if err != nil {
		t.Fatalf("list launches: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"spawn-active"}) {
		t.Fatalf("ids: got %v, want [spawn-active]", collectIDs(got))
	}
	if got[0].Status != "completed" {
		t.Fatalf("stored status projection leaked into store query: got %q, want completed", got[0].Status)
	}
	if active, err := s.HasLiveCodexSubagentLaunch("t"); err != nil {
		t.Fatalf("has live Codex subagent launch: %v", err)
	} else if !active {
		t.Fatal("HasLiveCodexSubagentLaunch = false, want true")
	}
}

func TestListLiveCodexSubagentLaunches_ExcludesInactiveAndCompleted(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "codex")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.CreateThread(makeThread("claude-thread", "claude")); err != nil {
		t.Fatalf("create Claude thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:           "spawn-inactive",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent inactive",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"live_background_active":false,"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"]}}`,
		CreatedAt:    1000,
	}); err != nil {
		t.Fatalf("seed inactive spawn: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:           "spawn-with-completion",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent completed",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-2"]}}`,
		CreatedAt:    1001,
	}); err != nil {
		t.Fatalf("seed completed spawn: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:           "spawn-completion",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    2,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent done",
		IsBackground: true,
		CompletionOf: "spawn-with-completion",
		CreatedAt:    1002,
	}); err != nil {
		t.Fatalf("seed completion: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:           "spawn-running",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    3,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      "spawn_agent running",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-3"]}}`,
		CreatedAt:    1003,
	}); err != nil {
		t.Fatalf("seed running spawn: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:           "send-input",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    4,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "send_input",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"input":{"tool":"send_input","receiverThreadIds":["child-4"]}}`,
		CreatedAt:    1004,
	}); err != nil {
		t.Fatalf("seed non-spawn collab item: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:           "claude-spawn-shaped",
		ThreadID:     "claude-thread",
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent on Claude thread",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-5"]}}`,
		CreatedAt:    1005,
	}); err != nil {
		t.Fatalf("seed Claude spawn-shaped item: %v", err)
	}

	got, err := s.ListLiveCodexSubagentLaunches("t")
	if err != nil {
		t.Fatalf("list launches: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want no live Codex subagent launches", collectIDs(got))
	}
	if active, err := s.HasLiveCodexSubagentLaunch("t"); err != nil {
		t.Fatalf("has live Codex subagent launch: %v", err)
	} else if active {
		t.Fatal("HasLiveCodexSubagentLaunch = true, want false")
	}

	running, err := s.ListLiveBackgroundTasks("t", 2000)
	if err != nil {
		t.Fatalf("list ordinary live background tasks: %v", err)
	}
	if !equalStringSlice(collectIDs(running), []string{"spawn-running"}) {
		t.Fatalf("ordinary live background ids: got %v, want [spawn-running]", collectIDs(running))
	}

	claudeGot, err := s.ListLiveCodexSubagentLaunches("claude-thread")
	if err != nil {
		t.Fatalf("list Claude spawn-shaped items: %v", err)
	}
	if len(claudeGot) != 0 {
		t.Fatalf("Claude spawn-shaped row should not project as Codex live work, got %v", collectIDs(claudeGot))
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

func TestListLiveBackgroundTasks_ExcludesSubagentScopedBackgroundRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	parent := Item{
		ID:        "spawn-parent",
		ThreadID:  "t",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "Agent",
		ToolName:  "Agent",
		CreatedAt: 100,
		UpdatedAt: 100,
	}
	if err := s.InsertItem(parent); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	scopedLaunch := Item{
		ID:           "child-bash",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      "Bash: sleep 60",
		ParentID:     "spawn-parent",
		IsBackground: true,
		ToolName:     "Bash",
		CreatedAt:    200,
		UpdatedAt:    200,
	}
	if err := s.InsertItem(scopedLaunch); err != nil {
		t.Fatalf("seed scoped launch: %v", err)
	}
	scopedCompletion := Item{
		ID:           "child-bash-done",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    2,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "Bash: sleep 60 -> done",
		ParentID:     "spawn-parent",
		IsBackground: true,
		CompletionOf: "child-bash",
		ToolName:     "Bash",
		CreatedAt:    5000,
		UpdatedAt:    5000,
	}
	if err := s.InsertItem(scopedCompletion); err != nil {
		t.Fatalf("seed scoped completion: %v", err)
	}

	seedBackgroundItem(t, s, "t", "top-level-bash", 0, 3, "running", "", 300)

	got, err := s.ListLiveBackgroundTasks("t", 4000)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"top-level-bash"}) {
		t.Fatalf("ids: got %v, want [top-level-bash]", collectIDs(got))
	}
}

func TestHasLiveBackgroundToolCall_ExcludesSubagentScopedBackgroundRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	parent := Item{
		ID:        "spawn-parent",
		ThreadID:  "t",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "Agent",
		ToolName:  "Agent",
		CreatedAt: 100,
		UpdatedAt: 100,
	}
	if err := s.InsertItem(parent); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	scopedLaunch := Item{
		ID:           "child-bash",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      "Bash: sleep 60",
		ParentID:     "spawn-parent",
		IsBackground: true,
		ToolName:     "Bash",
		CreatedAt:    200,
		UpdatedAt:    200,
	}
	if err := s.InsertItem(scopedLaunch); err != nil {
		t.Fatalf("seed scoped launch: %v", err)
	}

	active, err := s.HasLiveBackgroundToolCall("t")
	if err != nil {
		t.Fatalf("has live background: %v", err)
	}
	if active {
		t.Fatal("subagent-scoped background row should not block the top-level queue")
	}

	seedBackgroundItem(t, s, "t", "top-level-bash", 0, 2, "running", "", 300)
	active, err = s.HasLiveBackgroundToolCall("t")
	if err != nil {
		t.Fatalf("has live background after top-level seed: %v", err)
	}
	if !active {
		t.Fatal("top-level background row should block the queue")
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

// TestListLiveBackgroundTasks_StashKeepsLaunchVisible pins that a
// stashed `pending_background_task_terminals` row does NOT hide the
// associated launch. The launch stays "running" in the tray until the
// observation event writes the real `tool_completion` sibling.
func TestListLiveBackgroundTasks_StashKeepsLaunchVisible(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Two running background launches; one has a stash entry (process
	// exited, agent hasn't observed yet). Both must stay visible.
	seedBackgroundItem(t, s, "t", "exited", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "t", "still-running", 0, 1, "running", "", 200)

	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "t",
		TaskID:    "task-x",
		ToolUseID: "exited",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 500,
	}); err != nil {
		t.Fatalf("upsert stash: %v", err)
	}

	got, err := s.ListLiveBackgroundTasks("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"exited", "still-running"}) {
		t.Errorf("ids: got %v, want [exited still-running] (stash must NOT hide launch)", collectIDs(got))
	}

	// Drain the stash: launch still surfaces because nothing changed
	// about the launch itself — it stayed visible the whole time.
	if _, _, err := s.TakePendingBackgroundTerminal("t", "task-x"); err != nil {
		t.Fatalf("take stash: %v", err)
	}
	got, err = s.ListLiveBackgroundTasks("t", 0)
	if err != nil {
		t.Fatalf("list after drain: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"exited", "still-running"}) {
		t.Errorf("ids after drain: got %v, want [exited still-running]", collectIDs(got))
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

package store

import (
	"fmt"
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

func TestReconcileProposedPlanStateFromAcceptedTurns(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "plan-1", 0, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("t", "plan-1", 100); err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	if err := s.InsertTurn(Turn{TurnID: "turn-1", ThreadID: "t", TurnIndex: 1, StartedAt: 200}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:        "user:1",
		ThreadID:  "t",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "Implement the plan.",
		Meta:      `{"sourceProposedPlan":{"threadId":"t","itemId":"plan-1"}}`,
		CreatedAt: 200,
		UpdatedAt: 200,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}

	if err := s.ReconcileProposedPlanStateFromAcceptedTurns(300); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	state, found, err := s.GetProposedPlanState("t", "plan-1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !found || state.ImplementedByItemID != "user:1" || state.ImplementedAt != 200 {
		t.Fatalf("state = %+v, want implemented by user:1 at accepted turn time", state)
	}
}

func TestReconcileProposedPlanStateKeepsFirstImplementationAttribution(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "plan-1", 0, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("t", "plan-1", 100); err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	if err := s.InsertTurn(Turn{TurnID: "turn-1", ThreadID: "t", TurnIndex: 1, StartedAt: 200}); err != nil {
		t.Fatalf("insert turn 1: %v", err)
	}
	if err := s.InsertTurn(Turn{TurnID: "turn-2", ThreadID: "t", TurnIndex: 2, StartedAt: 250}); err != nil {
		t.Fatalf("insert turn 2: %v", err)
	}
	for _, item := range []Item{
		{
			ID:        "user:1",
			ThreadID:  "t",
			TurnIndex: 1,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   "Implement the plan.",
			Meta:      `{"sourceProposedPlan":{"threadId":"t","itemId":"plan-1"}}`,
			CreatedAt: 200,
			UpdatedAt: 200,
		},
		{
			ID:        "user:2",
			ThreadID:  "t",
			TurnIndex: 2,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   "Implement the plan again.",
			Meta:      `{"sourceProposedPlan":{"threadId":"t","itemId":"plan-1"}}`,
			CreatedAt: 250,
			UpdatedAt: 250,
		},
	} {
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert user item %s: %v", item.ID, err)
		}
	}

	if err := s.ReconcileProposedPlanStateFromAcceptedTurns(300); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	state, found, err := s.GetProposedPlanState("t", "plan-1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !found || state.ImplementedByItemID != "user:1" || state.ImplementedAt != 200 {
		t.Fatalf("state = %+v, want first accepted implementation user:1 at 200", state)
	}
}

func TestReconcileProposedPlanStateAllowsCrossThreadImplementationSource(t *testing.T) {
	s := newTestStore(t)
	for _, threadID := range []string{"source", "impl"} {
		if err := s.CreateThread(makeThread(threadID, "claude")); err != nil {
			t.Fatalf("create thread %s: %v", threadID, err)
		}
	}
	seedPayloadItem(t, s, "source", "plan-1", 0, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("source", "plan-1", 100); err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	if err := s.InsertTurn(Turn{TurnID: "turn-1", ThreadID: "impl", TurnIndex: 1, StartedAt: 200}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:        "user:1",
		ThreadID:  "impl",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "Implement the plan.",
		Meta:      `{"sourceProposedPlan":{"threadId":"source","itemId":"plan-1"}}`,
		CreatedAt: 200,
		UpdatedAt: 200,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}

	if err := s.ReconcileProposedPlanStateFromAcceptedTurns(300); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	state, found, err := s.GetProposedPlanState("source", "plan-1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !found || state.ImplementedAt != 200 || state.ImplementedByThreadID != "impl" || state.ImplementedByItemID != "user:1" {
		t.Fatalf("state = %+v, want implemented by impl/user:1", state)
	}
}

func TestReconcileProposedPlanStateMarksRevisionCommentsSent(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "plan-1", 0, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("t", "plan-1", 100); err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	if _, err := s.CreateProposedPlanComment(ProposedPlanComment{
		ID: "comment-1", ThreadID: "t", PlanItemID: "plan-1", StartLine: 1, EndLine: 1,
		Body: "revise", CreatedAt: 150, UpdatedAt: 150,
	}); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	if err := s.InsertTurn(Turn{TurnID: "turn-1", ThreadID: "t", TurnIndex: 1, StartedAt: 200}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:        "user:1",
		ThreadID:  "t",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "Revise the plan.",
		Meta:      `{"revisionSourceProposedPlan":{"threadId":"t","itemId":"plan-1"},"revisionSourceCommentIds":["comment-1"]}`,
		CreatedAt: 200,
		UpdatedAt: 200,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}

	if err := s.ReconcileProposedPlanStateFromAcceptedTurns(300); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	comment, err := s.GetProposedPlanComment("t", "comment-1")
	if err != nil {
		t.Fatalf("get comment: %v", err)
	}
	if comment.Status != "sent" || comment.SentTurnID != "turn-1" || comment.SentAt != 200 {
		t.Fatalf("comment = %+v, want sent by accepted turn", comment)
	}
}

// TestReconcileProposedPlanStateMarksImplementedWithoutTurnsRow guards
// the LEFT JOIN in ReconcileProposedPlanStateFromAcceptedTurns:
// when the user message landed but the matching turns row never did
// (crash between PersistItem and EventTurnStart, or older data from
// before the send-time mark), reconcile must still mark the source
// plan implemented using the item's created_at as the timestamp.
func TestReconcileProposedPlanStateMarksImplementedWithoutTurnsRow(t *testing.T) {
	s := newTestStore(t)
	for _, threadID := range []string{"source", "impl"} {
		if err := s.CreateThread(makeThread(threadID, "claude")); err != nil {
			t.Fatalf("create thread %s: %v", threadID, err)
		}
	}
	seedPayloadItem(t, s, "source", "plan-1", 0, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("source", "plan-1", 100); err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	// Deliberately skip InsertTurn — this is the "user message persisted,
	// turn row never written" scenario. Without the LEFT JOIN, the
	// reconcile's INNER JOIN drops this row entirely.
	if err := s.InsertItem(Item{
		ID:        "user:1",
		ThreadID:  "impl",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "Implement the plan.",
		Meta:      `{"sourceProposedPlan":{"threadId":"source","itemId":"plan-1"}}`,
		CreatedAt: 250,
		UpdatedAt: 250,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}

	if err := s.ReconcileProposedPlanStateFromAcceptedTurns(900); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	state, found, err := s.GetProposedPlanState("source", "plan-1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !found {
		t.Fatal("plan state missing after reconcile")
	}
	// Without a turns row, reconcile must fall back to the item's
	// created_at (250), not the now sentinel (900) or zero. The latter
	// would falsely advertise an unimplemented plan; the former would
	// retroactively claim the plan landed long after it actually did.
	if state.ImplementedAt != 250 {
		t.Errorf("ImplementedAt = %d, want 250 (item's created_at)", state.ImplementedAt)
	}
	if state.ImplementedByThreadID != "impl" || state.ImplementedByItemID != "user:1" {
		t.Errorf("attribution = %s/%s, want impl/user:1", state.ImplementedByThreadID, state.ImplementedByItemID)
	}
}

// TestReconcileProposedPlanStateSkipsFailedSendUserMessages pins the
// "no turns row + sibling error item = send failed, keep plan
// retryable" rule. Without this exclusion the LEFT JOIN reconcile
// would mark the plan implemented at restart even though the
// implementation never actually ran, hiding the Implement button and
// stranding the user.
func TestReconcileProposedPlanStateSkipsFailedSendUserMessages(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "plan-1", 0, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("t", "plan-1", 100); err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	// Failed-send shape: user_text with sourceProposedPlan + sibling
	// error item, no turns row (EventTurnStart never fired).
	if err := s.InsertItem(Item{
		ID:        "user:1",
		ThreadID:  "t",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "Implement the plan.",
		Meta:      `{"sourceProposedPlan":{"threadId":"t","itemId":"plan-1"}}`,
		CreatedAt: 250,
		UpdatedAt: 250,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:        "error:1:0",
		ThreadID:  "t",
		TurnIndex: 1,
		ItemIndex: 1,
		Kind:      "error",
		Role:      "system",
		Status:    "completed",
		Summary:   "Failed to send: session has no provider",
		CreatedAt: 251,
		UpdatedAt: 251,
	}); err != nil {
		t.Fatalf("insert error item: %v", err)
	}

	if err := s.ReconcileProposedPlanStateFromAcceptedTurns(900); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	state, _, err := s.GetProposedPlanState("t", "plan-1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.ImplementedAt != 0 {
		t.Errorf("ImplementedAt = %d, want 0 (failed-send shape must stay retryable across reconcile)", state.ImplementedAt)
	}
}

// TestReconcileProposedPlanStateMarksRevisionCommentsSentWithoutTurnsRow
// is the revision-comment counterpart to the implemented-plan test
// above. Same scenario: user revision message persisted, no turns row.
// Without the LEFT JOIN + synthesized turn_id, the comment would stay
// "draft" forever even though the user has already sent it.
func TestReconcileProposedPlanStateMarksRevisionCommentsSentWithoutTurnsRow(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "plan-1", 0, 0, "assistant_text", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("t", "plan-1", 100); err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	if _, err := s.CreateProposedPlanComment(ProposedPlanComment{
		ID: "comment-1", ThreadID: "t", PlanItemID: "plan-1", StartLine: 1, EndLine: 1,
		Body: "revise", CreatedAt: 150, UpdatedAt: 150,
	}); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	// No InsertTurn for turn 1.
	if err := s.InsertItem(Item{
		ID:        "user:1",
		ThreadID:  "t",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "Revise the plan.",
		Meta:      `{"revisionSourceProposedPlan":{"threadId":"t","itemId":"plan-1"},"revisionSourceCommentIds":["comment-1"]}`,
		CreatedAt: 250,
		UpdatedAt: 250,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}

	if err := s.ReconcileProposedPlanStateFromAcceptedTurns(900); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	comment, err := s.GetProposedPlanComment("t", "comment-1")
	if err != nil {
		t.Fatalf("get comment: %v", err)
	}
	if comment.Status != "sent" {
		t.Errorf("status = %q, want sent", comment.Status)
	}
	if comment.SentAt != 250 {
		t.Errorf("SentAt = %d, want 250 (item's created_at)", comment.SentAt)
	}
	// resolveTurnID's Claude fallback: <threadID>:<turnIndex>.
	if comment.SentTurnID != "t:1" {
		t.Errorf("SentTurnID = %q, want %q (synthesized from thread+turn)", comment.SentTurnID, "t:1")
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

// TestListLiveBackgroundTasks_StashKeepsLaunchVisible pins the
// Tray-A behavior post-fix: once Claude system/task_updated lands
// and triage stashes the terminal in
// `pending_background_task_terminals`, the launch row STAYS visible
// so the tray can pair it with the synthetic completion item from
// `ListPendingBackgroundCompletionsAsItems`. The previous design
// hid the launch and left the tray with nothing to render between
// process-exit and the agent observation event.
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

// TestListPendingBackgroundCompletionsAsItems_SynthesizesFromStash pins
// the happy path: stash row exists, no chat sibling has landed, and the
// query synthesizes a tray-only tool_completion item shaped like the
// real sibling that will eventually arrive.
func TestListPendingBackgroundCompletionsAsItems_SynthesizesFromStash(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedBackgroundItem(t, s, "t", "launch", 0, 0, "running", "", 100)
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:   "t",
		TaskID:     "task-x",
		ToolUseID:  "launch",
		Status:     "completed",
		OutputFile: "/tmp/out.txt",
		Source:     "task_updated",
		CreatedAt:  500,
	}); err != nil {
		t.Fatalf("upsert stash: %v", err)
	}

	got, err := s.ListPendingBackgroundCompletionsAsItems("t", 0)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(got), got)
	}
	item := got[0]
	if item.ID != "complete:launch" {
		t.Errorf("id = %q, want complete:launch", item.ID)
	}
	if item.Kind != "tool_completion" {
		t.Errorf("kind = %q, want tool_completion", item.Kind)
	}
	if item.CompletionOf != "launch" {
		t.Errorf("completion_of = %q, want launch", item.CompletionOf)
	}
	if !item.IsBackground {
		t.Errorf("is_background = false, want true")
	}
	if item.Status != "completed" {
		t.Errorf("status = %q, want completed", item.Status)
	}
	if item.Summary != "launch -> done" {
		t.Errorf("summary = %q, want %q (mirrors triage's completed-with-no-exitcode outcome)", item.Summary, "launch -> done")
	}
	if item.CreatedAt != 500 {
		t.Errorf("created_at = %d, want 500 (stash created_at)", item.CreatedAt)
	}
	for _, want := range []string{`"task_id":"task-x"`, `"tool_use_id":"launch"`, `"status":"completed"`, `"status_source":"task_updated"`, `"synthetic":true`, `"output_file":"/tmp/out.txt"`} {
		if !strings.Contains(item.Meta, want) {
			t.Errorf("meta missing %q: %s", want, item.Meta)
		}
	}
}

// TestListPendingBackgroundCompletionsAsItems_ExcludedWhenRealSiblingExists
// guards the de-duplication invariant: once the chat-side completion has
// been written, the synthetic must not also be returned — otherwise the
// tray would render two completion rows for the same launch.
func TestListPendingBackgroundCompletionsAsItems_ExcludedWhenRealSiblingExists(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedBackgroundItem(t, s, "t", "launch", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "t", "real-completion", 0, 1, "completed", "launch", 600)
	// A late stash arrival paired with a launch that already has a
	// sibling — this happens when the chat write races ahead of triage's
	// drain on the next reconnect-replay.
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "t",
		TaskID:    "task-x",
		ToolUseID: "launch",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 500,
	}); err != nil {
		t.Fatalf("upsert stash: %v", err)
	}

	got, err := s.ListPendingBackgroundCompletionsAsItems("t", 0)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d items, want 0 (real sibling must suppress synth): %+v", len(got), got)
	}
}

// TestListPendingBackgroundCompletionsAsItems_StatusMapping verifies the
// wire-status → canonical-status translation matches
// `triage.backgroundTerminalStatus`. The tray's `completionStatusFor`
// keys on the canonical value, so a drift here renders failures as
// "completed" and vice versa.
//
// Outcome suffixes mirror `triage.backgroundTerminalOutcome` exactly,
// so the synthetic and real sibling produce the same "Bash -> done" /
// "Bash -> exit 1" / "Bash -> killed" shape end-to-end. `stopped` and
// any future unknown status default to no suffix because triage's
// outcome switch doesn't enumerate them.
func TestListPendingBackgroundCompletionsAsItems_StatusMapping(t *testing.T) {
	exit0 := int64(0)
	exit1 := int64(1)
	cases := []struct {
		name       string
		wire       string
		exitCode   *int64
		wantStatus string
		wantSumSfx string // expected suffix appended to launch summary ("" = no suffix)
	}{
		{"completed-exit-0", "completed", &exit0, "completed", " -> exit 0"},
		{"completed-no-exit", "completed", nil, "completed", " -> done"},
		{"completed-nonzero-exit", "completed", &exit1, "errored", " -> exit 1"},
		{"failed", "failed", nil, "errored", " -> failed"},
		{"interrupted", "interrupted", nil, "errored", " -> interrupted"},
		{"killed", "killed", nil, "killed", " -> killed"},
		{"stopped", "stopped", nil, "killed", ""},
		{"errored-wire-status", "errored", nil, "errored", ""},
		{"unknown-wire-status", "timeout", nil, "errored", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateThread(makeThread("t", "claude")); err != nil {
				t.Fatalf("create thread: %v", err)
			}
			seedBackgroundItem(t, s, "t", "launch", 0, 0, "running", "", 100)
			if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
				ThreadID:  "t",
				TaskID:    "task-x",
				ToolUseID: "launch",
				Status:    tc.wire,
				ExitCode:  tc.exitCode,
				Source:    "task_updated",
				CreatedAt: 500,
			}); err != nil {
				t.Fatalf("upsert stash: %v", err)
			}

			got, err := s.ListPendingBackgroundCompletionsAsItems("t", 0)
			if err != nil {
				t.Fatalf("list pending: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d items, want 1", len(got))
			}
			if got[0].Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got[0].Status, tc.wantStatus)
			}
			wantSummary := "launch" + tc.wantSumSfx
			if got[0].Summary != wantSummary {
				t.Errorf("summary = %q, want %q", got[0].Summary, wantSummary)
			}
		})
	}
}

// TestListPendingBackgroundCompletionsAsItems_RespectsRetentionCutoff
// pins that synthetic completions age out on the same clock as the
// persisted siblings: a stash row older than the cutoff must not
// surface, so the launch (which `ListLiveBackgroundTasks` ages out by
// its own retention rule) doesn't get a phantom completion.
func TestListPendingBackgroundCompletionsAsItems_RespectsRetentionCutoff(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedBackgroundItem(t, s, "t", "fresh-launch", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "t", "stale-launch", 0, 1, "running", "", 100)
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "t",
		TaskID:    "fresh",
		ToolUseID: "fresh-launch",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 5000,
	}); err != nil {
		t.Fatalf("upsert fresh: %v", err)
	}
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "t",
		TaskID:    "stale",
		ToolUseID: "stale-launch",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}

	got, err := s.ListPendingBackgroundCompletionsAsItems("t", 4000)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"complete:fresh-launch"}) {
		t.Errorf("ids: got %v, want [complete:fresh-launch] (stale stash must be filtered)", collectIDs(got))
	}
}

// TestListPendingBackgroundCompletionsAsItems_ThreadIsolation guards
// the thread_id scoping on the stash join — a stash row on thread A
// must not synthesize a completion item on thread B even when both
// threads have a launch row sharing the same id.
func TestListPendingBackgroundCompletionsAsItems_ThreadIsolation(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("ta", "claude")); err != nil {
		t.Fatalf("create thread ta: %v", err)
	}
	if err := s.CreateThread(makeThread("tb", "claude")); err != nil {
		t.Fatalf("create thread tb: %v", err)
	}

	// Same launch id on both threads — a missing thread_id predicate on
	// the items join would cross-match and surface tb's launch under
	// ta's stash (or vice versa).
	seedBackgroundItem(t, s, "ta", "launch", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "tb", "launch", 0, 0, "running", "", 100)
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "ta",
		TaskID:    "task-x",
		ToolUseID: "launch",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 500,
	}); err != nil {
		t.Fatalf("upsert stash on ta: %v", err)
	}

	gotA, err := s.ListPendingBackgroundCompletionsAsItems("ta", 0)
	if err != nil {
		t.Fatalf("list ta: %v", err)
	}
	if !equalStringSlice(collectIDs(gotA), []string{"complete:launch"}) {
		t.Errorf("ta ids: got %v, want [complete:launch]", collectIDs(gotA))
	}

	gotB, err := s.ListPendingBackgroundCompletionsAsItems("tb", 0)
	if err != nil {
		t.Fatalf("list tb: %v", err)
	}
	if len(gotB) != 0 {
		t.Errorf("tb ids: got %v, want [] (stash is on ta only)", collectIDs(gotB))
	}
}

// TestListPendingBackgroundCompletionsAsItems_RequiresBackgroundLaunch
// guards the `launch.is_background = 1` predicate. A foreground tool
// has no business synthesizing a tray completion even if a stash entry
// somehow points at it.
func TestListPendingBackgroundCompletionsAsItems_RequiresBackgroundLaunch(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Foreground tool — IsBackground stays false because we use seedItem
	// (assistant_text), but stash points at it anyway.
	seedItem(t, s, "t", "fg", 0, 0, "")
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "t",
		TaskID:    "task-fg",
		ToolUseID: "fg",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 500,
	}); err != nil {
		t.Fatalf("upsert stash: %v", err)
	}

	got, err := s.ListPendingBackgroundCompletionsAsItems("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d items, want 0 (foreground launch must not synthesize)", len(got))
	}
}

// TestListPendingBackgroundCompletionsAsItems_EmptyThreadID returns
// nil, nil — callers shouldn't crash if they hand in a missing thread
// id (e.g. during early bootstrap before the active thread is
// resolved). Matches the defensive guard at the top of the function.
func TestListPendingBackgroundCompletionsAsItems_EmptyThreadID(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ListPendingBackgroundCompletionsAsItems("", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d items, want 0 for empty thread id", len(got))
	}
}

// TestListPendingBackgroundCompletionsAsItems_SiblingScopedPerLaunch
// pins that the `NOT EXISTS` suppression keys on `completion_of = launch.id`,
// not just `thread_id`. A real sibling on launch B must not suppress
// the synthetic for a different launch A on the same thread.
func TestListPendingBackgroundCompletionsAsItems_SiblingScopedPerLaunch(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Launch A is stashed, no sibling yet — synth must surface.
	// Launch B has a real sibling — irrelevant to A's synth.
	seedBackgroundItem(t, s, "t", "launch-a", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "t", "launch-b", 0, 1, "running", "", 100)
	seedBackgroundItem(t, s, "t", "sibling-b", 0, 2, "completed", "launch-b", 600)
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "t",
		TaskID:    "task-a",
		ToolUseID: "launch-a",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 500,
	}); err != nil {
		t.Fatalf("upsert stash: %v", err)
	}

	got, err := s.ListPendingBackgroundCompletionsAsItems("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !equalStringSlice(collectIDs(got), []string{"complete:launch-a"}) {
		t.Errorf("ids: got %v, want [complete:launch-a] (sibling on launch-b must not suppress synth for launch-a)", collectIDs(got))
	}
}

// TestListPendingBackgroundCompletionsAsItems_MultipleStashesOrderedByCreatedAt
// pins that multiple simultaneous stash entries surface in the order
// they arrived, mirroring the ORDER BY p.created_at clause. The tray
// renders rows in arrival order so a future LIMIT or accidental ordering
// change would silently desync the visible list.
func TestListPendingBackgroundCompletionsAsItems_MultipleStashesOrderedByCreatedAt(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedBackgroundItem(t, s, "t", "first", 0, 0, "running", "", 100)
	seedBackgroundItem(t, s, "t", "second", 0, 1, "running", "", 100)
	seedBackgroundItem(t, s, "t", "third", 0, 2, "running", "", 100)
	for _, tc := range []struct {
		taskID    string
		toolUseID string
		createdAt int64
	}{
		{"task-second", "second", 600},
		{"task-first", "first", 400},
		{"task-third", "third", 800},
	} {
		if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
			ThreadID:  "t",
			TaskID:    tc.taskID,
			ToolUseID: tc.toolUseID,
			Status:    "completed",
			Source:    "task_updated",
			CreatedAt: tc.createdAt,
		}); err != nil {
			t.Fatalf("upsert %s: %v", tc.taskID, err)
		}
	}

	got, err := s.ListPendingBackgroundCompletionsAsItems("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"complete:first", "complete:second", "complete:third"}
	if !equalStringSlice(collectIDs(got), want) {
		t.Errorf("ids: got %v, want %v (ordered by stash created_at)", collectIDs(got), want)
	}
}

// TestListPendingBackgroundCompletionsAsItems_EmptyToolUseIDSkipped
// pins that a stash row whose tool_use_id is empty (the rare
// documented case where the parser map lost the correlation) doesn't
// fabricate a synthetic against the wrong launch. The JOIN on
// `launch.id = p.tool_use_id` makes empty tool_use_id un-joinable
// because no real item carries id="".
func TestListPendingBackgroundCompletionsAsItems_EmptyToolUseIDSkipped(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedBackgroundItem(t, s, "t", "launch", 0, 0, "running", "", 100)
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID:  "t",
		TaskID:    "task-x",
		ToolUseID: "", // unknown correlation
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: 500,
	}); err != nil {
		t.Fatalf("upsert stash: %v", err)
	}

	got, err := s.ListPendingBackgroundCompletionsAsItems("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d items, want 0 (empty tool_use_id must not synthesize): %+v", len(got), got)
	}
}

// TestPendingTerminalStatusParityWithTriage is the contract test the
// `mapPendingTerminalStatus` docstring promises. Triage's
// `backgroundTerminalStatus` (re-implemented here in package-local
// form because the canonical version takes a triage-internal struct)
// is the authoritative mapping; this test feeds the same (wire,
// exitCode) inputs through both and asserts equal output. A drift
// here means the synth and the persisted sibling would disagree on
// status, which renders as a green-vs-red badge flip when the real
// sibling lands.
func TestPendingTerminalStatusParityWithTriage(t *testing.T) {
	// Local re-implementation of triage.backgroundTerminalStatus, kept
	// in sync by hand because store cannot import triage. If this
	// drifts, the cross-package status renderings drift too.
	triageEquivalent := func(wire string, exit *int64, isError bool) string {
		if wire == "killed" || wire == "stopped" {
			return "killed"
		}
		if isError {
			return "errored"
		}
		if exit != nil && *exit != 0 {
			return "errored"
		}
		switch wire {
		case "completed":
			return "completed"
		case "", "failed", "interrupted", "errored":
			if wire == "" {
				return "completed"
			}
			return "errored"
		default:
			return "errored"
		}
	}

	exit0 := int64(0)
	exit1 := int64(1)
	wireStatuses := []string{"completed", "failed", "interrupted", "errored", "killed", "stopped", "timeout", ""}
	exitCodes := []*int64{nil, &exit0, &exit1}
	for _, wire := range wireStatuses {
		for _, exit := range exitCodes {
			got := mapPendingTerminalStatus(wire, exit)
			// isError=false: triage's IsError is sourced from
			// task_output meta, never present in a stash row,
			// so the parity test fixes it false.
			want := triageEquivalent(wire, exit, false)
			if got != want {
				exitDesc := "nil"
				if exit != nil {
					exitDesc = fmt.Sprintf("%d", *exit)
				}
				t.Errorf("mapPendingTerminalStatus(%q, %s) = %q, want %q (triage equivalent)", wire, exitDesc, got, want)
			}
		}
	}
}

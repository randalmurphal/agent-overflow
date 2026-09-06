package app

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// threadBroadcasts captures the `thread:updated` frames one mutation
// produced. Same emit-capturing hook TestUpdateThreadBranchBroadcastsChangedRows
// uses; this recorder just makes the two assertions every mutation shares
// ("emitted the changed row" / "said nothing") reusable across a table.
type threadBroadcasts struct {
	t      *testing.T
	events []triage.ThreadUpdateEvent
}

func captureThreadBroadcasts(t *testing.T, app *App) *threadBroadcasts {
	t.Helper()
	recorder := &threadBroadcasts{t: t}
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		evt, ok := data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Errorf("thread:updated payload type = %T, want triage.ThreadUpdateEvent", data)
			return
		}
		recorder.events = append(recorder.events, evt)
	}
	return recorder
}

func (r *threadBroadcasts) reset() { r.events = nil }

// expectRow asserts exactly one frame carrying the named thread's row under
// the given action, and returns it for field assertions.
func (r *threadBroadcasts) expectRow(action, threadID string) store.Thread {
	r.t.Helper()
	if len(r.events) != 1 {
		r.t.Fatalf("emitted %d thread:updated events, want 1: %+v", len(r.events), r.events)
	}
	evt := r.events[0]
	if evt.Action != action {
		r.t.Fatalf("action = %q, want %q", evt.Action, action)
	}
	if evt.Thread == nil {
		r.t.Fatalf("action %q carried no row", action)
	}
	if evt.Thread.ID != threadID {
		r.t.Fatalf("broadcast row id = %q, want %q", evt.Thread.ID, threadID)
	}
	return *evt.Thread
}

func (r *threadBroadcasts) expectSilence(what string) {
	r.t.Helper()
	if len(r.events) != 0 {
		r.t.Fatalf("%s emitted %d thread:updated events, want 0: %+v", what, len(r.events), r.events)
	}
}

// TestThreadMutationsBroadcastTheChangedRow is the backend half of state-sync
// completeness: a second attached client only ever learns about a thread-row
// mutation from `thread:updated`, because the RPC's return value reaches the
// client that issued it and nobody else. Every persisted mutation is
// exercised here for both halves of the contract — the changed row goes out,
// and repeating the same write says nothing.
func TestThreadMutationsBroadcastTheChangedRow(t *testing.T) {
	t.Run("empty draft cleanup announces deletion only after content is cleared", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		if err := app.SaveDraft(t.Context(), thread.ID, "unsent", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		broadcasts := captureThreadBroadcasts(t, app)
		deleted, err := app.DeleteEmptyDraftThread(thread.ID)
		if err != nil || deleted {
			t.Fatalf("cleanup nonempty draft = %v, %v", deleted, err)
		}
		broadcasts.expectSilence("retaining draft content")
		if err := app.ClearDraft(t.Context(), thread.ID); err != nil {
			t.Fatal(err)
		}
		broadcasts.reset()
		deleted, err = app.DeleteEmptyDraftThread(thread.ID)
		if err != nil || !deleted {
			t.Fatalf("cleanup empty draft = %v, %v", deleted, err)
		}
		if len(broadcasts.events) != 1 || broadcasts.events[0].Action != triage.ThreadActionDeleted || broadcasts.events[0].ID != thread.ID || broadcasts.events[0].Thread != nil {
			t.Fatalf("cleanup broadcast = %+v, want one deletion for %s", broadcasts.events, thread.ID)
		}
		broadcasts.reset()
		_, _ = app.DeleteEmptyDraftThread(thread.ID)
		broadcasts.expectSilence("repeating empty draft cleanup")
	})

	t.Run("create is listed", func(t *testing.T) {
		app := newTestAppWithStore(t)
		broadcasts := captureThreadBroadcasts(t, app)
		thread, err := createTestThread(t, app, "claude", t.TempDir(), "claude-sonnet-4-6", "")
		if err != nil {
			t.Fatalf("createTestThread: %v", err)
		}
		broadcasts.expectRow(triage.ThreadActionListed, thread.ID)
	})

	t.Run("archive is unlisted, re-archive is silent", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		broadcasts := captureThreadBroadcasts(t, app)

		if err := app.ArchiveThread(thread.ID); err != nil {
			t.Fatalf("ArchiveThread: %v", err)
		}
		row := broadcasts.expectRow(triage.ThreadActionUnlisted, thread.ID)
		if !row.Archived {
			t.Fatal("broadcast row Archived = false, want true")
		}

		broadcasts.reset()
		if err := app.ArchiveThread(thread.ID); err != nil {
			t.Fatalf("ArchiveThread(repeat): %v", err)
		}
		broadcasts.expectSilence("re-archiving an archived thread")
	})

	t.Run("unarchive is listed, re-unarchive is silent", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		if err := app.ArchiveThread(thread.ID); err != nil {
			t.Fatalf("ArchiveThread: %v", err)
		}
		broadcasts := captureThreadBroadcasts(t, app)

		restored, err := app.UnarchiveThread(thread.ID)
		if err != nil {
			t.Fatalf("UnarchiveThread: %v", err)
		}
		row := broadcasts.expectRow(triage.ThreadActionListed, thread.ID)
		if row.Archived {
			t.Fatal("broadcast row Archived = true, want false")
		}
		if restored.ID != thread.ID {
			t.Fatalf("UnarchiveThread returned %q, want %q", restored.ID, thread.ID)
		}

		broadcasts.reset()
		again, err := app.UnarchiveThread(thread.ID)
		if err != nil {
			t.Fatalf("UnarchiveThread(repeat): %v", err)
		}
		broadcasts.expectSilence("re-unarchiving an active thread")
		// The RPC still owes its caller a row even when nothing moved.
		if again.ID != thread.ID {
			t.Fatalf("UnarchiveThread(repeat) returned %q, want %q", again.ID, thread.ID)
		}
	})

	t.Run("pin, group move and unpin", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		broadcasts := captureThreadBroadcasts(t, app)

		if _, err := app.PinThread(thread.ID); err != nil {
			t.Fatalf("PinThread: %v", err)
		}
		if row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID); row.PinnedAt == nil {
			t.Fatal("broadcast row PinnedAt = nil, want a timestamp")
		}

		broadcasts.reset()
		if _, err := app.SetThreadPinGroup(thread.ID, store.PinGroupBack); err != nil {
			t.Fatalf("SetThreadPinGroup: %v", err)
		}
		row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID)
		if row.PinGroup == nil || *row.PinGroup != store.PinGroupBack {
			t.Fatalf("broadcast row PinGroup = %v, want %d", row.PinGroup, store.PinGroupBack)
		}

		broadcasts.reset()
		if _, err := app.SetThreadPinGroup(thread.ID, store.PinGroupBack); err != nil {
			t.Fatalf("SetThreadPinGroup(repeat): %v", err)
		}
		broadcasts.expectSilence("moving a thread to the group it is already in")

		broadcasts.reset()
		if _, err := app.UnpinThread(thread.ID); err != nil {
			t.Fatalf("UnpinThread: %v", err)
		}
		if row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID); row.PinnedAt != nil {
			t.Fatalf("broadcast row PinnedAt = %v, want nil", row.PinnedAt)
		}

		broadcasts.reset()
		if _, err := app.UnpinThread(thread.ID); err != nil {
			t.Fatalf("UnpinThread(repeat): %v", err)
		}
		broadcasts.expectSilence("unpinning an unpinned thread")
	})

	t.Run("mark unread then read", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		// A completed turn is what gives the read marker a target to clamp
		// to. Without one every stamp writes wall-clock now and therefore
		// always changes the row, so the no-op half of the contract is only
		// observable on a thread that has actually run.
		mustCompleteBroadcastTurn(t, app, thread.ID)
		broadcasts := captureThreadBroadcasts(t, app)

		if err := app.MarkThreadUnread(thread.ID); err != nil {
			t.Fatalf("MarkThreadUnread: %v", err)
		}
		row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID)
		if row.LastReadAt == nil || *row.LastReadAt != 0 {
			t.Fatalf("broadcast row LastReadAt = %v, want 0", row.LastReadAt)
		}

		broadcasts.reset()
		if err := app.MarkThreadUnread(thread.ID); err != nil {
			t.Fatalf("MarkThreadUnread(repeat): %v", err)
		}
		broadcasts.expectSilence("marking an already-unread thread unread")

		broadcasts.reset()
		if err := app.MarkThreadRead(thread.ID); err != nil {
			t.Fatalf("MarkThreadRead: %v", err)
		}
		row = broadcasts.expectRow(triage.ThreadActionFull, thread.ID)
		if row.LastReadAt == nil || *row.LastReadAt == 0 {
			t.Fatalf("broadcast row LastReadAt = %v, want a real timestamp", row.LastReadAt)
		}

		broadcasts.reset()
		if err := app.MarkThreadRead(thread.ID); err != nil {
			t.Fatalf("MarkThreadRead(repeat): %v", err)
		}
		broadcasts.expectSilence("re-reading a thread whose marker is already current")
	})

	t.Run("reasoning effort", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		broadcasts := captureThreadBroadcasts(t, app)

		target := "low"
		if seeded, err := app.store.GetThread(thread.ID); err != nil {
			t.Fatalf("GetThread: %v", err)
		} else if seeded.ReasoningEffort == target {
			target = "high"
		}
		updated, err := app.UpdateThreadReasoningEffort(thread.ID, target)
		if err != nil {
			t.Fatalf("UpdateThreadReasoningEffort: %v", err)
		}
		row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID)
		if row.ReasoningEffort != target {
			t.Fatalf("broadcast row ReasoningEffort = %q, want %q", row.ReasoningEffort, target)
		}
		// The broadcast row IS the row the RPC returned, which is what makes
		// the initiator's optimistic apply and its own echo the same state.
		if row.ReasoningEffort != updated.ReasoningEffort || row.ID != updated.ID {
			t.Fatalf("broadcast row %+v differs from the RPC's return %+v", row, updated)
		}

		broadcasts.reset()
		if _, err := app.UpdateThreadReasoningEffort(thread.ID, target); err != nil {
			t.Fatalf("UpdateThreadReasoningEffort(repeat): %v", err)
		}
		broadcasts.expectSilence("re-selecting the effort a thread already carries")
	})

	t.Run("fast mode", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread, err := createTestThread(t, app, "claude", t.TempDir(), "claude-opus-4-8", "")
		if err != nil {
			t.Fatalf("createTestThread: %v", err)
		}
		broadcasts := captureThreadBroadcasts(t, app)

		if _, err := app.UpdateThreadFastMode(thread.ID, false); err != nil {
			t.Fatalf("UpdateThreadFastMode(false): %v", err)
		}
		broadcasts.expectSilence("setting fast mode to the value a thread already carries")

		if _, err := app.UpdateThreadFastMode(thread.ID, true); err != nil {
			t.Fatalf("UpdateThreadFastMode(true): %v", err)
		}
		if row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID); !row.FastMode {
			t.Fatal("broadcast row FastMode = false, want true")
		}
	})

	t.Run("context window", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		before, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		broadcasts := captureThreadBroadcasts(t, app)

		if _, err := app.UpdateThreadContextWindow(thread.ID, before.ContextWindow); err != nil {
			t.Fatalf("UpdateThreadContextWindow(same): %v", err)
		}
		broadcasts.expectSilence("restating the context window a thread already carries")

		if _, err := app.UpdateThreadContextWindow(thread.ID, 1000000); err != nil {
			t.Fatalf("UpdateThreadContextWindow: %v", err)
		}
		if row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID); row.ContextWindow != 1000000 {
			t.Fatalf("broadcast row ContextWindow = %d, want 1000000", row.ContextWindow)
		}
	})

	t.Run("model selection", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		broadcasts := captureThreadBroadcasts(t, app)

		updated, err := app.UpdateThreadModel(thread.ID, "claude-opus-4-1")
		if err != nil {
			t.Fatalf("UpdateThreadModel: %v", err)
		}
		row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID)
		if row.Model != updated.Model {
			t.Fatalf("broadcast model = %q, want the RPC's %q", row.Model, updated.Model)
		}

		broadcasts.reset()
		if _, err := app.UpdateThreadModel(thread.ID, updated.Model); err != nil {
			t.Fatalf("UpdateThreadModel(repeat): %v", err)
		}
		broadcasts.expectSilence("re-selecting the model a thread already carries")
	})

	t.Run("delete announces every dropped row", func(t *testing.T) {
		app := newTestAppWithStore(t)
		parent := mustCreateBroadcastThread(t, app)
		child := mustCreateBroadcastThread(t, app)
		stored, err := app.store.GetThread(child.ID)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		stored.ParentThreadID = parent.ID
		if err := app.store.UpdateThread(stored); err != nil {
			t.Fatalf("UpdateThread: %v", err)
		}
		broadcasts := captureThreadBroadcasts(t, app)

		if err := app.DeleteThread(parent.ID); err != nil {
			t.Fatalf("DeleteThread: %v", err)
		}
		announced := map[string]bool{}
		for _, evt := range broadcasts.events {
			if evt.Action != triage.ThreadActionDeleted {
				t.Fatalf("action = %q, want %q", evt.Action, triage.ThreadActionDeleted)
			}
			if evt.Thread != nil {
				t.Fatalf("a deleted frame carried a row: %+v", evt.Thread)
			}
			announced[evt.ID] = true
		}
		for _, id := range []string{parent.ID, child.ID} {
			if !announced[id] {
				t.Fatalf("no deleted frame for %s; got %+v", id, broadcasts.events)
			}
		}

		broadcasts.reset()
		if err := app.DeleteThread(parent.ID); err != nil {
			t.Fatalf("DeleteThread(repeat): %v", err)
		}
		broadcasts.expectSilence("deleting a thread that is already gone")
	})
}

// TestThreadWorkspaceSwitchBroadcastsOnlyRealMoves keeps the workspace path on
// the same contract as the rest: `store.UpdateThread` rewrites the whole row,
// so the no-change test is the fields the switch owns rather than a
// rows-affected count.
func TestThreadWorkspaceSwitchBroadcastsOnlyRealMoves(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := initBroadcastRepo(t, app)
	thread := mustCreateBroadcastThreadIn(t, app, repo)
	broadcasts := captureThreadBroadcasts(t, app)

	if _, err := app.UpdateThreadWorkspace(thread.ID, repo); err != nil {
		t.Fatalf("UpdateThreadWorkspace(same): %v", err)
	}
	broadcasts.expectSilence("switching a thread to the workspace it already sits in")
}

// The sidebar's Plan ready pill is a DERIVED COLUMN of the thread row
// (threads.hasActionableProposedPlan), and since provider:item_event was
// narrowed to the threads a client watches, the row is the only way a
// client with no surface on this thread learns the pill moved. So every
// write that changes the derivation broadcasts the row — the in-turn
// persist (covered in triage), and these two App-side settles.
func TestProposedPlanStateChangesBroadcastTheThreadRow(t *testing.T) {
	t.Run("ensure-state settle raises the pill", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		seedBroadcastProposedPlan(t, app, thread.ID)
		broadcasts := captureThreadBroadcasts(t, app)

		if _, err := app.CreateProposedPlanComment(thread.ID, store.ProposedPlanCommentInput{
			PlanItemID: "plan-item", StartLine: 1, EndLine: 1, Body: "tighten this",
		}); err != nil {
			t.Fatalf("CreateProposedPlanComment: %v", err)
		}

		row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID)
		if !row.HasActionableProposedPlan {
			t.Fatal("broadcast row HasActionableProposedPlan = false, want true")
		}
	})

	t.Run("implementing the plan clears the pill", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := mustCreateBroadcastThread(t, app)
		seedBroadcastProposedPlan(t, app, thread.ID)
		if _, err := app.store.EnsureProposedPlanState(thread.ID, "plan-item", time.Now().UnixMilli()); err != nil {
			t.Fatalf("EnsureProposedPlanState: %v", err)
		}
		// A session with no provider: applyProposedPlanAcceptance runs
		// synchronously before sendToProvider, so the mark — and its
		// broadcast — are observable without a provider write.
		app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "no-provider"})
		broadcasts := captureThreadBroadcasts(t, app)

		if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Implement the plan.", SendMessageOptions{
			SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
		}); err == nil {
			t.Fatal("SendMessageWithOptions() error = nil, want session-has-no-provider failure")
		}

		var last *store.Thread
		for i := range broadcasts.events {
			evt := broadcasts.events[i]
			if evt.Action == triage.ThreadActionFull && evt.Thread != nil && evt.Thread.ID == thread.ID {
				last = evt.Thread
			}
		}
		if last == nil {
			t.Fatalf("no thread row broadcast for the implemented plan: %+v", broadcasts.events)
		}
		if last.HasActionableProposedPlan {
			t.Fatal("broadcast row HasActionableProposedPlan = true, want false (the plan was just implemented)")
		}
	})
}

func seedBroadcastProposedPlan(t *testing.T, app *App, threadID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID: "plan-item", ThreadID: threadID, TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed", Summary: "Plan",
		PayloadID: "plan-payload", ToolName: "plan", CreatedAt: now, UpdatedAt: now,
	}, store.Payload{
		ID:   "plan-payload",
		Kind: "proposed_plan",
		Meta: `{"title":"Plan","preview":"one","lineCount":1,"charCount":3}`,
		Data: []byte("# Plan"),

		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
}

func mustCompleteBroadcastTurn(t *testing.T, app *App, threadID string) {
	t.Helper()
	if err := app.store.InsertTurn(store.Turn{
		TurnID: threadID + "-turn-0", ThreadID: threadID, TurnIndex: 0, StartedAt: 1000,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if err := app.store.UpdateTurnCompleted(threadID+"-turn-0", 1500, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}
}

func mustCreateBroadcastThread(t *testing.T, app *App) store.Thread {
	t.Helper()
	return mustCreateBroadcastThreadIn(t, app, t.TempDir())
}

func mustCreateBroadcastThreadIn(t *testing.T, app *App, workspace string) store.Thread {
	t.Helper()
	thread, err := createTestThread(t, app, "claude", workspace, "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	return thread
}

func initBroadcastRepo(t *testing.T, app *App) string {
	t.Helper()
	repo := testutil.InitGitRepo(t)
	if _, err := app.ensureProjectForWorkspace(repo); err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	return repo
}

// TestGitBranchChangeBroadcastsTheThreadRow covers the sibling class the
// sweep found outside the thread bindings: the git paths persist a branch
// onto the row directly, so they carry the same broadcast obligation.
func TestGitBranchChangeBroadcastsTheThreadRow(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := initBroadcastRepo(t, app)
	testutil.RunGit(t, repo, "branch", "feature/broadcast")
	thread := mustCreateBroadcastThreadIn(t, app, repo)
	broadcasts := captureThreadBroadcasts(t, app)

	if _, err := app.GitCheckout(workspaceRefForThread(thread), "feature/broadcast"); err != nil {
		t.Fatalf("GitCheckout: %v", err)
	}
	row := broadcasts.expectRow(triage.ThreadActionFull, thread.ID)
	if row.Branch != "feature/broadcast" {
		t.Fatalf("broadcast Branch = %q, want feature/broadcast", row.Branch)
	}

	broadcasts.reset()
	if _, err := app.GitCheckout(workspaceRefForThread(thread), "feature/broadcast"); err != nil {
		t.Fatalf("GitCheckout(same branch): %v", err)
	}
	broadcasts.expectSilence("checking out the branch the thread is already on")
}

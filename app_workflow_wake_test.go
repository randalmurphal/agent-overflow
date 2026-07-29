package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// wakeHarness wires the two seams a wake crosses — the ordinary send and the
// flush queue — so a test can assert which one a delivery took without a
// provider process.
type wakeHarness struct {
	app        *App
	mu         sync.Mutex
	sends      []string
	queued     []string
	events     []string
	errorTexts []string
}

func newWakeHarness(t *testing.T) *wakeHarness {
	t.Helper()
	app, _ := setupE2EApp(t)
	h := &wakeHarness{app: app}
	app.sendMessageFn = func(threadID, content string, _ []string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.sends = append(h.sends, content)
		return nil
	}
	app.triage.SetFlushDispatcher(func(_ string, items []triage.QueuedFlushItem) {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, item := range items {
			h.queued = append(h.queued, item.Message)
		}
	})
	app.testEmitHook = func(name string, data any) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, name)
		if event, ok := data.(engine.ErrorEvent); ok {
			h.errorTexts = append(h.errorTexts, event.Error)
		}
	}
	app.configDir = t.TempDir()
	return h
}

func (h *wakeHarness) snapshot() (sends, queued, events, errorTexts []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sends...), append([]string(nil), h.queued...),
		append([]string(nil), h.events...), append([]string(nil), h.errorTexts...)
}

// drain waits for the app's wake worker, which runs off the engine's emit
// goroutine and therefore is not finished when the transition returns.
func (h *wakeHarness) drain() { h.app.workflowWake.Wait() }

func (h *wakeHarness) chatThread(t *testing.T, id string) store.Thread {
	t.Helper()
	thread := store.Thread{
		ID: id, ProjectID: defaultTestProjectID, ProjectPath: "/tmp/project",
		Title: id, Provider: string(provider.Claude), Model: "sonnet",
		Mode: threadmode.ModeChat, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := h.app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

func (h *wakeHarness) run(t *testing.T, id string, state engine.State, reason engine.Reason) store.WorkItem {
	t.Helper()
	item := store.WorkItem{
		ID: id, ProjectID: defaultTestProjectID, Goal: "Ship " + id,
		WorkflowID: "build", WorkflowScope: "shared", State: string(state),
		Reason: string(reason), Source: "manual",
		CreatedAt: time.Now().UnixMilli(), StartedAt: time.Now().UnixMilli(),
	}
	if err := h.app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func (h *wakeHarness) phase(t *testing.T, itemID, phaseID string, attempt int, status, threadID string, envelope json.RawMessage) {
	t.Helper()
	if err := h.app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: itemID, PhaseID: phaseID, Attempt: attempt, ThreadID: threadID,
		InputEnvelope: json.RawMessage(`{}`), OutputEnvelope: envelope,
		Status: status, StartedAt: 10, EndedAt: 11,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowWakeDeliversToAnIdleBoundThread(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-thread")
	item := h.run(t, "wake-done", engine.StateDone, "")
	h.phase(t, item.ID, "verify", 1, "completed", "phase-thread",
		json.RawMessage(`{"status":"done","outputs":{"ok":true}}`))
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	// A draft the user has typed but not sent must survive a background wake.
	if err := h.app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID: thread.ID, Content: "half-written thought", UpdatedAt: 5,
	}); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()

	sends, queued, _, _ := h.snapshot()
	if len(sends) != 1 || len(queued) != 0 {
		t.Fatalf("idle delivery took sends=%d queued=%d, want one ordinary send", len(sends), len(queued))
	}
	for _, want := range []string{`Run "wake-done"`, "is done", `Phase "verify"`, "nothing is waiting on a reply"} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("wake missing %q:\n%s", want, sends[0])
		}
	}
	draft, found, err := h.app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || draft.Content != "half-written thought" {
		t.Fatalf("wake destroyed the composer draft: found=%v draft=%+v", found, draft)
	}
}

func TestWorkflowWakeQueuesIntoABusyBoundThread(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-busy")
	h.app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "live"}
	item := h.run(t, "wake-question", engine.StateNeedsHuman, engine.ReasonQuestion)
	h.phase(t, item.ID, "plan", 1, "parked", "phase-thread",
		json.RawMessage(`{"status":"question","question":"Which base branch?"}`))
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, queued, _, _ := h.snapshot()
	if len(queued) != 1 || len(sends) != 0 {
		t.Fatalf("live-session delivery took sends=%d queued=%d, want the queued path", len(sends), len(queued))
	}
	for _, want := range []string{"needs-human (question)", "Which base branch?", "does not continue until this is resolved"} {
		if !strings.Contains(queued[0], want) {
			t.Fatalf("queued wake missing %q:\n%s", want, queued[0])
		}
	}
}

func TestWorkflowWakeFallsBackWhenTheBoundThreadIsGone(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-deleted")
	item := h.run(t, "wake-orphan", engine.StateFailed, engine.ReasonAgentError)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.app.store.DeleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateFailed,
	})
	h.drain()

	sends, queued, _, errorTexts := h.snapshot()
	if len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("a deleted thread still received a wake: sends=%d queued=%d", len(sends), len(queued))
	}
	// The fallback is loud: the binding is cleared and the run says so.
	stored, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OriginThreadID != "" {
		t.Fatalf("stale binding survived: %q", stored.OriginThreadID)
	}
	if len(errorTexts) == 0 || !strings.Contains(errorTexts[0], "bound thread is gone") {
		t.Fatalf("error events = %v, want the fallback reported", errorTexts)
	}
}

func TestWorkflowWakeIgnoresUnboundAndCalledRuns(t *testing.T) {
	h := newWakeHarness(t)
	unbound := h.run(t, "wake-unbound", engine.StateDone, "")
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: unbound.ID, ProjectID: unbound.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("an unbound run woke a thread: sends=%d queued=%d", len(sends), len(queued))
	}

	// A called run finishing is the caller's business, not a wake of its own.
	child := store.WorkItem{
		ID: "wake-child", ProjectID: defaultTestProjectID, Goal: "called", WorkflowID: "child",
		WorkflowScope: "shared", State: string(engine.StateDone), Source: "call",
		ParentItemID: unbound.ID, ParentPhaseID: "audit", ParentAttempt: 1, CallDepth: 1,
		CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("a called run woke a thread as itself: sends=%d queued=%d", len(sends), len(queued))
	}
}

// A grandchild parking while the root waits is announced at the ROOT — the run
// a human watches and the run that carries the binding.
func TestWorkflowDescendantParkSurfacesAtTheRoot(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-root")
	root := h.run(t, "tree-root", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	child := store.WorkItem{
		ID: "tree-child", ProjectID: defaultTestProjectID, Goal: "middle", WorkflowID: "mid",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "call-mid", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	grandchild := store.WorkItem{
		ID: "tree-grandchild", ProjectID: defaultTestProjectID, Goal: "deep", WorkflowID: "leaf",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion),
		Source: "call", ParentItemID: child.ID, ParentPhaseID: "call-leaf", ParentAttempt: 1,
		CallDepth: 2, CreatedAt: 3,
	}
	if err := h.app.store.CreateWorkItem(grandchild); err != nil {
		t.Fatal(err)
	}
	h.phase(t, grandchild.ID, "ask", 1, "parked", "leaf-thread",
		json.RawMessage(`{"status":"question","question":"Which environment?"}`))

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: grandchild.ID, ProjectID: grandchild.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("descendant park produced %d wakes, want exactly one at the root", len(sends))
	}
	for _, want := range []string{
		`Run "tree-root"`, "is waiting", "A called run 2 levels down parked",
		`run "tree-grandchild"`, "needs-human (question)", "Which environment?",
		`cannot continue until called run "tree-grandchild" is resolved`,
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("root wake missing %q:\n%s", want, sends[0])
		}
	}
}

// Once the root itself rests, its own transition is the surface; announcing the
// descendant again would be a duplicate.
func TestWorkflowDescendantParkIsSilentOnceTheRootRests(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-settled")
	root := h.run(t, "settled-root", engine.StateNeedsHuman, engine.ReasonChildFailed)
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	child := store.WorkItem{
		ID: "settled-child", ProjectID: defaultTestProjectID, Goal: "called", WorkflowID: "child",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonStuck),
		Source: "call", ParentItemID: root.ID, ParentPhaseID: "audit", ParentAttempt: 1,
		CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("descendant park duplicated the root's own surface: sends=%d queued=%d", len(sends), len(queued))
	}
}

// A descendant park is only actionable if the wake carries what the repair verb
// takes: the parked run's id, the waves between it and the root, and the failed
// units of THAT run rather than of the root.
func TestWorkflowDescendantParkCarriesTheChainAndItsOwnFailedUnits(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-campaign")
	root := h.run(t, "campaign-root", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	// The root has a failed unit of its own from an earlier wave. It must not be
	// reported as the parked run's, or a repair verb would be pointed at the
	// wrong run.
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: root.ID, PhaseID: "fan", Attempt: 1, UnitID: "root-unit", UnitIndex: 0,
		Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed, ThreadID: "root-unit-thread",
		Provider: string(provider.Claude), Model: "sonnet",
	}}); err != nil {
		t.Fatal(err)
	}
	parent := root
	for wave := 1; wave <= 2; wave++ {
		child := store.WorkItem{
			ID:            fmt.Sprintf("campaign-wave-%d", wave),
			ProjectID:     defaultTestProjectID,
			Goal:          fmt.Sprintf("wave %d", wave),
			WorkflowID:    "wave",
			WorkflowScope: "shared",
			State:         string(engine.StateRunning),
			Source:        engine.WorkItemSourceCall,
			ParentItemID:  parent.ID,
			ParentPhaseID: "advance",
			ParentAttempt: 1,
			CallDepth:     wave,
			CreatedAt:     int64(wave + 1),
		}
		if err := h.app.store.CreateWorkItem(child); err != nil {
			t.Fatal(err)
		}
		parent = child
	}
	parked := parent
	if err := h.app.store.UpdateWorkItemState(
		parked.ID, string(engine.StateNeedsHuman), string(engine.ReasonUnitFailed), 20,
	); err != nil {
		t.Fatal(err)
	}
	parked.State = string(engine.StateNeedsHuman)
	parked.Reason = string(engine.ReasonUnitFailed)
	h.phase(t, parked.ID, "fan", 1, "parked", "wave-thread",
		json.RawMessage(`{"status":"stuck","reason":"two units failed"}`))
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: parked.ID, PhaseID: "fan", Attempt: 1, UnitID: "pkg-engine", UnitIndex: 0,
		Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed, ThreadID: "wave-unit-thread",
		Provider: string(provider.Claude), Model: "sonnet",
	}}); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: parked.ID, ProjectID: parked.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("descendant park produced %d wakes, want one", len(sends))
	}
	for _, want := range []string{
		`Call chain: "campaign-root" → "campaign-wave-1" → "campaign-wave-2".`,
		`- "called run": "campaign-wave-2"`,
		`- "called run failed unit": "pkg-engine (thread wave-unit-thread)"`,
		`act on run "campaign-wave-2", not on "campaign-root"`,
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("campaign wake missing %q:\n%s", want, sends[0])
		}
	}
	if strings.Contains(sends[0], "root-unit") {
		t.Fatalf("campaign wake reported the root's failed unit as the parked run's:\n%s", sends[0])
	}
}

// The one park that is not a fault: the stop a human asked for. It must not read
// like every other one, at either level of the tree.
func TestWorkflowCheckpointParkReadsAsTheStopThatWasAskedFor(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-checkpoint")
	root := h.run(t, "checkpoint-root", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	child := store.WorkItem{
		ID: "checkpoint-wave", ProjectID: defaultTestProjectID, Goal: "wave 2", WorkflowID: "wave",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonCheckpoint), Source: engine.WorkItemSourceCall,
		ParentItemID: root.ID, ParentPhaseID: "advance", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("checkpoint park produced %d wakes, want one", len(sends))
	}
	for _, want := range []string{
		"needs-human (checkpoint)",
		`run "checkpoint-wave" reached the checkpoint and did not start the next one`,
		// The literal command, against the DESCENDANT's id: the run to act on is
		// one the reader has never seen, and "resume" is one of four control
		// verbs it could otherwise be mapped onto.
		"`agent-overflow run resume \"checkpoint-wave\"` takes the call it skipped, or leave it parked.",
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("checkpoint wake missing %q:\n%s", want, sends[0])
		}
	}
	if strings.Contains(sends[0], "cannot continue until called run") {
		t.Fatalf("checkpoint wake read as a failure:\n%s", sends[0])
	}
}

func TestWorkflowWakeCarriesNarrativeArtifactAndFailedUnitReferences(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-refs")
	item := h.run(t, "wake-refs", engine.StateNeedsHuman, engine.ReasonUnitFailed)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "fan", 1, "parked", "join-thread", json.RawMessage(`{"status":"stuck","reason":"a unit failed"}`))
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: item.ID, PhaseID: "fan", Attempt: 1, UnitID: "pkg-store", UnitIndex: 0,
		Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed,
		ThreadID: "unit-thread", Provider: string(provider.Claude), Model: "sonnet",
	}}); err != nil {
		t.Fatal(err)
	}
	narrative, err := workflowrunner.NarrativePath(h.app.workflowDataRoot(), item.ID, "fan", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(narrative), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(narrative, []byte("what happened"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wakes = %d, want one", len(sends))
	}
	for _, want := range []string{
		`- "narrative":`, `- "phase thread": "join-thread"`,
		`- "failed unit": "pkg-store (thread unit-thread)"`, "a unit failed",
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("wake missing %q:\n%s", want, sends[0])
		}
	}
	// References are pointers, never content.
	if strings.Contains(sends[0], "what happened") {
		t.Fatalf("wake inlined narrative content:\n%s", sends[0])
	}
}

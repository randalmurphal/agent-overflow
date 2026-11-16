package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

// bootStepsHeldFor is how long a test lets a held port run before asserting
// it has done nothing.
const bootStepsHeldFor = 200 * time.Millisecond

// holdBootSteps puts a the state Start leaves it in until the boot's steps
// before clients have run, and returns the call that finishes them.
func holdBootSteps(t *testing.T, a *App) (finish func()) {
	t.Helper()
	a.activation.beginBoot()
	return func() {
		t.Helper()
		noop := []bootStep{func() error { return nil }}
		if err := a.activation.Run(context.Background(), noop, noop, func(err error) { t.Errorf("report: %v", err) }); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
}

// The workflow engine rebuilds before a trial's commit, and the wakes its
// transitions owe start provider turns. Both wake paths, the ordinary send to
// an idle thread and the queue into a busy one, wait for the boot's steps.
func TestWorkflowWakesWaitForTheBootStepsBeforeClients(t *testing.T) {
	t.Parallel()
	h := newWakeHarness(t)
	idle := h.chatThread(t, "origin-idle")
	busy := h.chatThread(t, "origin-busy")
	h.app.sessionManager().put(busy.ID, session{Provider: string(provider.Claude), Token: "live"})
	done := h.run(t, "wake-done", engine.StateDone, "")
	h.phase(t, done.ID, "verify", 1, "completed", "phase-thread", json.RawMessage(`{"status":"done","outputs":{"ok":true}}`))
	question := h.run(t, "wake-question", engine.StateNeedsHuman, engine.ReasonQuestion)
	h.phase(t, question.ID, "plan", 1, "parked", "phase-thread", json.RawMessage(`{"status":"question","question":"Which base branch?"}`))
	for item, thread := range map[string]string{done.ID: idle.ID, question.ID: busy.ID} {
		if _, err := h.app.store.UpdateWorkItemOriginThread(item, thread); err != nil {
			t.Fatal(err)
		}
	}
	finish := holdBootSteps(t, h.app)

	h.app.afterWorkflowStateEvent(engine.StateEvent{ItemID: done.ID, ProjectID: done.ProjectID, From: engine.StateRunning, To: engine.StateDone})
	h.app.afterWorkflowStateEvent(engine.StateEvent{ItemID: question.ID, ProjectID: question.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman})
	time.Sleep(bootStepsHeldFor)
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("before the boot steps: sends=%d queued=%d, want neither", len(sends), len(queued))
	}

	finish()
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 1 || len(queued) != 1 {
		t.Fatalf("after the boot steps: sends=%d queued=%d, want one of each", len(sends), len(queued))
	}
}

// A run the rebuild leaves needing a person raises a banner, which a
// rolled-back trial cannot take back.
func TestWorkflowNotificationsWaitForTheBootStepsBeforeClients(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	sender := &recordingWorkflowNotificationSender{wake: make(chan struct{}, 4)}
	app.osNotifications = sender
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	item := store.WorkItem{
		ID: "needs-item", ProjectID: projectRow.ID, Goal: "Choose deployment target",
		WorkflowID: "wf", WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonQuestion), Source: "manual", CreatedAt: 1, EndedAt: 2,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	finish := holdBootSteps(t, app)

	emitter := workflowEmitter{app: app, emit: func(eventchan.Channel, any) {}}
	emitter.Emit("workflow:item-state", engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman, Reason: engine.ReasonQuestion,
	})
	select {
	case <-sender.wake:
		t.Fatalf("notified before the boot steps: %+v", sender.snapshot())
	case <-time.After(bootStepsHeldFor):
	}

	finish()
	select {
	case <-sender.wake:
	case <-time.After(10 * time.Second):
		t.Fatal("no notification after the boot steps")
	}
}

// A push reaches the network.
func TestPushWaitsForTheBootStepsBeforeClients(t *testing.T) {
	t.Parallel()
	app, sender := pushApp(t)
	pairPhone(t, app, "thumb-phone", "token-phone")
	finish := holdBootSteps(t, app)

	app.pushFanout(turnCompleteSend())
	time.Sleep(bootStepsHeldFor)
	if messages := sender.messages(); len(messages) != 0 {
		t.Fatalf("pushed before the boot steps: %+v", messages)
	}

	finish()
	app.push.queue.Wait()
	if messages := sender.messages(); len(messages) != 1 {
		t.Fatalf("pushed after the boot steps = %d messages, want 1", len(messages))
	}
}

// An auto-disposition merges, pushes or opens a pull request, and a digest
// upgrade is a provider call. Neither starts before the boot's steps. The run
// named here does not exist, so once released each fails on its first read.
func TestWorkflowDispositionAndDigestWaitForTheBootStepsBeforeClients(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	finish := holdBootSteps(t, app)

	disposed := make(chan struct{})
	go func() {
		app.autoDisposeWorkflowItem("missing-run")
		close(disposed)
	}()
	digested := make(chan error, 1)
	go func() {
		_, err := app.generateWorkflowDigest(store.WorkItem{ID: "missing-run", ProjectID: "missing-project"}, WorkflowDigest{})
		digested <- err
	}()
	select {
	case <-disposed:
		t.Fatal("auto-disposition ran before the boot steps")
	case err := <-digested:
		t.Fatalf("digest generation ran before the boot steps: %v", err)
	case <-time.After(bootStepsHeldFor):
	}

	finish()
	select {
	case <-disposed:
	case <-time.After(10 * time.Second):
		t.Fatal("auto-disposition still waiting after the boot steps")
	}
	select {
	case err := <-digested:
		if err == nil {
			t.Fatal("digest for a missing project succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("digest generation still waiting after the boot steps")
	}
}

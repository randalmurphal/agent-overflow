package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowapp"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedWorkflowNotification struct {
	Title  string
	Body   string
	Target notify.Target
}

type recordingWorkflowNotificationSender struct {
	mu      sync.Mutex
	records []recordedWorkflowNotification
	wake    chan struct{}
}

func (s *recordingWorkflowNotificationSender) send(title, body string, target notify.Target) error {
	s.mu.Lock()
	s.records = append(s.records, recordedWorkflowNotification{Title: title, Body: body, Target: target})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *recordingWorkflowNotificationSender) snapshot() []recordedWorkflowNotification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedWorkflowNotification(nil), s.records...)
}

func TestWorkflowStateEmitterPersistsTemplateAndSendsTypedNotifications(t *testing.T) {
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
	if err := app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "plan", Attempt: 1, Status: "parked", StartedAt: 1, EndedAt: 2,
		OutputEnvelope: json.RawMessage(`{"status":"question","outputs":null,"question":"Deploy to staging or production?","reason":null}`),
	}); err != nil {
		t.Fatal(err)
	}
	emitter := workflowEmitter{app: app, emit: func(eventchan.Channel, any) {}}
	emitter.Emit("workflow:item-state", engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman, Reason: engine.ReasonQuestion,
	})

	select {
	case <-sender.wake:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for workflow notifications")
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var digest WorkflowDigest
	if err := json.Unmarshal(stored.Digest, &digest); err != nil {
		t.Fatal(err)
	}
	if digest.WhatItNeeds != "Deploy to staging or production?" {
		t.Fatalf("digest = %+v", digest)
	}
	records := sender.snapshot()
	if len(records) != 1 || records[0].Target.Kind != "workflow-item" ||
		records[0].Target.WorkItemID != item.ID || records[0].Body != digest.WhatItNeeds {
		t.Fatalf("notifications = %+v", records)
	}
}

// TestDoneItemSendsNoNotification pins the rev-2 notification surface: the
// coalesced drain summary died with the queue, so a run reaching done is
// silent — only needs-human and failed interrupt the user.
func TestDoneItemSendsNoNotification(t *testing.T) {
	app, _ := setupE2EApp(t)
	sender := &recordingWorkflowNotificationSender{wake: make(chan struct{}, 2)}
	app.osNotifications = sender
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	item := store.WorkItem{
		ID: "done-item", ProjectID: projectRow.ID, Goal: "Done", WorkflowID: "wf", WorkflowScope: "shared",
		State: string(engine.StateDone), Source: "manual", CreatedAt: 1, EndedAt: 2,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	workflowEmitter{app: app, emit: func(eventchan.Channel, any) {}}.Emit(
		"workflow:item-state", engine.StateEvent{ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone},
	)
	select {
	case <-sender.wake:
		t.Fatalf("done item sent a notification: %+v", sender.snapshot())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWorkflowTemplateDigestUsesCheckAndStuckInputs(t *testing.T) {
	failed := workflowTemplateDigest(store.WorkItem{
		State: string(engine.StateFailed), Reason: string(engine.ReasonCheckFailedGenuine),
	}, "verify", nil, "go-test")
	if failed.WhatItNeeds != "Investigate the failed checks: go-test." {
		t.Fatalf("failed digest = %+v", failed)
	}
	stuck := workflowTemplateDigest(store.WorkItem{
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonStuck),
	}, "build", json.RawMessage(`{"reason":"registry unavailable"}`), "")
	if stuck.WhatHappened != "The run is stuck in build: registry unavailable." {
		t.Fatalf("stuck digest = %+v", stuck)
	}
}

func TestWorkflowTemplateDigestExplainsProviderUsageRecovery(t *testing.T) {
	digest := workflowTemplateDigest(store.WorkItem{
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonProviderUsageLimited),
	}, "implement", nil, "")
	if digest.WhatHappened != "The run paused in implement because the provider account reached its usage limit." {
		t.Fatalf("usage-limit digest happened = %q", digest.WhatHappened)
	}
	if digest.WhatItNeeds != "Wait for the provider usage limit to reset or switch provider accounts, then resume the run." {
		t.Fatalf("usage-limit digest need = %q", digest.WhatItNeeds)
	}
}

func TestWorkflowDigestAsyncUpgradePersistsAndReemits(t *testing.T) {
	app, events := setupE2EApp(t)
	app.testEmitHook = events.emit
	bus := transport.NewEventBus(16)
	app.SetEventBus(bus)
	t.Cleanup(bus.Close)
	app.osNotifications = &recordingWorkflowNotificationSender{wake: make(chan struct{}, 4)}
	called := make(chan struct{}, 1)
	app.workflowApplication().ConfigureDigestGeneratorForTesting(func(_ context.Context, _ store.WorkItem, _ workflowapp.Digest) (workflowapp.Digest, error) {
		called <- struct{}{}
		return workflowapp.Digest{WhatHappened: "Generated happened.", WhatItNeeds: "Generated need."}, nil
	})
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	item := store.WorkItem{
		ID: "upgrade-item", ProjectID: projectRow.ID, Goal: "Upgrade", WorkflowID: "wf", WorkflowScope: "shared",
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonGate), Source: "manual", CreatedAt: 1, EndedAt: 2,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "review", Attempt: 1, Status: "parked", StartedAt: 1, EndedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	workflowEmitter{app: app, emit: app.emitWithReplay()}.Emit(
		"workflow:item-state", engine.StateEvent{
			ItemID: item.ID, ProjectID: item.ProjectID,
			From: engine.StateRunning, To: engine.StateNeedsHuman, Reason: engine.ReasonGate,
		},
	)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("async digest generator was not triggered")
	}
	deadline := time.Now().Add(time.Second)
	for {
		event := events.nextOfKind(t, "workflow:item-state", time.Until(deadline))
		state, ok := event.Data.(engine.StateEvent)
		if ok && state.ItemID == item.ID && state.From == engine.StateNeedsHuman && state.To == engine.StateNeedsHuman {
			break
		}
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var digest WorkflowDigest
	if err := json.Unmarshal(stored.Digest, &digest); err != nil {
		t.Fatal(err)
	}
	if digest.WhatHappened != "Generated happened." || digest.WhatItNeeds != "Generated need." {
		t.Fatalf("upgraded digest = %+v", digest)
	}
}

func TestWorkflowDigestAsyncFailureAndStaleResultKeepTemplate(t *testing.T) {
	for _, test := range []struct {
		name  string
		stale bool
	}{
		{name: "generator failure"},
		{name: "stale result", stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, events := setupE2EApp(t)
			app.testEmitHook = events.emit
			bus := transport.NewEventBus(16)
			app.SetEventBus(bus)
			t.Cleanup(bus.Close)
			app.osNotifications = &recordingWorkflowNotificationSender{wake: make(chan struct{}, 4)}
			called := make(chan struct{}, 1)
			release := make(chan struct{})
			app.workflowApplication().ConfigureDigestGeneratorForTesting(func(_ context.Context, _ store.WorkItem, _ workflowapp.Digest) (workflowapp.Digest, error) {
				called <- struct{}{}
				if test.stale {
					<-release
					return workflowapp.Digest{WhatHappened: "Stale happened.", WhatItNeeds: "Stale need."}, nil
				}
				return workflowapp.Digest{}, errors.New("generator unavailable")
			})
			projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
			item := store.WorkItem{
				ID: "digest-" + test.name, ProjectID: projectRow.ID, Goal: "Keep template", WorkflowID: "wf", WorkflowScope: "shared",
				State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonGate), Source: "manual", CreatedAt: 1, EndedAt: 2,
			}
			if err := app.store.CreateWorkItem(item); err != nil {
				t.Fatal(err)
			}
			workflowEmitter{app: app, emit: app.emitWithReplay()}.Emit(
				"workflow:item-state", engine.StateEvent{ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman, Reason: engine.ReasonGate},
			)
			select {
			case <-called:
			case <-time.After(time.Second):
				t.Fatal("async digest generator was not called")
			}
			before, err := app.store.GetWorkItem(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.stale {
				if err := app.store.UpdateWorkItemState(item.ID, string(engine.StateRunning), "", 0); err != nil {
					t.Fatal(err)
				}
				close(release)
			}
			deadline := time.Now().Add(time.Second)
			for !app.workflowApplication().DigestUpgradesIdle() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			after, err := app.store.GetWorkItem(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if string(after.Digest) != string(before.Digest) {
				t.Fatalf("async failure/stale result replaced template: before=%s after=%s", before.Digest, after.Digest)
			}
			for _, event := range events.allEvents() {
				state, ok := event.Data.(engine.StateEvent)
				if event.Name == "workflow:item-state" && ok && state.ItemID == item.ID && state.From == engine.StateNeedsHuman && state.To == engine.StateNeedsHuman {
					t.Fatal("async failure/stale result emitted a refresh")
				}
			}
		})
	}
}

// The app half of a `notify:` route (K1): the engine says a decorated gate was
// taken, and the run's bound thread hears about it without anything parking.

func (h *wakeHarness) notify(event engine.NotifyEvent) {
	h.app.afterWorkflowEngineEvent("workflow:gate-notify", event)
	h.drain()
}

func waveNotify(itemID string, attempt int) engine.NotifyEvent {
	return engine.NotifyEvent{
		ItemID: itemID, ProjectID: defaultTestProjectID,
		PhaseID: "wave", Attempt: attempt,
		Decision: string(def.DecisionLoop), Target: "wave",
	}
}

func TestWorkflowGateNotifyWakesTheBoundThreadWithoutParking(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-progress")
	item := h.boundRun(t, "progress-run", thread.ID, engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemWorkspace(item.ID, "/work/progress", "ao/progress", "main"); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{"status": "done", "outputs": map[string]any{
		"verdict": "pass", "landed": "D40-D50",
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "wave", 12, "completed", "phase-thread", encoded)

	h.notify(waveNotify(item.ID, 12))

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("progress wakes = %d, want one", len(sends))
	}
	for _, want := range []string{
		`Run "progress-run" (workflow "build") is running`,
		`This run finished phase "wave" (attempt 12) and continued: the gate chose "loop" to phase "wave".`,
		`Workspace: "/work/progress" on branch "ao/progress".`,
		`- "verdict": "pass"`,
		`- "landed": "D40-D50"`,
		"Nothing is waiting on a reply — the run is still going.",
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("progress wake missing %q:\n%s", want, sends[0])
		}
	}
	// The run is untouched: a progress wake reports, it never parks.
	stored, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateRunning) || stored.Reason != "" {
		t.Fatalf("progress wake moved the run: state=%q reason=%q", stored.State, stored.Reason)
	}
}

// Every wave of a loop is its own news. Coalescing must separate them, or the
// mechanism built to report progress would report the first lap and go quiet.
func TestWorkflowGateNotifyCoalescesPerTraversalNotPerRoute(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-waves")
	item := h.boundRun(t, "waves-run", thread.ID, engine.StateRunning, "")
	for attempt := 1; attempt <= 3; attempt++ {
		h.phase(t, item.ID, "wave", attempt, "completed", "phase-thread",
			json.RawMessage(`{"status":"done","outputs":{"verdict":"pass"}}`))
	}

	h.notify(waveNotify(item.ID, 1))
	h.notify(waveNotify(item.ID, 2))
	// The same traversal announced twice — a replay, not a wave — is one ask.
	h.notify(waveNotify(item.ID, 2))
	h.notify(waveNotify(item.ID, 3))

	sends, _, _, _ := h.snapshot()
	if len(sends) != 3 {
		t.Fatalf("progress wakes = %d, want one per traversal:\n%s", len(sends), strings.Join(sends, "\n---\n"))
	}
	for index, attempt := range []int{1, 2, 3} {
		if !strings.Contains(sends[index], phraseForAttempt(attempt)) {
			t.Fatalf("wake %d does not report attempt %d:\n%s", index, attempt, sends[index])
		}
	}
}

func phraseForAttempt(attempt int) string {
	return `phase "wave" (attempt ` + string(rune('0'+attempt)) + `)`
}

// A called run's notify surfaces as the ROOT's progress wake, naming the
// descendant — the same rule a descendant's park follows, for the same reason:
// only a root binds a thread.
func TestWorkflowGateNotifyFromACalledRunSurfacesAtTheRoot(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-campaign-progress")
	root := h.boundRun(t, "campaign-root", thread.ID, engine.StateRunning, "")
	child := store.WorkItem{
		ID: "campaign-wave-2", ProjectID: defaultTestProjectID, Goal: "wave 2", WorkflowID: "wave",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: engine.WorkItemSourceCall,
		ParentItemID: root.ID, ParentPhaseID: "advance", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	h.phase(t, child.ID, "review", 1, "completed", "wave-thread",
		json.RawMessage(`{"status":"done","outputs":{"verdict":"pass"}}`))

	h.notify(engine.NotifyEvent{
		ItemID: child.ID, ProjectID: child.ProjectID, PhaseID: "review", Attempt: 1,
		Decision: string(def.DecisionAdvance), Target: "fix",
	})

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("progress wakes = %d, want one at the root", len(sends))
	}
	for _, want := range []string{
		`Run "campaign-root" (workflow "build") is running`,
		`A called run one level down (run "campaign-wave-2", workflow "wave") finished phase "review" (attempt 1) and continued: the gate chose "advance" to phase "fix".`,
		`Call chain: "campaign-root" → "campaign-wave-2".`,
		`- "verdict": "pass"`,
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("root progress wake missing %q:\n%s", want, sends[0])
		}
	}
}

// An unbound run has no thread to wake, and progress is not an interruption:
// notify is simply inert there rather than reaching for the OS notification a
// run that needs a human gets.
func TestWorkflowGateNotifyIsInertForAnUnboundRun(t *testing.T) {
	h := newWakeHarness(t)
	item := h.run(t, "unbound-progress", engine.StateRunning, "")
	h.phase(t, item.ID, "wave", 1, "completed", "phase-thread",
		json.RawMessage(`{"status":"done","outputs":{"verdict":"pass"}}`))

	h.notify(waveNotify(item.ID, 1))

	sends, queued, events, _ := h.snapshot()
	if len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("an unbound run was woken: sends=%d queued=%d", len(sends), len(queued))
	}
	for _, name := range events {
		if strings.HasPrefix(name, "notification:") {
			t.Fatalf("progress rang the desktop: %s", name)
		}
	}
}

// A progress wake never delays or fails the run it reports on: a record it
// cannot read costs the outputs, not the message.
func TestWorkflowGateNotifyStillReportsWhenTheAttemptIsUnreadable(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-unreadable")
	item := h.boundRun(t, "unreadable-run", thread.ID, engine.StateRunning, "")
	// No phase row at all: the notify names an attempt this store has never
	// seen, which is what a pruned or racing record looks like.
	h.notify(waveNotify(item.ID, 7))

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("progress wakes = %d, want the traversal reported anyway", len(sends))
	}
	if strings.Contains(sends[0], "What it produced") {
		t.Fatalf("wake invented outputs for an attempt it could not read:\n%s", sends[0])
	}
	if !strings.Contains(sends[0], `finished phase "wave" (attempt 7)`) {
		t.Fatalf("wake dropped the fact it did have:\n%s", sends[0])
	}
}

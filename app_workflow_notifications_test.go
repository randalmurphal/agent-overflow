package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/engine"
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
	app.generateWorkflowDigestFn = func(_ context.Context, _ store.WorkItem, _ WorkflowDigest) (WorkflowDigest, error) {
		called <- struct{}{}
		return WorkflowDigest{WhatHappened: "Generated happened.", WhatItNeeds: "Generated need."}, nil
	}
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
			app.generateWorkflowDigestFn = func(_ context.Context, _ store.WorkItem, _ WorkflowDigest) (WorkflowDigest, error) {
				called <- struct{}{}
				if test.stale {
					<-release
					return WorkflowDigest{WhatHappened: "Stale happened.", WhatItNeeds: "Stale need."}, nil
				}
				return WorkflowDigest{}, errors.New("generator unavailable")
			}
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
			for len(app.workflowDigestSlots) != 0 && time.Now().Before(deadline) {
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

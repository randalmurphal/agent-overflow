package main

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

func TestWorkflowUpdateProjectQueuePersistsAndUpdatesLiveState(t *testing.T) {
	app, events := setupE2EApp(t)
	app.testEmitHook = events.emit
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	seedPaused := true
	seedConcurrency := 2
	if _, err := app.store.UpdateProjectWorkflowQueue(projectRow.ID, &seedPaused, &seedConcurrency); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
	assertAppProjectQueueEvent(t, events, projectRow.ID, true, 2)

	paused := false
	concurrency := 3
	if err := app.WorkflowUpdateProjectQueue(projectRow.ID, &paused, &concurrency); err != nil {
		t.Fatal(err)
	}
	stored, err := app.store.GetProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkflowQueuePaused || stored.WorkflowConcurrency != 3 {
		t.Fatalf("persisted project queue = paused %v concurrency %d", stored.WorkflowQueuePaused, stored.WorkflowConcurrency)
	}
	assertAppProjectQueueEvent(t, events, projectRow.ID, false, 3)
}

func TestWorkflowUpdateProjectQueueRejectsUnknownArchivedAndEmptyUpdates(t *testing.T) {
	app, _ := setupE2EApp(t)
	active := testutil.EnsureProject(t, app.store, t.TempDir())
	archived := store.Project{
		ID: "archived-queue", Path: t.TempDir(), Name: "Archived Queue",
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(), Archived: true,
	}
	if err := app.store.CreateProject(archived); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })

	paused := true
	if err := app.WorkflowUpdateProjectQueue("missing", &paused, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown project error = %v, want sql.ErrNoRows", err)
	}
	if err := app.WorkflowUpdateProjectQueue(archived.ID, &paused, nil); err == nil {
		t.Fatal("archived project update unexpectedly succeeded")
	}
	if err := app.WorkflowUpdateProjectQueue(active.ID, nil, nil); err == nil {
		t.Fatal("empty project queue update unexpectedly succeeded")
	}
}

func assertAppProjectQueueEvent(t *testing.T, events *capturedEventBus, projectID string, paused bool, concurrency int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, emitted := range events.allEvents() {
			queue, ok := emitted.Data.(engine.QueueEvent)
			if emitted.Name != "workflow:queue-state" || !ok {
				continue
			}
			for _, project := range queue.Projects {
				if project.ProjectID == projectID && project.Paused == paused && project.Concurrency == concurrency {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("project queue event %s paused=%v concurrency=%d not emitted: %+v", projectID, paused, concurrency, events.allEvents())
}

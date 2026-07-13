package engine

import (
	"errors"
	"testing"

	"agent-overflow/internal/workflow/def"
)

func TestProjectQueuePauseCapacityAndStableResumeOrder(t *testing.T) {
	workflow := onePhaseWorkflow("project-queue", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{
		Active: false, GlobalConcurrency: 2,
		ProjectQueues: []ProjectQueueConfig{
			{ProjectID: "a", Paused: true, Concurrency: 1},
			{ProjectID: "b"},
		},
	}, map[string]def.Workflow{"project-queue": workflow}, []string{"a", "b"}, nil)

	for _, item := range []struct {
		id, project string
		position    int
	}{
		{id: "a-first", project: "a", position: 0},
		{id: "b-first", project: "b", position: 1},
		{id: "a-second", project: "a", position: 2},
	} {
		if err := h.engine.Enqueue(testItem(item.id, item.project, "project-queue", item.position)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.engine.SetQueue(true, 0, 2); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "b-first" {
		t.Fatalf("starts while project a paused = %+v, want only b-first", starts)
	}

	paused := false
	if err := h.engine.UpdateProjectQueueSettings("a", &paused, 1); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != "a-first" {
		t.Fatalf("starts after project a resume = %+v, want a-first next", starts)
	}

	h.runner.complete(t, "b-first", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := len(h.runner.started()); got != 2 {
		t.Fatalf("project a cap admitted a-second while a-first ran: %d starts", got)
	}
	h.runner.complete(t, "a-first", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != "a-second" {
		t.Fatalf("project a stable resume order = %+v, want a-second last", starts)
	}
}

func TestProjectRunningCountReleasesOnParkAndStartupFailure(t *testing.T) {
	workflow := onePhaseWorkflow("project-count", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{
		Active: true, GlobalConcurrency: 2,
		ProjectQueues: []ProjectQueueConfig{{ProjectID: "project", Concurrency: 1}},
	}, map[string]def.Workflow{"project-count": workflow}, []string{"project"}, nil)

	question := testItem("question", "project", "project-count", 0)
	if err := h.engine.Enqueue(question); err != nil {
		t.Fatal(err)
	}
	assertProjectRunningCount(t, h, "project", 1)
	h.runner.complete(t, question.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	assertProjectRunningCount(t, h, "project", 0)

	h.runner.mu.Lock()
	h.runner.startErrs["setup-failure"] = errors.New("provisioning failed")
	h.runner.mu.Unlock()
	failure := testItem("setup-failure", "project", "project-count", 1)
	if err := h.engine.Enqueue(failure); err == nil {
		t.Fatal("runner startup failure unexpectedly succeeded")
	}
	requireItemState(t, h.store, failure.ID, StateNeedsHuman, ReasonAgentError)
	assertProjectRunningCount(t, h, "project", 0)
}

func TestProjectQueueSettingsValidation(t *testing.T) {
	workflow := onePhaseWorkflow("validation", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"validation": workflow}, []string{"project"}, nil)
	if err := h.engine.UpdateProjectQueueSettings("", nil, 0); err == nil {
		t.Fatal("empty project id unexpectedly accepted")
	}
	if err := h.engine.UpdateProjectQueueSettings("project", nil, MaxProjectConcurrency+1); err == nil {
		t.Fatal("out-of-range project concurrency unexpectedly accepted")
	}
}

func assertProjectRunningCount(t *testing.T, h *testHarness, projectID string, want int) {
	t.Helper()
	events := h.emitter.queueEvents()
	if len(events) == 0 {
		t.Fatal("no queue events emitted")
	}
	for _, project := range events[len(events)-1].Projects {
		if project.ProjectID == projectID {
			if project.RunningCount != want {
				t.Fatalf("project %s running count = %d, want %d", projectID, project.RunningCount, want)
			}
			return
		}
	}
	t.Fatalf("project %s absent from queue event %+v", projectID, events[len(events)-1])
}

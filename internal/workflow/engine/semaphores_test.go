package engine

import (
	"errors"
	"fmt"
	"testing"

	"agent-overflow/internal/workflow/def"
)

func TestSemaphoreReleaseOnEveryImplementedExitPath(t *testing.T) {
	tests := []struct {
		name       string
		routes     []def.Route
		trigger    func(*testing.T, *testHarness, string)
		wantState  State
		wantReason Reason
	}{
		{"success", []def.Route{{To: "done"}}, completeWith(OutcomeDone, doneEnvelope(true)), StateDone, ""},
		{"question", []def.Route{{To: "done"}}, completeWith(OutcomeQuestion, questionEnvelope()), StateNeedsHuman, ReasonQuestion},
		{"stuck", []def.Route{{To: "done"}}, completeWith(OutcomeStuck, stuckEnvelope()), StateNeedsHuman, ReasonStuck},
		{"stalled", []def.Route{{To: "done"}}, completeWith(OutcomeStalled, nil), StateNeedsHuman, ReasonStalled},
		{"transient retries exhausted", []def.Route{{To: "done"}}, completeWith(OutcomeTransientExhausted, nil), StateNeedsHuman, ReasonRetriesExhausted},
		{"execution failure", []def.Route{{To: "done"}}, completeWith(OutcomeExecutionFailure, nil), StateNeedsHuman, ReasonAgentError},
		{"runner stopped", []def.Route{{To: "done"}}, completeWith(OutcomeStopped, nil), StateNeedsHuman, ReasonInterrupted},
		{"unknown outcome", []def.Route{{To: "done"}}, completeWith(OutcomeKind("unknown"), nil), StateNeedsHuman, ReasonAgentError},
		{"evaluator error", []def.Route{{When: &def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: true}, Exists: "work.ok"}, To: "done"}}, completeWith(OutcomeDone, doneEnvelope(true)), StateNeedsHuman, ReasonWiringError},
		{"human park", []def.Route{{Human: &def.HumanRoute{Approve: "done"}}}, completeWith(OutcomeDone, doneEnvelope(true)), StateNeedsHuman, ReasonGate},
		{"explicit park", []def.Route{{Park: "review"}}, completeWith(OutcomeDone, doneEnvelope(true)), StateNeedsHuman, ReasonGate},
		{"failed route", []def.Route{{To: "failed"}}, completeWith(OutcomeDone, doneEnvelope(true)), StateFailed, ReasonCheckFailedGenuine},
		{"wiring no-match", []def.Route{{When: &def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: true}}, To: "done"}}, completeWith(OutcomeDone, doneEnvelope(false)), StateNeedsHuman, ReasonWiringError},
		{"cancel", []def.Route{{To: "done"}}, func(t *testing.T, h *testHarness, itemID string) {
			t.Helper()
			if err := h.engine.Cancel(itemID); err != nil {
				t.Fatal(err)
			}
		}, StateCancelled, ReasonInterrupted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workflow := onePhaseWorkflow("resource", []string{"stack"}, tc.routes)
			h := newHarness(t, Config{Active: true, GlobalConcurrency: 2}, map[string]def.Workflow{"resource": workflow}, []string{"project"}, nil)
			h.profiles.setCapacity("project", "stack", 1)
			if err := h.engine.Enqueue(testItem("holder", "project", "resource", 0)); err != nil {
				t.Fatal(err)
			}
			if err := h.engine.Enqueue(testItem("waiter", "project", "resource", 1)); err != nil {
				t.Fatal(err)
			}
			if starts := h.runner.started(); len(starts) != 1 || starts[0].Key.ItemID != "holder" {
				t.Fatalf("starts before exit = %+v", starts)
			}
			tc.trigger(t, h, "holder")
			if err := h.engine.Sync(); err != nil {
				t.Fatal(err)
			}
			requireItemState(t, h.store, "holder", tc.wantState, tc.wantReason)
			starts := h.runner.started()
			if len(starts) != 2 || starts[1].Key.ItemID != "waiter" {
				t.Fatalf("resource leaked on %s: starts = %+v", tc.name, starts)
			}
		})
	}
}

func TestSemaphoreReleaseWhenRunnerStartFailsAfterAcquisition(t *testing.T) {
	workflow := onePhaseWorkflow("resource", []string{"stack"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 2}, map[string]def.Workflow{"resource": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "stack", 1)
	h.runner.startErrs["holder"] = errors.New("start failed")
	if err := h.engine.Enqueue(testItem("holder", "project", "resource", 0)); err == nil {
		t.Fatal("expected surfaced start error")
	}
	if err := h.engine.Enqueue(testItem("waiter", "project", "resource", 1)); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "holder", StateNeedsHuman, ReasonAgentError)
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != "waiter" {
		t.Fatalf("start failure leaked resource: %+v", starts)
	}
}

func TestSemaphoreReleaseStartsWaiterWithManyNonWaitingItems(t *testing.T) {
	workflow := onePhaseWorkflow("resource", []string{"stack"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 2}, map[string]def.Workflow{"resource": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "stack", 1)
	if err := h.engine.Enqueue(testItem("holder", "project", "resource", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Enqueue(testItem("waiter", "project", "resource", 1)); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 256; index++ {
		item := testItem(fmt.Sprintf("backlog-%03d", index), "project", "resource", index+2)
		if err := h.engine.Enqueue(item); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.engine.waiting) != 1 || h.engine.waiting[0].item.ID != "waiter" {
		t.Fatalf("waiting set = %+v", h.engine.waiting)
	}
	h.runner.complete(t, "holder", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) < 2 || starts[1].Key.ItemID != "waiter" {
		t.Fatalf("waiter was not started on release: %+v", starts)
	}
}

func TestSemaphoreReleaseContinuesWhenRunnerStopFails(t *testing.T) {
	workflow := onePhaseWorkflow("resource", []string{"stack"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 2}, map[string]def.Workflow{"resource": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "stack", 1)
	if err := h.engine.Enqueue(testItem("holder", "project", "resource", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Enqueue(testItem("waiter", "project", "resource", 1)); err != nil {
		t.Fatal(err)
	}
	h.runner.stopErrs["holder"] = errors.New("stop failed")
	if err := h.engine.Cancel("holder"); err == nil {
		t.Fatal("expected surfaced stop error")
	}
	requireItemState(t, h.store, "holder", StateCancelled, ReasonInterrupted)
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != "waiter" {
		t.Fatalf("stop error leaked resource: %+v", starts)
	}
}

func completeWith(kind OutcomeKind, envelope []byte) func(*testing.T, *testHarness, string) {
	return func(t *testing.T, h *testHarness, itemID string) {
		t.Helper()
		h.runner.complete(t, itemID, Outcome{Kind: kind, Envelope: envelope})
	}
}

func TestMultiResourceAcquisitionIsAllOrNothing(t *testing.T) {
	workflows := map[string]def.Workflow{
		"b":    onePhaseWorkflow("b", []string{"b"}, []def.Route{{To: "done"}}),
		"both": onePhaseWorkflow("both", []string{"b", "a"}, []def.Route{{To: "done"}}),
		"a":    onePhaseWorkflow("a", []string{"a"}, []def.Route{{To: "done"}}),
	}
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 3}, workflows, []string{"project"}, nil)
	h.profiles.setCapacity("project", "a", 1)
	h.profiles.setCapacity("project", "b", 1)
	for _, item := range []struct {
		id, workflow string
	}{{"holds-b", "b"}, {"waits-both", "both"}, {"uses-a", "a"}} {
		if err := h.engine.Enqueue(testItem(item.id, "project", item.workflow, len(h.runner.started()))); err != nil {
			t.Fatal(err)
		}
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[0].Key.ItemID != "holds-b" || starts[1].Key.ItemID != "uses-a" {
		t.Fatalf("all-or-nothing starts = %+v", starts)
	}
}

func TestResourceNamesAreProjectLocal(t *testing.T) {
	workflow := onePhaseWorkflow("resource", []string{"stack"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 2}, map[string]def.Workflow{"resource": workflow}, []string{"one", "two"}, nil)
	h.profiles.setCapacity("one", "stack", 1)
	h.profiles.setCapacity("two", "stack", 1)
	if err := h.engine.Enqueue(testItem("one-item", "one", "resource", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Enqueue(testItem("two-item", "two", "resource", 0)); err != nil {
		t.Fatal(err)
	}
	if starts := h.runner.started(); len(starts) != 2 {
		t.Fatalf("project-local resources contended: %+v", starts)
	}
}

func TestLoweredCapacityBlocksWithoutEvictingHolders(t *testing.T) {
	workflow := onePhaseWorkflow("resource", []string{"stack"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 3}, map[string]def.Workflow{"resource": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "stack", 2)
	for _, id := range []string{"one", "two"} {
		if err := h.engine.Enqueue(testItem(id, "project", "resource", 0)); err != nil {
			t.Fatal(err)
		}
	}
	h.profiles.setCapacity("project", "stack", 1)
	if err := h.engine.Enqueue(testItem("three", "project", "resource", 2)); err != nil {
		t.Fatal(err)
	}
	if got := len(h.runner.started()); got != 2 {
		t.Fatalf("starts after lowering = %d, want 2", got)
	}
	h.runner.complete(t, "one", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := len(h.runner.started()); got != 2 {
		t.Fatalf("capacity below/equal holders admitted new item: %d starts", got)
	}
	h.runner.complete(t, "two", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != "three" {
		t.Fatalf("waiter did not start after holders drained: %+v", starts)
	}
}

func TestLoopPhaseExitReleasesBeforeRetryAndExhaustionParks(t *testing.T) {
	workflow := def.Workflow{ID: "loop", Phases: []def.Phase{
		{ID: "build", Driver: def.DriverAgent, Resources: []string{"stack"}, Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}, Gate: def.Gate{Routes: []def.Route{{To: "review"}}}},
		{ID: "review", Driver: def.DriverAgent, Resources: []string{"stack"}, Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}, Gate: def.Gate{Routes: []def.Route{{Loop: "build", Max: 1}}}},
	}}
	waiterWorkflow := onePhaseWorkflow("waiter", []string{"stack"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 2}, map[string]def.Workflow{"loop": workflow, "waiter": waiterWorkflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "stack", 1)
	if err := h.engine.Enqueue(testItem("looping", "project", "loop", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Enqueue(testItem("waiter", "project", "waiter", 1)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "looping", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != "waiter" {
		t.Fatalf("waiter did not receive phase-scoped release: %+v", starts)
	}
	h.runner.complete(t, "waiter", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != "looping" || starts[2].Key.PhaseID != "review" {
		t.Fatalf("review did not resume after waiter: %+v", starts)
	}
	h.runner.complete(t, "looping", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 4 || starts[3].Key.PhaseID != "build" || starts[3].Key.Attempt != 2 {
		t.Fatalf("loop retry did not start: %+v", starts)
	}
	h.runner.complete(t, "looping", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 5 || starts[4].Key.PhaseID != "review" || starts[4].Key.Attempt != 2 {
		t.Fatalf("second review did not start: %+v", starts)
	}
	h.runner.complete(t, "looping", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "looping", StateNeedsHuman, ReasonRetriesExhausted)
	if err := h.engine.Enqueue(testItem("after-exhaustion", "project", "waiter", 2)); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if starts[len(starts)-1].Key.ItemID != "after-exhaustion" {
		t.Fatalf("retries-exhausted park leaked resource: %+v", starts)
	}
}

package engine

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

func TestBudgetExceededBeforePhaseAttemptStarts(t *testing.T) {
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	h.spend.spends["limited"] = Spend{Tokens: 101, USD: 1.25}
	item := testItem("limited", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"tokens":100}`)

	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonBudgetExhausted)
	if len(h.runner.started()) != 0 {
		t.Fatalf("runner starts = %d, want 0", len(h.runner.started()))
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 0 {
		t.Fatalf("phase rows = %+v, want none before budget trip", phases)
	}

	events := h.emitter.errorEvents(item.ID)
	if len(events) != 1 || events[0].Spend == nil || events[0].Spend.Tokens != 101 || events[0].Spend.USD != 1.25 {
		t.Fatalf("budget error events = %+v", events)
	}
}

func TestProfileBudgetTripsAtNextPhaseBoundary(t *testing.T) {
	workflow := def.Workflow{ID: "flow", Phases: []def.Phase{
		{ID: "first", Driver: def.DriverAgent, Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}, Gate: def.Gate{Routes: []def.Route{{To: "second"}}}},
		{ID: "second", Driver: def.DriverAgent, Gate: def.Gate{Routes: []def.Route{{To: "done"}}}},
	}}
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"flow": workflow}, []string{"p"}, nil)
	limit := int64(10)
	h.profiles.profiles["p"].Reliability.PerItemBudget = &profile.Budget{Tokens: &limit}
	item := testItem("boundary", "p", "flow", 0)

	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	h.spend.spends[item.ID] = Spend{Tokens: 11}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonBudgetExhausted)
	if starts := h.runner.started(); len(starts) != 1 || starts[0].Phase.ID != "first" {
		t.Fatalf("runner starts = %+v, want first phase only", starts)
	}
}

func TestWallClockBudgetUsesEngineClock(t *testing.T) {
	workflow := def.Workflow{ID: "flow", Phases: []def.Phase{
		{ID: "first", Driver: def.DriverAgent, Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}, Gate: def.Gate{Routes: []def.Route{{To: "second"}}}},
		{ID: "second", Driver: def.DriverAgent, Gate: def.Gate{Routes: []def.Route{{To: "done"}}}},
	}}
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"flow": workflow}, []string{"p"}, nil)
	ceiling := profile.Duration("1s")
	h.profiles.profiles["p"].Reliability.PerItemBudget = &profile.Budget{WallClock: &ceiling}
	now := time.UnixMilli(100)
	h.engine.now = func() time.Time { return now }
	item := testItem("wall", "p", "flow", 0)

	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	now = now.Add(1500 * time.Millisecond)
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonBudgetExhausted)
	events := h.emitter.errorEvents(item.ID)
	if len(events) != 1 || events[0].WallClockMillis != 1500 {
		t.Fatalf("wall-clock error events = %+v", events)
	}
}

func TestNoBudgetDoesNotQuerySpend(t *testing.T) {
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	item := testItem("unlimited", "p", "flow", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	if len(h.runner.started()) != 1 || h.spend.callCount() != 0 {
		t.Fatalf("starts=%d spend calls=%d", len(h.runner.started()), h.spend.callCount())
	}
}

func TestWallClockBudgetRecheckedAfterResourceWait(t *testing.T) {
	workflow := onePhaseWorkflow("resource", []string{"stack"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 2}, map[string]def.Workflow{
		"resource": workflow,
	}, []string{"p"}, nil)
	h.profiles.setCapacity("p", "stack", 1)
	ceiling := profile.Duration("1s")
	h.profiles.profiles["p"].Reliability.PerItemBudget = &profile.Budget{WallClock: &ceiling}
	now := time.UnixMilli(100)
	h.engine.now = func() time.Time { return now }

	if err := h.engine.Enqueue(testItem("holder", "p", "resource", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Enqueue(testItem("waiter", "p", "resource", 1)); err != nil {
		t.Fatal(err)
	}
	if starts := h.runner.started(); len(starts) != 1 || starts[0].Key.ItemID != "holder" {
		t.Fatalf("starts before release = %+v", starts)
	}

	now = now.Add(1500 * time.Millisecond)
	h.runner.complete(t, "holder", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "waiter", StateNeedsHuman, ReasonBudgetExhausted)
	if starts := h.runner.started(); len(starts) != 1 {
		t.Fatalf("expired waiter started after resource wait: %+v", starts)
	}
	events := h.emitter.errorEvents("waiter")
	if len(events) != 1 || events[0].WallClockMillis <= 1000 {
		t.Fatalf("waiter budget events = %+v", events)
	}
}

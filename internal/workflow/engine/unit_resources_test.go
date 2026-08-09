package engine

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"agent-overflow/internal/workflow/def"
)

func TestUnitResourcesCombineDeclaredCapacityWithTheAgentProviderBound(t *testing.T) {
	agent := def.Unit{
		ID: "audit", Provider: testProvider, Model: "test-model", Prompt: "unit.md",
		Resources: []string{"container-slot"},
	}
	got, err := unitResources(agent)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"container-slot", ProviderResource(testProvider)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agent unit resources = %v, want %v", got, want)
	}

	// A command unit runs no model turn, so it takes no provider slot — the
	// declared capacity is the only thing pacing it.
	command := def.Unit{ID: "gate", Command: "gate-check", Resources: []string{"container-slot"}}
	got, err = unitResources(command)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"container-slot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("command unit resources = %v, want %v", want, got)
	}

	call := def.Unit{ID: "lane", Call: "child"}
	if got, err := unitResources(call); err != nil || len(got) != 0 {
		t.Fatalf("call unit resources = %v, %v; want nothing", got, err)
	}

	// Validation refuses both of these statically, so reaching runtime means a
	// frozen definition cannot produce a runnable acquisition — a wiring error,
	// never a silently dropped claim.
	call.Resources = []string{"container-slot"}
	if _, err := unitResources(call); !errors.Is(err, ErrWiringFailed) {
		t.Fatalf("call unit with resources returned %v, want a wiring failure", err)
	}
	providerless := agent
	providerless.Provider = "  "
	if _, err := unitResources(providerless); !errors.Is(err, ErrWiringFailed) {
		t.Fatalf("providerless agent unit returned %v, want a wiring failure", err)
	}
}

// commandFanOutWorkflow is the gate-check shape per-unit `resources:` exists
// for: units that run commands rather than turns, paced only by the capacity
// they declare, consolidated by an ordinary agent join.
func commandFanOutWorkflow(id string, width int, unitResourceNames []string) def.Workflow {
	phase := staticFanOutPhase("work", width, nil, []def.Route{{To: "done"}})
	for index := range phase.FanOut {
		phase.FanOut[index] = def.Unit{
			ID:      phase.FanOut[index].ID,
			Command: "gate-check", Resources: unitResourceNames,
		}
	}
	return def.Workflow{ID: id, Phases: []def.Phase{phase}}
}

func TestCommandUnitsContendOnDeclaredCapacityThroughTheSharedFIFO(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{
		workflows:  map[string]def.Workflow{"fan": commandFanOutWorkflow("fan", 3, []string{"container-slot"})},
		projectIDs: []string{"project"},
		capacities: map[string]map[string]int{"project": {
			"container-slot": 1, ProviderResource(testProvider): 8,
		}},
	})
	item := startFanOut(t, h, "fan")

	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"work-unit-0"}) {
		t.Fatalf("started units = %v, want only the first under a container slot of 1", got)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	held := h.engine.holders[resourceKey{projectID: "project", name: "container-slot"}]
	if held != 1 {
		t.Fatalf("container-slot holders = %d, want the one running unit", held)
	}

	for index := 0; index < 3; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope(fmt.Sprintf("v%d", index))})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{
		"work-unit-0", "work-unit-1", "work-unit-2", "work-join",
	}) {
		t.Fatalf("launch order = %v, want each unit in turn then the join", got)
	}

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"),
		Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
	h.requireNoHeldResources(t)
}

// A frozen snapshot is decoded and never re-validated, so the definition
// validation refuses can still reach admission. Nothing runnable comes out of
// it, so the attempt parks as a wiring error rather than starting a child that
// holds a slot the author asked for and the unit cannot take.
func TestCallUnitDeclaringResourcesParksWiringError(t *testing.T) {
	phase := callUnitFanOutPhase("work", "child", 1, []def.Route{{To: "done"}})
	phase.FanOut[0].Args = nil
	phase.FanOut[0].Resources = []string{"container-slot"}
	h := newHarnessWith(t, harnessOptions{
		workflows: map[string]def.Workflow{
			"fan":   {ID: "fan", Phases: []def.Phase{phase}},
			"child": childWorkflow("child"),
		},
		projectIDs: []string{"project"},
		capacities: map[string]map[string]int{"project": {
			"container-slot": 2, ProviderResource(testProvider): 8,
		}},
	})
	if err := h.engine.StartItem(testItem("item", "project", "fan", 0)); err == nil {
		t.Fatal("a call unit claiming capacity started silently")
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonWiringError)
	if got := h.runner.startedUnitIDs(); len(got) != 0 {
		t.Fatalf("started units = %v, want none", got)
	}
	h.requireNoHeldResources(t)
}

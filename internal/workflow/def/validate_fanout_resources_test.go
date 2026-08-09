package def

import (
	"strings"
	"testing"
)

// A unit that runs work declares the project capacities it holds while running
// it — an agent unit and a command unit alike. The command case is the point:
// a gate-check unit inside a review fan-out claims a container slot that no
// provider bound would pace for it.
func TestUnitResourcesAreLegalOnEveryWorkingUnit(t *testing.T) {
	bindings := testBindings{
		checks:     map[string]bool{"test": true},
		commands:   map[string]bool{"gate-check": true},
		capacities: map[string]bool{"builder": true, "container-slot": true},
	}
	t.Run("agent unit", func(t *testing.T) {
		workflow := dynamicFanOutWorkflow()
		workflow.Phases[1].Unit.Resources = []string{"container-slot"}
		requireValid(t, Validate(fanOutFixture(t, workflow, fanOutPrompts()), bindings, nil))
	})
	t.Run("command unit and join", func(t *testing.T) {
		workflow := dynamicFanOutWorkflow()
		workflow.Phases[1].Unit = &Unit{ID: "port-section", Command: "gate-check", Resources: []string{"container-slot"}}
		workflow.Phases[1].Join = &Unit{ID: "merge", Command: "gate-check", Resources: []string{"container-slot"}}
		prompts := map[string]string{"plan.md": "plan {{goal}}"}
		requireValid(t, Validate(fanOutFixture(t, workflow, prompts), bindings, nil))
	})
}

// A unit acquires from the same project profile a phase does, so an unsized
// name is refused by the dry-run rather than parking the wave at its first
// admission.
func TestUnitResourcesMustBeBindable(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		mutate  func(*Workflow)
		element string
	}{
		{"unit", func(w *Workflow) { w.Phases[1].Unit.Resources = []string{"container-slot"} }, `fan-out unit "port-section"`},
		{"join", func(w *Workflow) { w.Phases[1].Join.Resources = []string{"container-slot"} }, "join"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workflow := dynamicFanOutWorkflow()
			testCase.mutate(&workflow)
			result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
			finding := requireFinding(t, result, "binding.capacity", `resource capacity "container-slot" is not bindable`)
			if !strings.Contains(finding.Element, testCase.element) {
				t.Fatalf("finding must name the element that declared it: %s", finding.Element)
			}
		})
	}
}

// A call unit runs no work of its own, so capacity it claimed would be held for
// a child run that acquires its own on the same bounds.
func TestCallUnitRefusesResources(t *testing.T) {
	workflow := callFanOutWorkflow()
	workflow.Phases[1].Unit.Resources = []string{"container-slot"}
	requireFinding(t, validateCallFanOut(t, workflow), "phase.fan-out-unit",
		"resources is not valid on a call unit")
}

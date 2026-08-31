package projectapp

import "testing"

func TestWorkflowFootprintSameAsComparesIdentitiesNotCounts(t *testing.T) {
	before := WorkflowFootprint{runIDs: []string{"run-a"}, automationIDs: []string{"automation-a"}}
	if !before.SameAs(before) {
		t.Fatal("a footprint did not equal itself")
	}
	if before.SameAs(WorkflowFootprint{runIDs: []string{"run-b"}, automationIDs: []string{"automation-a"}}) {
		t.Fatal("different run identity with the same count compared equal")
	}
	if before.SameAs(WorkflowFootprint{runIDs: []string{"run-a"}, automationIDs: []string{"automation-b"}}) {
		t.Fatal("different automation identity with the same count compared equal")
	}
}

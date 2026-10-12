package projectapp

import "testing"

func TestWorkflowFootprintSameAsComparesIdentitiesNotCounts(t *testing.T) {
	before := WorkflowFootprint{runIDs: []string{"run-a"}}
	if !before.SameAs(before) {
		t.Fatal("a footprint did not equal itself")
	}
	if before.SameAs(WorkflowFootprint{runIDs: []string{"run-b"}}) {
		t.Fatal("different run identity with the same count compared equal")
	}
}

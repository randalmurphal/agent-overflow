package store

import (
	"encoding/json"
	"testing"
)

func pendingUnit(unitID string, index int, kind, provider string) WorkItemUnit {
	return WorkItemUnit{
		ItemID: "item", PhaseID: "port", Attempt: 1, UnitID: unitID, UnitIndex: index,
		Kind: kind, Provider: provider, Model: "test-model",
		Status: WorkItemUnitPending, UnitAttempt: 1,
	}
}

func TestWorkItemUnitLifecycle(t *testing.T) {
	s := newTestStore(t)
	units := []WorkItemUnit{
		pendingUnit("port-0", 0, WorkItemUnitKindUnit, "claude"),
		pendingUnit("port-1", 1, WorkItemUnitKindUnit, "codex"),
		pendingUnit("merge", 2, WorkItemUnitKindJoin, "claude"),
	}
	if err := s.CreateWorkItemUnits(units); err != nil {
		t.Fatalf("create units: %v", err)
	}
	if err := s.CreateWorkItemUnits([]WorkItemUnit{pendingUnit("port-0", 0, WorkItemUnitKindUnit, "claude")}); err == nil {
		t.Fatal("duplicate unit id within one attempt succeeded")
	}
	if err := s.CreateWorkItemUnits(nil); err != nil {
		t.Fatalf("empty expansion: %v", err)
	}

	if err := s.StartWorkItemUnit("item", "port", 1, "port-0", 1, "", 100); err != nil {
		t.Fatalf("start unit: %v", err)
	}
	if err := s.AttachWorkItemUnitWorkspace("item", "port", 1, "port-0", "ao/port-0", "/wt/port-0"); err != nil {
		t.Fatalf("attach unit workspace: %v", err)
	}
	if err := s.AttachWorkItemUnitRun("item", "port", 1, "port-0", "thread-0", "/runs/port-0/narrative.md"); err != nil {
		t.Fatalf("attach unit run: %v", err)
	}
	envelope := json.RawMessage(`{"status":"done","outputs":{"ok":true}}`)
	if err := s.CompleteWorkItemUnit("item", "port", 1, "port-0", WorkItemUnitDone, envelope, "", 110); err != nil {
		t.Fatalf("complete unit: %v", err)
	}

	unit, found, err := s.GetWorkItemUnit("item", "port", 1, "port-0")
	if err != nil || !found {
		t.Fatalf("get unit = %v found=%v", err, found)
	}
	if unit.Status != WorkItemUnitDone || unit.ThreadID != "thread-0" || unit.Branch != "ao/port-0" ||
		unit.WorktreePath != "/wt/port-0" || unit.NarrativePath != "/runs/port-0/narrative.md" ||
		string(unit.Envelope) != string(envelope) || unit.StartedAt != 100 || unit.EndedAt != 110 ||
		unit.UnitAttempt != 1 || unit.Provider != "claude" || unit.Kind != WorkItemUnitKindUnit {
		t.Fatalf("completed unit = %#v", unit)
	}
	if _, found, err := s.GetWorkItemUnit("item", "port", 1, "absent"); err != nil || found {
		t.Fatalf("missing unit = found %v err %v", found, err)
	}
	for _, missing := range []func() error{
		func() error { return s.StartWorkItemUnit("item", "port", 1, "absent", 1, "", 1) },
		func() error {
			return s.CompleteWorkItemUnit("item", "port", 1, "absent", WorkItemUnitDone, nil, "", 1)
		},
		func() error { return s.AttachWorkItemUnitRun("item", "port", 1, "absent", "t", "n") },
		func() error { return s.AttachWorkItemUnitWorkspace("item", "port", 1, "absent", "b", "w") },
	} {
		if err := missing(); err == nil {
			t.Fatal("update of an absent unit row reported success")
		}
	}
	if err := s.StartWorkItemUnit("item", "port", 1, "port-0", 0, "", 1); err == nil {
		t.Fatal("unit attempt 0 was accepted")
	}

	listed, err := s.ListWorkItemPhaseUnits("item", "port", 1)
	if err != nil {
		t.Fatalf("list phase units: %v", err)
	}
	if len(listed) != 3 || listed[0].UnitID != "port-0" || listed[1].UnitID != "port-1" || listed[2].Kind != WorkItemUnitKindJoin {
		t.Fatalf("phase units = %#v", listed)
	}
	if other, err := s.ListWorkItemPhaseUnits("item", "port", 2); err != nil || len(other) != 0 {
		t.Fatalf("units of another attempt = %#v err %v", other, err)
	}
	all, err := s.ListWorkItemUnits("item")
	if err != nil || len(all) != 3 {
		t.Fatalf("list units = %#v err %v", all, err)
	}
}

// A retry starts a new per-unit attempt on the same row: the record keeps one
// lineage per unit, carries the feedback the retry was given, and drops the
// envelope of the attempt that failed.
func TestWorkItemUnitRetryReusesTheRowLineage(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateWorkItemUnits([]WorkItemUnit{pendingUnit("port-0", 0, WorkItemUnitKindUnit, "claude")}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartWorkItemUnit("item", "port", 1, "port-0", 1, "", 100); err != nil {
		t.Fatal(err)
	}
	partial := json.RawMessage(`{"status":"stuck","outputs":null}`)
	if err := s.CompleteWorkItemUnit("item", "port", 1, "port-0", WorkItemUnitFailed, partial, "unit outcome stuck", 110); err != nil {
		t.Fatal(err)
	}
	if err := s.StartWorkItemUnit("item", "port", 1, "port-0", 2, "retry after: unit outcome stuck", 120); err != nil {
		t.Fatal(err)
	}
	unit, _, err := s.GetWorkItemUnit("item", "port", 1, "port-0")
	if err != nil {
		t.Fatal(err)
	}
	if unit.UnitAttempt != 2 || unit.Status != WorkItemUnitRunning || len(unit.Envelope) != 0 ||
		unit.Feedback != "retry after: unit outcome stuck" || unit.StartedAt != 120 || unit.EndedAt != 0 {
		t.Fatalf("retried unit = %#v", unit)
	}
}

// The teardown contract's store half: nothing may stay `running` after an
// attempt stops, and a unit that never started stays launchable.
func TestFailRunningWorkItemUnitsLeavesPendingLaunchable(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateWorkItemUnits([]WorkItemUnit{
		pendingUnit("port-0", 0, WorkItemUnitKindUnit, "claude"),
		pendingUnit("port-1", 1, WorkItemUnitKindUnit, "claude"),
		pendingUnit("port-2", 2, WorkItemUnitKindUnit, "claude"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartWorkItemUnit("item", "port", 1, "port-0", 1, "", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.StartWorkItemUnit("item", "port", 1, "port-1", 1, "", 101); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteWorkItemUnit("item", "port", 1, "port-1", WorkItemUnitDone, nil, "", 105); err != nil {
		t.Fatal(err)
	}
	affected, err := s.FailRunningWorkItemUnits("item", "port", 1, "interrupted", 200)
	if err != nil {
		t.Fatalf("fail running units: %v", err)
	}
	if affected != 1 {
		t.Fatalf("failed %d running units, want 1", affected)
	}
	units, err := s.ListWorkItemPhaseUnits("item", "port", 1)
	if err != nil {
		t.Fatal(err)
	}
	if units[0].Status != WorkItemUnitFailed || units[0].Feedback != "interrupted" || units[0].EndedAt != 200 {
		t.Fatalf("running unit = %#v", units[0])
	}
	if units[1].Status != WorkItemUnitDone || units[1].Feedback != "" {
		t.Fatalf("completed unit was rewritten: %#v", units[1])
	}
	if units[2].Status != WorkItemUnitPending || units[2].StartedAt != 0 {
		t.Fatalf("pending unit was failed: %#v", units[2])
	}
}

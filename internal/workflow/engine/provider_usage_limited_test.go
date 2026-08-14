package engine

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestProviderUsageLimitedParksWithDurableScopeAndBareResumeContinues(t *testing.T) {
	h := newPauseHarness(t)
	item := testItem("usage-item", "project", "pausable", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "usage-thread")
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "usage-thread", ""); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{
		Kind: OutcomeProviderUsageLimited, Detail: "provider usage limit reached",
		ProviderUsageScopeID: 41,
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonProviderUsageLimited)
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked, ok := phaseAttempt(phases, "work", 1)
	if !ok || parked.ProviderUsageScopeID != 41 || parked.ParkCause != "provider usage limit reached" {
		t.Fatalf("parked phase = %+v found=%v", parked, ok)
	}

	if err := h.engine.Resume(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Launch.ThreadID() != "usage-thread" || !starts[1].Launch.ContinuesThread() {
		t.Fatalf("resume starts = %+v", starts)
	}
	if starts[1].Feedback == nil || !strings.Contains(starts[1].Feedback.Note, "provider usage limit was reached") {
		t.Fatalf("resume feedback = %+v", starts[1].Feedback)
	}
	// Resume creates a continuation attempt. The failed attempt keeps its
	// provenance as history; no admission decision consults it.
	phases, err = h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prior, ok := phaseAttempt(phases, "work", 1); !ok || prior.ProviderUsageScopeID != 41 {
		t.Fatalf("historical usage scope was lost: %+v", prior)
	}
}

func TestProviderUsageLimitedFanOutPersistsPerUnitAndRetriesOnlyFailedWork(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"), Outcome{
		Kind: OutcomeProviderUsageLimited, Detail: "usage limited", ProviderUsageScopeID: 73,
	})
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"), Outcome{
		Kind: OutcomeDone, Envelope: unitDoneEnvelope("work-unit-1"),
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)
	rows, err := h.store.ListWorkItemPhaseUnits(item, "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]store.WorkItemUnit, len(rows))
	for _, row := range rows {
		byID[row.UnitID] = row
	}
	if failed := byID["work-unit-0"]; failed.Status != store.WorkItemUnitFailed || failed.ProviderUsageScopeID != 73 {
		t.Fatalf("usage-limited unit = %+v", failed)
	}
	if done := byID["work-unit-1"]; done.Status != store.WorkItemUnitDone || done.ProviderUsageScopeID != 0 {
		t.Fatalf("successful sibling = %+v", done)
	}

	if err := h.engine.Resume(item, "", false); err != nil {
		t.Fatal(err)
	}
	rows, err = h.store.ListWorkItemPhaseUnits(item, "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	byID = make(map[string]store.WorkItemUnit, len(rows))
	for _, row := range rows {
		byID[row.UnitID] = row
	}
	if retried := byID["work-unit-0"]; retried.Status != store.WorkItemUnitRunning || retried.ProviderUsageScopeID != 0 {
		t.Fatalf("retried usage-limited unit = %+v", retried)
	}
	if done := byID["work-unit-1"]; done.Status != store.WorkItemUnitDone {
		t.Fatalf("resume re-ran completed sibling: %+v", done)
	}
}

func TestNonUsageFanOutOutcomesCannotPersistStaleUsageScope(t *testing.T) {
	t.Run("work unit", func(t *testing.T) {
		h := newFanOutHarness(t, 2)
		item := startFanOut(t, h, "fan")
		h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"), Outcome{
			Kind: OutcomeExecutionFailure, Detail: "agent failed", ProviderUsageScopeID: 91,
		})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		rows, err := h.store.ListWorkItemPhaseUnits(item, "work", 1)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.UnitID == "work-unit-0" {
				found = true
				if row.ProviderUsageScopeID != 0 {
					t.Fatalf("execution failure persisted stale usage scope: %+v", row)
				}
			}
		}
		if !found {
			t.Fatal("failed work unit row was not persisted")
		}
	})

	t.Run("join", func(t *testing.T) {
		h := newFanOutHarness(t, 1)
		item := startFanOut(t, h, "fan")
		h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"), Outcome{
			Kind: OutcomeDone, Envelope: unitDoneEnvelope("ready"),
		})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"), Outcome{
			Kind: OutcomeExecutionFailure, Detail: "join failed", ProviderUsageScopeID: 92,
		})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		rows, err := h.store.ListWorkItemPhaseUnits(item, "work", 1)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.UnitID == "work-join" {
				found = true
				if row.ProviderUsageScopeID != 0 {
					t.Fatalf("join failure persisted stale usage scope: %+v", row)
				}
			}
		}
		if !found {
			t.Fatal("failed join row was not persisted")
		}
	})
}

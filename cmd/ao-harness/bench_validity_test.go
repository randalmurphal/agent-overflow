package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func activeReadinessView() (uiViewport, []string) {
	ids := []string{"thread-1", "thread-2", "thread-3", "thread-4", "thread-5", "thread-6"}
	view := uiViewport{Settled: true, Panes: make([]uiPane, 0, len(ids))}
	for i, id := range ids {
		pane := uiPane{
			PaneID:   "pane-" + id,
			ThreadID: id,
			Rect:     uiRect{X: float64(i), Y: 0, W: 100, H: 100},
		}
		if i < benchActiveStreamCount {
			pane.Rows = []uiRow{{Role: "assistant", Status: "streaming", InViewport: true, TextLength: 10, Rect: uiRect{Y: 10, H: 20}}}
		}
		view.Panes = append(view.Panes, pane)
	}
	return view, ids
}

func TestValidateActivePaneReadinessRequiresExactShape(t *testing.T) {
	view, ids := activeReadinessView()
	got, err := validateActivePaneReadiness(view, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PaneIDs) != 6 || len(got.ActiveThreadIDs) != 4 || len(got.InactiveThreadIDs) != 2 {
		t.Fatalf("readiness = %+v", got)
	}

	view.Panes[0].PaneID = view.Panes[1].PaneID
	if _, err := validateActivePaneReadiness(view, ids); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate pane ID error = %v", err)
	}
}

func TestValidateActivePaneReadinessRequiresSettledViewport(t *testing.T) {
	view, ids := activeReadinessView()
	view.Settled = false
	if _, err := validateActivePaneReadiness(view, ids); err == nil || !strings.Contains(err.Error(), "not settled") {
		t.Fatalf("unsettled viewport error = %v", err)
	}
}

func TestValidateActivePaneReadinessRejectsClippedAndInactiveStreamingRows(t *testing.T) {
	view, ids := activeReadinessView()
	view.Panes[0].Rows[0].Rect.Y = 90
	if _, err := validateActivePaneReadiness(view, ids); err == nil || !strings.Contains(err.Error(), "clipped") {
		t.Fatalf("clipped row error = %v", err)
	}
	view, ids = activeReadinessView()
	view.Panes[4].Rows = []uiRow{{Role: "assistant", Status: "streaming", InViewport: true}}
	if _, err := validateActivePaneReadiness(view, ids); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("inactive row error = %v", err)
	}
}

func TestValidateActivePaneReadinessRejectsInvisibleInactivePane(t *testing.T) {
	view, ids := activeReadinessView()
	view.Panes[5].Rect.H = 0
	if _, err := validateActivePaneReadiness(view, ids); err == nil || !strings.Contains(err.Error(), "no visible geometry") {
		t.Fatalf("invisible inactive pane error = %v", err)
	}
}

func TestValidateBenchProgressCadenceReportsMaximumGap(t *testing.T) {
	samples := []benchVisibleProgress{{AtMs: 0}, {AtMs: 1000}, {AtMs: 2500}}
	got, err := validateBenchProgressCadence(samples, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxObservationGapMs != 1500 || got.Samples != 3 {
		t.Fatalf("cadence = %+v", got)
	}
	if _, err := validateBenchProgressCadence([]benchVisibleProgress{{AtMs: 0}, {AtMs: 2501}}, time.Second); err == nil {
		t.Fatal("an observation gap beyond two cadence intervals was accepted")
	}
}

func TestBenchEvidenceCursorsOnlyReadLinesAppendedAfterStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frontend-errors.jsonl")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cursors := benchEvidenceCursors{FrontendErrors: newBenchEvidenceCursor(path)}
	if err := os.WriteFile(path, []byte("old\nnew\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	faults, err := collectBenchFaultReceipt(cursors)
	if err != nil {
		t.Fatal(err)
	}
	if faults.FrontendErrors != 1 || len(faults.Sample) != 1 || faults.Sample[0] != "new" {
		t.Fatalf("fault receipt = %+v", faults)
	}
}

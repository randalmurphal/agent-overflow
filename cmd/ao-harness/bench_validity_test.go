package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func activeMountView() (uiViewport, []string) {
	ids := []string{"thread-1", "thread-2", "thread-3", "thread-4", "thread-5", "thread-6"}
	view := uiViewport{Settled: true, Panes: make([]uiPane, 0, len(ids))}
	for i, id := range ids {
		view.Panes = append(view.Panes, uiPane{
			PaneID:   "pane-" + id,
			ThreadID: id,
			Rect:     uiRect{X: float64(i), Y: 0, W: 100, H: 100},
		})
	}
	return view, ids
}

func TestValidateActivePaneMountRequiresExactShape(t *testing.T) {
	view, ids := activeMountView()
	if err := validateActivePaneMount(view, ids); err != nil {
		t.Fatal(err)
	}

	view.Panes[0].PaneID = view.Panes[1].PaneID
	if err := validateActivePaneMount(view, ids); err == nil || !strings.Contains(err.Error(), "invalid pane") {
		t.Fatalf("duplicate pane ID error = %v", err)
	}

	view, ids = activeMountView()
	view.Panes = view.Panes[:5]
	if err := validateActivePaneMount(view, ids); err == nil || !strings.Contains(err.Error(), "want exactly 6") {
		t.Fatalf("missing pane error = %v", err)
	}
}

func TestValidateActivePaneMountRequiresSettledViewport(t *testing.T) {
	view, ids := activeMountView()
	view.Settled = false
	if err := validateActivePaneMount(view, ids); err == nil || !strings.Contains(err.Error(), "not settled") {
		t.Fatalf("unsettled viewport error = %v", err)
	}
}

func TestValidateActivePaneMountRejectsInvisiblePane(t *testing.T) {
	view, ids := activeMountView()
	view.Panes[5].Rect.H = 0
	if err := validateActivePaneMount(view, ids); err == nil || !strings.Contains(err.Error(), "invalid pane") {
		t.Fatalf("invisible pane error = %v", err)
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

// A diagnostic the app reported itself is counted apart from uncaught
// errors, so it cannot invalidate a run as one.
func TestBenchFaultReceiptCountsAppDiagnosticsApart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frontend-errors.jsonl")
	cursors := benchEvidenceCursors{FrontendErrors: newBenchEvidenceCursor(path)}
	lines := `{"kind":"diagnostic","message":"transport: reconnected after outage","stack":"down 1.2s"}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	faults, err := collectBenchFaultReceipt(cursors)
	if err != nil {
		t.Fatal(err)
	}
	if faults.FrontendErrors != 0 || faults.AppDiagnostics != 1 {
		t.Fatalf("fault receipt = %+v, want 0 uncaught errors and 1 app diagnostic", faults)
	}
}

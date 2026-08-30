package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIQueryErrorNamesHowToAttachAPage(t *testing.T) {
	err := uiQueryError(errors.New("harness ui query timed out: no frontend attached or harness bridge inactive"))
	for _, want := range []string{"make harness-window", "ao-harness open"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// The snapshot FILE is the other half of `ui diff`: one side of every
// comparison comes off disk, and nothing rewrites it when the app is
// upgraded. A baseline that decoded silently into the wrong shape would
// produce the same "nothing moved" lie a mistyped live reply would.

func snapshotTarget(t *testing.T) target {
	t.Helper()
	root := t.TempDir()
	return target{ID: "abcd1234", DataRoot: root, DataDir: filepath.Join(root, appDataDirName)}
}

func TestUISnapshotFileRoundTrips(t *testing.T) {
	tgt := snapshotTarget(t)
	view := cannedViewport(cannedRow("item-a", 0, 10, true), cannedRow("item-b", 1, 60, false))
	view.Panes[0].Scroll.AtBottom = true

	if err := writeUISnapshot(tgt, view); err != nil {
		t.Fatal(err)
	}
	// The write creates its own directory: `ui snapshot` is usually the
	// first thing to touch a fresh instance's data dir.
	loaded, err := readUISnapshot(tgt)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Instance != tgt.ID || loaded.TakenAt == "" {
		t.Errorf("provenance lost: %+v", loaded)
	}
	// Every discriminating field has to survive, not just the ids: a diff
	// reads geometry and viewport membership off this.
	if diff := diffViewports(loaded.Viewport, view, uiGeometryThresholdPx); !diff.Empty() {
		t.Fatalf("the round trip changed the snapshot: %+v", diff)
	}
	if !loaded.Viewport.Panes[0].Scroll.AtBottom {
		t.Error("scroll state did not survive the round trip")
	}
}

func TestReadUISnapshotRejectsAnotherVersion(t *testing.T) {
	tgt := snapshotTarget(t)
	view := cannedViewport(cannedRow("item-a", 0, 10, true))
	view.V = 2
	if err := writeUISnapshot(tgt, view); err != nil {
		t.Fatal(err)
	}
	_, err := readUISnapshot(tgt)
	if err == nil {
		t.Fatal("a v2 baseline was accepted")
	}
	if !strings.Contains(err.Error(), "version 2") {
		t.Errorf("error should name the version it found: %v", err)
	}
}

func TestReadUISnapshotNamesTheMissingBaselineAndTheFix(t *testing.T) {
	tgt := snapshotTarget(t)
	_, err := readUISnapshot(tgt)
	if err == nil {
		t.Fatal("reading a baseline that was never written must fail")
	}
	// The error is the whole UX of a first `ui diff`, so it has to name the
	// command that produces the file and where the file goes.
	for _, want := range []string{"ui snapshot", uiSnapshotPath(tgt)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestReadUISnapshotRejectsGarbage(t *testing.T) {
	tgt := snapshotTarget(t)
	path := uiSnapshotPath(tgt)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUISnapshot(tgt); err == nil {
		t.Fatal("a truncated baseline was accepted")
	}

	// A file that IS json but carries no viewport is the same class: v is
	// absent, so it reads as 0 rather than 1.
	body, err := json.Marshal(map[string]any{"takenAt": "t0", "instance": tgt.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUISnapshot(tgt); err == nil {
		t.Fatal("a baseline with no viewport was accepted")
	}
}

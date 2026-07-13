package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkflowDefinitionsWatcherDebouncesWritesAndDiscoversProjectRename(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "workflows")
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	emitted := make(chan struct{}, 8)
	watcher, err := newWorkflowDefinitionsWatcher(root, 25*time.Millisecond, func() { emitted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	definition := filepath.Join(shared, "build.yaml")
	for index := range 3 {
		if err := os.WriteFile(definition, []byte{byte('a' + index)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitForDefinitionsChanged(t, emitted)
	select {
	case <-emitted:
		t.Fatal("write burst emitted more than one debounced event")
	case <-time.After(75 * time.Millisecond):
	}

	outside := t.TempDir()
	incoming := filepath.Join(outside, "renamed-project")
	incomingWorkflows := filepath.Join(incoming, "workflows")
	if err := os.MkdirAll(incomingWorkflows, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomingWorkflows, "ship.yaml"), []byte("id: ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(incoming, filepath.Join(projects, "renamed-project")); err != nil {
		t.Fatal(err)
	}
	waitForDefinitionsChanged(t, emitted)
	if err := os.WriteFile(filepath.Join(projects, "renamed-project", "workflows", "ship.yaml"), []byte("id: changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForDefinitionsChanged(t, emitted)

	// Data-root noise (SQLite WAL rotation, atomic settings renames) shares
	// the watched root but must never reach the emit path.
	noise := filepath.Join(root, "store.db-wal")
	if err := os.WriteFile(noise, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(noise, filepath.Join(root, "settings.json")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-emitted:
		t.Fatal("data-root noise emitted a definitions-changed event")
	case <-time.After(75 * time.Millisecond):
	}
}

func TestWorkflowDefinitionsWatcherRejectsInvalidSetupAndClosesCleanly(t *testing.T) {
	if _, err := newWorkflowDefinitionsWatcher(filepath.Join(t.TempDir(), "missing"), time.Millisecond, func() {}); err == nil {
		t.Fatal("missing root unexpectedly accepted")
	}
	invalidRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalidRoot, "workflows"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newWorkflowDefinitionsWatcher(invalidRoot, time.Millisecond, func() {}); err == nil {
		t.Fatal("workflow file in place of directory unexpectedly accepted")
	}
	root := t.TempDir()
	watcher, err := newWorkflowDefinitionsWatcher(root, time.Millisecond, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func waitForDefinitionsChanged(t *testing.T, emitted <-chan struct{}) {
	t.Helper()
	select {
	case <-emitted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for definitions-changed event")
	}
}

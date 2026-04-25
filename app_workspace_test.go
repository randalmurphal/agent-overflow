package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteThreadWorkspaceFile(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-save-plan")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	writtenPath, err := app.WriteThreadWorkspaceFile(thread.ID, "plans/ship-it.md", "# Ship it\n")
	if err != nil {
		t.Fatalf("WriteThreadWorkspaceFile() error = %v", err)
	}
	if writtenPath != filepath.Join("plans", "ship-it.md") {
		t.Fatalf("writtenPath = %q, want %q", writtenPath, filepath.Join("plans", "ship-it.md"))
	}

	data, err := os.ReadFile(filepath.Join(workspace, writtenPath))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "# Ship it\n" {
		t.Fatalf("file contents = %q, want %q", string(data), "# Ship it\n")
	}
	// File mode is 0o600 — workspace writes can carry user content
	// the user wouldn't want world-readable on a shared host. Pin
	// the mode so a future change can't loosen it without surfacing.
	info, err := os.Stat(filepath.Join(workspace, writtenPath))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 0o600 (workspace writes are user-private)", got)
	}
}

func TestWriteThreadWorkspaceFileRejectsParentEscape(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-save-plan-escape")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.WriteThreadWorkspaceFile(thread.ID, "../outside.md", "nope"); err == nil {
		t.Fatal("expected parent escape path to fail")
	}
}

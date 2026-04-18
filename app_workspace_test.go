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

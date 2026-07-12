package main

import (
	"testing"

	"agent-overflow/internal/threadmode"
)

func TestWorkflowOpenStudioThreadUsesProjectWorkflowSingleton(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })

	newFirst, err := app.WorkflowOpenStudioThread(defaultTestProjectID, "")
	if err != nil {
		t.Fatal(err)
	}
	newSecond, err := app.WorkflowOpenStudioThread(defaultTestProjectID, "")
	if err != nil {
		t.Fatal(err)
	}
	edit, err := app.WorkflowOpenStudioThread(defaultTestProjectID, "build")
	if err != nil {
		t.Fatal(err)
	}
	if newFirst.ID != newSecond.ID || edit.ID == newFirst.ID {
		t.Fatalf("studio identities new=%s/%s edit=%s", newFirst.ID, newSecond.ID, edit.ID)
	}
	if newFirst.Mode != threadmode.ModeWorkflowStudio || newFirst.WorkspacePath != "/tmp/workspace" ||
		newFirst.Title != "Workflow studio — new workflow" || !newFirst.IsDraft {
		t.Fatalf("new studio = %+v", newFirst)
	}
	if edit.Title != "Workflow studio — build" || edit.ProjectID != defaultTestProjectID {
		t.Fatalf("edit studio = %+v", edit)
	}
	if err := app.RenameThread(edit.ID, "A title the user chose"); err != nil {
		t.Fatal(err)
	}
	renamed, err := app.WorkflowOpenStudioThread(defaultTestProjectID, "build")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != edit.ID || renamed.Title != "A title the user chose" {
		t.Fatalf("renamed singleton = %+v, want id %s", renamed, edit.ID)
	}

	if err := app.workflowEngine.Close(); err != nil {
		t.Fatal(err)
	}
	app.workflowEngine = nil
	app.workflowRunner = nil
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.WorkflowOpenStudioThread(defaultTestProjectID, "build")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ID != edit.ID {
		t.Fatalf("restart singleton = %s, want %s", reopened.ID, edit.ID)
	}
}

func TestWorkflowOpenStudioThreadRequiresProject(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
	if _, err := app.WorkflowOpenStudioThread("", "build"); err == nil {
		t.Fatal("empty project id unexpectedly succeeded")
	}
}

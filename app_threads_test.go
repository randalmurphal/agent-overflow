package main

import (
	"strings"
	"testing"
)

// Smoke tests for the per-field thread update bindings. These sit at the
// binding boundary: they validate the input, call the store, and return
// the refreshed thread. The restart-if-affected logic is exercised with
// a stubbed active session so the binding returns after persisting even
// though the session is "live".

func TestUpdateThreadProviderPersistsAndValidates(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tp", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadProvider(thread.ID, "codex")
	if err != nil {
		t.Fatalf("UpdateThreadProvider: %v", err)
	}
	if updated.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", updated.Provider)
	}

	if _, err := app.UpdateThreadProvider(thread.ID, "bogus"); err == nil {
		t.Fatal("UpdateThreadProvider(bogus) error = nil, want validation error")
	}
}

func TestUpdateThreadReasoningEffortValidates(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/te", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadReasoningEffort(thread.ID, "xhigh")
	if err != nil {
		t.Fatalf("UpdateThreadReasoningEffort: %v", err)
	}
	if updated.ReasoningEffort != "xhigh" {
		t.Fatalf("ReasoningEffort = %q, want xhigh", updated.ReasoningEffort)
	}

	if _, err := app.UpdateThreadReasoningEffort(thread.ID, "ultranope"); err == nil {
		t.Fatal("UpdateThreadReasoningEffort(ultranope) error = nil, want validation error")
	}
}

func TestUpdateThreadFastModeToggles(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tfm", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	if thread.FastMode {
		t.Fatal("FastMode default should be false")
	}

	updated, err := app.UpdateThreadFastMode(thread.ID, true)
	if err != nil {
		t.Fatalf("UpdateThreadFastMode(true): %v", err)
	}
	if !updated.FastMode {
		t.Fatal("FastMode = false, want true")
	}

	updated, err = app.UpdateThreadFastMode(thread.ID, false)
	if err != nil {
		t.Fatalf("UpdateThreadFastMode(false): %v", err)
	}
	if updated.FastMode {
		t.Fatal("FastMode = true, want false")
	}
}

func TestUpdateThreadContextWindowValidates(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tcw", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadContextWindow(thread.ID, 200000)
	if err != nil {
		t.Fatalf("UpdateThreadContextWindow(200000): %v", err)
	}
	if updated.ContextWindow != 200000 {
		t.Fatalf("ContextWindow = %d, want 200000", updated.ContextWindow)
	}

	if _, err := app.UpdateThreadContextWindow(thread.ID, 999); err == nil {
		t.Fatal("UpdateThreadContextWindow(999) error = nil, want validation error")
	}
}

func TestUpdateThreadRuntimeModeValidates(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/trm", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadRuntimeMode(thread.ID, "approval-required")
	if err != nil {
		t.Fatalf("UpdateThreadRuntimeMode: %v", err)
	}
	if updated.RuntimeMode != "approval-required" {
		t.Fatalf("RuntimeMode = %q, want approval-required", updated.RuntimeMode)
	}

	if _, err := app.UpdateThreadRuntimeMode(thread.ID, "yolo"); err == nil {
		t.Fatal("UpdateThreadRuntimeMode(yolo) error = nil, want validation error")
	}
}

func TestUpdateThreadBranchAndWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tbw", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadBranch(thread.ID, "feat/abc")
	if err != nil {
		t.Fatalf("UpdateThreadBranch: %v", err)
	}
	if updated.Branch != "feat/abc" {
		t.Fatalf("Branch = %q, want feat/abc", updated.Branch)
	}

	updated, err = app.UpdateThreadWorkspace(thread.ID, "/tmp/alt-workspace")
	if err != nil {
		t.Fatalf("UpdateThreadWorkspace: %v", err)
	}
	if updated.WorkspacePath != "/tmp/alt-workspace" {
		t.Fatalf("WorkspacePath = %q, want /tmp/alt-workspace", updated.WorkspacePath)
	}

	if _, err := app.UpdateThreadWorkspace(thread.ID, ""); err == nil ||
		!strings.Contains(err.Error(), "path is required") {
		t.Fatalf("UpdateThreadWorkspace(empty) error = %v, want 'path is required'", err)
	}
}

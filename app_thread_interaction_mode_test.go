package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestUpdateThreadModePersistsValidMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-mode-set")
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Only chat ↔ plan transitions are valid post-creation. Thread type
	// (design / discussion) is immutable.
	for _, mode := range []string{"chat", "plan"} {
		got, err := app.UpdateThreadMode(thread.ID, mode)
		if err != nil {
			t.Fatalf("UpdateThreadMode(%q) error = %v", mode, err)
		}
		if got.Mode != mode {
			t.Fatalf("returned Mode = %q, want %q", got.Mode, mode)
		}
		if got.UpdatedAt != thread.UpdatedAt {
			t.Fatalf("returned UpdatedAt = %d, want %d", got.UpdatedAt, thread.UpdatedAt)
		}
		stored, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Fatalf("GetThread() error = %v", err)
		}
		if stored.Mode != mode {
			t.Fatalf("stored Mode = %q, want %q", stored.Mode, mode)
		}
		if stored.UpdatedAt != thread.UpdatedAt {
			t.Fatalf("stored UpdatedAt = %d, want %d", stored.UpdatedAt, thread.UpdatedAt)
		}
	}
}

// TestUpdateThreadModeRejectsTypeMutations enforces immutable thread type:
// design and discussion modes cannot be set on an existing thread, and a
// design/discussion thread cannot be flipped to chat or plan either.
func TestUpdateThreadModeRejectsTypeMutations(t *testing.T) {
	app := newTestAppWithStore(t)
	chatThread := testThread("chat-thread")
	if err := app.store.CreateThread(chatThread); err != nil {
		t.Fatalf("CreateThread(chat) error = %v", err)
	}
	for _, target := range []string{"design", "discussion"} {
		if _, err := app.UpdateThreadMode(chatThread.ID, target); err == nil {
			t.Fatalf("UpdateThreadMode(chat→%s) error = nil, want rejection", target)
		}
	}

	designThread := testThread("design-thread")
	designThread.Mode = "design"
	if err := app.store.CreateThread(designThread); err != nil {
		t.Fatalf("CreateThread(design) error = %v", err)
	}
	for _, target := range []string{"chat", "plan"} {
		if _, err := app.UpdateThreadMode(designThread.ID, target); err == nil {
			t.Fatalf("UpdateThreadMode(design→%s) error = nil, want rejection", target)
		}
	}
}

func TestUpdateThreadModeRejectsInvalidMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-mode-invalid")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	for _, mode := range []string{"nonsense", "DEFAULT", " ", "debate", "PLAN", "design", "discussion", "workflow"} {
		if _, err := app.UpdateThreadMode(thread.ID, mode); err == nil {
			t.Fatalf("UpdateThreadMode(%q) error = nil, want validation error", mode)
		} else if !strings.Contains(err.Error(), "invalid mode") {
			t.Fatalf("UpdateThreadMode(%q) error = %v, want invalid-mode message", mode, err)
		}
	}
}

func TestUpdateThreadModeRejectsEmpty(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-mode-empty-arg")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if _, err := app.UpdateThreadMode(thread.ID, ""); err == nil {
		t.Fatal("UpdateThreadMode(\"\") error = nil, want validation error")
	}
}

func TestUpdateThreadModeUnknownThread(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.UpdateThreadMode("does-not-exist", "plan"); err == nil {
		t.Fatal("UpdateThreadMode(unknown) error = nil, want not-found")
	}
}

func TestUpdateThreadModeRoundTripPersists(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-mode-round-trip")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.UpdateThreadMode(thread.ID, "plan"); err != nil {
		t.Fatalf("UpdateThreadMode() error = %v", err)
	}
	if _, err := app.UpdateThreadMode(thread.ID, "chat"); err != nil {
		t.Fatalf("UpdateThreadMode() error = %v", err)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "chat" {
		t.Fatalf("final Mode = %q, want chat", stored.Mode)
	}
}

func TestCreateThreadAcceptsValidInteractionMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, string(provider.Codex), "/tmp/workspace", "gpt-5.4", "plan")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.Mode != "plan" {
		t.Fatalf("Mode = %q, want plan", thread.Mode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "plan" {
		t.Fatalf("stored Mode = %q, want plan", stored.Mode)
	}
}

func TestCreateThreadDefaultsEmptyMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, string(provider.Codex), "/tmp/workspace", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.Mode != "chat" {
		t.Fatalf("Mode = %q, want chat", thread.Mode)
	}
}

func TestCreateThreadRejectsInvalidMode(t *testing.T) {
	app := newTestAppWithStore(t)
	for _, mode := range []string{"bogus", "DISCUSSION", "discussion"} {
		if _, err := createTestThread(t, app, string(provider.Codex), "/tmp/workspace", "gpt-5.4", mode); err == nil {
			t.Fatalf("CreateThread(%q) error = nil, want validation error", mode)
		}
	}
}

func TestCreateThreadAllowsDesign(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/workspace", "claude-sonnet-4-6", "design")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.Mode != "design" {
		t.Fatalf("Mode = %q, want design", thread.Mode)
	}
}

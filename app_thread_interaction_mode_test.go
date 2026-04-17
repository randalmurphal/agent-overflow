package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestSetThreadInteractionModePersistsValidMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-mode-set")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	for _, mode := range []string{"default", "plan", "design", "discussion"} {
		got, err := app.SetThreadInteractionMode(thread.ID, mode)
		if err != nil {
			t.Fatalf("SetThreadInteractionMode(%q) error = %v", mode, err)
		}
		if got.InteractionMode != mode {
			t.Fatalf("returned InteractionMode = %q, want %q", got.InteractionMode, mode)
		}
		stored, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Fatalf("GetThread() error = %v", err)
		}
		if stored.InteractionMode != mode {
			t.Fatalf("stored InteractionMode = %q, want %q", stored.InteractionMode, mode)
		}
	}
}

func TestSetThreadInteractionModeRejectsInvalidMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-mode-invalid")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	for _, mode := range []string{"nonsense", "DEFAULT", " ", "debate", "PLAN"} {
		if _, err := app.SetThreadInteractionMode(thread.ID, mode); err == nil {
			t.Fatalf("SetThreadInteractionMode(%q) error = nil, want validation error", mode)
		} else if !strings.Contains(err.Error(), "invalid interaction mode") {
			t.Fatalf("SetThreadInteractionMode(%q) error = %v, want invalid-mode message", mode, err)
		}
	}
}

func TestSetThreadInteractionModeRejectsEmpty(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-mode-empty-arg")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if _, err := app.SetThreadInteractionMode(thread.ID, ""); err == nil {
		t.Fatal("SetThreadInteractionMode(\"\") error = nil, want validation error")
	}
}

func TestSetThreadInteractionModeUnknownThread(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.SetThreadInteractionMode("does-not-exist", "plan"); err == nil {
		t.Fatal("SetThreadInteractionMode(unknown) error = nil, want not-found")
	}
}

func TestSetThreadInteractionModeRoundTripPersists(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-mode-round-trip")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.SetThreadInteractionMode(thread.ID, "plan"); err != nil {
		t.Fatalf("SetThreadInteractionMode() error = %v", err)
	}
	if _, err := app.SetThreadInteractionMode(thread.ID, "design"); err != nil {
		t.Fatalf("SetThreadInteractionMode() error = %v", err)
	}
	if _, err := app.SetThreadInteractionMode(thread.ID, "default"); err != nil {
		t.Fatalf("SetThreadInteractionMode() error = %v", err)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.InteractionMode != "default" {
		t.Fatalf("final InteractionMode = %q, want default", stored.InteractionMode)
	}
}

func TestCreateThreadAcceptsValidInteractionMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := app.CreateThread(string(provider.Codex), "/tmp/workspace", "gpt-5.4", "plan")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.InteractionMode != "plan" {
		t.Fatalf("InteractionMode = %q, want plan", thread.InteractionMode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.InteractionMode != "plan" {
		t.Fatalf("stored InteractionMode = %q, want plan", stored.InteractionMode)
	}
}

func TestCreateThreadDefaultsEmptyMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := app.CreateThread(string(provider.Codex), "/tmp/workspace", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.InteractionMode != "default" {
		t.Fatalf("InteractionMode = %q, want default", thread.InteractionMode)
	}
}

func TestCreateThreadRejectsInvalidMode(t *testing.T) {
	app := newTestAppWithStore(t)
	for _, mode := range []string{"bogus", "DISCUSSION", "discussion"} {
		if _, err := app.CreateThread(string(provider.Codex), "/tmp/workspace", "gpt-5.4", mode); err == nil {
			t.Fatalf("CreateThread(%q) error = nil, want validation error", mode)
		}
	}
}

func TestCreateThreadAllowsDesign(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := app.CreateThread(string(provider.Claude), "/tmp/workspace", "claude-sonnet-4-6", "design")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.InteractionMode != "design" {
		t.Fatalf("InteractionMode = %q, want design", thread.InteractionMode)
	}
}

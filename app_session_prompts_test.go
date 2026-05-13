package main

import (
	"testing"
)

func TestThreadSystemPromptHandlesNilMap(t *testing.T) {
	// Construct an App directly so threadSystemPrompts starts nil. The
	// lookup path must not panic on a nil map.
	app := &App{}
	if got := app.threadSystemPrompt("any"); got != "" {
		t.Fatalf("threadSystemPrompt(nil map) = %q, want empty", got)
	}
}

func TestSetAndGetThreadSystemPrompt(t *testing.T) {
	app := newTestAppWithStore(t)

	app.setThreadSystemPrompt("thread-a", "Alpha prompt")
	app.setThreadSystemPrompt("thread-b", "Beta prompt")

	if got := app.threadSystemPrompt("thread-a"); got != "Alpha prompt" {
		t.Fatalf("thread-a = %q, want Alpha prompt", got)
	}
	if got := app.threadSystemPrompt("thread-b"); got != "Beta prompt" {
		t.Fatalf("thread-b = %q, want Beta prompt", got)
	}
	if got := app.threadSystemPrompt("thread-missing"); got != "" {
		t.Fatalf("thread-missing = %q, want empty", got)
	}
}

func TestSetThreadSystemPromptTrimsAndRejectsBlanks(t *testing.T) {
	app := newTestAppWithStore(t)

	// Blank thread ID ignored.
	app.setThreadSystemPrompt("   ", "blank-id prompt")
	if got := app.threadSystemPrompt("   "); got != "" {
		t.Fatalf("blank ID retained prompt: %q", got)
	}

	// Blank prompt ignored (does not overwrite an existing one either).
	app.setThreadSystemPrompt("thread-trim", "original")
	app.setThreadSystemPrompt("thread-trim", "   \t")
	if got := app.threadSystemPrompt("thread-trim"); got != "original" {
		t.Fatalf("prompt = %q, want original preserved", got)
	}

	// Whitespace around values is trimmed before storage.
	app.setThreadSystemPrompt("  thread-trim-ws  ", "  padded prompt  ")
	if got := app.threadSystemPrompt("thread-trim-ws"); got != "padded prompt" {
		t.Fatalf("prompt = %q, want trimmed", got)
	}
}

func TestClearThreadSystemPromptRemovesEntry(t *testing.T) {
	app := newTestAppWithStore(t)

	app.setThreadSystemPrompt("thread-clear", "to be cleared")
	if got := app.threadSystemPrompt("thread-clear"); got != "to be cleared" {
		t.Fatalf("seed prompt = %q", got)
	}

	app.clearThreadSystemPrompt("thread-clear")

	if got := app.threadSystemPrompt("thread-clear"); got != "" {
		t.Fatalf("prompt after clear = %q, want empty", got)
	}
}

func TestClearThreadSystemPromptIgnoresBlankID(t *testing.T) {
	app := newTestAppWithStore(t)

	app.setThreadSystemPrompt("thread-keep", "kept")
	// Blank ID is a no-op — the existing entry must remain.
	app.clearThreadSystemPrompt("   ")
	if got := app.threadSystemPrompt("thread-keep"); got != "kept" {
		t.Fatalf("unrelated prompt wiped: %q", got)
	}
}

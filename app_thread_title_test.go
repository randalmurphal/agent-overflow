package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestMaybeGenerateThreadTitleAppliesGeneratedTitleAndEmits covers the happy
// path of maybeGenerateThreadTitle → generatedThreadTitle →
// applyGeneratedThreadTitle. The thread title advances from the default to
// the generated value, and a provider "thread_renamed" event is emitted.
func TestMaybeGenerateThreadTitleAppliesGeneratedTitleAndEmits(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-happy")
	thread.Title = defaultGeneratedThreadTitle
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(got store.Thread, message string) (string, error) {
		if got.ID != thread.ID {
			t.Fatalf("generate called with thread %q, want %q", got.ID, thread.ID)
		}
		if message != "fix the reconnect bug" {
			t.Fatalf("generate message = %q, want first user turn", message)
		}
		return "Reconnect spinner fix", nil
	}

	emitted := make(chan provider.ProviderEvent, 1)
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventThreadRenamed {
			emitted <- evt
		}
	}

	app.maybeGenerateThreadTitle(thread, "fix the reconnect bug", false)

	select {
	case evt := <-emitted:
		if evt.ThreadID != thread.ID {
			t.Fatalf("event threadID = %q, want %q", evt.ThreadID, thread.ID)
		}
		if evt.Content != "Reconnect spinner fix" {
			t.Fatalf("event content = %q", evt.Content)
		}
		if !strings.Contains(string(evt.Meta), `"newTitle":"Reconnect spinner fix"`) {
			t.Fatalf("event meta = %s, want newTitle", string(evt.Meta))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rename event")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Reconnect spinner fix" {
		t.Fatalf("stored title = %q", stored.Title)
	}
}

// TestMaybeGenerateThreadTitleSkipsCodexThread enforces the Claude-only gate
// in maybeGenerateThreadTitle. Codex threads must not spawn the title-
// generation subprocess (and the test-injection fn must not be called).
// This is a protocol-adjacent regression guard.
func TestMaybeGenerateThreadTitleSkipsCodexThread(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-codex")
	thread.Title = defaultGeneratedThreadTitle
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string) (string, error) {
		calls <- struct{}{}
		return "Should not be applied", nil
	}

	emitted := make(chan provider.ProviderEvent, 1)
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventThreadRenamed {
			emitted <- evt
		}
	}

	app.maybeGenerateThreadTitle(thread, "fix the reconnect bug", false)

	// Give the (would-be) goroutine a chance to run; it must not actually
	// invoke the fn or emit a rename event.
	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called for Codex thread")
	case <-emitted:
		t.Fatal("emitted rename event for Codex thread")
	case <-time.After(150 * time.Millisecond):
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != defaultGeneratedThreadTitle {
		t.Fatalf("stored title = %q, want %q (unchanged)", stored.Title, defaultGeneratedThreadTitle)
	}
}

// TestMaybeGenerateThreadTitleSkipsWhenPriorItemsExist ensures we only
// generate titles on the first turn, not on every subsequent send.
func TestMaybeGenerateThreadTitleSkipsWhenPriorItemsExist(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-prior")
	thread.Title = defaultGeneratedThreadTitle
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string) (string, error) {
		calls <- struct{}{}
		return "Late generated", nil
	}

	app.maybeGenerateThreadTitle(thread, "another user message", true)

	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called when prior items exist")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestMaybeGenerateThreadTitleSkipsWhenTitleCustom ensures a thread that has
// already been renamed (title is NOT the default) is left alone.
func TestMaybeGenerateThreadTitleSkipsWhenTitleCustom(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-custom")
	thread.Title = "Custom user title"
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string) (string, error) {
		calls <- struct{}{}
		return "Regenerated", nil
	}

	app.maybeGenerateThreadTitle(thread, "pick me", false)

	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called when title is not default")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestMaybeGenerateThreadTitleSkipsOnBlankContent covers the empty-message
// guard that prevents Claude from being asked to title a no-op message.
func TestMaybeGenerateThreadTitleSkipsOnBlankContent(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-blank")
	thread.Title = defaultGeneratedThreadTitle
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string) (string, error) {
		calls <- struct{}{}
		return "Unexpected", nil
	}

	app.maybeGenerateThreadTitle(thread, "   \n\t", false)

	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called on blank content")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestMaybeGenerateThreadTitleSwallowsSubprocessError ensures a title-gen
// failure does NOT panic, does NOT update the title, and does NOT emit a
// rename event — it logs and moves on.
func TestMaybeGenerateThreadTitleSwallowsSubprocessError(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-error")
	thread.Title = defaultGeneratedThreadTitle
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	done := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string) (string, error) {
		defer func() { done <- struct{}{} }()
		return "", errors.New("subprocess boom")
	}

	emitted := make(chan provider.ProviderEvent, 1)
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventThreadRenamed {
			emitted <- evt
		}
	}

	app.maybeGenerateThreadTitle(thread, "fix the reconnect bug", false)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("generateThreadTitleFn never called")
	}

	select {
	case <-emitted:
		t.Fatal("rename event emitted despite subprocess error")
	case <-time.After(150 * time.Millisecond):
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != defaultGeneratedThreadTitle {
		t.Fatalf("stored title = %q, want unchanged", stored.Title)
	}
}

// TestMaybeGenerateThreadTitleIgnoresEmptyResponse enforces the behavior when
// the title generator returns an empty string: no rename event, no store
// update. (Sanitization converts empty input into the default title, which
// is also treated as "skip" by the callsite.)
func TestMaybeGenerateThreadTitleIgnoresEmptyResponse(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-empty-response")
	thread.Title = defaultGeneratedThreadTitle
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	done := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string) (string, error) {
		defer func() { done <- struct{}{} }()
		return "", nil
	}

	emitted := make(chan provider.ProviderEvent, 1)
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventThreadRenamed {
			emitted <- evt
		}
	}

	app.maybeGenerateThreadTitle(thread, "something", false)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("generateThreadTitleFn never called")
	}

	select {
	case <-emitted:
		t.Fatal("rename event emitted despite empty title response")
	case <-time.After(150 * time.Millisecond):
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != defaultGeneratedThreadTitle {
		t.Fatalf("stored title = %q, want unchanged default", stored.Title)
	}
}

// TestApplyGeneratedThreadTitleCompareAndSwapSkipsWhenTitleChanged exercises
// the race guard: if the user renamed the thread between the generation call
// and the apply call, UpdateTitleIfCurrent reports 0 rows affected and the
// rename event must NOT be emitted.
func TestApplyGeneratedThreadTitleCompareAndSwapSkipsWhenTitleChanged(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-cas-lost")
	thread.Title = "User picked this"
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	emitted := make(chan provider.ProviderEvent, 1)
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventThreadRenamed {
			emitted <- evt
		}
	}

	if err := app.applyGeneratedThreadTitle(thread.ID, "Generated fallback"); err != nil {
		t.Fatalf("applyGeneratedThreadTitle() error = %v", err)
	}

	select {
	case <-emitted:
		t.Fatal("rename event emitted when current title != default (CAS should fail)")
	case <-time.After(150 * time.Millisecond):
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "User picked this" {
		t.Fatalf("stored title = %q, want user-picked preserved", stored.Title)
	}
}

func TestSanitizeGeneratedThreadTitle(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain", raw: "Fix the bug", want: "Fix the bug"},
		{name: "trims whitespace", raw: "  Trimmed  ", want: "Trimmed"},
		{name: "strips quotes", raw: `"quoted title"`, want: "quoted title"},
		{name: "collapses whitespace", raw: "lots   of\tspaces", want: "lots of spaces"},
		{name: "keeps first line only", raw: "First line\nSecond", want: "First line"},
		{name: "empty falls back to default", raw: "   ", want: defaultGeneratedThreadTitle},
		{
			name: "truncates when over 50 runes",
			raw:  "This is a very long title that exceeds fifty runes for sure indeed",
			want: "This is a very long title that exceeds fifty ru...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeGeneratedThreadTitle(tt.raw)
			if got != tt.want {
				t.Fatalf("sanitizeGeneratedThreadTitle(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBuildThreadTitlePromptIncludesMessage(t *testing.T) {
	got := buildThreadTitlePrompt("fix the login bug")
	if !strings.Contains(got, "fix the login bug") {
		t.Fatalf("prompt missing user message: %q", got)
	}
	if !strings.Contains(got, "Return a JSON object") {
		t.Fatalf("prompt missing JSON object directive: %q", got)
	}
}

func TestDecodeClaudeThreadTitleExtractsFromStructuredOutput(t *testing.T) {
	payload := []byte(`
{"some":"preamble"}
{"structured_output":{"title":"Refactor worktree rename"}}
`)
	got, err := decodeClaudeThreadTitle(payload)
	if err != nil {
		t.Fatalf("decodeClaudeThreadTitle() error = %v", err)
	}
	if got != "Refactor worktree rename" {
		t.Fatalf("title = %q", got)
	}
}

func TestDecodeClaudeThreadTitleErrorsOnEmptyOutput(t *testing.T) {
	_, err := decodeClaudeThreadTitle([]byte("   \n\t"))
	if err == nil {
		t.Fatal("decodeClaudeThreadTitle(blank) error = nil, want empty-output error")
	}
}

func TestDecodeClaudeThreadTitleErrorsOnMalformedJSON(t *testing.T) {
	_, err := decodeClaudeThreadTitle([]byte("not-json"))
	if err == nil {
		t.Fatal("decodeClaudeThreadTitle(garbage) error = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode claude structured output") {
		t.Fatalf("decodeClaudeThreadTitle() error = %v, want decode context", err)
	}
}

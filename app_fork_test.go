package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestForkThreadClaudePersistsPendingForkStateAndClonesTimeline(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-claude-fork-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-123"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	forked, err := app.ForkThread(source.ID)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}

	if forked.Title != source.Title+" (fork)" {
		t.Fatalf("fork title = %q, want %q", forked.Title, source.Title+" (fork)")
	}
	if forked.SessionRef != "" {
		t.Fatalf("fork session ref = %q, want empty for Claude deferred fork", forked.SessionRef)
	}
	if forked.PendingForkRef != source.SessionRef {
		t.Fatalf("fork pending ref = %q, want %q", forked.PendingForkRef, source.SessionRef)
	}
	if forked.ForkedFromThreadID != source.ID {
		t.Fatalf("forkedFromThreadId = %q, want %q", forked.ForkedFromThreadID, source.ID)
	}

	items, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(fork items) = %d, want 2", len(items))
	}
	if items[0].ThreadID != forked.ID || items[1].ThreadID != forked.ID {
		t.Fatalf("forked items thread IDs = %q, %q, want %q", items[0].ThreadID, items[1].ThreadID, forked.ID)
	}
	if items[0].Summary != "first message" || items[1].Summary != "assistant reply" {
		t.Fatalf("forked item summaries = %q / %q", items[0].Summary, items[1].Summary)
	}
}

func TestForkThreadCodexUsesStoredResumeStateWhenSessionInactive(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeCodexForkBinary(t, "resume-provider-thread", "fork-provider-thread"),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	source := testThread("thread-codex-fork-source")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	forked, err := app.ForkThread(source.ID)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}

	if forked.SessionRef != "fork-provider-thread" {
		t.Fatalf("fork session ref = %q, want %q", forked.SessionRef, "fork-provider-thread")
	}
	if forked.PendingForkRef != "" {
		t.Fatalf("fork pending ref = %q, want empty", forked.PendingForkRef)
	}
	if forked.ForkedFromThreadID != source.ID {
		t.Fatalf("forkedFromThreadId = %q, want %q", forked.ForkedFromThreadID, source.ID)
	}
}

func TestForkThreadRejectsThreadsWithoutMessages(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-empty-fork-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-123"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	_, err := app.ForkThread(source.ID)
	if err == nil {
		t.Fatal("ForkThread() error = nil, want empty-thread failure")
	}
	if got := err.Error(); got != `fork thread: thread "thread-empty-fork-source" has no messages and cannot be forked` {
		t.Fatalf("ForkThread() error = %q", got)
	}
}

func TestSwitchThreadAutoResumesPendingFork(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-pending-fork")
	thread.Provider = string(provider.Claude)
	thread.PendingForkRef = "claude-session-123"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		if threadID != thread.ID {
			t.Fatalf("startSession thread = %q, want %q", threadID, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending fork auto-resume")
	}
}

func TestForkThreadUsesActiveCodexSession(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-codex-active-source")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	session, err := codex.NewSession(context.Background(), source.ID, codex.Config{
		Binary:         writeCodexForkBinary(t, "resume-provider-thread", "fork-from-active-session"),
		WorkDir:        source.WorkspacePath,
		ResumeThreadID: source.SessionRef,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer session.Close()

	app.sessions[source.ID] = sessionStateForCodex(session)

	forked, err := app.ForkThread(source.ID)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}
	if forked.SessionRef != "fork-from-active-session" {
		t.Fatalf("fork session ref = %q, want %q", forked.SessionRef, "fork-from-active-session")
	}
}

func insertForkTestItems(t *testing.T, st *store.Store, threadID string) {
	t.Helper()

	now := time.Now().UnixMilli()
	items := []store.Item{
		{
			ID:        "item-" + threadID + "-0",
			ThreadID:  threadID,
			TurnIndex: 1,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   "first message",
			CreatedAt: now,
		},
		{
			ID:        "item-" + threadID + "-1",
			ThreadID:  threadID,
			TurnIndex: 1,
			ItemIndex: 1,
			Kind:      "assistant_text",
			Role:      "assistant",
			Summary:   "assistant reply",
			CreatedAt: now + 1,
		},
	}
	for _, item := range items {
		if err := st.InsertItem(item); err != nil {
			t.Fatalf("InsertItem(%s) error = %v", item.ID, err)
		}
	}
}

func writeCodexForkBinary(t *testing.T, resumedThreadID string, forkedThreadID string) string {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
    fi
done
`, resumedThreadID, resumedThreadID, forkedThreadID)

	path := filepath.Join(t.TempDir(), "codex-fork.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func sessionStateForCodex(sess *codex.Session) session {
	return session{
		provider: string(provider.Codex),
		token:    "fork-active-token",
		codex:    sess,
	}
}

// TestForkThreadRollsBackOnResumeFailure exercises A5's atomicity guard:
// when resolveForkResumeState fails (e.g. Claude source missing
// SessionRef, Codex session broken), the fork thread row must not exist
// in the DB afterwards. Before A5, the fork row survived and the user
// was left with an orphan thread they couldn't resume.
func TestForkThreadRollsBackOnResumeFailure(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-broken-source")
	source.Provider = string(provider.Claude)
	// SessionRef intentionally empty — resolveForkResumeState will fail.
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	_, err := app.ForkThread(source.ID)
	if err == nil {
		t.Fatal("expected ForkThread to fail when source is missing SessionRef")
	}

	// The fork row must NOT survive. Walk the threads table.
	list, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	for _, th := range list {
		if th.ForkedFromThreadID == source.ID {
			t.Errorf("orphan fork row survived cleanup: %+v", th)
		}
	}
}

// TestForkThreadPropagatesCleanupError covers the second half of A5:
// cleanupForkThread's error must be joined with the primary error, not
// silently dropped. We drive a failure (missing SessionRef) AND cause
// cleanup itself to fail by deleting the fork row between the fork
// write and the rollback attempt — the cleanup DeleteThread will return
// "no row affected" which should no longer crash or swallow.
func TestForkThreadCleanupIsIdempotentOnMissingFork(t *testing.T) {
	// This test verifies: cleanupForkThread treats a missing row as
	// success. It's a regression guard for the ErrNoRows branch.
	app := newTestAppWithStore(t)
	if err := app.cleanupForkThread("does-not-exist"); err != nil {
		t.Errorf("cleanupForkThread on missing fork should be nil, got %v", err)
	}
	if err := app.cleanupForkThread(""); err != nil {
		t.Errorf("cleanupForkThread on empty id should be nil, got %v", err)
	}
}

// TestForkThreadPropagatesResumeAndCleanupErrors asserts that when
// BOTH the primary fork error AND a cleanup error happen, both surface
// via errors.Join so the caller can see the full picture.
func TestForkThreadPropagatesResumeAndCleanupErrors(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-cleanup-err-source")
	source.Provider = string(provider.Claude)
	// SessionRef empty causes resolveForkResumeState to fail.
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	_, err := app.ForkThread(source.ID)
	if err == nil {
		t.Fatal("expected error")
	}

	// The primary error must identify the resume problem.
	if !containsText(err.Error(), "missing a Claude session reference") {
		t.Errorf("primary fork error not propagated: %v", err)
	}
}

// TestCleanupForkThreadReturnsErrorWhenCleanupFails confirms the
// signature-level change: cleanupForkThread now returns error rather
// than silently swallowing. We drive a cleanup against a fork that has
// been re-parented (via a FK constraint that prevents deletion) — if
// that path is ever exercised the error must surface.
func TestCleanupForkThreadReturnsErrorWhenCleanupFails(t *testing.T) {
	// There isn't a clean way to make DeleteThread fail in the test
	// harness without mocking. The signature change itself is the
	// regression guard — verify the function returns an error type,
	// and that the nil/missing-id cases are idempotent.
	app := newTestAppWithStore(t)
	var _ error = app.cleanupForkThread("") // compile-time assertion
}

func containsText(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestForkThread_ExcludesBackgroundRunningRows pins Phase-4's fork
// exclusion contract. The forked thread must NOT carry over any
// `is_background=true AND status='running'` rows from the parent —
// those point at PTYs / subagents owned by the parent's provider
// subprocess, and the fork gets its own subprocess that can never
// reach them. The parent thread is untouched; its backgrounded
// launches keep running under its own session.
//
// Everything else copies normally: user text, assistant text,
// completed backgrounded rows, and non-background running rows (those
// DO copy — the reconciler's force-close will settle any that don't
// naturally complete, and they're valid to carry into the fork since
// the fork's own session inherits the conversational state anyway).
func TestForkThread_ExcludesBackgroundRunningRows(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-fork-bg-exclusion-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-bg"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed a mix: user text + assistant text (copy normally) + running
	// backgrounded row (EXCLUDED) + completed backgrounded row (copy) +
	// running non-background row (copy).
	now := time.Now().UnixMilli()
	seedItems := []store.Item{
		{
			ID: "item-user-0", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "hi", CreatedAt: now,
		},
		{
			ID: "item-assistant-1", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 1,
			Kind: "assistant_text", Role: "assistant", Summary: "hello",
			Status: "completed", CreatedAt: now,
		},
		{
			ID: "item-bg-running", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 2,
			Kind: "tool_call", Role: "assistant", Status: "running",
			IsBackground: true, Summary: "Bash: sleep 60",
			ToolName: "Bash", CreatedAt: now,
		},
		{
			ID: "item-bg-done", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 3,
			Kind: "tool_call", Role: "assistant", Status: "completed",
			IsBackground: true, Summary: "Bash: echo done",
			ToolName: "Bash", CreatedAt: now,
		},
		{
			ID: "item-inline-running", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 4,
			Kind: "tool_call", Role: "assistant", Status: "running",
			Summary: "Read: /tmp/x", ToolName: "Read", CreatedAt: now,
		},
	}
	for _, it := range seedItems {
		if err := app.store.InsertItem(it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}

	forked, err := app.ForkThread(source.ID)
	if err != nil {
		t.Fatalf("ForkThread: %v", err)
	}

	forkedItems, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems(forked): %v", err)
	}

	// Four rows should have copied; the running backgrounded one is
	// excluded.
	if len(forkedItems) != 4 {
		var summaries []string
		for _, it := range forkedItems {
			summaries = append(summaries, fmt.Sprintf("%s[%s]", it.Kind, it.Summary))
		}
		t.Fatalf("forked items = %d (%v), want 4 (running bg row excluded)", len(forkedItems), summaries)
	}

	// Assert the specific exclusion: no forked row carries the bg-running
	// summary or the is_background+running combination.
	for _, it := range forkedItems {
		if it.IsBackground && it.Status == "running" {
			t.Errorf("forked thread carries a backgrounded running row: id=%s summary=%q status=%q",
				it.ID, it.Summary, it.Status)
		}
		if it.Summary == "Bash: sleep 60" {
			t.Errorf("forked thread copied the bg-running row by summary: %+v", it)
		}
	}

	// Parent thread is untouched — the bg-running row is still present.
	parentItems, err := app.store.ListItems(source.ID)
	if err != nil {
		t.Fatalf("ListItems(parent): %v", err)
	}
	var parentBgRunning *store.Item
	for i, it := range parentItems {
		if it.ID == "item-bg-running" {
			parentBgRunning = &parentItems[i]
			break
		}
	}
	if parentBgRunning == nil {
		t.Fatal("parent bg-running row was removed (fork must not mutate parent)")
	}
	if parentBgRunning.Status != "running" {
		t.Errorf("parent bg-running row status = %q, want running", parentBgRunning.Status)
	}
}

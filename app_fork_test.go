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
	source.ProjectPath = source.WorkspacePath
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
	source.ProjectPath = source.WorkspacePath
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
			Kind:      "text",
			Role:      "user",
			Summary:   "first message",
			CreatedAt: now,
		},
		{
			ID:        "item-" + threadID + "-1",
			ThreadID:  threadID,
			TurnIndex: 1,
			ItemIndex: 1,
			Kind:      "text",
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

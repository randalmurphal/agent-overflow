package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

func TestSendMessageHappyPath(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var sentThread, sentContent string
	app.sendMessageFn = func(threadID, content string) error {
		sentThread = threadID
		sentContent = content
		return nil
	}
	// sendMessageFn bypasses session lookup, but sendMessage still checks
	// for it first. Populate the session map so the real codepath up to the
	// sendMessageFn shortcut is exercised.
	app.sessions[thread.ID] = session{provider: string(provider.Codex)}

	if err := app.SendMessage(thread.ID, "Hello"); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if sentThread != thread.ID {
		t.Fatalf("sent threadID = %q, want %q", sentThread, thread.ID)
	}
	if sentContent != "Hello" {
		t.Fatalf("sent content = %q, want Hello", sentContent)
	}
}

func TestSendMessageNoActiveSessionError(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-no-session")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	err := app.SendMessage(thread.ID, "Hello")
	if err == nil {
		t.Fatal("SendMessage() error = nil, want no active session error")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("SendMessage() error = %v, want no active session", err)
	}
}

func TestSendMessageIncrementsTurnIndex(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-turn-index")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Seed a prior turn so the next user message should get turn index 1.
	if err := app.store.InsertItem(store.Item{
		ID:        "existing-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "assistant",
		Summary:   "prior turn",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem() error = %v", err)
	}

	// Use a real claude session backed by a passthrough binary so sendMessage
	// exercises the full code path including item persistence and turn indexing.
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "Next message"); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}

	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" {
			userItem = item
			break
		}
	}
	if userItem.ID == "" {
		t.Fatal("expected persisted user item")
	}
	if userItem.TurnIndex != 1 {
		t.Fatalf("user item TurnIndex = %d, want 1", userItem.TurnIndex)
	}
	if userItem.Summary != "Next message" {
		t.Fatalf("user item Summary = %q, want Next message", userItem.Summary)
	}
}

func TestSendMessageRenamesTemporaryWorktreeBranchOnFirstTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	thread := testThread("thread-send-rename-worktree")
	thread.Provider = string(provider.Claude)
	thread.ProjectPath = repo
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(thread.ID, "")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	thread, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = worktreePath
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}

	app.generateBranchNameFn = func(thread store.Thread, message string) (string, error) {
		if message != "Fix reconnect spinner on resume" {
			t.Fatalf("message = %q, want first user turn", message)
		}
		return "feature/reconnect-spinner", nil
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: worktreePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "Fix reconnect spinner on resume"); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "forge/feature/reconnect-spinner" {
		t.Fatalf("stored Branch = %q, want forge/feature/reconnect-spinner", stored.Branch)
	}

	status, err := app.GetGitStatus(thread.ID)
	if err != nil {
		t.Fatalf("GetGitStatus() error = %v", err)
	}
	if status.Branch != "forge/feature/reconnect-spinner" {
		t.Fatalf("status.Branch = %q, want forge/feature/reconnect-spinner", status.Branch)
	}
}

func TestRespondToApprovalNoActiveSessionError(t *testing.T) {
	app := newTestAppWithStore(t)

	err := app.RespondToApproval("nonexistent-thread", provider.ApprovalResponse{
		RequestID: "1",
		Decision:  "accept",
	})
	if err == nil {
		t.Fatal("RespondToApproval() error = nil, want no active session error")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("RespondToApproval() error = %v, want no active session", err)
	}
}

func TestRespondToApprovalHappyPathClaude(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-approval-claude")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	err = app.RespondToApproval(thread.ID, provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	})
	if err != nil {
		t.Fatalf("RespondToApproval() error = %v", err)
	}
}

func TestRespondToApprovalNoProviderError(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-approval-no-provider")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Session exists but has no provider set -- both claude and codex are nil.
	app.sessions[thread.ID] = session{
		provider: "unknown",
		token:    "test-token",
	}

	err := app.RespondToApproval(thread.ID, provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	})
	if err == nil {
		t.Fatal("RespondToApproval() error = nil, want session has no provider error")
	}
	if !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("RespondToApproval() error = %v, want no provider", err)
	}
}

func TestInterruptTurnNoActiveSessionError(t *testing.T) {
	app := newTestAppWithStore(t)

	err := app.InterruptTurn("nonexistent-thread")
	if err == nil {
		t.Fatal("InterruptTurn() error = nil, want no active session error")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("InterruptTurn() error = %v, want no active session", err)
	}
}

func TestInterruptTurnHappyPathClaude(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-interrupt-claude")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	// Interrupt writes to the process stdin; with the passthrough binary this succeeds.
	err = app.InterruptTurn(thread.ID)
	if err != nil {
		t.Fatalf("InterruptTurn() error = %v", err)
	}
}

func TestStopSessionRemovesFromMap(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-stop")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	app.mu.Lock()
	_, exists := app.sessions[thread.ID]
	app.mu.Unlock()
	if exists {
		t.Fatalf("sessions[%s] still present after StopSession", thread.ID)
	}
}

func TestStopSessionNoSessionIsNoOp(t *testing.T) {
	app := newTestAppWithStore(t)

	// StopSession on a thread with no session should not error.
	if err := app.StopSession("nonexistent-thread"); err != nil {
		t.Fatalf("StopSession() error = %v, want nil", err)
	}
}

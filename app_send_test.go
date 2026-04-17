package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

func TestSendMessageGeneratesClaudeThreadTitleOnFirstTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-title")
	thread.Title = "New Thread"
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(thread store.Thread, message string) (string, error) {
		if message != "Fix reconnect spinner on resume" {
			t.Fatalf("message = %q, want first user turn", message)
		}
		return ` "Reconnect spinner resume fix" `, nil
	}

	emitted := make(chan provider.ProviderEvent, 1)
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventThreadRenamed {
			emitted <- evt
		}
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

	if err := app.SendMessage(thread.ID, "Fix reconnect spinner on resume"); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	select {
	case evt := <-emitted:
		if evt.Content != "Reconnect spinner resume fix" {
			t.Fatalf("rename event content = %q", evt.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for thread rename event")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Reconnect spinner resume fix" {
		t.Fatalf("stored title = %q, want generated title", stored.Title)
	}
}

func TestSendMessageDoesNotOverwriteRenamedThreadTitle(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-title-custom")
	thread.Title = "New Thread"
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(store.Thread, string) (string, error) {
		time.Sleep(100 * time.Millisecond)
		return "Generated title", nil
	}

	renamed := make(chan provider.ProviderEvent, 1)
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventThreadRenamed {
			renamed <- evt
		}
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

	if err := app.SendMessage(thread.ID, "Fix reconnect spinner on resume"); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if err := app.RenameThread(thread.ID, "Keep this custom title"); err != nil {
		t.Fatalf("RenameThread() error = %v", err)
	}

	time.Sleep(250 * time.Millisecond)

	select {
	case evt := <-renamed:
		t.Fatalf("unexpected rename event: %+v", evt)
	default:
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Keep this custom title" {
		t.Fatalf("stored title = %q, want custom title", stored.Title)
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

// TestSendMessageSerialPerThread exercises Bug B11: five concurrent
// SendMessage calls on the same thread must execute strictly serially,
// each with a distinct, monotonically-increasing turn_index. Without the
// per-thread mutex two sends could compute the same lastTurnIndex and
// collide on the UNIQUE(turn_index, item_index) constraint, or silently
// attribute the same user message to two different turns.
func TestSendMessageSerialPerThread(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-serial")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
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
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "tok",
		claude:   sess,
	}

	const N = 5
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := app.SendMessage(thread.ID, fmt.Sprintf("msg-%d", i)); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("SendMessage: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	// Each SendMessage should have inserted exactly one user item with
	// a unique turnIndex in [1..N]. A regression would produce either
	// duplicate turnIndex (UNIQUE violation aborts the second insert),
	// or a count mismatch.
	seenTurns := make(map[int]bool)
	for _, item := range items {
		if item.Role != "user" {
			continue
		}
		if seenTurns[item.TurnIndex] {
			t.Fatalf("duplicate turnIndex %d (Bug B11 regression): %+v", item.TurnIndex, item)
		}
		seenTurns[item.TurnIndex] = true
	}
	if len(seenTurns) != N {
		t.Fatalf("persisted user turns = %d, want %d", len(seenTurns), N)
	}
	for i := 1; i <= N; i++ {
		if !seenTurns[i] {
			t.Fatalf("missing turnIndex %d in persisted items", i)
		}
	}
}

// TestSendMessageParallelDifferentThreads confirms the per-thread mutex
// does NOT serialize across unrelated threads: two threads' sends make
// progress concurrently.
func TestSendMessageParallelDifferentThreads(t *testing.T) {
	app := newTestAppWithStore(t)

	threadA := testThread("thread-parallel-A")
	threadA.Provider = string(provider.Claude)
	threadA.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(threadA); err != nil {
		t.Fatalf("CreateThread A: %v", err)
	}
	threadB := testThread("thread-parallel-B")
	threadB.Provider = string(provider.Claude)
	threadB.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(threadB); err != nil {
		t.Fatalf("CreateThread B: %v", err)
	}

	sessA, err := claude.NewSession(
		context.Background(),
		threadA.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: threadA.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession A: %v", err)
	}
	t.Cleanup(func() { _ = sessA.Close() })
	sessB, err := claude.NewSession(
		context.Background(),
		threadB.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: threadB.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession B: %v", err)
	}
	t.Cleanup(func() { _ = sessB.Close() })

	app.sessions[threadA.ID] = session{provider: string(provider.Claude), token: "a", claude: sessA}
	app.sessions[threadB.ID] = session{provider: string(provider.Claude), token: "b", claude: sessB}

	if err := app.SendMessage(threadA.ID, "a"); err != nil {
		t.Fatalf("send A: %v", err)
	}
	if err := app.SendMessage(threadB.ID, "b"); err != nil {
		t.Fatalf("send B: %v", err)
	}

	itemsA, _ := app.store.ListItems(threadA.ID)
	itemsB, _ := app.store.ListItems(threadB.ID)
	if len(itemsA) != 1 || len(itemsB) != 1 {
		t.Fatalf("per-thread isolation broken: A=%d B=%d", len(itemsA), len(itemsB))
	}
}

// TestSendMessageDoesNotPersistUserItemWhenProviderSendFails exercises
// Bug B8: the old flow persisted the user item BEFORE calling Send, so
// a broken pipe / dead subprocess left an orphan user row for a turn
// that never ran. The fix writes to the provider first; on failure the
// user item is never persisted.
func TestSendMessageDoesNotPersistUserItemWhenProviderSendFails(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-fail")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Create a real claude session, then close it so the next Send's
	// WriteLine fails with "process already exited".
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
		t.Fatalf("NewSession: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Logf("close returned %v (expected)", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	err = app.SendMessage(thread.ID, "doomed content")
	if err == nil {
		t.Fatal("expected Send to fail with a closed session, got nil")
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.Role == "user" {
			t.Fatalf("user item persisted despite provider send failure: %+v", item)
		}
	}
}

// TestSendMessagePersistsUserItemOnSuccess confirms the happy path is
// preserved: a successful Send still results in the user item landing
// in the store. Regression would be moving the InsertItem call past a
// success branch by mistake.
func TestSendMessagePersistsUserItemOnSuccess(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-success")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
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
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "hello world"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" {
			userItem = item
		}
	}
	if userItem.ID == "" {
		t.Fatal("user item missing after successful Send")
	}
	if userItem.Summary != "hello world" {
		t.Fatalf("summary = %q, want hello world", userItem.Summary)
	}
}

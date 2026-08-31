package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// TestRespondToApprovalRejectsUntrackedCodexRequest covers the Codex routing branch in
// app_approval.go. The Claude branch and the "no active session" /
// "no provider" branches are already covered in app_send_test.go.
func TestRespondToApprovalRejectsUntrackedCodexRequest(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-approval-codex")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	binary := writeCodexForkBinary(t, "codex-session-approval", "codex-session-approval-fork")
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:  binary,
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "test-token",
		Codex:    sess,
	})

	err = app.RespondToApproval(thread.ID, provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	})
	if !errors.Is(err, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("RespondToApproval() error = %v, want stale interactive request", err)
	}
}

// TestRespondToApprovalPropagatesProviderError verifies that a provider-level
// error (e.g. a non-numeric Codex request ID) is surfaced to the caller
// instead of being swallowed. Regression guard for the Codex code path.
func TestRespondToApprovalPropagatesProviderError(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-approval-codex-error")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	binary := writeCodexForkBinary(t, "codex-session-approval-err", "codex-session-approval-err-fork")
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:  binary,
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "test-token",
		Codex:    sess,
	})

	// Non-numeric request ID — codex.RespondToApproval must refuse.
	err = app.RespondToApproval(thread.ID, provider.ApprovalResponse{
		RequestID: "not-a-number",
		Decision:  "accept",
	})
	if err == nil {
		t.Fatal("RespondToApproval() error = nil, want invalid request ID error")
	}
	if !strings.Contains(err.Error(), "invalid approval request ID") {
		t.Fatalf("RespondToApproval() error = %v, want invalid request ID context", err)
	}
}

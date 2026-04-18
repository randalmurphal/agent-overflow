package main

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// TestRespondToApprovalHappyPathCodex covers the Codex routing branch in
// app_approval.go. The Claude branch and the "no active session" /
// "no provider" branches are already covered in app_send_test.go; this adds
// the missing codex case so every branch of RespondToApproval has a test.
func TestRespondToApprovalHappyPathCodex(t *testing.T) {
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

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "test-token",
		codex:    sess,
	}

	// RequestID must parse as an int64 — RespondToApproval returns an error
	// otherwise. "42" is the same value used in the codex session tests.
	if err := app.RespondToApproval(thread.ID, provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	}); err != nil {
		t.Fatalf("RespondToApproval() error = %v", err)
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

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "test-token",
		codex:    sess,
	}

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

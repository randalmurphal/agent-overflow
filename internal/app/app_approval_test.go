package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// TestRespondToApprovalOnACodexSession covers the Codex routing branch in
// app_approval.go: a request the session does not track is stale, and a
// provider-level error (a non-numeric Codex request ID) reaches the caller
// instead of being swallowed. The Claude branch and the "no active session"
// / "no provider" branches are already covered in app_send_test.go.
func TestRespondToApprovalOnACodexSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-approval-codex")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	binary := writeCodexForkBinary(t, "codex-session-approval", "codex-session-approval-fork", "")
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Reaper:  procutiltest.Reaper(),
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
	respond := func(requestID string) error {
		return app.RespondToApproval(context.Background(), thread.ID, provider.ApprovalResponse{
			RequestID: requestID,
			Decision:  "accept",
		})
	}

	t.Run("untracked request", func(t *testing.T) {
		if err := respond("42"); !errors.Is(err, provider.ErrStaleInteractiveRequest) {
			t.Fatalf("RespondToApproval() error = %v, want stale interactive request", err)
		}
	})
	t.Run("provider error", func(t *testing.T) {
		// Non-numeric request ID: codex.RespondToApproval must refuse.
		err := respond("not-a-number")
		if err == nil {
			t.Fatal("RespondToApproval() error = nil, want invalid request ID error")
		}
		if !strings.Contains(err.Error(), "invalid approval request ID") {
			t.Fatalf("RespondToApproval() error = %v, want invalid request ID context", err)
		}
	})
}

package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestCodexNewSessionWithMock(t *testing.T) {
	// Create a mock Codex app-server script that responds to JSON-RPC requests.
	script := `#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -n "$id" ]; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
    fi
done
`
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	ctx := context.Background()
	eventCh := make(chan provider.ProviderEvent, 100)
	s, err := NewSession(ctx, testThread, Config{
		Binary:  scriptPath,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(evt provider.ProviderEvent) {
		eventCh <- evt
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if s.threadID != testThread {
		t.Errorf("threadID: got %q, want %q", s.threadID, testThread)
	}
	if s.rootThreadID() != "mock-thread-123" {
		t.Errorf("rootThreadID: got %q, want %q", s.rootThreadID(), "mock-thread-123")
	}

	// EventInit should have been emitted.
	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventInit {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventInit)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(evt.Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	if info.SessionID != "mock-thread-123" {
		t.Errorf("sessionID: got %q, want %q", info.SessionID, "mock-thread-123")
	}
	if info.Model != "test-model" {
		t.Errorf("model: got %q, want %q", info.Model, "test-model")
	}
}

// TestSessionStartVerifiesApprovalsReviewerEcho is the wire-level guard for
// the auto runtime mode on Codex. `ThreadStartParams` has no
// deny_unknown_fields, so a codex that predates `approvalsReviewer` accepts
// the request, drops the field, and hands back an ordinary user-reviewer
// thread. Nothing else on the wire reports this: `initialize` carries no
// version or capability list and `thread/started` does not carry the reviewer,
// so the handshake RESPONSE is the only place the drop is visible.
//
// The failure has to be an error rather than a downgrade. Continuing would run
// the session with a human on the other end of approvals while the thread row,
// the picker, and the user all say a reviewer is answering them.
func TestSessionStartVerifiesApprovalsReviewerEcho(t *testing.T) {
	cases := []struct {
		name         string
		mode         provider.RuntimeMode
		threadResult string
		wantErr      string
	}{
		{
			name:         "auto accepted and echoed",
			mode:         provider.RuntimeAuto,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"},\"approvalsReviewer\":\"auto_review\"}`,
		},
		{
			name:         "auto silently dropped by an old app-server",
			mode:         provider.RuntimeAuto,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"}}`,
			wantErr:      "auto_review",
		},
		{
			name:         "auto downgraded to the user reviewer",
			mode:         provider.RuntimeAuto,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"},\"approvalsReviewer\":\"user\"}`,
			wantErr:      "auto_review",
		},
		{
			name:         "non-auto tier tolerates an absent echo",
			mode:         provider.RuntimeApprovalRequired,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"}}`,
		},
		{
			name:         "non-auto tier rejects a sticky auto reviewer",
			mode:         provider.RuntimeApprovalRequired,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"},\"approvalsReviewer\":\"auto_review\"}`,
			wantErr:      "user",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:    "codex",
				RuntimeMode: tc.mode,
				WorkDir:     "/tmp",
			})
			cfg.Binary = codexReviewerEchoScript(t, capturePath, tc.threadResult)

			s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
			if s != nil {
				t.Cleanup(func() { _ = s.Close() })
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("NewSession: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("NewSession accepted a reviewer mismatch")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not name the requested reviewer %q", err, tc.wantErr)
				}
			}

			params := codexCapturedRequest(t, capturePath, "thread/start")
			want := codexApprovalsReviewer(tc.mode)
			if params["approvalsReviewer"] != want {
				t.Errorf("thread/start approvalsReviewer = %v, want %q", params["approvalsReviewer"], want)
			}
		})
	}
}

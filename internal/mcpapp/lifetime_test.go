//go:build !windows

package mcpapp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// An MCP sign-in waits on a person in a browser, so its provider process is
// handed to the service's reaper and ends with the backend rather than at a
// timeout. The Claude sign-in runs a session, configured here; the Codex one
// is started against a mock that never answers.
func TestAWorkspaceSignInProcessIsWatchedByTheReaper(t *testing.T) {
	reaper := &procutiltest.Recorder{}
	binary := mockexec.Write(t, filepath.Join(t.TempDir(), "codex"), "#!/bin/sh\nexec sleep 60\n")
	service := New(Deps{Reaper: reaper, ProviderBinary: func(string) string { return binary }})

	if got := service.claudeWorkspaceAuthConfig(t.TempDir()).Reaper; got != reaper {
		t.Fatalf("the Claude sign-in's reaper is %v, want the service's", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := service.startWorkspaceMCPAuth(ctx, string(provider.Codex), t.TempDir(), "server"); err == nil {
		t.Fatal("a sign-in the app-server never answered started")
	}
	if watched, _ := reaper.Calls(); len(watched) != 1 {
		t.Fatalf("watched %v, want the Codex sign-in's app-server", watched)
	}
}

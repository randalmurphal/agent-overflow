package mcpapp

import (
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider/claude"
)

// A Claude MCP sign-in is a session process against the user's home, so it
// carries the transcript retention a thread's session does.
func TestClaudeWorkspaceAuthCarriesCleanupPeriodDays(t *testing.T) {
	// The period is resolved for the sign-in's own workspace.
	service := New(Deps{Reaper: procutiltest.Reaper(), ClaudeRetention: func(workDir string) claude.Retention {
		if workDir != "/repo" {
			return claude.UserHomeRetention(0)
		}
		return claude.UserHomeRetention(90)
	}})
	if got := service.claudeWorkspaceAuthConfig("/repo").Retention; got != claude.UserHomeRetention(90) {
		t.Fatalf("Retention = %+v, want 90 days", got)
	}
	// With no dependency the sign-in gives no answer, which refuses it.
	if got := New(Deps{Reaper: procutiltest.Reaper()}).claudeWorkspaceAuthConfig("/repo").Retention; got != (claude.Retention{}) {
		t.Fatalf("Retention with no dependency = %+v, want none", got)
	}
}

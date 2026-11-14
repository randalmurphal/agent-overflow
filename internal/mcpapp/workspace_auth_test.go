package mcpapp

import "testing"

// A Claude MCP sign-in is a session process against the user's home, so it
// carries the transcript retention a thread's session does.
func TestClaudeWorkspaceAuthCarriesCleanupPeriodDays(t *testing.T) {
	// The period is resolved for the sign-in's own workspace.
	service := New(Deps{ClaudeCleanupPeriodDays: func(workDir string) int {
		if workDir != "/repo" {
			return 0
		}
		return 90
	}})
	if got := service.claudeWorkspaceAuthConfig("/repo").CleanupPeriodDays; got != 90 {
		t.Fatalf("CleanupPeriodDays = %d, want 90", got)
	}
	if got := New(Deps{}).claudeWorkspaceAuthConfig("/repo").CleanupPeriodDays; got != 0 {
		t.Fatalf("CleanupPeriodDays with no dependency = %d, want 0", got)
	}
}

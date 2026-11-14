package provideraccountapp

import "testing"

// A probe held open for a token rotation outlives the delay before the CLI's
// transcript sweep, so the one probe constructor carries the retention a
// session does, for pinned and unpinned homes alike.
func TestClaudeProbeConfigCarriesCleanupPeriodDays(t *testing.T) {
	// The period is resolved for the directory the probe runs in.
	manager := NewManager(Deps{ClaudeCleanupPeriodDays: func(workDir string) int {
		if workDir != providerProbeWorkDir() {
			return 0
		}
		return 90
	}})
	for name, pins := range map[string]map[string]string{
		"canonical home": nil,
		"pinned home":    {"CLAUDE_CONFIG_DIR": t.TempDir()},
	} {
		if got := manager.claudeProbeConfig("claude", pins).CleanupPeriodDays; got != 90 {
			t.Errorf("%s: CleanupPeriodDays = %d, want 90", name, got)
		}
	}
	if got := NewManager(Deps{}).claudeProbeConfig("claude", nil).CleanupPeriodDays; got != 0 {
		t.Fatalf("CleanupPeriodDays with no dependency = %d, want 0", got)
	}
}

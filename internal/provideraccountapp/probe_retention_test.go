package provideraccountapp

import (
	"testing"

	"agent-overflow/internal/provider/claude"
)

// A probe held open for a token rotation outlives the delay before the CLI's
// transcript sweep, so the one probe constructor carries the retention a
// session does, for pinned and unpinned homes alike.
func TestClaudeProbeConfigCarriesCleanupPeriodDays(t *testing.T) {
	// The period is resolved for the directory the probe runs in.
	manager := NewManager(Deps{ClaudeRetention: func(workDir string) claude.Retention {
		if workDir != providerProbeWorkDir() {
			return claude.UserHomeRetention(0)
		}
		return claude.UserHomeRetention(90)
	}})
	for name, pins := range map[string]map[string]string{
		"canonical home": nil,
		"pinned home":    {"CLAUDE_CONFIG_DIR": t.TempDir()},
	} {
		if got := manager.claudeProbeConfig("claude", pins).Retention.CleanupPeriodDays(); got != 90 {
			t.Errorf("%s: CleanupPeriodDays = %d, want 90", name, got)
		}
	}
	// With no dependency the probe gives no answer, which refuses it.
	if got := NewManager(Deps{}).claudeProbeConfig("claude", nil).Retention; got != (claude.Retention{}) {
		t.Fatalf("Retention with no dependency = %+v, want none", got)
	}
}

package provideraccountapp

import (
	"fmt"
	"path/filepath"
	"time"

	"agent-overflow/internal/provider"
)

// ReconcileProviderHome binds metadata to its credential home, prunes only
// slots owned by that pairing, and recovers crash-orphaned Claude rotations.
func (m *Manager) ReconcileProviderHome(dbDir, userHome string) error {
	if !m.available() {
		return fmt.Errorf("provider account storage is unavailable")
	}
	claimedHome, homeMatches, err := m.store.ClaimProviderHome(userHome)
	if err != nil {
		return fmt.Errorf("bind provider account metadata to credential home: %w", err)
	}
	if !homeMatches {
		m.audit(
			"prune skipped: metadata store %s is bound to provider home %s but this process resolves %s",
			filepath.Join(dbDir, "provider-accounts.json"), claimedHome, userHome,
		)
	} else {
		for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
			keep := make(map[string]bool)
			for _, account := range m.store.List(providerName, time.Now()) {
				keep[account.ID] = true
			}
			pruned, pruneErr := m.credentials.PruneOrphanedAccounts(providerName, keep)
			for _, accountID := range pruned {
				m.audit("removed orphaned %s credential slot %s (no saved account references it)", providerName, accountID)
			}
			if pruneErr != nil {
				return fmt.Errorf("clean orphaned %s account credentials: %w", providerName, pruneErr)
			}
		}
	}

	sweepResults, sweepErr := m.credentials.SweepEphemeralClaudeCredentials(time.Now())
	for _, result := range sweepResults {
		if result.Action != "skipped" {
			m.audit("ephemeral claude sweep: %s crash-orphaned home %s (account %q)", result.Action, result.ConfigHome, result.AccountID)
		}
	}
	if sweepErr != nil {
		m.audit("ephemeral claude sweep errors: %v", sweepErr)
	}
	return nil
}

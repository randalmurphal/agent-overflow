package providerdiscoveryapp

import (
	"context"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// ProbeCodexAccount runs the zero-token Codex identity/rate-limit probe.
func (s *Service) ProbeCodexAccount() (provider.AccountInfo, error) {
	if s == nil || s.deps.ProviderBinary == nil || s.deps.Selection == nil ||
		s.deps.RunAccountProbe == nil || s.deps.CodexProbe == nil {
		return provider.AccountInfo{}, fmt.Errorf("provider discovery: Codex probe unavailable")
	}
	providerName := string(provider.Codex)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	// Read together before the probe waits for the account lock; the probe
	// runs with this configuration.
	key, cfg := s.deps.CodexProbe(binary, selection.AccountID)
	var observedSnapshot *provider.RateLimitsSnapshot
	var credits *codex.ResetCredits
	var creditsRead bool
	return s.deps.RunAccountProbe(AccountProbeRequest{
		ProviderName: providerName,
		Cache:        s.caches.Codex,
		Key:          key,
		Probe: func(ctx context.Context) (provider.AccountInfo, error) {
			cfg.OnSnapshot = func(snapshot provider.RateLimitsSnapshot) {
				copy := cloneRateLimitsSnapshot(snapshot)
				observedSnapshot = &copy
			}
			cfg.OnResetCredits = func(value *codex.ResetCredits) {
				credits, creditsRead = value, true
			}
			return s.deps.ProbeCodex(ctx, cfg)
		},
		ResetCredits: func() (*codex.ResetCredits, bool) { return credits, creditsRead },
		AfterAdopt: func(account provideraccounts.Account) {
			if observedSnapshot == nil || s.deps.EmitRateLimits == nil {
				return
			}
			observedSnapshot.AccountID = account.ID
			s.deps.EmitRateLimits(*observedSnapshot)
		},
	})
}

// RecheckCodexAccount invalidates the current identity and probes again.
func (s *Service) RecheckCodexAccount() (provider.AccountInfo, error) {
	providerName := string(provider.Codex)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	s.caches.Codex.Invalidate(s.deps.ProbeKey(providerName, binary, selection.AccountID))
	return s.ProbeCodexAccount()
}

func cloneRateLimitsSnapshot(snapshot provider.RateLimitsSnapshot) provider.RateLimitsSnapshot {
	snapshot.Limits = append([]provider.RateLimitEntry(nil), snapshot.Limits...)
	return snapshot
}

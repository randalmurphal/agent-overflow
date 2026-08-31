package providerdiscoveryapp

import (
	"context"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// ProbeCodexAccount runs the zero-token Codex identity/rate-limit probe.
func (s *Service) ProbeCodexAccount() (provider.AccountInfo, error) {
	if s == nil || s.deps.ProviderBinary == nil || s.deps.Selection == nil ||
		s.deps.ProbeKey == nil || s.deps.RunAccountProbe == nil || s.deps.CodexConfig == nil {
		return provider.AccountInfo{}, fmt.Errorf("provider discovery: Codex probe unavailable")
	}
	providerName := string(provider.Codex)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	key := s.deps.ProbeKey(providerName, binary, selection.AccountID)
	var observedSnapshot *provider.RateLimitsSnapshot
	return s.deps.RunAccountProbe(AccountProbeRequest{
		ProviderName: providerName,
		Cache:        s.caches.Codex,
		Key:          key,
		Probe: func(ctx context.Context) (provider.AccountInfo, error) {
			cfg := s.deps.CodexConfig(binary)
			cfg.OnSnapshot = func(snapshot provider.RateLimitsSnapshot) {
				copy := cloneRateLimitsSnapshot(snapshot)
				observedSnapshot = &copy
			}
			return s.deps.ProbeCodex(ctx, cfg)
		},
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

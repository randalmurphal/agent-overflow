package providerdiscoveryapp

import (
	"context"
	"fmt"

	"agent-overflow/internal/provider"
)

// ProbeAgyAccount runs the zero-token Antigravity identity/tier probe.
func (s *Service) ProbeAgyAccount() (provider.AccountInfo, error) {
	if s == nil || s.deps.ProviderBinary == nil || s.deps.Selection == nil ||
		s.deps.ProbeKey == nil || s.deps.RunAccountProbe == nil || s.deps.AgyConfig == nil {
		return provider.AccountInfo{}, fmt.Errorf("provider discovery: Antigravity probe unavailable")
	}
	providerName := string(provider.Agy)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	key := s.deps.ProbeKey(providerName, binary, selection.AccountID)
	return s.deps.RunAccountProbe(AccountProbeRequest{
		ProviderName: providerName,
		Cache:        s.caches.Agy,
		Key:          key,
		Probe: func(ctx context.Context) (provider.AccountInfo, error) {
			return s.deps.ProbeAgy(ctx, s.deps.AgyConfig(binary))
		},
	})
}

// RecheckAgyAccount invalidates the cached Antigravity identity and probes again.
func (s *Service) RecheckAgyAccount() (provider.AccountInfo, error) {
	providerName := string(provider.Agy)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	if s.caches.Agy != nil {
		s.caches.Agy.Invalidate(s.deps.ProbeKey(providerName, binary, selection.AccountID))
	}
	return s.ProbeAgyAccount()
}

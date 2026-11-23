package providerdiscoveryapp

import (
	"fmt"

	"agent-overflow/internal/provider"
)

// ProbeAgyAccount runs the zero-token Antigravity identity/tier probe.
func (s *Service) ProbeAgyAccount() (provider.AccountInfo, error) {
	if s == nil || s.deps.ProviderBinary == nil || s.deps.Selection == nil ||
		s.deps.RunAccountProbe == nil || s.deps.AgyProbe == nil {
		return provider.AccountInfo{}, fmt.Errorf("provider discovery: Antigravity probe unavailable")
	}
	providerName := string(provider.Agy)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	key, probe := s.deps.AgyProbe(binary, selection.AccountID)
	return s.deps.RunAccountProbe(AccountProbeRequest{
		ProviderName: providerName,
		Cache:        s.caches.Agy,
		Key:          key,
		Probe:        probe,
	})
}

// RecheckAgyAccount invalidates the cached Antigravity identity and probes again.
func (s *Service) RecheckAgyAccount() (provider.AccountInfo, error) {
	providerName := string(provider.Agy)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	if s.caches.Agy != nil && s.deps.AgyProbe != nil {
		key, _ := s.deps.AgyProbe(binary, selection.AccountID)
		s.caches.Agy.Invalidate(key)
	}
	return s.ProbeAgyAccount()
}

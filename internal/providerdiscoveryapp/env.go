package providerdiscoveryapp

import (
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// SetProviderCustomEnvVar persists one provider variable and invalidates every
// probe answer reachable through the old or new environment identity.
func (s *Service) SetProviderCustomEnvVar(providerName, name, value string, sensitive bool) (settings.Settings, error) {
	settingsService := s.settingsService()
	if settingsService == nil {
		return settings.Settings{}, fmt.Errorf("settings service unavailable")
	}
	previous := s.deps.CurrentSettings().ProviderEnvMap(providerName)
	next, err := settingsService.SetProviderEnvVar(providerName, name, value, sensitive)
	if err != nil {
		return settings.Settings{}, err
	}
	s.invalidateCustomEnvChange(providerName, previous)
	return next, nil
}

// DeleteProviderCustomEnvVar removes one provider variable and invalidates the
// same old/new probe identities as SetProviderCustomEnvVar.
func (s *Service) DeleteProviderCustomEnvVar(providerName, name string) (settings.Settings, error) {
	settingsService := s.settingsService()
	if settingsService == nil {
		return settings.Settings{}, fmt.Errorf("settings service unavailable")
	}
	previous := s.deps.CurrentSettings().ProviderEnvMap(providerName)
	next, err := settingsService.DeleteProviderEnvVar(providerName, name)
	if err != nil {
		return settings.Settings{}, err
	}
	s.invalidateCustomEnvChange(providerName, previous)
	return next, nil
}

func (s *Service) invalidateCustomEnvChange(providerName string, previous map[string]string) {
	canonical := providerName
	if canonical == string(provider.ClaudeTUI) {
		canonical = string(provider.Claude)
	}
	selection := s.deps.Selection(canonical)
	binary := s.deps.ProviderBinary(canonical)
	stale := s.deps.ProbeKeyForEnv(binary, selection.AccountID, previous)
	current := s.deps.ProbeKey(canonical, binary, selection.AccountID)
	s.InvalidateProbe(canonical, stale)
	s.InvalidateProbe(canonical, current)
}

// InvalidateProbe evicts one provider-specific identity answer.
func (s *Service) InvalidateProbe(providerName string, key provider.ProbeCacheKey) {
	switch providerName {
	case string(provider.Claude):
		s.caches.Claude.Invalidate(key)
	case string(provider.Codex):
		s.caches.Codex.Invalidate(key)
	}
}

func (s *Service) settingsService() *settings.Service {
	if s == nil || s.deps.SettingsService == nil {
		return nil
	}
	return s.deps.SettingsService()
}

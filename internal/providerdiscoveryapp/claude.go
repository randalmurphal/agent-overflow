package providerdiscoveryapp

import (
	"context"
	"fmt"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
)

// ProbeClaudeAccount runs the zero-token Claude identity probe. The initialize
// response's model and command catalogs are committed only after the managed-
// account runner accepts the corresponding identity.
func (s *Service) ProbeClaudeAccount() (provider.AccountInfo, error) {
	if s == nil || s.deps.ProviderBinary == nil || s.deps.Selection == nil ||
		s.deps.ProbeKey == nil || s.deps.RunAccountProbe == nil || s.deps.ClaudeConfig == nil {
		return provider.AccountInfo{}, fmt.Errorf("provider discovery: Claude probe unavailable")
	}
	providerName := string(provider.Claude)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	key := s.deps.ProbeKey(providerName, binary, selection.AccountID)

	var wire claudecatalog.ModelCapture
	var wireCommands claudecatalog.CommandCapture
	info, err := s.deps.RunAccountProbe(AccountProbeRequest{
		ProviderName: providerName,
		Cache:        s.caches.Claude,
		Key:          key,
		Probe: func(ctx context.Context) (provider.AccountInfo, error) {
			cfg := s.deps.ClaudeConfig(binary)
			cfg.OnModels = wire.Capture
			cfg.OnCommands = wireCommands.Capture
			return s.deps.ProbeClaude(ctx, cfg)
		},
		Unauthenticated: providerstatus.ClaudeUnauthenticated,
		EmitUnauth:      s.EmitClaudeUnauthenticatedStatus,
	})
	if err != nil {
		return provider.AccountInfo{}, err
	}
	wire.Store(key)
	wireCommands.Store(key)
	return info, nil
}

// RecheckClaudeAccount invalidates the current identity and probes again.
func (s *Service) RecheckClaudeAccount() (provider.AccountInfo, error) {
	providerName := string(provider.Claude)
	binary := s.deps.ProviderBinary(providerName)
	selection := s.deps.Selection(providerName)
	s.caches.Claude.Invalidate(s.deps.ProbeKey(providerName, binary, selection.AccountID))
	return s.ProbeClaudeAccount()
}

// ClaudeModels returns the probe-enriched catalog for one Claude-family
// provider without spawning a process.
func (s *Service) ClaudeModels(providerName string) []provider.ModelInfo {
	return claudecatalog.Models(s.ClaudeProbeKey(), providerName)
}

// ClaudeCommands returns the last probe-reported command list for the current
// Claude identity. Probed preserves the missing-vs-empty distinction.
func (s *Service) ClaudeCommands() (commands []provider.SlashCommand, probed bool) {
	return claudecatalog.Commands(s.ClaudeProbeKey())
}

// ClaudeProbeKey returns the complete current Claude probe identity.
func (s *Service) ClaudeProbeKey() provider.ProbeCacheKey {
	binary := s.deps.ProviderBinary(string(provider.Claude))
	selection := s.deps.Selection(string(provider.Claude))
	return s.deps.ProbeKey(string(provider.Claude), binary, selection.AccountID)
}

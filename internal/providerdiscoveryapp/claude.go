package providerdiscoveryapp

import (
	"context"
	"fmt"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/providerstatus"
)

// ProbeClaudeAccount runs the zero-token Claude identity probe. The initialize
// response's model and command catalogs are committed only after the managed-
// account runner accepts the corresponding identity, and before it emits
// `provider:account`, so a client refreshing the catalog on that event reads
// the enriched answer.
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
		AfterAdopt: func(account provideraccounts.Account) {
			wire.Store(key)
			wireCommands.Store(key)
			// Exported from the catalog rather than taken from `wire`
			// directly: the entry also holds the wire-only models earlier
			// probes of this binary learned and this one omitted, which is
			// exactly what the next process must serve again.
			//
			// Filed under the ADOPTED account, because that is the account
			// the next boot will probe as: a first probe runs with no
			// selection at all, and a login changed outside AO adopts the
			// account that actually answered. The selection the key was
			// built from is the fallback for a probe that adopted nothing
			// (no credential to attribute), where it is the only account
			// the answer can belong to.
			if s.deps.RememberClaudeCatalog == nil {
				return
			}
			accountID := account.ID
			if accountID == "" {
				accountID = selection.AccountID
			}
			if snapshot, ok := claudecatalog.Export(key); ok {
				s.deps.RememberClaudeCatalog(accountID, snapshot)
			}
		},
	})
	if err != nil {
		return provider.AccountInfo{}, err
	}
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
// provider without spawning a process. Go-internal capability lookups use it;
// callers that must show the user where the list came from use ClaudeCatalog.
func (s *Service) ClaudeModels(providerName string) []provider.ModelInfo {
	return s.ClaudeCatalog(providerName).Models
}

// ClaudeCatalog is ClaudeModels plus its provenance: CatalogProbed once a
// probe of this binary has reported (live or restored at boot from the
// account's persisted record), CatalogShipped until then.
func (s *Service) ClaudeCatalog(providerName string) provider.ModelCatalog {
	models, enriched := claudecatalog.Models(s.ClaudeProbeKey(), providerName)
	provenance := provider.CatalogShipped
	if enriched {
		provenance = provider.CatalogProbed
	}
	return provider.ModelCatalog{Models: models, Provenance: provenance}
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

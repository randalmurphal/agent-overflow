package providerdiscoveryapp

import (
	"cmp"
	"context"
	"fmt"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/claudemodels"
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
			// The live entry and the persisted record are filed under the
			// identity that answered: the key the probe started under, with
			// the ADOPTED account, the one the selection names once the probe
			// returns. A first probe runs with no selection at all, and a
			// login changed outside AO adopts the account that actually
			// answered. The selection the probe started under is the
			// fallback for a probe that adopted nothing (no credential to
			// attribute), where it is the only account the answer can
			// belong to.
			filed := key.WithAccount(cmp.Or(account.ID, key.AccountID))
			wire.Store(filed)
			wireCommands.Store(filed)
			if s.deps.RememberClaudeCatalog == nil {
				return
			}
			// Exported from the catalog rather than taken from `wire`
			// directly: the entry also holds the wire-only models earlier
			// probes of this binary learned and this one omitted, which is
			// exactly what the next process must serve again.
			if snapshot, ok := claudecatalog.Export(filed); ok {
				s.deps.RememberClaudeCatalog(filed, snapshot)
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
// account's persisted record), CatalogShipped until then. A list another
// account on the binary reported counts as probed, so the picker keeps it
// across an account switch; ClaudeAccountCatalog tells the two apart.
func (s *Service) ClaudeCatalog(providerName string) provider.ModelCatalog {
	catalog, _ := s.ClaudeAccountCatalog(providerName)
	return catalog
}

// ClaudeAccountCatalog is ClaudeCatalog and what backs it, so a caller
// that must run on what the current account's CLI lists can wait for
// claudemodels.BackedByIdentity.
func (s *Service) ClaudeAccountCatalog(providerName string) (provider.ModelCatalog, claudemodels.Backing) {
	models, backing := claudecatalog.Models(s.ClaudeProbeKey(), providerName)
	provenance := provider.CatalogShipped
	if backing.Enriched() {
		provenance = provider.CatalogProbed
	}
	return provider.ModelCatalog{Models: models, Provenance: provenance}, backing
}

// ClaudeModelAliases returns the current Claude identity's probe-reported
// alias map, nil until a probe has reported one.
func (s *Service) ClaudeModelAliases() map[string]string {
	return claudecatalog.ModelAliases(s.ClaudeProbeKey())
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

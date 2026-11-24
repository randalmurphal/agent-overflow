package providerdiscoveryapp

import (
	"context"
	"strings"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// ModelsForProvider returns the catalog selected by the provider's declared
// catalog capability, stamped with its provenance so a client can tell an
// answer from the installed binary apart from the shipped fallback.
func (s *Service) ModelsForProvider(ctx context.Context, providerName string) (provider.ModelCatalog, error) {
	switch provider.CapabilitiesForProvider(providerName).ModelCatalog {
	case provider.CodexLiveModelCatalog:
		models, err := s.CodexModelsForBinary(ctx, s.deps.ProviderBinary(providerName))
		if err != nil {
			return provider.ModelCatalog{}, err
		}
		return provider.ModelCatalog{Models: models, Provenance: provider.CatalogLive}, nil
	case provider.ACPLiveModelCatalog:
		if models, ok := s.AgyModelsForBinary(ctx, s.deps.ProviderBinary(providerName)); ok {
			return provider.ModelCatalog{Models: models, Provenance: provider.CatalogLive}, nil
		}
		return provider.ModelCatalog{
			Models:     provider.ModelsForProvider(providerName),
			Provenance: provider.CatalogShipped,
		}, nil
	case provider.ClaudeProbeEnrichedCatalog:
		return s.ClaudeCatalog(providerName), nil
	case provider.StaticModelCatalog:
		fallthrough
	default:
		return provider.ModelCatalog{
			Models:     provider.ModelsForProvider(providerName),
			Provenance: provider.CatalogShipped,
		}, nil
	}
}

// CodexModelsForBinary returns the TTL-cached live Codex model catalog.
func (s *Service) CodexModelsForBinary(ctx context.Context, binary string) ([]provider.ModelInfo, error) {
	return s.caches.CodexModels.Get(ctx, s.codexLaunch(binary), s.deps.Reaper)
}

// CachedCodexModelsForBinary performs a nonblocking catalog read.
func (s *Service) CachedCodexModelsForBinary(binary string) ([]provider.ModelInfo, error, bool) {
	return s.caches.CodexModels.Peek(s.codexLaunch(binary))
}

// codexLaunch is how the catalog starts binary: as a session would.
func (s *Service) codexLaunch(binary string) codexmodels.Launch {
	binary = normalizedCodexBinary(binary)
	if s.deps.CodexLaunch == nil {
		return codexmodels.Launch{Binary: binary}
	}
	launch := s.deps.CodexLaunch(binary)
	launch.Binary = binary
	return launch
}

// RefreshCodexModelCatalog invalidates all binary-scoped Codex model rows.
func (s *Service) RefreshCodexModelCatalog() { s.caches.CodexModels.Reset() }

func normalizedCodexBinary(binary string) string {
	if binary = strings.TrimSpace(binary); binary != "" {
		return binary
	}
	return settings.DefaultSettings.CodexBinaryPath
}

// KnownCodexModel returns capability evidence without starting or joining a probe.
func (s *Service) KnownCodexModel(binary, model string) (provider.ModelInfo, bool) {
	return s.caches.CodexModels.KnownModel(s.codexLaunch(binary), model)
}

// AgyModelsForBinary returns cached or live-probed Antigravity models.
// Returns (nil, false) when live probing is disabled or unavailable so callers
// fall back to the shipped Antigravity model catalog.
func (s *Service) AgyModelsForBinary(ctx context.Context, binary string) ([]provider.ModelInfo, bool) {
	if s.caches.AgyModels == nil || s.caches.AgyModels.Disabled() {
		return nil, false
	}
	key := normalizedAgyBinary(binary)
	models, err := s.caches.AgyModels.GetOrProbe(ctx, key, s.deps.ProbeAgyModels)
	if err != nil || len(models) == 0 {
		return nil, false
	}
	return models, true
}

// KnownAgyModel returns cached Antigravity model metadata without spawning.
func (s *Service) KnownAgyModel(binary, model string) (provider.ModelInfo, bool) {
	if s.caches.AgyModels == nil {
		return provider.ModelInfo{}, false
	}
	cached, ok := s.caches.AgyModels.Get(normalizedAgyBinary(binary))
	if !ok {
		return provider.ModelInfo{}, false
	}
	for _, m := range cached {
		if m.Slug == model {
			return m, true
		}
	}
	return provider.ModelInfo{}, false
}

// CachedAgyModelsForBinary performs a nonblocking read of the Antigravity live
// model catalog cache.
func (s *Service) CachedAgyModelsForBinary(binary string) ([]provider.ModelInfo, bool) {
	if s.caches.AgyModels == nil {
		return nil, false
	}
	return s.caches.AgyModels.Get(normalizedAgyBinary(binary))
}

// RefreshAgyModelCatalog clears cached Antigravity model catalogs.
func (s *Service) RefreshAgyModelCatalog() {
	if s.caches.AgyModels != nil {
		s.caches.AgyModels.Clear()
	}
}

func normalizedAgyBinary(binary string) string {
	if binary = strings.TrimSpace(binary); binary != "" {
		return binary
	}
	return settings.DefaultSettings.AgyBinaryPath
}

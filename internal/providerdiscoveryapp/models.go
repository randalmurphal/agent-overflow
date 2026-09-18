package providerdiscoveryapp

import (
	"context"
	"strings"

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
	case provider.ClaudeProbeEnrichedCatalog:
		return s.ClaudeCatalog(providerName), nil
	default:
		return provider.ModelCatalog{
			Models:     provider.ModelsForProvider(providerName),
			Provenance: provider.CatalogShipped,
		}, nil
	}
}

// CodexModelsForBinary returns the TTL-cached live Codex model catalog.
func (s *Service) CodexModelsForBinary(ctx context.Context, binary string) ([]provider.ModelInfo, error) {
	return s.caches.CodexModels.Get(ctx, normalizedCodexBinary(binary))
}

// CachedCodexModelsForBinary performs a nonblocking catalog read.
func (s *Service) CachedCodexModelsForBinary(binary string) ([]provider.ModelInfo, error, bool) {
	return s.caches.CodexModels.Peek(normalizedCodexBinary(binary))
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
	return s.caches.CodexModels.KnownModel(normalizedCodexBinary(binary), model)
}

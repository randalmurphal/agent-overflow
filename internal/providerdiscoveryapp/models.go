package providerdiscoveryapp

import (
	"context"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// ModelsForProvider returns the catalog selected by the provider's declared
// catalog capability.
func (s *Service) ModelsForProvider(ctx context.Context, providerName string) ([]provider.ModelInfo, error) {
	switch provider.CapabilitiesForProvider(providerName).ModelCatalog {
	case provider.CodexLiveModelCatalog:
		return s.CodexModelsForBinary(ctx, s.deps.ProviderBinary(providerName))
	case provider.ClaudeProbeEnrichedCatalog:
		return s.ClaudeModels(providerName), nil
	default:
		return provider.ModelsForProvider(providerName), nil
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

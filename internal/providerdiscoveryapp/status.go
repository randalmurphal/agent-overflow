package providerdiscoveryapp

import (
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
)

// ProviderStatuses detects both configured providers and emits each non-ready
// status so pull and push consumers observe the same answer.
func (s *Service) ProviderStatuses() []provider.ProviderStatus {
	cfg := s.deps.CurrentSettings()
	statuses := []provider.ProviderStatus{
		s.deps.DetectProvider(string(provider.Claude), cfg.ClaudeBinaryPath),
		s.deps.DetectProvider(string(provider.Codex), cfg.CodexBinaryPath),
	}
	for _, status := range statuses {
		if status.Status != "ready" && s.deps.EmitStatus != nil {
			s.deps.EmitStatus(providerstatus.EventFromDetect(status))
		}
	}
	return statuses
}

// EmitClaudeUnauthenticatedStatus publishes Claude's actionable login state.
func (s *Service) EmitClaudeUnauthenticatedStatus() {
	if s.deps.EmitStatus == nil {
		return
	}
	const message = "Claude is not authenticated. Run `claude login` to sign in."
	s.deps.EmitStatus(providerstatus.Event{
		Provider:   string(provider.Claude),
		Status:     "unauthenticated",
		Message:    message,
		Actionable: true,
		ActionURL:  providerstatus.ActionURL(string(provider.Claude), "unauthenticated"),
	})
}

// EmitStatusOnSessionStartError re-detects a provider and emits only a
// provider-level failure. Session/transport errors do not become banners.
func (s *Service) EmitStatusOnSessionStartError(providerName string) {
	status := s.deps.DetectProvider(providerName, s.deps.ProviderBinary(providerName))
	if status.Status == "ready" || s.deps.EmitStatus == nil {
		return
	}
	s.deps.EmitStatus(providerstatus.EventFromDetect(status))
}

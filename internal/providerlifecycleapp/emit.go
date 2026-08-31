package providerlifecycleapp

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// EmitSnapshot attributes an unmanaged snapshot to the current selection and
// publishes the account-scoped provider:usage shape.
func (s *Service) EmitSnapshot(snapshot provider.RateLimitsSnapshot) {
	if s.shuttingDown() || s.deps.Emit == nil {
		return
	}
	if snapshot.AccountID == "" {
		snapshot.AccountID = s.selection(snapshot.Provider).AccountID
	}
	s.deps.Emit(eventchan.ProviderUsage, provider.UsageEvent{
		Action: "rate_limits", RateLimits: &snapshot,
	})
}

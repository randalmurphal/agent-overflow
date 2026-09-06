package providerdiscoveryapp

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
)

// CheckTransferReadiness verifies the target provider before preparation is
// acknowledged. Use fresh account checks, never the five-minute display cache.
// No source credential travels with the conversation.
func (s *Service) CheckTransferReadiness(ctx context.Context, name, minimumCodexVersion string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name != string(provider.Claude) && name != string(provider.Codex) {
		return errors.New("Unsupported conversation provider.")
	}
	binary := s.deps.ProviderBinary(name)
	status := s.deps.DetectProvider(name, binary)
	if err := ctx.Err(); err != nil {
		return err
	}
	if status.Status != "ready" {
		if s.deps.EmitStatus != nil {
			s.deps.EmitStatus(providerstatus.EventFromDetect(status))
		}
		return fmt.Errorf("Set up %s on this computer before receiving the conversation (%s).", name, status.Status)
	}
	if minimumCodexVersion != "" {
		if name != string(provider.Codex) {
			return errors.New("Unsupported provider format requirement.")
		}
		if !provider.CodexCLIVersionAtLeast(provider.ParseCodexCLIVersion(status.Version), minimumCodexVersion) {
			return fmt.Errorf("This conversation uses paginated Codex history. Update Codex on this computer to %s or newer before receiving it.", minimumCodexVersion)
		}
	}
	if name == string(provider.Claude) && s.deps.CheckClaudeTransferAccount != nil {
		return s.deps.CheckClaudeTransferAccount(ctx, binary)
	}
	if name == string(provider.Codex) && s.deps.CheckCodexTransferAccount != nil {
		return s.deps.CheckCodexTransferAccount(ctx, binary)
	}
	return errors.New("Provider account checks are unavailable while this computer is starting.")
}

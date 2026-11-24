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
func (s *Service) CheckTransferReadiness(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name != string(provider.Claude) && name != string(provider.Codex) && name != string(provider.Agy) {
		return errors.New("Unsupported conversation provider.")
	}
	binary := s.deps.ProviderBinary(name)
	status := s.deps.DetectProvider(name, binary)
	if err := ctx.Err(); err != nil {
		return err
	}
	// A Codex version read from `--version` runs in AO's own environment,
	// so it is display only here: the account handshake below starts Codex
	// with the configured environment and admits or refuses the build that
	// runs.
	codexVersionOnly := name == string(provider.Codex) && status.Status == "version_too_old"
	if status.Status != "ready" && !codexVersionOnly {
		if s.deps.EmitStatus != nil {
			s.deps.EmitStatus(providerstatus.EventFromDetect(status))
		}
		if name == string(provider.Codex) && status.Status == "error" && status.Message != "" {
			// The detection message names the binary and why its version
			// check failed.
			return fmt.Errorf("This computer cannot receive the conversation yet. %s", status.Message) //nolint:staticcheck // ST1005: the user sees this sentence as written.
		}
		return fmt.Errorf("Set up %s on this computer before receiving the conversation (%s).", name, status.Status) //nolint:staticcheck // ST1005: the user sees this sentence as written.
	}
	if name == string(provider.Claude) && s.deps.CheckClaudeTransferAccount != nil {
		return s.deps.CheckClaudeTransferAccount(ctx, binary)
	}
	if name == string(provider.Codex) && s.deps.CheckCodexTransferAccount != nil {
		return s.deps.CheckCodexTransferAccount(ctx, binary)
	}
	if name == string(provider.Agy) {
		if s.deps.CheckAgyTransferAccount != nil {
			return s.deps.CheckAgyTransferAccount(ctx, binary)
		}
		return nil
	}
	return errors.New("Provider account checks are unavailable while this computer is starting.")
}

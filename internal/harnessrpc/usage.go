package harnessrpc

import (
	"context"
	"errors"
	"time"

	"agent-overflow/internal/provider/claude/claudefake"
)

// HarnessClaudeUsageSpec is the Claude usage answer for one identity.
type HarnessClaudeUsageSpec struct {
	// Email is the identity the credential's mock token spells.
	Email string `json:"email"`
	// Limits is the answer; empty clears it, so the endpoint is refused for
	// Email again.
	Limits []claudefake.UsageLimit `json:"limits,omitempty"`
}

// harnessUsageReadTimeout bounds the read and auto-switch's decision on it.
// It stays below the e2e client's RPC wait (RPC_TIMEOUT_MS, 15 s), so a call
// that cannot settle answers with what did not settle instead of the
// client's bare timeout.
const harnessUsageReadTimeout = 10 * time.Second

// HarnessSetClaudeUsage sets or clears the isolated Claude usage answer for
// one identity, then reads the selected Claude account. It returns once that
// reading is cached and auto-switch has decided on it, so a caller can assert
// what the reading did or did not change; a failed read, or a decision not
// made within harnessUsageReadTimeout, is its error. Reset clears every
// answer.
func (h *Harness) HarnessSetClaudeUsage(ctx context.Context, spec HarnessClaudeUsageSpec) error {
	if h.config.Host == nil {
		return errors.New("harness host unavailable")
	}
	fake := h.config.Host.ClaudeUsageFake()
	if fake == nil {
		return errors.New("this boot has no isolated Claude usage endpoint")
	}
	if len(spec.Limits) == 0 {
		if spec.Email == "" {
			return errors.New("clearing usage needs an email")
		}
		fake.Clear(spec.Email)
	} else if err := fake.Set(spec.Email, spec.Limits); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, harnessUsageReadTimeout)
	defer cancel()
	return h.config.Host.ReadClaudeUsage(ctx)
}

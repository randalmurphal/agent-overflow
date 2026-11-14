package harnessrpc

import (
	"errors"

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

// HarnessSetClaudeUsage sets or clears the isolated Claude usage answer for
// one identity, then requests a reading of the selected Claude account so a
// caller need not wait for the poll. Reset clears every answer.
func (h *Harness) HarnessSetClaudeUsage(spec HarnessClaudeUsageSpec) error {
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
	h.config.Host.RequestClaudeUsageReading()
	return nil
}

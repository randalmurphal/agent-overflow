package harnessrpc

import (
	"errors"
	"fmt"

	"agent-overflow/internal/provider/codex/codexfake"
)

// HarnessCodexResetCreditsSpec is the earned resets the mock Codex
// app-server reports for one account.
type HarnessCodexResetCreditsSpec struct {
	// AccountID is the workspace the account's credential names
	// (`tokens.account_id`); the mock's own sign-in writes "mock-workspace".
	AccountID string `json:"accountId"`
	// Account is the state to report; nil removes it, after which the
	// account's rate-limit read carries no reset summary.
	Account *codexfake.Account `json:"account"`
}

// HarnessSetCodexResetCredits sets the resets the mock Codex reports for
// one account and spends from. It reads nothing: a caller follows it with a
// usage refresh. Reset clears every account's.
func (h *Harness) HarnessSetCodexResetCredits(spec HarnessCodexResetCreditsSpec) error {
	store, err := h.codexResetStore()
	if err != nil {
		return err
	}
	return store.Set(spec.AccountID, spec.Account)
}

// HarnessCodexResetCredits is the mock's reset state for one account as it
// stands, spends recorded; nil when none is set.
func (h *Harness) HarnessCodexResetCredits(accountID string) (*codexfake.Account, error) {
	store, err := h.codexResetStore()
	if err != nil {
		return nil, err
	}
	return store.Get(accountID)
}

// codexResetStore is the mock's reset state for this boot. It lives in the
// harness home, which every mock process inherits.
func (h *Harness) codexResetStore() (codexfake.Store, error) {
	if h.config.HomeDir == "" {
		return codexfake.Store{}, errors.New("codex reset seeding needs the harness home, which AO_HARNESS_KEEP_HOME replaces with the real one")
	}
	return codexfake.Store{Home: h.config.HomeDir}, nil
}

// resetCodexResetCredits clears the mock's reset state, so the next spec's
// accounts report none.
func (h *Harness) resetCodexResetCredits() error {
	if h.config.HomeDir == "" {
		return nil
	}
	if err := (codexfake.Store{Home: h.config.HomeDir}).Clear(); err != nil {
		return fmt.Errorf("clear the mock Codex resets: %w", err)
	}
	return nil
}

package settings

import (
	"fmt"
	"log"
)

// Account auto-switch settings (docs/specs/account-auto-switch.md#settings).
// They apply to every provider; the per-account pool lives in
// provider-accounts.json.

// Values of AccountAutoSwitchStrategy, the order auto-switch uses the pool in.
const (
	// AccountAutoSwitchSoonestReset orders the pool by the reset time of
	// each account's longest provider-wide window, soonest first.
	AccountAutoSwitchSoonestReset = "soonest-reset"
	// AccountAutoSwitchAccountOrder orders the pool as the cards are
	// ordered in Settings.
	AccountAutoSwitchAccountOrder = "account-order"
)

// AccountAutoSwitchThreshold bounds, in percent of a usage limit.
const (
	MinAccountAutoSwitchThreshold     = 50
	MaxAccountAutoSwitchThreshold     = 100
	DefaultAccountAutoSwitchThreshold = 95
)

var allowedAccountAutoSwitchStrategies = map[string]struct{}{
	AccountAutoSwitchSoonestReset: {},
	AccountAutoSwitchAccountOrder: {},
}

func validateAccountAutoSwitchThreshold(field string, value int) error {
	if value < MinAccountAutoSwitchThreshold || value > MaxAccountAutoSwitchThreshold {
		return fmt.Errorf("%s must be between %d and %d percent",
			field, MinAccountAutoSwitchThreshold, MaxAccountAutoSwitchThreshold)
	}
	return nil
}

// sanitizeAccountAutoSwitchThreshold clamps: an out-of-range value still
// says the person wanted an earlier or later switch, and the nearest legal
// value honors that (same rule as sanitizeClaudeMaxCommandTimeoutHours).
func sanitizeAccountAutoSwitchThreshold(field string, value int) int {
	if value < MinAccountAutoSwitchThreshold {
		log.Printf("settings: %s %d below %d, clamping", field, value, MinAccountAutoSwitchThreshold)
		return MinAccountAutoSwitchThreshold
	}
	if value > MaxAccountAutoSwitchThreshold {
		log.Printf("settings: %s %d exceeds %d, clamping", field, value, MaxAccountAutoSwitchThreshold)
		return MaxAccountAutoSwitchThreshold
	}
	return value
}

// Package providertest supplies what tests hand to provider launches that
// refuse to start without it. Production code must not import it.
package providertest

import (
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
)

// Lifetime is the lifetime a test's provider launch states: watched by a
// reaper that watches nothing. A test asserting what is watched builds its
// own with provider.Watched or provider.Bounded.
func Lifetime() provider.Lifetime { return provider.Watched(procutiltest.Reaper()) }

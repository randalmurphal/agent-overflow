package transport

import (
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/settings"
)

// Pins the power:keepawake directive's wire posture (see app_power.go for
// the pipeline). Same shape as the self-update and webview-trim contract
// tests, with one extra assertion those two do not need: the RETENTION is
// load-bearing here, because this directive is a level rather than an
// edge and the launcher's convergence after a reconnect depends entirely
// on the newest frame being replayed.
func TestKeepAwakeContractClassifications(t *testing.T) {
	if channelAudience(string(eventchan.PowerKeepAwake)) != AudienceLoopbackOnly {
		t.Errorf("%q must be loopback-only: it commands this machine's power state, and only the host-side launcher may act on it", eventchan.PowerKeepAwake)
	}
	if channelRetention(string(eventchan.PowerKeepAwake)) != RetentionLatestOnly {
		t.Errorf(
			"%q must be latest-only: the launcher subscribes with a zero replay cursor and converges on the newest frame. "+
				"Ephemeral would leave a reconnected launcher holding a stale execution state (or none at all) until the user next touched the setting; "+
				"a default-depth ring would replay superseded modes in order.",
			eventchan.PowerKeepAwake,
		)
	}
	// UpdateSettings is the only producer of this directive, and what
	// keeps a remote session from pinning the desktop awake is now the
	// KEY's tier rather than the caller's origin: both keep-awake keys are
	// host-tier, and a host-tier patch key goes through the step-up proof
	// in internal/app's requireSettingsTier. The method's own scope is the
	// floor under that — it must at least stay out of the observe tier, or
	// a read-only session would reach the recheck at all.
	for _, key := range []string{"keepAwakeEnabled", "keepAwakeScreen"} {
		tier, ok := settings.TierForKey(key)
		if !ok || tier != settings.TierHost {
			t.Errorf("%q must be host-tier (got %q, known=%t): it inhibits THIS machine's sleep, so writing it takes a fresh host-presence proof rather than a standing grant", key, tier, ok)
		}
	}
	if tier := classify("UpdateSettings").Scope.Tier(); tier == TierObserve {
		t.Error(`"UpdateSettings" resolved to the observe tier: a session granted only reads would reach the per-key settings recheck, which is the wrong floor for the only producer of this directive`)
	}
}

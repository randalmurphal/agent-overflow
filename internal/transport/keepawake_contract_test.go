package transport

import (
	"testing"

	"agent-overflow/internal/eventchan"
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
	if !LocalOnlyMethods["UpdateSettings"] {
		t.Error(`"UpdateSettings" must be LocalOnly: it is the only producer of the keep-awake directive, and a LAN peer must not be able to pin the desktop awake`)
	}
}

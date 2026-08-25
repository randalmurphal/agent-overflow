package transport

import (
	"testing"

	"agent-overflow/internal/selfupdate"
)

// The self-update wire contract lives in internal/selfupdate; this package's
// policy tables key the same names as literals, like every other entry in
// those tables. These assertions pin the two together so a rename on either
// side fails here instead of silently shipping a directive channel that is no
// longer loopback-only/ephemeral, or an install-status method that is no
// longer LocalOnly.
func TestSelfUpdateContractClassifications(t *testing.T) {
	if channelAudience(selfupdate.ChannelInstall) != AudienceLoopbackOnly {
		t.Errorf("%q must be loopback-only: it names a file in the local staging dir and commands a binary swap", selfupdate.ChannelInstall)
	}
	if channelRetention(selfupdate.ChannelInstall) != RetentionEphemeral {
		t.Errorf("%q must be ephemeral: replaying a stale directive after a reconnect would restart the app on a dead instruction", selfupdate.ChannelInstall)
	}
	if !LocalOnlyMethods[selfupdate.RPCReportStatus] {
		t.Errorf("%q must be LocalOnly: it settles install state, clears an on-disk marker, and releases the updater busy fence", selfupdate.RPCReportStatus)
	}
}

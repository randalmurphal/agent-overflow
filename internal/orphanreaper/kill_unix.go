//go:build !windows

package orphanreaper

import (
	"time"

	"agent-overflow/internal/procutil"
)

// stopGroup ends group pgid through procutil.StopGroup: SIGTERM, up to grace
// for the group to empty, SIGKILL, and a short wait for the killed members
// to be reaped. Signalling a group needs only same-uid permission, which is
// why the startup sweep can reach groups it never spawned. A pgid outside
// validLeader is refused, so a corrupted value can never fan out to every
// process (-1) or init's group.
func stopGroup(pgid int, grace time.Duration) error {
	if err := validLeader(pgid); err != nil {
		return err
	}
	return procutil.StopGroup(pgid, grace)
}

// probeGroup reports whether group pgid has a member that can still run:
// os.ErrProcessDone when none can, as procutil.SignalGroup classifies it.
func probeGroup(pgid int) error {
	if err := validLeader(pgid); err != nil {
		return err
	}
	return procutil.SignalGroup(pgid, 0)
}

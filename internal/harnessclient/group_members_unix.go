//go:build !windows

package harnessclient

import (
	"errors"
	"fmt"
	"os"

	"agent-overflow/internal/harness/instanceinfo"
)

// ownedGroupMember is the proof that a process group still belongs to the
// launch being torn down. A process-group id is reusable after its last
// member exits, so the id alone is not safe evidence after the leader dies.
type ownedGroupMember struct {
	pid      int
	identity instanceinfo.ProcessIdentity
}

// snapshotProcessGroup is indirected so lifecycle tests can deterministically
// act out a leader-exit and PID-reuse race without depending on PID reuse from
// the host scheduler.
var snapshotProcessGroup = snapshotProcessGroupOS

func snapshotProcessGroupOS(pgid int) ([]ownedGroupMember, error) {
	if pgid <= 0 {
		return nil, fmt.Errorf("process group %d is invalid", pgid)
	}
	data, err := processGroupListing()
	if err != nil {
		return nil, err
	}
	members := make([]ownedGroupMember, 0, 4)
	for _, member := range data {
		if member.pgid != pgid {
			continue
		}
		identity, err := instanceinfo.CaptureProcessIdentity(member.pid)
		if err != nil {
			// A member can disappear between the listing and the identity
			// read. That is harmless. Any remaining member must be fully
			// identified before it can authorize a group signal.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("capture process-group member %d: %w", member.pid, err)
		}
		members = append(members, ownedGroupMember{pid: member.pid, identity: identity})
	}
	return members, nil
}

type processGroupListingEntry struct {
	pid  int
	pgid int
}

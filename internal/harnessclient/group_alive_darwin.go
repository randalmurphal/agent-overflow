//go:build darwin

package harnessclient

import "agent-overflow/internal/procutil"

// processGroupHasLiveMember reports whether group pgid has a member that has
// not exited. A group that cannot be listed keeps the signal-0 answer.
func processGroupHasLiveMember(pgid int) bool {
	members, err := procutil.RunningGroupMembers(pgid)
	return err != nil || len(members) > 0
}

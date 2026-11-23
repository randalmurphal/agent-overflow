package orphanreaper

import (
	"errors"
	"fmt"
	"os"
)

// Identity tells the process running as a pid from any process that had or
// later gets the same pid, whatever the wall clock says. Start is the birth
// marker the kernel stores for the process (instanceinfo.ReadProcess):
// starttime in clock ticks since boot on Linux, P_starttime on macOS. Boot
// is the kernel's boot id on Linux, where starttime restarts at every boot;
// it is empty on macOS, whose start time is absolute. Both are compared,
// never interpreted: two reads of one pid name the same process exactly
// when their Identities are ==. Linux shifts starttime by the reader's
// boot-time namespace offset, so readers in two time namespaces differ.
type Identity struct {
	Boot  string `json:"boot,omitempty"`
	Start string `json:"start"`
}

// ErrNoProcess is a pid that names no process.
var ErrNoProcess = errors.New("no such process")

// maxLeader is the largest pid any supported kernel assigns: Linux's
// PID_MAX_LIMIT, which also covers macOS's 99999.
const maxLeader = 4 << 20

// validLeader rejects a pid that cannot lead a group this package signals:
// 1 is init, and kill(2) reads 0 and negative values as other targets.
func validLeader(pid int) error {
	if pid <= 1 || pid > maxLeader {
		return fmt.Errorf("leader %d is outside 2..%d", pid, maxLeader)
	}
	return nil
}

// Snapshot is a leader as one kernel read of its process record and the
// boot shows it.
type Snapshot struct {
	Identity Identity
	PPID     int
}

// observe reads pid's Snapshot through info. The capture and the sweep
// both read a leader here. An error wraps ErrNoProcess when pid names no
// process.
func observe(info ProcInfo, pid int) (Snapshot, error) {
	proc, found, err := info.Read(pid)
	if err != nil {
		return Snapshot{}, err
	}
	if !found {
		return Snapshot{}, fmt.Errorf("process %d: %w", pid, ErrNoProcess)
	}
	boot, err := info.Boot()
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Identity: Identity{Boot: boot, Start: proc.Start}, PPID: proc.PPID}, nil
}

// Capture is a group leader as this process observed it when it recorded
// the group. CaptureLeader is the only way to build one, so the registry
// cannot be given a group without its leader's identity.
type Capture struct {
	leader     int
	identity   Identity
	parent     int
	createUnix int64
}

// CaptureLeader observes pid, the leader of a group this process started.
// It also reads the start time earlier versions recorded, so an older build
// reading the record never mistakes a reused pid for the leader; this
// version never compares it for a record that has an identity.
func CaptureLeader(pid int) (Capture, error) {
	return capture(SystemProcInfo(), pid, os.Getpid())
}

func capture(info ProcInfo, pid, parent int) (Capture, error) {
	if err := validLeader(pid); err != nil {
		return Capture{}, err
	}
	snap, err := observe(info, pid)
	if err != nil {
		return Capture{}, err
	}
	ms, err := info.LegacyStartMillis(pid)
	if err != nil {
		return Capture{}, fmt.Errorf("read process %d start time for earlier versions: %w", pid, err)
	}
	if ms <= 0 {
		return Capture{}, fmt.Errorf("read process %d start time for earlier versions: %d", pid, ms)
	}
	return Capture{leader: pid, identity: snap.Identity, parent: parent, createUnix: ms}, nil
}

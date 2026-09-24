package supervise

import (
	"errors"
	"fmt"
)

// The free-space rule for an update's database snapshot, stated once. The
// running version asks it before it hands off, so the user sees a refusal in
// the app they are using, and the snapshot step asks it again.

// snapshotSpaceMarginFloor is the smallest margin kept free beyond the copy.
const snapshotSpaceMarginFloor = 512 << 20

// SnapshotSpaceNeeded is the free space a snapshot of databaseBytes needs:
// the copy plus a margin of a tenth of it, at least 512 MiB. The margin
// covers what the trial itself writes. A clone needs it too, because the
// trial's writes unshare cloned blocks.
func SnapshotSpaceNeeded(databaseBytes int64) uint64 {
	if databaseBytes < 0 {
		databaseBytes = 0
	}
	size := uint64(databaseBytes)
	margin := size / 10
	if margin < snapshotSpaceMarginFloor {
		margin = snapshotSpaceMarginFloor
	}
	return size + margin
}

// InsufficientSpaceError refuses an update whose database snapshot would not
// fit. Its message names the shortfall, because freeing that much is the one
// thing the user can do about it.
type InsufficientSpaceError struct {
	Need      uint64
	Available uint64
	// Where names the disk, as in "the disk that holds /home/u/.config/...".
	Where string
}

func (e *InsufficientSpaceError) Error() string {
	return fmt.Sprintf("An update backs up the database first and needs %s free on %s, but %s is free. "+
		"Free at least %s and try again.",
		FormatBytes(e.Need), e.Where, FormatBytes(e.Available), FormatBytes(e.Need-e.Available))
}

// hostDiskDescription names the bound SnapshotOptions.HostAvailable carries.
const hostDiskDescription = "the Windows drive that holds the WSL distribution"

// freeBytes is FreeBytes, a seam for tests.
var freeBytes = FreeBytes

// CheckSnapshotSpace refuses a snapshot of databaseBytes that the data
// directory's filesystem, or the host bound when one is given, has no room
// for.
func CheckSnapshotSpace(dataDir string, databaseBytes int64, hostAvailable *uint64) error {
	need := SnapshotSpaceNeeded(databaseBytes)
	available, err := freeBytes(dataDir)
	if err != nil {
		return fmt.Errorf("supervise: read the free space of %s: %w", dataDir, err)
	}
	if available < need {
		return &InsufficientSpaceError{Need: need, Available: available, Where: "the disk that holds " + dataDir}
	}
	if hostAvailable != nil && *hostAvailable < need {
		return &InsufficientSpaceError{Need: need, Available: *hostAvailable, Where: hostDiskDescription}
	}
	return nil
}

// CheckDatabaseSnapshotSpace is CheckSnapshotSpace for the database as it is
// now: what the running version asks before it hands an update off.
func CheckDatabaseSnapshotSpace(dataDir string) error {
	live, total, err := identifyDatabase(dataDir)
	if err != nil {
		return err
	}
	if !anyPresent(live) {
		return errors.New("supervise: there is no database to back up")
	}
	return CheckSnapshotSpace(dataDir, total, nil)
}

// FormatBytes renders a size the way the update messages show it.
func FormatBytes(n uint64) string {
	const gib = 1 << 30
	const mib = 1 << 20
	if n >= gib {
		return fmt.Sprintf("%.1f GB", float64(n)/gib)
	}
	return fmt.Sprintf("%d MB", (n+mib-1)/mib)
}

package supervise

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// The free-space rule for an update's database snapshot, stated once. The
// target version asks it for the snapshot it will take (SnapshotPlan) before
// the running version hands off, so the user sees a refusal in the app they
// are using, and the snapshot step asks it again.

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

// SnapshotPlan is what TakeSnapshot will copy for an update, and so the
// free space the update needs: the live triple, after the leftover snapshot
// it clears first.
type SnapshotPlan struct {
	// DatabaseBytes is the live triple's total size.
	DatabaseBytes int64
	// Reclaimable is the size of a leftover snapshot, which TakeSnapshot
	// removes before it copies.
	Reclaimable uint64
}

// PlanSnapshot measures the snapshot an update of dataDir would take. found
// is false when there is no database, which the snapshot itself refuses.
func PlanSnapshot(layout Layout, dataDir string) (plan SnapshotPlan, found bool, err error) {
	live, total, err := identifyDatabase(dataDir)
	if err != nil {
		return SnapshotPlan{}, false, err
	}
	if !anyPresent(live) {
		return SnapshotPlan{}, false, nil
	}
	leftover, err := treeBytes(layout.SnapshotDir())
	if err != nil {
		return SnapshotPlan{}, false, err
	}
	return SnapshotPlan{DatabaseBytes: total, Reclaimable: leftover}, true, nil
}

// Check refuses the plan when the data directory's filesystem, or the host
// bound when one is given, has no room for it. The shortfall it reports
// counts what clearing the leftover frees.
func (p SnapshotPlan) Check(dataDir string, hostAvailable *uint64) error {
	need := SnapshotSpaceNeeded(p.DatabaseBytes)
	if p.Reclaimable >= need {
		return nil
	}
	need -= p.Reclaimable
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

// treeBytes is the size of the regular files under dir, 0 when it is
// absent.
func treeBytes(dir string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += uint64(info.Size())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("supervise: measure %s: %w", dir, err)
	}
	return total, nil
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

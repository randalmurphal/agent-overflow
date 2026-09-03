package supervise

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/atomicfile"
)

// The database rollback boundary.
//
// A trial runs migrations and writes. Making that reversible without down
// migrations means copying the database out of the way first, which is only
// safe while NO process has it open — so the copy happens between stopping the
// old child and starting the trial, and nowhere else.
//
// What is inside the boundary is the SQLite triple and nothing more.
// Attachments, provider homes, narratives and the tailnet state directory are
// outside it, which is one of the two reasons the parked-subsystem set exists:
// a trial that swept retention or refreshed a credential would have done
// something no snapshot can undo.

// Snapshot records which of the database files existed when it was taken.
//
// The list is load-bearing on restore. A database restored beside a WAL from a
// different moment is not the database that was copied, so restore removes all
// three and puts back exactly the set that was there.
type Snapshot struct {
	Files     []string `json:"files"`
	TakenAtMs int64    `json:"takenAtMs"`
}

const snapshotManifest = "snapshot.json"

// TakeSnapshot copies the SQLite triple into the layout's snapshot directory.
//
// Any previous snapshot is cleared first: there is one update in flight at a
// time, so a leftover is residue from an update that already settled, and
// keeping it would make a later restore put back the wrong moment.
func TakeSnapshot(layout Layout, dataDir string, now time.Time) (Snapshot, error) {
	dir := layout.SnapshotDir()
	if err := os.RemoveAll(dir); err != nil {
		return Snapshot{}, fmt.Errorf("supervise: clear snapshot dir: %w", err)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return Snapshot{}, fmt.Errorf("supervise: create snapshot dir: %w", err)
	}
	snapshot := Snapshot{TakenAtMs: now.UnixMilli()}
	for _, name := range DatabaseFiles() {
		source := filepath.Join(dataDir, name)
		copied, err := copyFileIfPresent(source, filepath.Join(dir, name))
		if err != nil {
			return Snapshot{}, err
		}
		if copied {
			snapshot.Files = append(snapshot.Files, name)
		}
	}
	if len(snapshot.Files) == 0 {
		// A serve host with no database has nothing to roll back, and a
		// snapshot of nothing would restore an empty directory over a database
		// the trial legitimately created. Refuse instead: the caller records a
		// failed update rather than one it cannot undo.
		return Snapshot{}, fmt.Errorf("supervise: no database to snapshot in %s", dataDir)
	}
	if err := atomicfile.WriteJSON(filepath.Join(dir, snapshotManifest), snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("supervise: write snapshot manifest: %w", err)
	}
	if err := atomicfile.SyncDir(dir); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// SnapshotPresent reports whether a complete snapshot is on disk.
//
// Asked before a rollback is begun and before a trial that has already been
// spawned once is spawned again, because in both places the answer changes
// what may happen next rather than being a detail of how.
func SnapshotPresent(layout Layout) (bool, error) {
	_, found, err := readSnapshotManifest(layout)
	return found, err
}

// readSnapshotManifest reads the manifest, if there is one. found is false
// with a nil error when there is no snapshot, which is the ordinary state
// between updates.
func readSnapshotManifest(layout Layout) (Snapshot, bool, error) {
	var snapshot Snapshot
	found, err := atomicfile.ReadJSON(filepath.Join(layout.SnapshotDir(), snapshotManifest), &snapshot)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("supervise: read snapshot manifest: %w", err)
	}
	return snapshot, found, nil
}

// DiscardSnapshot removes a snapshot. Called after a commit is durable, which
// is the one moment the old database stops being worth keeping.
func DiscardSnapshot(layout Layout) error {
	if err := os.RemoveAll(layout.SnapshotDir()); err != nil {
		return fmt.Errorf("supervise: discard snapshot: %w", err)
	}
	return nil
}

// RestoreMarker is written, and made durable, BEFORE a restore begins.
//
// It is the whole reason a supervisor may be killed mid-rollback: the marker
// says "the database under this path is half a restore", and the next
// supervisor to boot finishes it before either version can open the file. A
// restore with no marker is a restore that can leave a database that is
// neither the trial's nor the snapshot's.
type RestoreMarker struct {
	UpdateID    string `json:"updateId"`
	DataDir     string `json:"dataDir"`
	Reason      string `json:"reason"`
	WrittenAtMs int64  `json:"writtenAtMs"`
}

// ReadRestoreMarker reports an interrupted restore.
func ReadRestoreMarker(layout Layout) (RestoreMarker, bool, error) {
	var marker RestoreMarker
	found, err := atomicfile.ReadJSON(layout.MarkerPath(), &marker)
	if err != nil {
		return RestoreMarker{}, false, fmt.Errorf("supervise: read restore marker: %w", err)
	}
	return marker, found, nil
}

// RestoreSnapshot puts the snapshot back over the live database, marker first.
//
// The order is the contract: write and sync the marker, remove every live
// database file, copy the snapshot's back, sync the directory, THEN remove the
// marker. A crash at any point leaves a marker, and ResumeRestore run on the
// next boot repeats the whole thing — which is safe because every step is
// idempotent against the snapshot, and unsafe to skip because the middle of it
// is a database with no WAL.
func RestoreSnapshot(layout Layout, dataDir, updateID, reason string, now time.Time) error {
	// The manifest is read BEFORE the marker is written, and the order is the
	// whole point of this check. A marker says "the database under this path
	// is half a restore", and every later boot finishes what it names before
	// anything may open the file. Writing one for a restore that has nothing
	// to restore from would therefore be permanent: each boot would find the
	// marker, fail the same way, and refuse to start anything, on a machine
	// whose whole reason for existing is that nobody is standing at it.
	present, err := SnapshotPresent(layout)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("supervise: there is no database snapshot in %s to restore", layout.SnapshotDir())
	}
	marker := RestoreMarker{
		UpdateID: updateID, DataDir: dataDir,
		Reason: reason, WrittenAtMs: now.UnixMilli(),
	}
	if err := atomicfile.WriteJSON(layout.MarkerPath(), marker); err != nil {
		return fmt.Errorf("supervise: write restore marker: %w", err)
	}
	if err := applyRestore(layout, dataDir); err != nil {
		return err
	}
	if err := os.Remove(layout.MarkerPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("supervise: clear restore marker: %w", err)
	}
	return atomicfile.SyncDir(layout.Root())
}

// ResumeRestore finishes an interrupted restore, if one is marked.
//
// Runs before the state file is even read: a supervisor that selected a
// version and spawned it while the database was half-restored would hand a
// live backend a file nothing can vouch for.
func ResumeRestore(layout Layout) (RestoreMarker, bool, error) {
	marker, found, err := ReadRestoreMarker(layout)
	if err != nil || !found {
		return marker, false, err
	}
	if err := applyRestore(layout, marker.DataDir); err != nil {
		return marker, true, err
	}
	if err := os.Remove(layout.MarkerPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return marker, true, fmt.Errorf("supervise: clear restore marker: %w", err)
	}
	if err := atomicfile.SyncDir(layout.Root()); err != nil {
		return marker, true, err
	}
	return marker, true, nil
}

// applyRestore is the copy itself: remove the live triple, put back exactly
// what the manifest recorded.
func applyRestore(layout Layout, dataDir string) error {
	if dataDir == "" {
		return errors.New("supervise: the restore names no data directory")
	}
	dir := layout.SnapshotDir()
	snapshot, found, err := readSnapshotManifest(layout)
	if err != nil {
		return err
	}
	if !found {
		// Only reachable when something removed the snapshot after the marker
		// was written, which is somebody deleting files under a live restore.
		// Say what has to happen, because the database is mid-copy and no
		// version may be started against it.
		return fmt.Errorf(
			"supervise: the database is part-way through a restore and the snapshot in %s is gone. "+
				"Restore the data directory from a backup, then delete %s",
			dir, layout.MarkerPath())
	}
	for _, name := range DatabaseFiles() {
		if err := os.Remove(filepath.Join(dataDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("supervise: remove %s before restore: %w", name, err)
		}
	}
	for _, name := range snapshot.Files {
		if _, err := copyFileIfPresent(filepath.Join(dir, name), filepath.Join(dataDir, name)); err != nil {
			return err
		}
	}
	return atomicfile.SyncDir(dataDir)
}

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// copyFileIfPresent copies one file, fsyncing the destination. An absent
// source is not an error and reports false: a cleanly closed SQLite database
// has no -wal and no -shm, and that IS the snapshot.
func copyFileIfPresent(source, destination string) (bool, error) {
	in, err := os.Open(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("supervise: open %s: %w", source, err)
	}
	defer in.Close()

	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return false, fmt.Errorf("supervise: create %s: %w", destination, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, fmt.Errorf("supervise: copy %s -> %s: %w", source, destination, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return false, fmt.Errorf("supervise: sync %s: %w", destination, err)
	}
	if err := out.Close(); err != nil {
		return false, fmt.Errorf("supervise: close %s: %w", destination, err)
	}
	return true, nil
}

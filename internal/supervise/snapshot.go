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
// safe while NO process has it open, so the copy happens between stopping the
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
	// UpdateID is the in-app update the snapshot was taken for. Serve's
	// snapshots leave it empty: its state file names the update.
	UpdateID string `json:"updateId,omitempty"`
	// Live is each live database file's identity as it was copied, absent
	// ones included. CheckLiveBeforeAttempt compares against it, so a
	// process that ran against the database between the snapshot and the
	// trial is caught before the trial and its rollback could discard that
	// work. Manifests written before this field existed have none.
	Live []FileIdentity `json:"live,omitempty"`
	// Left is the live database as the update's last trial attempt left
	// it, absent until an attempt starts. CheckLiveBeforeAttempt compares
	// against it instead of Live once it is present.
	Left *AttemptRecord `json:"left,omitempty"`
}

// AttemptRecord is what one trial attempt of an update left behind.
type AttemptRecord struct {
	// Attempt is the attempt's number.
	Attempt int `json:"attempt"`
	// Ended is false from the moment the attempt's trial may write until
	// its command records the database the trial left. A command that
	// died in between (a killed VM, lost power) leaves it false.
	Ended bool           `json:"ended"`
	Files []FileIdentity `json:"files,omitempty"`
}

// FileIdentity is what CheckLiveBeforeAttempt compares: presence, size and
// modification time. SQLite changes at least one of them on every commit
// that reaches the file, and a checkpoint or WAL reset does too.
type FileIdentity struct {
	Name      string `json:"name"`
	Present   bool   `json:"present"`
	Size      int64  `json:"size,omitempty"`
	ModTimeNs int64  `json:"modTimeNs,omitempty"`
}

// CopyProgress receives the bytes copied so far out of the total, after
// every chunk of a snapshot or restore copy. A clone reports its whole file
// at once.
type CopyProgress func(copied, total int64)

// SnapshotOptions tunes TakeSnapshot. The zero value is serve's snapshot.
type SnapshotOptions struct {
	// UpdateID is recorded in the manifest.
	UpdateID string
	// Progress, when set, receives copy progress.
	Progress CopyProgress
	// HostAvailable bounds the free space from outside the data directory's
	// own filesystem, for a filesystem whose statfs does not describe the
	// disk it lives on: inside WSL, the Windows drive that holds the
	// distribution's virtual disk. nil means no such bound.
	HostAvailable *uint64
}

const snapshotManifest = "snapshot.json"

var (
	errNoDatabase        = errors.New("supervise: no database to snapshot")
	errChangedDuringCopy = errors.New("changed while it was being backed up, so another process is using the database")
)

// TakeSnapshot copies the SQLite triple into the layout's snapshot directory.
//
// Any previous snapshot is cleared first: there is one update in flight at a
// time, so a leftover is residue from an update that already settled, and
// keeping it would make a later restore put back the wrong moment. Free space
// is checked after that, so a leftover does not count against this one.
func TakeSnapshot(layout Layout, dataDir string, now time.Time, opts SnapshotOptions) (Snapshot, error) {
	dir := layout.SnapshotDir()
	if err := os.RemoveAll(dir); err != nil {
		return Snapshot{}, fmt.Errorf("supervise: clear snapshot dir: %w", err)
	}
	live, total, err := identifyDatabase(dataDir)
	if err != nil {
		return Snapshot{}, err
	}
	if !anyPresent(live) {
		// A serve host with no database has nothing to roll back, and a
		// snapshot of nothing would restore an empty directory over a database
		// the trial legitimately created. Refuse instead: the caller records a
		// failed update rather than one it cannot undo.
		return Snapshot{}, fmt.Errorf("%w in %s", errNoDatabase, dataDir)
	}
	if err := (SnapshotPlan{DatabaseBytes: total}).Check(dataDir, opts.HostAvailable); err != nil {
		return Snapshot{}, err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return Snapshot{}, fmt.Errorf("supervise: create snapshot dir: %w", err)
	}
	snapshot := Snapshot{TakenAtMs: now.UnixMilli(), UpdateID: opts.UpdateID, Live: live}
	copied := int64(0)
	for _, before := range live {
		if !before.Present {
			continue
		}
		source := filepath.Join(dataDir, before.Name)
		if err := copyFile(source, filepath.Join(dir, before.Name), func(n int64) {
			if opts.Progress != nil {
				opts.Progress(copied+n, total)
			}
		}); err != nil {
			return Snapshot{}, err
		}
		copied += before.Size
		// A file that changed while it was copied is a copy of no moment at
		// all, and the only thing that changes it is another process using
		// the database.
		after, err := identify(dataDir, before.Name)
		if err != nil {
			return Snapshot{}, err
		}
		if after != before {
			return Snapshot{}, fmt.Errorf("supervise: %s %w", before.Name, errChangedDuringCopy)
		}
		snapshot.Files = append(snapshot.Files, before.Name)
	}
	if err := atomicfile.WriteJSON(filepath.Join(dir, snapshotManifest), snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("supervise: write snapshot manifest: %w", err)
	}
	if err := atomicfile.SyncDir(dir); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// identifyDatabase reads the live triple's identities and total size.
func identifyDatabase(dataDir string) ([]FileIdentity, int64, error) {
	var (
		live  []FileIdentity
		total int64
	)
	for _, name := range DatabaseFiles() {
		id, err := identify(dataDir, name)
		if err != nil {
			return nil, 0, err
		}
		live = append(live, id)
		total += id.Size
	}
	return live, total, nil
}

func identify(dataDir, name string) (FileIdentity, error) {
	info, err := os.Stat(filepath.Join(dataDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return FileIdentity{Name: name}, nil
	}
	if err != nil {
		return FileIdentity{}, fmt.Errorf("supervise: stat %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return FileIdentity{}, fmt.Errorf("supervise: %s is not a regular file", filepath.Join(dataDir, name))
	}
	return FileIdentity{Name: name, Present: true, Size: info.Size(), ModTimeNs: info.ModTime().UnixNano()}, nil
}

func anyPresent(ids []FileIdentity) bool {
	for _, id := range ids {
		if id.Present {
			return true
		}
	}
	return false
}

// LiveDatabaseChangedError reports that the live database is not the one the
// update left: not the moment the snapshot copied, or not what the update's
// previous trial attempt left. The trial must not run, and nothing may be
// restored: a restore would put back the snapshot and discard whatever
// changed the database.
type LiveDatabaseChangedError struct {
	Snapshot FileIdentity
	Live     FileIdentity
	// AfterTrial is set when the comparison was against what an earlier
	// attempt's trial left rather than against the snapshot.
	AfterTrial bool
}

func (e *LiveDatabaseChangedError) Error() string {
	if e.AfterTrial {
		return fmt.Sprintf("the database changed after the update's interrupted trial left it (%s: %s), "+
			"so another Agent Overflow backend used it. The update stopped without restoring the backup so that work is kept",
			e.Snapshot.Name, describeIdentityChange(e.Snapshot, e.Live))
	}
	return fmt.Sprintf("the database changed after it was backed up for the update (%s: %s), "+
		"so another Agent Overflow backend used it. The update stopped so a rollback cannot lose that work",
		e.Snapshot.Name, describeIdentityChange(e.Snapshot, e.Live))
}

func describeIdentityChange(before, after FileIdentity) string {
	switch {
	case before.Present && !after.Present:
		return "removed"
	case !before.Present && after.Present:
		return fmt.Sprintf("created, %d bytes", after.Size)
	case before.Size != after.Size:
		return fmt.Sprintf("%d bytes, was %d", after.Size, before.Size)
	default:
		return fmt.Sprintf("modified at %s, was %s",
			time.Unix(0, after.ModTimeNs).UTC().Format(time.RFC3339Nano),
			time.Unix(0, before.ModTimeNs).UTC().Format(time.RFC3339Nano))
	}
}

// CheckLiveBeforeAttempt checks, before a trial attempt, that the live
// triple is what the update left, by presence, size and modification time:
// the moment the snapshot copied before the first attempt, and what the
// previous attempt's trial left before a retry.
//
// It exists for a process that runs the snapshot and each attempt without
// holding the database lock in between (the Windows launcher's separate WSL
// commands): anything that opened the database in a gap wrote work the
// update does not know about, and the next trial or a rollback would build
// on it or discard it. A manifest without identities fails closed.
//
// checked is false, with a nil error, when the previous attempt never
// recorded what its trial left: its command died while the trial could
// write, and no identity can tell that trial's writes from another
// process's. The attempt then runs on what is there.
func CheckLiveBeforeAttempt(layout Layout, dataDir string) (checked bool, err error) {
	snapshot, found, err := readSnapshotManifest(layout)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("supervise: there is no database snapshot in %s to compare against", layout.SnapshotDir())
	}
	expected, afterTrial := snapshot.Live, false
	if left := snapshot.Left; left != nil {
		if !left.Ended {
			return false, nil
		}
		expected, afterTrial = left.Files, true
	}
	if len(expected) == 0 {
		return false, errors.New("supervise: the database snapshot records no file identities, so it cannot prove the database is unchanged")
	}
	for _, recorded := range expected {
		live, err := identify(dataDir, recorded.Name)
		if err != nil {
			return false, err
		}
		if live != recorded {
			return false, &LiveDatabaseChangedError{Snapshot: recorded, Live: live, AfterTrial: afterTrial}
		}
	}
	return true, nil
}

// RecordAttempt records in the manifest what trial attempt attempt leaves
// in the live database: ended false before the trial may write, true with
// the files as they are once it has stopped. Called under the data root's
// lock, which is what makes the identities the attempt's own.
func RecordAttempt(layout Layout, dataDir string, attempt int, ended bool) error {
	snapshot, found, err := readSnapshotManifest(layout)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("supervise: there is no database snapshot in %s to record attempt %d in", layout.SnapshotDir(), attempt)
	}
	files, _, err := identifyDatabase(dataDir)
	if err != nil {
		return err
	}
	snapshot.Left = &AttemptRecord{Attempt: attempt, Ended: ended, Files: files}
	if err := atomicfile.WriteJSON(filepath.Join(layout.SnapshotDir(), snapshotManifest), snapshot); err != nil {
		return fmt.Errorf("supervise: record trial attempt %d: %w", attempt, err)
	}
	return nil
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

// ReadSnapshot reads a complete snapshot's manifest. found is false with a
// nil error when there is none.
func ReadSnapshot(layout Layout) (Snapshot, bool, error) {
	return readSnapshotManifest(layout)
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
// next boot repeats the whole thing. That is safe because every step is
// idempotent against the snapshot, and unsafe to skip because the middle of it
// is a database with no WAL.
//
// It needs no free space beyond what the live files held, because they are
// removed before the copy.
func RestoreSnapshot(layout Layout, dataDir, updateID, reason string, now time.Time, progress CopyProgress) error {
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
	if err := applyRestore(layout, dataDir, progress); err != nil {
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
func ResumeRestore(layout Layout, progress CopyProgress) (RestoreMarker, bool, error) {
	marker, found, err := ReadRestoreMarker(layout)
	if err != nil || !found {
		return marker, false, err
	}
	if err := applyRestore(layout, marker.DataDir, progress); err != nil {
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
func applyRestore(layout Layout, dataDir string, progress CopyProgress) error {
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
	var total int64
	sizes := make(map[string]int64, len(snapshot.Files))
	for _, name := range snapshot.Files {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("supervise: the snapshot's %s: %w", name, err)
		}
		sizes[name] = info.Size()
		total += info.Size()
	}
	for _, name := range DatabaseFiles() {
		if err := os.Remove(filepath.Join(dataDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("supervise: remove %s before restore: %w", name, err)
		}
	}
	copied := int64(0)
	for _, name := range snapshot.Files {
		if err := copyFile(filepath.Join(dir, name), filepath.Join(dataDir, name), func(n int64) {
			if progress != nil {
				progress(copied+n, total)
			}
		}); err != nil {
			return err
		}
		copied += sizes[name]
	}
	return atomicfile.SyncDir(dataDir)
}

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// copyChunk is how much a copy moves between progress reports. Small enough
// that a slow disk still reports within the stall window, large enough that
// copy_file_range does the work in few calls.
const copyChunk = 8 << 20

// cloneFile is the platform's copy-on-write clone, a seam for tests. It
// reports false with a nil error when the filesystem cannot clone, which
// sends the caller to an ordinary copy.
var cloneFile = platformCloneFile

// copyFile clones or copies one file and fsyncs the destination. progress
// receives the bytes of THIS file copied so far.
func copyFile(source, destination string, progress func(int64)) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("supervise: open %s: %w", source, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("supervise: stat %s: %w", source, err)
	}
	cloned, err := cloneFile(in, destination)
	if err != nil {
		return fmt.Errorf("supervise: clone %s -> %s: %w", source, destination, err)
	}
	if cloned {
		progress(info.Size())
		return nil
	}

	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return fmt.Errorf("supervise: create %s: %w", destination, err)
	}
	var written int64
	for {
		// io.CopyN over two *os.File reaches copy_file_range on Linux, so
		// chunking costs one syscall per chunk and no userspace buffer.
		n, err := io.CopyN(out, in, copyChunk)
		written += n
		if n > 0 {
			progress(written)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			out.Close()
			return fmt.Errorf("supervise: copy %s -> %s: %w", source, destination, err)
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("supervise: sync %s: %w", destination, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("supervise: close %s: %w", destination, err)
	}
	return nil
}

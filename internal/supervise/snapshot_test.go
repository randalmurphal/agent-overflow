package supervise

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func absent(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// writeDatabase lays down the SQLite triple with recognizable contents.
func writeDatabase(t *testing.T, dataDir, marker string) {
	t.Helper()
	for _, name := range DatabaseFiles() {
		writeFile(t, filepath.Join(dataDir, name), marker+" "+name)
	}
}

func TestSnapshotRoundTripsTheWholeTriple(t *testing.T) {
	dataDir := t.TempDir()
	layout, err := NewLayout(dataDir)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	writeDatabase(t, dataDir, "before")

	snapshot, err := TakeSnapshot(layout, dataDir, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	if len(snapshot.Files) != len(DatabaseFiles()) {
		t.Fatalf("snapshot recorded %v, want all of %v", snapshot.Files, DatabaseFiles())
	}

	// A trial writes, and leaves the WAL and shm in a state of its own.
	writeDatabase(t, dataDir, "after")

	if err := RestoreSnapshot(layout, dataDir, "u1", "the trial crashed", time.Unix(0, 0)); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	for _, name := range DatabaseFiles() {
		if got, want := readFile(t, filepath.Join(dataDir, name)), "before "+name; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !absent(t, layout.MarkerPath()) {
		t.Error("the restore marker survived a completed restore")
	}
}

// A cleanly closed SQLite database has no -wal and no -shm, and that IS the
// snapshot. Restoring it must REMOVE the trial's, not leave them beside a
// database from a different moment.
func TestRestoreRemovesFilesTheSnapshotDidNotHave(t *testing.T) {
	dataDir := t.TempDir()
	layout, err := NewLayout(dataDir)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), "clean")

	if _, err := TakeSnapshot(layout, dataDir, time.Unix(0, 0)); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	writeDatabase(t, dataDir, "trial")

	if err := RestoreSnapshot(layout, dataDir, "u1", "budget", time.Unix(0, 0)); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if got := readFile(t, filepath.Join(dataDir, "agent-overflow.db")); got != "clean" {
		t.Errorf("database = %q, want %q", got, "clean")
	}
	for _, name := range []string{"agent-overflow.db-wal", "agent-overflow.db-shm"} {
		if !absent(t, filepath.Join(dataDir, name)) {
			t.Errorf("%s survived a restore from a snapshot that did not have it", name)
		}
	}
}

// The marker is the whole reason a supervisor may be killed mid-rollback.
func TestAnInterruptedRestoreIsFinishedFromTheMarkerAlone(t *testing.T) {
	dataDir := t.TempDir()
	layout, err := NewLayout(dataDir)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	writeDatabase(t, dataDir, "before")
	if _, err := TakeSnapshot(layout, dataDir, time.Unix(0, 0)); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	// Exactly what a supervisor killed one instruction after writing the
	// marker leaves behind: the marker, the snapshot, and a database that is
	// neither the trial's nor the snapshot's.
	writeFile(t, layout.MarkerPath(),
		`{"updateId":"u1","dataDir":`+quote(dataDir)+`,"reason":"killed mid-restore","writtenAtMs":1}`)
	if err := os.Remove(filepath.Join(dataDir, "agent-overflow.db-wal")); err != nil {
		t.Fatalf("remove wal: %v", err)
	}
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), "half-restored")

	marker, resumed, err := ResumeRestore(layout)
	if err != nil || !resumed {
		t.Fatalf("ResumeRestore = (%t, %v)", resumed, err)
	}
	if marker.UpdateID != "u1" {
		t.Errorf("marker update = %q, want u1", marker.UpdateID)
	}
	for _, name := range DatabaseFiles() {
		if got, want := readFile(t, filepath.Join(dataDir, name)), "before "+name; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !absent(t, layout.MarkerPath()) {
		t.Error("the marker survived the resumed restore")
	}
	// And a second boot has nothing to finish.
	if _, resumed, err := ResumeRestore(layout); err != nil || resumed {
		t.Fatalf("second ResumeRestore = (%t, %v), want (false, nil)", resumed, err)
	}
}

// A serve host with no database has nothing to roll back, and a snapshot of
// nothing would restore an empty directory over a database the trial created.
func TestSnapshotRefusesAnEmptyDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	layout, err := NewLayout(dataDir)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if _, err := TakeSnapshot(layout, dataDir, time.Unix(0, 0)); err == nil {
		t.Fatal("TakeSnapshot invented a snapshot of nothing")
	}
}

func TestDiscardSnapshotIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	layout, err := NewLayout(dataDir)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	writeDatabase(t, dataDir, "before")
	if _, err := TakeSnapshot(layout, dataDir, time.Unix(0, 0)); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	for range 2 {
		if err := DiscardSnapshot(layout); err != nil {
			t.Fatalf("DiscardSnapshot: %v", err)
		}
	}
	if !absent(t, layout.SnapshotDir()) {
		t.Error("the snapshot directory survived being discarded")
	}
}

// quote renders a JSON string literal for the hand-written fixtures above.
func quote(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(append(out, '"'))
}

// A restore with nothing to restore from must leave NO marker.
//
// The marker is the one durable instruction every later boot obeys before it
// may select or spawn anything, so writing one for a snapshot that does not
// exist is not a failed rollback: it is a supervisor that refuses to start
// anything on this machine, on every boot, forever. The check therefore comes
// before the write and not after it.
func TestRestoreLeavesNoMarkerWhenThereIsNothingToRestore(t *testing.T) {
	dataDir := t.TempDir()
	layout, err := NewLayout(dataDir)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	writeDatabase(t, dataDir, "live")

	err = RestoreSnapshot(layout, dataDir, "upd-1", "the trial crashed", time.Unix(0, 0))
	if err == nil {
		t.Fatal("RestoreSnapshot answered nil with no snapshot on disk")
	}
	if !strings.Contains(err.Error(), "no database snapshot") {
		t.Errorf("error = %v, which does not say the snapshot is missing", err)
	}
	if !absent(t, layout.MarkerPath()) {
		t.Fatalf("a restore with nothing to restore wrote %s, which no later boot could get past",
			layout.MarkerPath())
	}
	// And the next boot finds nothing to resume, so it proceeds normally.
	if _, resumed, err := ResumeRestore(layout); err != nil || resumed {
		t.Fatalf("ResumeRestore = (%t, %v), want (false, nil)", resumed, err)
	}
	// The live database was not touched on the way to the refusal.
	if got := readFile(t, filepath.Join(dataDir, DatabaseFiles()[0])); got != "live agent-overflow.db" {
		t.Errorf("database = %q, want it untouched", got)
	}
}

// SnapshotPresent is the question both the rollback and the second trial
// spawn ask, so it has to tell an absent snapshot from an unreadable one.
func TestSnapshotPresentAnswersTheThreeStates(t *testing.T) {
	dataDir := t.TempDir()
	layout, err := NewLayout(dataDir)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if present, err := SnapshotPresent(layout); err != nil || present {
		t.Fatalf("SnapshotPresent on a fresh layout = (%t, %v), want (false, nil)", present, err)
	}
	writeDatabase(t, dataDir, "before")
	if _, err := TakeSnapshot(layout, dataDir, time.Unix(0, 0)); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	if present, err := SnapshotPresent(layout); err != nil || !present {
		t.Fatalf("SnapshotPresent after one = (%t, %v), want (true, nil)", present, err)
	}
	if err := DiscardSnapshot(layout); err != nil {
		t.Fatalf("DiscardSnapshot: %v", err)
	}
	if present, err := SnapshotPresent(layout); err != nil || present {
		t.Fatalf("SnapshotPresent after a discard = (%t, %v), want (false, nil)", present, err)
	}
}

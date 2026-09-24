package supervise

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appLayout(t *testing.T, dataDir string) Layout {
	t.Helper()
	layout, err := NewAppUpdateLayout(dataDir)
	if err != nil {
		t.Fatalf("NewAppUpdateLayout: %v", err)
	}
	return layout
}

// withFreeBytes replaces the free-space reading for one test.
func withFreeBytes(t *testing.T, fn func(string) (uint64, error)) {
	t.Helper()
	previous := freeBytes
	freeBytes = fn
	t.Cleanup(func() { freeBytes = previous })
}

// withCloneFile replaces the platform clone for one test.
func withCloneFile(t *testing.T, fn func(*os.File, string) (bool, error)) {
	t.Helper()
	previous := cloneFile
	cloneFile = fn
	t.Cleanup(func() { cloneFile = previous })
}

func TestAppUpdateLayoutIsBesideServesUnderRuntime(t *testing.T) {
	dataDir := t.TempDir()
	layout := appLayout(t, dataDir)
	if want := filepath.Join(dataDir, "runtime", "app-update"); layout.Root() != want {
		t.Fatalf("root = %s, want %s", layout.Root(), want)
	}
	if _, err := NewAppUpdateLayout("relative"); err == nil {
		t.Fatal("a relative data directory was accepted")
	}
}

func TestSnapshotRecordsTheLiveIdentitiesAndUpdateID(t *testing.T) {
	dataDir := t.TempDir()
	layout := appLayout(t, dataDir)
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), "database")
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db-wal"), "wal!")

	if _, err := TakeSnapshot(layout, dataDir, time.Unix(5, 0), SnapshotOptions{UpdateID: "u1"}); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	snapshot, found, err := ReadSnapshot(layout)
	if err != nil || !found {
		t.Fatalf("ReadSnapshot = %v %v", found, err)
	}
	if snapshot.UpdateID != "u1" {
		t.Fatalf("update id = %q", snapshot.UpdateID)
	}
	if len(snapshot.Live) != 3 {
		t.Fatalf("live identities = %+v, want all three files", snapshot.Live)
	}
	byName := map[string]FileIdentity{}
	for _, id := range snapshot.Live {
		byName[id.Name] = id
	}
	if db := byName["agent-overflow.db"]; !db.Present || db.Size != int64(len("database")) || db.ModTimeNs == 0 {
		t.Fatalf("database identity = %+v", db)
	}
	if shm := byName["agent-overflow.db-shm"]; shm.Present {
		t.Fatalf("an absent file was recorded present: %+v", shm)
	}
	if err := VerifyLiveUnchanged(layout, dataDir); err != nil {
		t.Fatalf("VerifyLiveUnchanged on an untouched database: %v", err)
	}
}

func TestVerifyLiveUnchangedCatchesEveryKindOfChange(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, dataDir string)
		want   string
	}{
		{"size", func(t *testing.T, dataDir string) {
			writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), "database grew")
		}, "bytes, was"},
		{"mtime", func(t *testing.T, dataDir string) {
			path := filepath.Join(dataDir, "agent-overflow.db")
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		}, "modified at"},
		{"created", func(t *testing.T, dataDir string) {
			writeFile(t, filepath.Join(dataDir, "agent-overflow.db-shm"), "shm")
		}, "created"},
		{"removed", func(t *testing.T, dataDir string) {
			if err := os.Remove(filepath.Join(dataDir, "agent-overflow.db-wal")); err != nil {
				t.Fatal(err)
			}
		}, "removed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			layout := appLayout(t, dataDir)
			writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), "database")
			writeFile(t, filepath.Join(dataDir, "agent-overflow.db-wal"), "wal!")
			if _, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{}); err != nil {
				t.Fatalf("TakeSnapshot: %v", err)
			}
			c.change(t, dataDir)
			err := VerifyLiveUnchanged(layout, dataDir)
			var changed *LiveDatabaseChangedError
			if !errors.As(err, &changed) {
				t.Fatalf("VerifyLiveUnchanged = %v, want a LiveDatabaseChangedError", err)
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "another Agent Overflow backend") {
				t.Fatalf("message %q does not say %q", err, c.want)
			}
		})
	}
}

func TestVerifyLiveUnchangedFailsClosedWithoutIdentities(t *testing.T) {
	dataDir := t.TempDir()
	layout := appLayout(t, dataDir)
	if err := VerifyLiveUnchanged(layout, dataDir); err == nil {
		t.Fatal("no snapshot verified as unchanged")
	}
	// A manifest written before identities existed.
	writeFile(t, filepath.Join(layout.SnapshotDir(), "snapshot.json"), `{"files":["agent-overflow.db"],"takenAtMs":1}`)
	if err := VerifyLiveUnchanged(layout, dataDir); err == nil || !strings.Contains(err.Error(), "no file identities") {
		t.Fatalf("VerifyLiveUnchanged = %v, want a refusal naming the missing identities", err)
	}
}

func TestSnapshotRefusesAFileThatChangesWhileItIsCopied(t *testing.T) {
	dataDir := t.TempDir()
	layout := appLayout(t, dataDir)
	writeDatabase(t, dataDir, "before")
	withCloneFile(t, func(in *os.File, destination string) (bool, error) {
		// Another process writes the live file mid-copy.
		if filepath.Base(destination) == "agent-overflow.db" {
			writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), "a concurrent writer's longer contents")
		}
		return false, nil
	})
	_, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{})
	if !errors.Is(err, errChangedDuringCopy) {
		t.Fatalf("TakeSnapshot = %v, want the changed-during-copy refusal", err)
	}
	if present, _ := SnapshotPresent(layout); present {
		t.Fatal("a snapshot of no moment was recorded as complete")
	}
}

func TestSnapshotRefusesWhenEitherDiskIsShort(t *testing.T) {
	dataDir := t.TempDir()
	layout := appLayout(t, dataDir)
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), strings.Repeat("x", 1024))
	need := SnapshotSpaceNeeded(1024)

	withFreeBytes(t, func(string) (uint64, error) { return need - 1, nil })
	_, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{})
	var space *InsufficientSpaceError
	if !errors.As(err, &space) || space.Need != need || space.Available != need-1 {
		t.Fatalf("TakeSnapshot = %v, want the data disk's shortfall", err)
	}
	if !strings.Contains(err.Error(), "Free at least 1 MB") || !strings.Contains(err.Error(), dataDir) {
		t.Fatalf("message %q does not name the disk and the shortfall", err)
	}

	withFreeBytes(t, func(string) (uint64, error) { return need, nil })
	host := need - (256 << 20)
	_, err = TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{HostAvailable: &host})
	if !errors.As(err, &space) || space.Where != hostDiskDescription {
		t.Fatalf("TakeSnapshot = %v, want the host drive's shortfall", err)
	}
	if !strings.Contains(err.Error(), "Free at least 256 MB") {
		t.Fatalf("message %q does not name the host shortfall", err)
	}

	host = need
	if _, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{HostAvailable: &host}); err != nil {
		t.Fatalf("TakeSnapshot with exactly enough room: %v", err)
	}
}

func TestSnapshotSpaceNeededKeepsAMarginOfATenthAtLeast512MiB(t *testing.T) {
	if got := SnapshotSpaceNeeded(0); got != 512<<20 {
		t.Fatalf("empty database needs %d", got)
	}
	if got := SnapshotSpaceNeeded(10 << 30); got != (10<<30)+(1<<30) {
		t.Fatalf("10 GiB needs %d, want 11 GiB", got)
	}
	if got := SnapshotSpaceNeeded(1 << 30); got != (1<<30)+(512<<20) {
		t.Fatalf("1 GiB needs %d, want 1.5 GiB", got)
	}
}

func TestCheckDatabaseSnapshotSpaceMeasuresTheLiveTriple(t *testing.T) {
	dataDir := t.TempDir()
	if err := CheckDatabaseSnapshotSpace(dataDir); err == nil {
		t.Fatal("no database passed the space check")
	}
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), strings.Repeat("x", 3000))
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db-wal"), strings.Repeat("x", 1000))
	need := SnapshotSpaceNeeded(4000)
	withFreeBytes(t, func(string) (uint64, error) { return need - 1, nil })
	if err := CheckDatabaseSnapshotSpace(dataDir); err == nil {
		t.Fatal("a short disk passed")
	}
	withFreeBytes(t, func(string) (uint64, error) { return need, nil })
	if err := CheckDatabaseSnapshotSpace(dataDir); err != nil {
		t.Fatalf("an adequate disk failed: %v", err)
	}
}

func TestCopyReportsProgressAndACloneReportsTheWholeFile(t *testing.T) {
	dataDir := t.TempDir()
	layout := appLayout(t, dataDir)
	size := copyChunk + 100
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), strings.Repeat("d", size))
	writeFile(t, filepath.Join(dataDir, "agent-overflow.db-wal"), "wal")

	var reports [][2]int64
	record := func(copied, total int64) { reports = append(reports, [2]int64{copied, total}) }
	withCloneFile(t, func(*os.File, string) (bool, error) { return false, nil })
	if _, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{Progress: record}); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	total := int64(size + 3)
	want := [][2]int64{{copyChunk, total}, {int64(size), total}, {total, total}}
	if len(reports) != len(want) {
		t.Fatalf("copy reports = %v, want %v", reports, want)
	}
	for i := range want {
		if reports[i] != want[i] {
			t.Fatalf("copy reports = %v, want %v", reports, want)
		}
	}
	if got := readFile(t, filepath.Join(layout.SnapshotDir(), "agent-overflow.db")); len(got) != size {
		t.Fatalf("copied %d bytes, want %d", len(got), size)
	}

	// A clone reports its file in one step and is not copied again.
	reports = nil
	cloned := 0
	withCloneFile(t, func(in *os.File, destination string) (bool, error) {
		cloned++
		data, err := os.ReadFile(in.Name())
		if err != nil {
			return false, err
		}
		return true, os.WriteFile(destination, data, 0o600)
	})
	if err := RestoreSnapshot(layout, dataDir, "u", "test", time.Now(), record); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if cloned != 2 {
		t.Fatalf("cloned %d files, want 2", cloned)
	}
	if len(reports) != 2 || reports[0] != [2]int64{int64(size), total} || reports[1] != [2]int64{total, total} {
		t.Fatalf("clone reports = %v", reports)
	}
}

func TestAFailedCloneIsAnErrorNotACopy(t *testing.T) {
	dataDir := t.TempDir()
	layout := appLayout(t, dataDir)
	writeDatabase(t, dataDir, "before")
	withCloneFile(t, func(*os.File, string) (bool, error) { return false, errors.New("disk on fire") })
	if _, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{}); err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("TakeSnapshot = %v, want the clone's error", err)
	}
}

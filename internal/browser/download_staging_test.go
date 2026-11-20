//go:build unix

package browser

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeFiles writes "x" to each path.
func writeFiles(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A staged file's release removes it in the run directory it was staged in,
// however that directory was moved, and never the file a replacement
// directory holds at its path.
func TestReleasingAStagedFileNeverRemovesAReplacementsFile(t *testing.T) {
	downloads := testDownloadsFolder(t)
	staged, err := stageDownload()
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, staged)
	staging := filepath.Join(downloads, stagingDirName)
	aside := filepath.Join(downloads, "aside")
	if err := os.Rename(staging, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("the user's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stagedFileOf(staged); err == nil || !strings.Contains(err.Error(), "moved or replaced") {
		t.Fatalf("publishing from the replaced directory answers %v", err)
	}
	releaseStaged(staged)
	if got, err := os.ReadFile(staged); err != nil || string(got) != "the user's" {
		t.Fatalf("the replacement's file reads %q, %v", got, err)
	}
	moved := filepath.Join(aside, filepath.Base(filepath.Dir(staged)), filepath.Base(staged))
	if _, err := os.Lstat(moved); !os.IsNotExist(err) {
		t.Fatalf("the staged file in the moved directory is still there: %v", err)
	}
}

// The prune of a staging directory removes the staged files and the
// private names of unfinished publishes of a run only once it takes that
// run's lock: a live run's stay, as do a run directory with no lock file,
// anything not a staged or private name, and the files an earlier release
// left directly in the staging directory.
func TestTheStagingPruneTakesOnlyEndedRuns(t *testing.T) {
	downloads := testDownloadsFolder(t)
	staging := filepath.Join(downloads, stagingDirName)
	live, ended, emptied, unlocked := filepath.Join(staging, "run-aaaaaaaaaaaa"), filepath.Join(staging, "run-bbbbbbbbbbbb"), filepath.Join(staging, "run-cccccccccccc"), filepath.Join(staging, "run-dddddddddddd")
	for _, dir := range []string{live, ended, emptied, unlocked} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A publish the ended runs did not finish left a private name.
	private := placingNamePrefix + "0123456789abcdef"
	writeFiles(t,
		filepath.Join(live, runLockName), filepath.Join(live, "dl-1"), filepath.Join(live, private),
		filepath.Join(ended, runLockName), filepath.Join(ended, "dl-1"), filepath.Join(ended, "dl-1.wkdownload"), filepath.Join(ended, "notes.txt"), filepath.Join(ended, "dl-1.bak"),
		filepath.Join(ended, placingNamePrefix+"notes"),
		filepath.Join(emptied, runLockName), filepath.Join(emptied, "dl-7"), filepath.Join(emptied, private),
		filepath.Join(unlocked, "dl-1"),
		filepath.Join(staging, "dl-1"), filepath.Join(staging, "dl-0123456789ab-7"),
	)
	// Another process's live run: another open of its lock holds it.
	lock, err := os.OpenFile(filepath.Join(live, runLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	if locked, err := lockRun(lock); err != nil || !locked {
		t.Fatalf("lock the live run: %v, %v", locked, err)
	}

	staged, err := stageDownload()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseStaged(staged)
	if got := folderNames(t, live); !slices.Equal(got, []string{private, runLockName, "dl-1"}) {
		t.Errorf("the live run holds %q", got)
	}
	if got := folderNames(t, ended); !slices.Equal(got, []string{placingNamePrefix + "notes", "dl-1.bak", "notes.txt"}) {
		t.Errorf("the ended run holds %q, want only the files AO never staged", got)
	}
	if _, err := os.Lstat(emptied); !os.IsNotExist(err) {
		t.Errorf("the emptied ended run's directory stays: %v", err)
	}
	if got := folderNames(t, unlocked); !slices.Equal(got, []string{"dl-1"}) {
		t.Errorf("the run with no lock file holds %q", got)
	}
	for _, name := range []string{"dl-1", "dl-0123456789ab-7"} {
		if _, err := os.Lstat(filepath.Join(staging, name)); err != nil {
			t.Errorf("the earlier release's %s went: %v", name, err)
		}
	}
	if filepath.Dir(staged) == live || filepath.Dir(staged) == ended {
		t.Fatalf("the new download stages in another run's directory: %s", staged)
	}
}

// The last release of a staging directory's runs removes the run directory
// and the staging directory only as they were: a regular file put at
// either's old name after the directory was moved aside stays.
func TestTheLastStagingReleaseRemovesOnlyItsOwnDirectories(t *testing.T) {
	for _, moved := range []string{"the staging directory", "the run directory"} {
		t.Run(moved, func(t *testing.T) {
			downloads := testDownloadsFolder(t)
			staged, err := stageDownload()
			if err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(downloads, stagingDirName)
			if moved == "the run directory" {
				old = filepath.Dir(staged)
			}
			if err := os.Rename(old, filepath.Join(filepath.Dir(old), "aside")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(old, []byte("the user's"), 0o600); err != nil {
				t.Fatal(err)
			}
			releaseStaged(staged)
			if got, err := os.ReadFile(old); err != nil || string(got) != "the user's" {
				t.Fatalf("the file at %s reads %q, %v", old, got, err)
			}
		})
	}
}

// A run directory made and then not opened goes again only while its name
// still holds that directory: a file put at the name stays, and so does
// anything at it when the directory could not be read as it was made.
func TestAnUnopenedRunRemovesOnlyTheDirectoryItMade(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run-aaaaaaaaaaaa")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	made, err := os.Lstat(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := abandonMadeDir(runDir, made, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(runDir); !os.IsNotExist(err) {
		t.Fatalf("the run directory it made stayed: %v", err)
	}

	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	made, err = os.Lstat(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(runDir); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, runDir)
	if err := abandonMadeDir(runDir, made, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(runDir); err != nil || string(got) != "x" {
		t.Fatalf("the file put at the run's name reads %q, %v", got, err)
	}
	if err := abandonMadeDir(runDir, nil, os.ErrPermission); err == nil {
		t.Fatal("a run read as nothing when made answered no error")
	}
	if got, err := os.ReadFile(runDir); err != nil || string(got) != "x" {
		t.Fatalf("with no identity the file at the run's name reads %q, %v", got, err)
	}
}

// swapAsMade moves each directory openMadeDir makes aside before it is
// opened and puts another at its name holding a file of someone else's,
// and answers the names swapped.
func swapAsMade(t *testing.T) func() []string {
	t.Helper()
	var swapped []string
	openingMadeDir = func(path string) {
		if err := os.Rename(path, path+"-aside"); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Error(err)
			return
		}
		writeFiles(t, filepath.Join(path, "theirs"))
		swapped = append(swapped, path)
	}
	t.Cleanup(func() { openingMadeDir = func(string) {} })
	return func() []string { return swapped }
}

// wantTheirsKept checks each swapped-in directory still holds its file.
func wantTheirsKept(t *testing.T, swapped []string) {
	t.Helper()
	if len(swapped) == 0 {
		t.Fatal("no directory was swapped in")
	}
	for _, path := range swapped {
		if got, err := os.ReadFile(filepath.Join(path, "theirs")); err != nil || string(got) != "x" {
			t.Fatalf("the file in the directory put at %s reads %q, %v", path, got, err)
		}
	}
}

// A run directory replaced between its making and its opening is not
// held, and the directory put at its name keeps what is in it.
func TestAStagingRunReplacedAsItIsMadeIsNotHeld(t *testing.T) {
	dir := t.TempDir()
	staging, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	swapped := swapAsMade(t)
	if run, err := startStagingRun(dir, staging); err == nil {
		t.Fatalf("held %s, a directory put at the run's name as it was made", run.dir)
	}
	wantTheirsKept(t, swapped())
}

// So is a headless browser's download room.
func TestADownloadRoomReplacedAsItIsMadeIsNotHeld(t *testing.T) {
	swapped := swapAsMade(t)
	if room, err := openDownloadRoom(t.TempDir(), t.Logf); err == nil {
		room.close()
		t.Fatalf("held %s, a directory put at the room's name as it was made", room.path())
	}
	wantTheirsKept(t, swapped())
}

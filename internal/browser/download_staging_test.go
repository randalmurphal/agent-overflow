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

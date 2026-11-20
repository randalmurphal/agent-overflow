package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
)

// A tab closed after the engine took a finished download's end, and before
// that end was reported, leaves the row to the report: it lists completed
// with its file kept, an artifact moved to the workspace, a staged file
// published to the Downloads folder, a file the engine saved where it is.
func TestATabClosedBeforeAFakeDownloadsFinishIsReported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		saving downloadSaving
	}{
		{"artifact", downloadsAsArtifacts},
		{"saved by the engine", downloadsSavedByEngine},
		{"staged", downloadsStaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, tc.saving)
			if _, err := manager.Open(t.Context(), access, "https://example.test/sibling", OpenOptions{}); err != nil {
				t.Fatal(err)
			}
			startDownload(t, manager, access, page, "late.txt", 64, "hold")
			item, err := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
			if err != nil {
				t.Fatal(err)
			}
			written := heldFile(t, manager, item.engineID)
			manager.engine.(*fakeEngine).beforeFinishReport = func(string) {
				if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
					t.Error(err)
				}
			}
			if err := fakeProfileOf(t, manager).finishHeld(item.engineID); err != nil {
				t.Fatal(err)
			}
			got := threadDownloads(manager, access)[0]
			if got.State != downloadStateCompleted || got.Error != "" || got.ReceivedBytes != 64 {
				t.Fatalf("the download lists as %+v, want completed", got)
			}
			finished, err := manager.finishedDownloadPath(access, DownloadKey(got.ID))
			if err != nil {
				t.Fatalf("the finished file: %v", err)
			}
			if stat, err := os.Stat(finished.path); err != nil || stat.Size() != 64 {
				t.Fatalf("the finished file at %s: %v", finished.path, err)
			}
			if tc.saving != downloadsSavedByEngine {
				if _, err := os.Lstat(written); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("the engine's file %s stays beside the finished one: %v", written, err)
				}
			}
		})
	}
}

// A tab closed while its staged download runs cancels it and removes its
// staged file.
func TestATabClosedWhileAStagedDownloadRunsCancelsIt(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsStaged)
	if _, err := manager.Open(t.Context(), access, "https://example.test/sibling", OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "running.txt", 64, "hold")
	item, err := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	staged := heldFile(t, manager, item.engineID)
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("no staged file: %v", err)
	}
	if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateCanceled || got.Error == "" {
		t.Fatalf("the download lists as %+v, want canceled with its reason", got)
	}
	if _, err := os.Lstat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the canceled download's staged file stays: %v", err)
	}
}

// A profile disposed while a download runs, or is held, ends it as a
// cancel carrying the bytes written, with its file in the Downloads folder
// removed, and keeps no record of it.
func TestDisposingAFakeProfileCancelsItsDownloads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flags  []string
		inside bool
	}{
		{"during its first report", nil, true},
		{"during the first report of one to hold", []string{"hold"}, true},
		{"held", []string{"hold"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloads := testDownloadsFolder(t)
			engine := newFakeEngine()
			engine.saving = downloadsSavedByEngine
			var profile *fakeProfile
			var reports []downloadProgress
			var disposeErr error
			engine.events = engineEvents{
				DownloadStarted: func(downloadStart) downloadAdmission { return downloadAdmitted },
				DownloadProgress: func(progress downloadProgress) {
					reports = append(reports, progress)
					if tc.inside && len(reports) == 1 {
						disposeErr = profile.Dispose(context.Background())
					}
				},
			}
			if err := engine.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			opened, err := engine.NewProfile(t.Context(), profileOptions{Jar: "w", DownloadDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			profile = opened.(*fakeProfile)
			request, _ := fakeDownloadAddress(fakeDownloadURL("f.bin", 64, tc.flags...))
			if err := profile.download("frame", request); err != nil {
				t.Fatal(err)
			}
			if !tc.inside {
				disposeErr = profile.Dispose(context.Background())
			}
			if disposeErr != nil {
				t.Fatal(disposeErr)
			}
			if len(reports) != 2 || reports[0].State != downloadInProgress || reports[1].State != downloadCanceled {
				t.Fatalf("the download reported %+v, want its progress and then its cancel", reports)
			}
			if end := reports[1]; end.Received != 32 || end.Total != 64 {
				t.Fatalf("the cancel reports %v of %v bytes, want 32 of 64", end.Received, end.Total)
			}
			if names := folderNames(t, downloads); len(names) != 0 {
				t.Fatalf("the Downloads folder holds %q", names)
			}
			profile.mu.Lock()
			left := fmt.Sprint(profile.held)
			profile.mu.Unlock()
			if left != "map[]" {
				t.Fatalf("the disposed profile keeps %s", left)
			}
		})
	}
}

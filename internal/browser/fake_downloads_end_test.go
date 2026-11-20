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
// that end was reported, leaves the row to the report where the end needs
// nothing of the page: an artifact or a file the engine saved is listed
// completed. A staged file is published from its page's row, so the
// page's leave cancels it and its staged file goes, as on WebKit.
func TestATabClosedBeforeAFakeDownloadsFinishIsReported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		saving downloadSaving
		want   string
	}{
		{"artifact", downloadsAsArtifacts, downloadStateCompleted},
		{"saved by the engine", downloadsSavedByEngine, downloadStateCompleted},
		{"staged", downloadsStaged, downloadStateCanceled},
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
			var written string
			manager.engine.(*fakeEngine).beforeFinishReport = func(file string) {
				written = file
				if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
					t.Error(err)
				}
			}
			if err := fakeProfileOf(t, manager).finishHeld(item.engineID); err != nil {
				t.Fatal(err)
			}
			got := threadDownloads(manager, access)[0]
			if got.State != tc.want {
				t.Fatalf("the download lists as %+v, want %s", got, tc.want)
			}
			if tc.want == downloadStateCompleted {
				if got.Error != "" || got.ReceivedBytes != 64 {
					t.Fatalf("the finished download lists as %+v", got)
				}
				finished, err := manager.finishedDownloadPath(access, DownloadKey(got.ID))
				if err != nil {
					t.Fatalf("the finished file: %v", err)
				}
				if stat, err := os.Stat(finished.path); err != nil || stat.Size() != 64 {
					t.Fatalf("the finished file at %s: %v", finished.path, err)
				}
				return
			}
			if _, err := os.Lstat(written); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the canceled download's file %s stays: %v", written, err)
			}
		})
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

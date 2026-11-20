package browser

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Of a workspace's artifact downloads only the finished files open: the
// file the engine is still writing is not in the workspace's directory.
func TestARunningArtifactDownloadIsNotOpenable(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	startDownload(t, manager, access, page, "done.bin", 10)
	startDownload(t, manager, access, page, "running.bin", 10, "hold")
	var done, running threadDownload
	for _, row := range threadDownloads(manager, access) {
		item, _ := manager.threadDownload(access, DownloadKey(row.ID))
		if row.Name == "done.bin" {
			done = item
		} else {
			running = item
		}
	}
	if _, err := manager.authorizeFile(access.Caller(), done.path); err != nil {
		t.Fatalf("the finished download does not open: %v", err)
	}
	if _, err := manager.authorizeFile(access.Caller(), heldFile(t, manager, running.engineID)); err == nil {
		t.Fatal("the file of a running download opens")
	}
}

// A file an earlier release left in the workspace's old download directory,
// where its engine wrote running downloads beside finished ones, does not
// open: only the finished files AO moved since are granted.
func TestAnEarlierReleasesDownloadFileIsNotOpenable(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	workspace, err := canonicalRoot(access.Workspace())
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(manager.artifactRoot, "downloads", jarDirName(jarKey(workspace)))
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"4b1f6a2e-guid.crdownload", "report.pdf"} {
		path := filepath.Join(old, name)
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.authorizeFile(access.Caller(), path); err == nil {
			t.Fatalf("%s in an earlier release's download directory opens", name)
		}
	}
	startDownload(t, manager, access, page, "done.bin", 10)
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if _, err := manager.authorizeFile(access.Caller(), item.path); err != nil {
		t.Fatalf("a finished download does not open: %v", err)
	}
}

// A finish whose file was swapped for a link before its report was handled
// grants nothing: staged, the publish refuses the link and the row fails;
// saved by the engine, the row completes saying the file is a link, and
// neither the link nor its target opens.
func TestALinkSwappedInBeforeTheFinishGrantsNothing(t *testing.T) {
	for _, mode := range []struct {
		name   string
		saving downloadSaving
		state  string
		reason string
	}{
		{"staged", downloadsStaged, downloadStateFailed, "not a regular file"},
		{"saved by the engine", downloadsSavedByEngine, downloadStateCompleted, "is a link"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, mode.saving)
			downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
			private := filepath.Join(t.TempDir(), "private.txt")
			if err := os.WriteFile(private, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			manager.engine.(*fakeEngine).beforeFinishReport = func(file string) {
				if err := os.Remove(file); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(private, file); err != nil {
					t.Error(err)
				}
			}
			startDownload(t, manager, access, page, "file.txt", 10)
			got := threadDownloads(manager, access)[0]
			if got.State != mode.state || !strings.Contains(got.Error, mode.reason) || got.Openable {
				t.Fatalf("the swapped download lists as %+v, want %s saying %q", got, mode.state, mode.reason)
			}
			if _, err := manager.authorizeFile(access.Caller(), private); err == nil {
				t.Fatal("the link's target is openable")
			}
			if _, err := manager.authorizeFile(access.Caller(), filepath.Join(downloads, "file.txt")); err == nil {
				t.Fatal("the link is openable")
			}
			if _, err := manager.OpenThreadDownload(t.Context(), access, DownloadKey(got.ID)); err == nil {
				t.Fatal("the row opens")
			}
			if got, _ := os.ReadFile(private); string(got) != "private" {
				t.Fatalf("the link's target reads %q", got)
			}
		})
	}
}

// A download the engine saved under a path it could not name or reach
// (the launcher's report had no room for it, or wslpath failed) completes
// saying so, with nothing to open.
func TestAnEngineSavedFileWithNoPathCompletesWithItsReason(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsSavedByEngine)
	frame := paneHandle(manager, access, page.ID)
	if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "dl-x-1", URL: "https://example.test/x", SuggestedName: "x.bin"}) != downloadAdmitted {
		t.Fatal("the download was refused")
	}
	const reason = "the saved file's path is too long to report"
	manager.downloadProgress(downloadProgress{ID: "dl-x-1", Received: 5, Total: 5, State: downloadCompleted, Error: reason})
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateCompleted || got.Error != reason || got.Openable {
		t.Fatalf("the download lists as %+v, want completed saying %q with nothing to open", got, reason)
	}
	if _, err := manager.OpenThreadDownload(t.Context(), access, DownloadKey(got.ID)); err == nil {
		t.Fatal("the row opens")
	}
}

// browser_open_file may open a finished download of its workspace that the
// Manager still lists, and only that file: not its neighbor, not from
// another workspace, not a file that replaced it, and not once no list
// holds its row.
func TestOpenFileAllowsAListedDownloadAndNotItsNeighbor(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	neighbor := filepath.Join(downloads, "neighbor.txt")
	if err := os.WriteFile(neighbor, []byte("not a download"), 0o600); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "notes.txt", 10)
	item, err := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	// Listed in the thread's pane after its page closed.
	if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.OpenFile(t.Context(), access, item.path, OpenOptions{}); err != nil {
		t.Fatalf("the workspace's listed download does not open: %v", err)
	}
	if _, err := manager.OpenFile(t.Context(), access, neighbor, OpenOptions{}); err == nil {
		t.Fatal("a file beside the download opened")
	}
	other := manager.Bind(Caller{ThreadID: "other", Workspace: t.TempDir()})
	if _, err := manager.authorizeFile(other.Caller(), item.path); err == nil {
		t.Fatal("another workspace may open this workspace's download")
	}
	// A link to the download resolves to it.
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(item.path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access.Caller(), link); err != nil {
		t.Fatalf("a link to the download is refused: %v", err)
	}
	// A file put in its place is not the download, even on the download's
	// inode, at another time.
	if err := os.Remove(item.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(item.path, bytes.Repeat([]byte("x"), 10), 0o600); err != nil {
		t.Fatal(err)
	}
	replacedAt := item.file.ModTime().Add(time.Minute)
	if err := os.Chtimes(item.path, replacedAt, replacedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access.Caller(), item.path); err == nil {
		t.Fatal("a file that replaced the download opened")
	}
	// Once no list holds its row, the download is not openable.
	startDownload(t, manager, access, mustOpen(t, manager, access, "https://example.test/files"), "later.txt", 10)
	later, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	manager.closeAllPages(t, access)
	if err := manager.ClearThreadDownloads(access); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access.Caller(), later.path); err == nil {
		t.Fatal("a download no list holds opened")
	}
}

// browser_downloads lists a page's download after the thread's pane list
// dropped it, and a download it lists opens.
func TestADownloadThePageStillListsOpens(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "notes.txt", 10)
	entries, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: PageID(page.ID)})
	if err != nil {
		t.Fatal(err)
	}
	path := entries.([]DownloadInfo)[0].Path
	if err := manager.ClearThreadDownloads(access); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access.Caller(), path); err != nil {
		t.Fatalf("a download the page lists is refused: %v", err)
	}
}

// Another thread of the same workspace completes many downloads, and many
// other workspaces complete one each; a row the pane still lists opens
// throughout.
func TestAListedRowStaysOpenableWhateverOthersDownload(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "original.txt", 10)
	original := threadDownloads(manager, access)[0]

	busy := manager.Bind(Caller{ThreadID: "busy", Workspace: access.Workspace()})
	busyPage, err := manager.Open(t.Context(), busy, "https://example.test/files", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		startDownload(t, manager, busy, busyPage, fmt.Sprintf("b%d.txt", i), 10)
	}
	for i := range 40 {
		elsewhere := manager.Bind(Caller{ThreadID: ThreadID(fmt.Sprintf("w%d", i)), Workspace: t.TempDir()})
		other, err := manager.Open(t.Context(), elsewhere, "https://example.test/files", OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		startDownload(t, manager, elsewhere, other, "w.txt", 10)
		if err := manager.ClosePage(t.Context(), elsewhere, PageID(other.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.OpenThreadDownload(t.Context(), access, DownloadKey(original.ID)); err != nil {
		t.Fatalf("the listed download does not open after others downloaded: %v", err)
	}
}

// What an engine that saves downloads itself reports must be the regular
// file at an absolute path, of the size it reported, to be openable.
func TestAnEngineSavedFileMustBeTheRegularFileItReported(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("a.bin", []byte("ten bytes!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engineSavedDownload("a.bin", 10); err == nil {
		t.Fatal("a relative saved path was accepted")
	}
	absolute, err := filepath.Abs("a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, file, err := engineSavedDownload(absolute, 10); err != nil || file == nil {
		t.Fatalf("the absolute path is refused: %v", err)
	}
	if _, _, err := engineSavedDownload(absolute, 0); err != nil {
		t.Fatalf("an unknown size is refused: %v", err)
	}
	if _, _, err := engineSavedDownload(absolute, 11); err == nil || !strings.Contains(err.Error(), "not the 11") {
		t.Fatalf("a file of another size answers %v", err)
	}
	if _, _, err := engineSavedDownload(filepath.Dir(absolute), 0); err == nil {
		t.Fatal("a folder was accepted as the saved file")
	}
	link := filepath.Join(filepath.Dir(absolute), "link.bin")
	if err := os.Symlink(absolute, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engineSavedDownload(link, 10); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Fatalf("a link answers %v", err)
	}
}

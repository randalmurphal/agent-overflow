package browser

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Where a download goes is the engine's kind: the user's Downloads folder,
// which AO bounds, moves and deletes nothing in, for every desktop engine;
// AO's artifact directories, with quotas, a per-download cap and pruning,
// for headless Chromium alone.

func TestOnlyHeadlessChromiumKeepsDownloadsAsArtifacts(t *testing.T) {
	artifactFake := newFakeEngine()
	artifactFake.artifactDownloads = true
	for _, tc := range []struct {
		name   string
		engine browserEngine
		want   bool
	}{
		{"headless Chromium", &headlessEngine{}, true},
		{"launcher-hosted WebView2", &hostedEngine{}, false},
		{"fake standing in for a desktop engine", newFakeEngine(), false},
		{"fake standing in for headless Chromium", artifactFake, true},
		{"no engine", unavailableEngine{}, false},
	} {
		manager := &Manager{engine: tc.engine}
		if got := manager.downloadsToArtifacts(); got != tc.want {
			t.Errorf("%s keeps downloads as artifacts: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The Downloads folder resolves through the environment a home controls:
// XDG_DOWNLOAD_DIR in the user-dirs file on Linux, $HOME/Downloads
// otherwise. A folder that is missing, or not a folder, is an error, never
// a fallback.
func TestTheDownloadsFolderResolvesFromTheHome(t *testing.T) {
	downloads := testDownloadsFolder(t)
	home := filepath.Dir(downloads)
	if got, err := userDownloadsDir(); err != nil || got != downloads {
		t.Fatalf("userDownloadsDir() = %q, %v, want %q", got, err, downloads)
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(home, "Elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	userDirs := filepath.Join(config, "user-dirs.dirs")
	for _, tc := range []struct{ line, want string }{
		{`XDG_DOWNLOAD_DIR="$HOME/Elsewhere"`, elsewhere},
		{`XDG_DOWNLOAD_DIR="` + elsewhere + `"`, elsewhere},
		{`XDG_DESKTOP_DIR="$HOME/Desktop"`, downloads},
	} {
		if err := os.WriteFile(userDirs, []byte("# xdg-user-dirs\n"+tc.line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		want := tc.want
		if runtime.GOOS != "linux" {
			want = downloads
		}
		if got, err := userDownloadsDir(); err != nil || got != want {
			t.Errorf("with %s: userDownloadsDir() = %q, %v, want %q", tc.line, got, err, want)
		}
	}
	if err := os.Remove(userDirs); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(downloads); err != nil {
		t.Fatal(err)
	}
	if _, err := userDownloadsDir(); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a missing Downloads folder answers %v", err)
	}
	if err := os.WriteFile(downloads, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := userDownloadsDir(); err == nil || !strings.Contains(err.Error(), "is not a folder") {
		t.Fatalf("a Downloads file answers %v", err)
	}
}

// A download in the Downloads folder takes a name nothing has: the name the
// site suggests, then " (1)", " (2)" before its extension, skipping a file
// already there and a name a running download holds. The existing file is
// never touched.
func TestADownloadNeverOverwritesAFileInDownloads(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	mine := filepath.Join(downloads, "report.pdf")
	if err := os.WriteFile(mine, []byte("the user's own report"), 0o600); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "report.pdf", 10, "hold")
	startDownload(t, manager, access, page, "report.pdf", 10)
	listed := threadDownloads(manager, access)
	if len(listed) != 2 || listed[1].Name != "report (1).pdf" || listed[0].Name != "report (2).pdf" {
		t.Fatalf("the thread lists %+v, want report (1).pdf running and report (2).pdf finished", listed)
	}
	if got, err := os.ReadFile(mine); err != nil || string(got) != "the user's own report" {
		t.Fatalf("the user's file reads %q, %v", got, err)
	}

	// A file another program creates at the name just as the download
	// takes it is left as it is, and the download takes the next name.
	theirs := takeCheckedDownloadPath(t)
	startDownload(t, manager, access, page, "race.bin", 10)
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCompleted || got.Name != "race (1).bin" {
		t.Fatalf("a download whose name was taken lists as %+v, want race (1).bin", got)
	}
	if got, _ := os.ReadFile(filepath.Join(downloads, "race.bin")); string(got) != "theirs" || *theirs != filepath.Join(downloads, "race.bin") {
		t.Fatalf("the other program's file reads %q (taken %q)", got, *theirs)
	}
}

// A name given to a running download is not given again, even before the
// engine writes its file (WKWebView's nameDownloadFile), and is free once
// the download ends.
func TestReservedNamesAreHeldUntilTheDownloadEnds(t *testing.T) {
	testDownloadsFolder(t)
	first, err := reserveDownloadPath("a.bin", nameDownloadFile)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reserveDownloadPath("a.bin", nameDownloadFile)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "a.bin" || filepath.Base(second) != "a (1).bin" {
		t.Fatalf("reserved %s and %s, want a.bin and a (1).bin", first, second)
	}
	if _, err := os.Lstat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("naming a path created it: %v", err)
	}
	releaseDownloadPath(first)
	releaseDownloadPath(second)
	again, err := reserveDownloadPath("a.bin", nameDownloadFile)
	if err != nil || again != first {
		t.Fatalf("after both ended the name reserves as %s, %v, want %s", again, err, first)
	}
	releaseDownloadPath(again)
}

// takeCheckedDownloadPath has another program create a file at the next
// Downloads path that looks free, just before a download takes it, and
// answers the path.
func takeCheckedDownloadPath(t *testing.T) *string {
	t.Helper()
	var taken string
	downloadPaths.Lock()
	downloadPathChecked = func(path string) {
		if _, err := os.Lstat(path); taken != "" || err == nil {
			return
		}
		taken = path
		if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
			t.Errorf("take %s: %v", path, err)
		}
	}
	downloadPaths.Unlock()
	t.Cleanup(func() {
		downloadPaths.Lock()
		downloadPathChecked = nil
		downloadPaths.Unlock()
	})
	return &taken
}

// A canceled or failed download in the Downloads folder takes its own
// partial file with it and nothing beside it.
func TestACanceledOrFailedDownloadRemovesOnlyItsOwnPartial(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	neighbor := filepath.Join(downloads, "neighbor.bin")
	if err := os.WriteFile(neighbor, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "held.bin", 100, "hold")
	held := threadDownloads(manager, access)[0]
	item, _ := manager.threadDownload(access.ThreadID, held.ID)
	partial := heldFile(t, manager, item.engineID)
	if err := manager.CancelThreadDownload(access, held.ID); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "broken.bin", 100, "fail")
	listed := threadDownloads(manager, access)
	if listed[0].State != downloadStateFailed || listed[1].State != downloadStateCanceled {
		t.Fatalf("the thread lists %+v, want a failure and a cancel", listed)
	}
	entries, err := os.ReadDir(downloads)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != "neighbor.bin" {
		t.Fatalf("the Downloads folder holds %q, want only the neighbor (the partial was %s)", names, partial)
	}
}

// In the Downloads folder no quota, per-download cap or pruning applies;
// all three still bound downloads AO keeps as artifacts.
func TestOnlyArtifactDownloadsAreBounded(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, mode.artifacts)
			frame := paneHandle(manager, access, page.ID)

			authorityOf(t, manager, access).downloadBytes.Store(maxWorkspaceDownloadBytes)
			admitted := manager.downloadStarted(downloadStart{Frame: frame, ID: "quota", SuggestedName: "q.bin"})
			if admitted == mode.artifacts {
				t.Fatalf("a download past the workspace quota was admitted: %v", admitted)
			}
			authorityOf(t, manager, access).downloadBytes.Store(0)

			if !manager.downloadStarted(downloadStart{Frame: frame, ID: "big", SuggestedName: "big.bin"}) {
				t.Fatal("a download within quota was refused")
			}
			manager.downloadProgress(downloadProgress{ID: "big", Received: maxDownloadBytes + 1, State: downloadInProgress})
			got := threadDownloads(manager, access)[0]
			capped := got.State == downloadStateCanceled && strings.Contains(got.Error, "download exceeds")
			if capped != mode.artifacts {
				t.Fatalf("past the per-download cap the download lists as %+v", got)
			}

			startDownload(t, manager, access, page, "old.txt", 10)
			item, err := manager.threadDownload(access.ThreadID, threadDownloads(manager, access)[0].ID)
			if err != nil || item.path == "" {
				t.Fatalf("the finished download: %+v, %v", item, err)
			}
			old := time.Now().Add(-2 * artifactMaxAge)
			if err := os.Chtimes(item.path, old, old); err != nil {
				t.Fatal(err)
			}
			manager.artifactInitMu.Lock()
			manager.artifactReady = false
			manager.artifactInitMu.Unlock()
			manager.prepareArtifacts()
			_, statErr := os.Stat(item.path)
			if pruned := errors.Is(statErr, os.ErrNotExist); pruned != mode.artifacts {
				t.Fatalf("an old download was pruned: %v (%v)", pruned, statErr)
			}
		})
	}
}

// A Downloads folder that is missing or cannot be written fails the
// download on its row with the reason; nothing falls back elsewhere.
func TestAnUnusableDownloadsFolderFailsTheRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, downloads string)
		reason string
	}{
		{"missing", func(t *testing.T, downloads string) {
			if err := os.Remove(downloads); err != nil {
				t.Fatal(err)
			}
		}, "does not exist"},
		{"not a folder", func(t *testing.T, downloads string) {
			if err := os.Remove(downloads); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(downloads, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "is not a folder"},
		{"read-only", func(t *testing.T, downloads string) {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a read-only folder")
			}
			if err := os.Chmod(downloads, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(downloads, 0o700) })
		}, "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, log, access, page := newDownloadManager(t)
			tc.break_(t, filepath.Join(os.Getenv("HOME"), "Downloads"))
			log.take()
			startDownload(t, manager, access, page, "a.bin", 10)
			listed := threadDownloads(manager, access)
			if len(listed) != 1 || listed[0].State != downloadStateFailed || !strings.Contains(listed[0].Error, tc.reason) {
				t.Fatalf("the thread lists %+v, want a failure that says %q", listed, tc.reason)
			}
			if n := countKind(log.take(), "download"); n != 1 {
				t.Fatalf("the failure was announced %d times, want once", n)
			}
		})
	}
}

// Nothing that deletes a thread, clears site data, forgets the browser or
// prunes artifacts deletes a file in the Downloads folder: neither a
// finished download nor the user's own file.
func TestForgettingBrowsingLeavesTheDownloadsFolderAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(t *testing.T, manager *Manager, access Access)
	}{
		{"thread delete", func(t *testing.T, manager *Manager, access Access) {
			if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
				t.Fatal(err)
			}
		}},
		{"clear site data", func(t *testing.T, manager *Manager, _ Access) {
			if err := manager.ClearSiteData(t.Context()); err != nil {
				t.Fatal(err)
			}
		}},
		{"browser turned off", func(t *testing.T, manager *Manager, _ Access) {
			if err := manager.Reconfigure(Config{Enabled: false}); err != nil {
				t.Fatal(err)
			}
		}},
		{"jar switch", func(t *testing.T, manager *Manager, access Access) {
			if err := manager.SetJar(access, JarPrivate); err != nil {
				t.Fatal(err)
			}
		}},
		{"artifact prune", func(t *testing.T, manager *Manager, _ Access) {
			manager.artifactInitMu.Lock()
			manager.artifactReady = false
			manager.artifactInitMu.Unlock()
			manager.prepareArtifacts()
		}},
		{"clear the list", func(t *testing.T, manager *Manager, access Access) {
			manager.ClearThreadDownloads(access)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManager(t)
			downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
			mine := filepath.Join(downloads, "mine.txt")
			if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
				t.Fatal(err)
			}
			startDownload(t, manager, access, page, "done.txt", 10)
			item, err := manager.threadDownload(access.ThreadID, threadDownloads(manager, access)[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * artifactMaxAge)
			for _, path := range []string{mine, item.path} {
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			tc.act(t, manager, access)
			for _, path := range []string{mine, item.path} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("%s: %v", path, err)
				}
			}
		})
	}
}

// browser_open_file may open a finished download its workspace's pane
// saved in the Downloads folder, and only that file: not its neighbor, not
// another workspace, and not a file that replaced it. The allowance
// outlives the download's page and the thread's list.
func TestOpenFileAllowsARecordedDownloadAndNotItsNeighbor(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	neighbor := filepath.Join(downloads, "neighbor.txt")
	if err := os.WriteFile(neighbor, []byte("not a download"), 0o600); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "notes.txt", 10)
	item, err := manager.threadDownload(access.ThreadID, threadDownloads(manager, access)[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	manager.ClearThreadDownloads(access)

	if _, err := manager.OpenFile(t.Context(), access, item.path, OpenOptions{}); err != nil {
		t.Fatalf("the workspace's finished download does not open: %v", err)
	}
	if _, err := manager.OpenFile(t.Context(), access, neighbor, OpenOptions{}); err == nil {
		t.Fatal("a file beside the download opened")
	}
	other := Access{ThreadID: "other", Workspace: t.TempDir()}
	if _, err := manager.authorizeFile(other, item.path); err == nil {
		t.Fatal("another workspace may open this workspace's download")
	}
	// A link to the download resolves to it.
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(item.path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access, link); err != nil {
		t.Fatalf("a link to the download is refused: %v", err)
	}
	// A file put in its place is not the download.
	if err := os.Remove(item.path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(item.path, bytes.Repeat([]byte("x"), 10), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access, item.path); err == nil {
		t.Fatal("a file that replaced the download opened")
	}
}

// A scope with no artifact directory has no partial file of AO's to remove:
// the engine id is never taken as a path.
func TestRemovePartialDownloadNeedsAnArtifactDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("dl-1", []byte("not AO's"), 0o600); err != nil {
		t.Fatal(err)
	}
	removePartialDownload(&jarScope{}, "dl-1")
	if _, err := os.Stat("dl-1"); err != nil {
		t.Fatalf("a scope with no artifact directory removed a file: %v", err)
	}
}

// A saved path the engine reports must name the file itself: a relative one
// would resolve against this process's directory, never the engine's.
func TestASavedDownloadPathMustBeAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("a.bin", []byte("not a download"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := savedDownload("a.bin"); err == nil {
		t.Fatal("a relative saved path was accepted")
	}
	absolute, err := filepath.Abs("a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := savedDownload(absolute); err != nil {
		t.Fatalf("the absolute path is refused: %v", err)
	}
	if _, _, err := savedDownload(filepath.Dir(absolute)); err == nil {
		t.Fatal("a folder was accepted as the saved file")
	}
}

// The record behind that allowance is bounded per workspace and in
// workspaces, dropping the oldest.
func TestTheDownloadRecordIsBounded(t *testing.T) {
	manager := &Manager{}
	file, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxRecordedDownloads + 5 {
		manager.recordFinishedDownload("/w0", fmt.Sprintf("/d/%d", i), file)
	}
	for i := 1; i <= maxRecordedWorkspaces; i++ {
		manager.recordFinishedDownload(fmt.Sprintf("/w%d", i), "/d/x", file)
	}
	if len(manager.finishedDownloads) != maxRecordedWorkspaces {
		t.Fatalf("%d workspaces recorded, want %d", len(manager.finishedDownloads), maxRecordedWorkspaces)
	}
	if _, kept := manager.finishedDownloads["/w0"]; kept {
		t.Fatal("the workspace with the oldest record was kept")
	}
	manager.finishedDownloads = nil
	for i := range maxRecordedDownloads + 5 {
		manager.recordFinishedDownload("/w0", fmt.Sprintf("/d/%d", i), file)
	}
	files := manager.finishedDownloads["/w0"].files
	if len(files) != maxRecordedDownloads || files[0].path != "/d/5" {
		t.Fatalf("the workspace records %d files from %s, want %d from /d/5", len(files), files[0].path, maxRecordedDownloads)
	}
}

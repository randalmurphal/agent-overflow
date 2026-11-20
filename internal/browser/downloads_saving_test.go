package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Where a download goes is the engine's kind (downloadSaving): an engine in
// this process writes into AO's directory and the Manager publishes the
// finished file into the user's Downloads folder; WebView2 saves there
// itself; headless Chromium's stay AO's artifacts, with quotas, a
// per-download cap and pruning. In the Downloads folder AO bounds nothing
// and deletes or replaces nothing it did not put there.

func TestEachEngineSavesDownloadsItsOwnWay(t *testing.T) {
	artifactFake := newFakeEngine()
	artifactFake.saving = downloadsAsArtifacts
	for _, tc := range []struct {
		name   string
		engine browserEngine
		want   downloadSaving
	}{
		{"headless Chromium", &headlessEngine{}, downloadsAsArtifacts},
		{"launcher-hosted WebView2", &hostedEngine{}, downloadsSavedByEngine},
		{"fake standing in for an engine in this process", newFakeEngine(), downloadsStaged},
		{"fake standing in for headless Chromium", artifactFake, downloadsAsArtifacts},
	} {
		if got := tc.engine.DownloadSaving(); got != tc.want {
			t.Errorf("%s saves downloads as %d, want %d", tc.name, got, tc.want)
		}
		manager := &Manager{engine: tc.engine}
		if got := manager.downloadsToArtifacts(); got != (tc.want == downloadsAsArtifacts) {
			t.Errorf("%s keeps downloads as artifacts: %v", tc.name, got)
		}
	}
}

// The staging directory is the app's own, private, in the Downloads
// folder: created with a missing Downloads folder, a real directory (a link
// put in its place is refused, and the download fails on its row with
// nothing written through it), and gone once no download stages in it.
func TestTheStagingDirectoryIsPrivateAndGoesWhenEmpty(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	if err := os.Remove(downloads); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "held.bin", 10, "hold")
	staging := filepath.Join(downloads, stagingDirName)
	info, err := os.Lstat(staging)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the staging directory: %v, %v", info, err)
	}
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if err := fakeProfileOf(t, manager).finishHeld(item.engineID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the empty staging directory stayed: %v", err)
	}
	if names := folderNames(t, downloads); !slices.Equal(names, []string{"held.bin"}) {
		t.Fatalf("the Downloads folder holds %q", names)
	}

	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, staging); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "linked.bin", 10)
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateFailed || !strings.Contains(got.Error, "is not a folder") {
		t.Fatalf("a download through a linked staging directory lists as %+v", got)
	}
	if names := folderNames(t, elsewhere); len(names) != 0 {
		t.Fatalf("the link's target holds %q", names)
	}
}

// A staged download the Manager refuses gives up its staged file, and the
// staging folder goes once no download stages in it, even one whose engine
// has not created its file yet; a staged file is in its process's run
// directory.
func TestARefusedStagedDownloadLeavesNothing(t *testing.T) {
	manager, _, _, _ := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	if _, err := manager.stageDownload("unwritten"); err != nil {
		t.Fatal(err)
	}
	staged, err := manager.stageDownload("refused")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^run-[0-9a-f]{12}$`).MatchString(filepath.Base(filepath.Dir(staged))) {
		t.Fatalf("the staged file %s is in no run directory of its process", staged)
	}
	if err := os.WriteFile(staged, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if manager.downloadStarted(downloadStart{Frame: "no such frame", ID: "refused", SuggestedName: "r.bin"}) == downloadAdmitted {
		t.Fatal("a download from no page was admitted")
	}
	if names := folderNames(t, downloads); !slices.Equal(names, []string{stagingDirName}) {
		t.Fatalf("with a download still staging the Downloads folder holds %q", names)
	}
	if names := folderNames(t, filepath.Dir(staged)); !slices.Equal(names, []string{runLockName}) {
		t.Fatalf("after the refusal the run directory holds %q", names)
	}
	manager.releaseStagedDownload("unwritten")
	if names := folderNames(t, downloads); len(names) != 0 {
		t.Fatalf("after the refusal the Downloads folder holds %q", names)
	}
}

// Releasing a staged file removes it and its engine's sidecar, and nothing
// else beside it that starts with its name.
func TestReleasingAStagedFileRemovesOnlyItAndItsSidecar(t *testing.T) {
	downloads := testDownloadsFolder(t)
	staged, err := stageDownload()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{staged, staged + ".wkdownload", staged + ".user-notes.txt"} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	releaseStaged(staged)
	if got := folderNames(t, filepath.Dir(staged)); !slices.Equal(got, []string{filepath.Base(staged) + ".user-notes.txt"}) {
		t.Fatalf("after the release the run directory holds %q, want only the file AO never staged", got)
	}
	if got := folderNames(t, filepath.Join(downloads, stagingDirName)); len(got) != 1 {
		t.Fatalf("the staging folder holds %q, want the run directory the file AO never staged keeps", got)
	}
}

// A canceled or failed download leaves the Downloads folder as it was: an
// engine in this process wrote only into the staging folder, which goes
// with its staged file, and one that saves there itself takes its own
// partial file with it and nothing beside it.
func TestACanceledOrFailedDownloadLeavesDownloadsAsItWas(t *testing.T) {
	for _, mode := range []struct {
		name   string
		saving downloadSaving
	}{{"staged", downloadsStaged}, {"saved by the engine", downloadsSavedByEngine}} {
		t.Run(mode.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, mode.saving)
			downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
			neighbor := filepath.Join(downloads, "neighbor.bin")
			if err := os.WriteFile(neighbor, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			startDownload(t, manager, access, page, "held.bin", 100, "hold")
			held := threadDownloads(manager, access)[0]
			item, _ := manager.threadDownload(access, DownloadKey(held.ID))
			partial := heldFile(t, manager, item.engineID)
			// While it runs, only the staging folder is new in Downloads, or
			// the engine's own file where it saves there itself.
			want := []string{stagingDirName, "neighbor.bin"}
			if mode.saving == downloadsSavedByEngine {
				want = []string{"held.bin", "neighbor.bin"}
			} else if filepath.Dir(filepath.Dir(partial)) != filepath.Join(downloads, stagingDirName) {
				t.Fatalf("the running download writes %s", partial)
			}
			if names := folderNames(t, downloads); !slices.Equal(names, want) {
				t.Fatalf("during the download the Downloads folder holds %q, want %q", names, want)
			}
			if err := manager.CancelThreadDownload(access, DownloadKey(held.ID)); err != nil {
				t.Fatal(err)
			}
			startDownload(t, manager, access, page, "broken.bin", 100, "fail")
			listed := threadDownloads(manager, access)
			if listed[0].State != downloadStateFailed || listed[1].State != downloadStateCanceled {
				t.Fatalf("the thread lists %+v, want a failure and a cancel", listed)
			}
			if _, err := os.Lstat(partial); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the partial file %s is still there: %v", partial, err)
			}
			if names := folderNames(t, downloads); !slices.Equal(names, []string{"neighbor.bin"}) {
				t.Fatalf("the Downloads folder holds %q, want only the neighbor", names)
			}
		})
	}
}

// No quota, per-download cap or pruning applies to a download that ends up
// in the Downloads folder; all three bound downloads AO keeps as
// artifacts.
func TestOnlyArtifactDownloadsAreBounded(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			artifacts := mode.saving == downloadsAsArtifacts
			manager, _, access, page := newDownloadManagerFor(t, mode.saving)
			frame := paneHandle(manager, access, page.ID)

			authorityOf(t, manager, access).downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
			admitted := manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "quota", SuggestedName: "q.bin"}) == downloadAdmitted
			if admitted == artifacts {
				t.Fatalf("a download past the workspace quota was admitted: %v", admitted)
			}
			authorityOf(t, manager, access).downloadBytes.SetUsed(0)

			if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "big", SuggestedName: "big.bin"}) != downloadAdmitted {
				t.Fatal("a download within quota was refused")
			}
			manager.downloadProgress(downloadProgress{ID: "big", Received: maxDownloadBytes + 1, State: downloadInProgress})
			got := threadDownloads(manager, access)[0]
			capped := got.State == downloadStateCanceled && strings.Contains(got.Error, "download exceeds")
			if capped != artifacts {
				t.Fatalf("past the per-download cap the download lists as %+v", got)
			}

			startDownload(t, manager, access, page, "old.txt", 10)
			item, err := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
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
			if err := manager.prepareArtifacts(); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(item.path)
			if pruned := errors.Is(statErr, os.ErrNotExist); pruned != artifacts {
				t.Fatalf("an old download was pruned: %v (%v)", pruned, statErr)
			}
		})
	}
}

// Nothing that deletes a thread, clears site data, forgets the browser or
// prunes artifacts deletes a file in the Downloads folder: neither a
// finished download nor the user's own file.
func TestForgettingBrowsingLeavesTheDownloadsFolderAlone(t *testing.T) {
	for _, mode := range []struct {
		name   string
		saving downloadSaving
	}{{"staged", downloadsStaged}, {"saved by the engine", downloadsSavedByEngine}} {
		for _, tc := range []struct {
			name string
			act  func(t *testing.T, manager *Manager, access Access)
		}{
			{"thread delete", func(t *testing.T, manager *Manager, access Access) {
				if err := manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
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
				if err := manager.SetJar(context.Background(), access, JarPrivate, JarSwitchConfirmed); err != nil {
					t.Fatal(err)
				}
			}},
			{"artifact prune", func(t *testing.T, manager *Manager, _ Access) {
				manager.artifactInitMu.Lock()
				manager.artifactReady = false
				manager.artifactInitMu.Unlock()
				if err := manager.prepareArtifacts(); err != nil {
					t.Fatal(err)
				}
			}},
			{"clear the list", func(t *testing.T, manager *Manager, access Access) {
				if err := manager.ClearThreadDownloads(access); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				manager, _, access, page := newDownloadManagerFor(t, mode.saving)
				downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
				mine := filepath.Join(downloads, "mine.txt")
				if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
					t.Fatal(err)
				}
				startDownload(t, manager, access, page, "done.txt", 10)
				item, err := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
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
}

// A scope with no artifact directory, and a download with no staged file,
// has no partial file of AO's to remove: the engine id is never taken as a
// path.
func TestADiscardNeedsAFileOfAOs(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			manager, _, _, _ := newDownloadManagerFor(t, mode.saving)
			t.Chdir(t.TempDir())
			if err := os.WriteFile("dl-1", []byte("not AO's"), 0o600); err != nil {
				t.Fatal(err)
			}
			manager.finishDiscard(downloadDiscard{id: "dl-1", file: manager.downloadFileOf(&jarScope{}, "dl-1")})
			manager.finishDiscard(downloadDiscard{id: "dl-1", file: manager.downloadFileOf(nil, "dl-1")})
			if _, err := os.Stat("dl-1"); err != nil {
				t.Fatalf("a download with no file of AO's removed one: %v", err)
			}
		})
	}
}

package browser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// settleRig starts a download on a page of an engine keeping downloads as
// artifacts, with the file the engine wrote for it, and answers what the
// quotas held before it started.
func settleRig(t *testing.T) (m *Manager, access Access, file string, workspaceBefore, artifactsBefore int64) {
	t.Helper()
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	authority := authorityOf(t, manager, access)
	workspaceBefore, artifactsBefore = authority.downloadBytes.Used(), manager.artifactBytes.Used()
	file = engineArtifact(t, manager, "dl-1")
	if manager.downloadStarted(downloadStart{Frame: frameID(paneHandle(manager, access, page.ID)), ID: "dl-1", SuggestedName: "a.bin"}) != downloadAdmitted {
		t.Fatal("the download was refused")
	}
	return manager, access, file, workspaceBefore, artifactsBefore
}

func wantCharged(t *testing.T, m *Manager, access Access, workspace, artifacts int64) {
	t.Helper()
	if got := authorityOf(t, m, access).downloadBytes.Used(); got != workspace {
		t.Errorf("the workspace holds %d download bytes, want %d", got, workspace)
	}
	if got := m.artifactBytes.Used(); got != artifacts {
		t.Errorf("the artifact quota holds %d bytes, want %d", got, artifacts)
	}
}

// A completion naming another file of the jar's directory than the one the
// row has owned since its admission publishes nothing: the sibling's bytes
// stay where they are and are no one's download, the row fails with the
// reason, and its own file goes with its reservation.
func TestAnArtifactCompletionNamingAnotherFileIsRefused(t *testing.T) {
	manager, access, file, workspaceBefore, artifactsBefore := settleRig(t)
	sibling := filepath.Join(filepath.Dir(file), "dl-2")
	if err := os.WriteFile(sibling, []byte("another download's bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.downloadProgress(downloadProgress{ID: "dl-1", Received: 23, Total: 23, State: downloadCompleted, FilePath: sibling})
	row := threadDownloads(manager, access)[0]
	if row.State != downloadStateFailed || !strings.Contains(row.Error, "other than the file it was given") {
		t.Fatalf("the row is %+v, want it failed for the other file", row)
	}
	if got, err := os.ReadFile(sibling); err != nil || string(got) != "another download's bytes" {
		t.Fatalf("the sibling holds %q (%v), want it untouched", got, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the row's own file is still there: %v", err)
	}
	finished, err := os.ReadDir(authorityOf(t, manager, access).downloadDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(finished) != 0 {
		t.Fatalf("the workspace's finished downloads hold %v", finished)
	}
	wantCharged(t, manager, access, workspaceBefore, artifactsBefore)
}

// A finished download's file replaced while the tab it opens in is made is
// not opened: the open refuses with the reason, and the new tab never
// navigates to the replacement.
func TestOpeningADownloadRefusesAFileReplacedWhileItsTabOpens(t *testing.T) {
	for _, saving := range []downloadSaving{downloadsAsArtifacts, downloadsStaged} {
		t.Run(fmt.Sprint(saving), func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, saving)
			startDownload(t, manager, access, page, "chart.png", 10)
			row := threadDownloads(manager, access)[0]
			item, err := manager.threadDownload(access, DownloadKey(row.ID))
			if err != nil {
				t.Fatal(err)
			}
			// Opening the download makes its tab first, then opens the file.
			manager.engine.(*fakeEngine).beforeNewPage = func() {
				replacement := item.path + ".new"
				if err := os.WriteFile(replacement, []byte("not the download"), 0o600); err != nil {
					t.Error(err)
				}
				if err := os.Rename(replacement, item.path); err != nil {
					t.Error(err)
				}
			}
			_, err = manager.OpenThreadDownload(t.Context(), access, DownloadKey(row.ID))
			if !errors.Is(err, errDownloadReplaced) {
				t.Fatalf("opening a replaced download answered %v", err)
			}
			for _, tab := range manager.CompanionState(access.Caller()).Pages {
				if strings.HasSuffix(tab.URL, filepath.ToSlash(item.path)) {
					t.Fatalf("a tab shows the replacement at %s", tab.URL)
				}
			}
		})
	}
}

// A parked download stopped over the cap before its fate, then refused at
// its fate, is canceled in its engine once.
func TestAParkedDownloadStoppedAndThenRefusedIsCanceledOnce(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	// Another tab keeps the jar open, so a cancel can reach the engine.
	openPage(t, manager, access, "https://example.test/other")
	file := engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, maxDownloadBytes+1))
	awaitRemoved(t, file)
	manager.removeClosedPage(ids.PageHandle(opener))
	manager.removeClosedPage("popup")
	time.Sleep(50 * time.Millisecond)
	if got := engine.cancels(); len(got) != 1 {
		t.Fatalf("the engine was asked to cancel %q, want dl-1 once", got)
	}
}

// A popup's parked download whose jar's last profile went before its fate
// is refused at the fate: its file in the jar's directory goes and both
// quotas are back as they were, with no row anywhere.
func TestAParkedDownloadWhoseJarWentIsRemovedAtItsFate(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	manager.parkTimeout = 50 * time.Millisecond
	authority := authorityOf(t, manager, access)
	workspace, artifacts := authority.downloadBytes.Used(), manager.artifactBytes.Used()
	file := engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	if got := authority.downloadBytes.Used() - workspace; got != maxDownloadBytes {
		t.Fatalf("before its fate the download holds %d workspace bytes, want %d", got, maxDownloadBytes)
	}
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, 3))
	if err := manager.ClosePage(t.Context(), access, PageID(manager.CompanionState(access.Caller()).Pages[0].ID)); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	open := len(manager.scopes)
	manager.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d jars are open after the last tab closed", open)
	}
	awaitRemoved(t, file)
	if popups, downloads := parkedState(manager); popups != 0 || downloads != 0 {
		t.Fatalf("after its fate %d popups and %d downloads are parked", popups, downloads)
	}
	if got := authority.downloadBytes.Used(); got != workspace {
		t.Errorf("after its fate the workspace holds %d bytes, want %d", got, workspace)
	}
	if got := manager.artifactBytes.Used(); got != artifacts {
		t.Errorf("after its fate the artifact quota holds %d bytes, want %d", got, artifacts)
	}
	if listed := threadDownloads(manager, access); len(listed) != 0 {
		t.Fatalf("the thread lists %+v", listed)
	}
}

// A download the engine ends on its own (a network failure) leaves no file
// of AO's and holds nothing of either quota.
func TestAFailedArtifactDownloadLeavesNoFile(t *testing.T) {
	manager, access, file, workspace, artifacts := settleRig(t)
	manager.downloadProgress(downloadProgress{ID: "dl-1", Received: 3, State: downloadFailed, Error: "the network went away"})
	awaitRemoved(t, file)
	wantCharged(t, manager, access, workspace, artifacts)
	if listed := threadDownloads(manager, access); len(listed) != 1 || listed[0].State != downloadStateFailed {
		t.Fatalf("the thread lists %+v, want the failure", listed)
	}
}

// A finished download is charged the size of the file it kept, whatever
// the engine reported receiving.
func TestAFinishedArtifactIsChargedItsSizeOnDisk(t *testing.T) {
	manager, access, _, workspace, artifacts := settleRig(t)
	manager.downloadProgress(downloadProgress{ID: "dl-1", Received: 0, State: downloadCompleted})
	listed := threadDownloads(manager, access)
	if len(listed) != 1 || listed[0].State != downloadStateCompleted || listed[0].ReceivedBytes != int64(len("bytes")) {
		t.Fatalf("the thread lists %+v, want it completed with its file's 5 bytes", listed)
	}
	wantCharged(t, manager, access, workspace+5, artifacts+5)
}

// A finished file past the per-download cap on disk is not kept, though
// the engine reported it within the cap.
func TestAFinishedArtifactOverTheCapOnDiskIsNotKept(t *testing.T) {
	manager, access, file, workspace, artifacts := settleRig(t)
	if err := os.Truncate(file, maxDownloadBytes+1); err != nil {
		t.Fatal(err)
	}
	manager.downloadProgress(downloadProgress{ID: "dl-1", Received: 10, State: downloadCompleted})
	awaitRemoved(t, file)
	listed := threadDownloads(manager, access)
	if len(listed) != 1 || listed[0].State != downloadStateFailed || listed[0].Error != fmt.Sprintf("download exceeds %d bytes", maxDownloadBytes) {
		t.Fatalf("the thread lists %+v, want it failed over the cap", listed)
	}
	wantCharged(t, manager, access, workspace, artifacts)
}

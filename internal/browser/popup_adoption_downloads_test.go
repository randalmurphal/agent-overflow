package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// A download a popup started and finished before its adoption began is
// listed in the popup's tab, finished.
func TestCompletedDownloadBeforeAdoptionStarts(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, 40))
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 100))
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", "dl-1 completed 100")
	wantDownloads(t, manager, opener)
	if len(manager.parked) != 0 || len(manager.parkedIDs) != 0 {
		t.Fatalf("the Manager still parks %d popups, %d downloads", len(manager.parked), len(manager.parkedIDs))
	}
}

// A popup that closes before its tab is listed leaves its downloads in its
// opener's tab, a running one running, and a download it starts after goes
// there at once.
func TestAPopupClosedBeforeItsListingLeavesItsDownloadsWithItsOpener(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	popupDownload(manager, engine, "popup", "dl-1", opener)
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, 10))
	popupDownload(manager, engine, "popup", "dl-2", opener)
	manager.downloadProgress(progressOf("dl-2", downloadCompleted, 100))
	manager.removeClosedPage("popup")
	wantDownloads(t, manager, opener, "dl-1 in_progress 10", "dl-2 completed 100")
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 100))
	if !popupDownload(manager, engine, "popup", "dl-3", opener) {
		t.Fatal("a download of the closed popup was refused")
	}
	wantDownloads(t, manager, opener, "dl-1 completed 100", "dl-2 completed 100", "dl-3 in_progress 0")
}

// A popup whose attach fails is discarded, and its downloads go to its
// opener, whose handle the adoption knew even when the download did not.
func TestAPopupThatIsNotAdoptedLeavesItsDownloadsWithItsOpener(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	engine.failing["popup"] = true
	popupDownload(manager, engine, "popup", "dl-1")
	manager.adoptPopup(engine.popup(opener, "popup"))
	if got := engine.discards(); !slices.Equal(got, []ids.PageHandle{"popup"}) {
		t.Fatalf("discarded %v", got)
	}
	wantDownloads(t, manager, opener, "dl-1 in_progress 0")
}

// A popup's download is listed with its terminal state before the popup's
// close takes its tab, however the close, the download's last progress
// and the park timeout fall around the popup's listing: before it, between
// its tab's listing and its downloads' (popupListing), or after.
func TestAPopupsDownloadEndsBeforeItsCloseInEveryOrder(t *testing.T) {
	type step func(m *Manager, e *adoptionEngine)
	complete := func(m *Manager, _ *adoptionEngine) { m.downloadProgress(progressOf("dl-1", downloadCompleted, 100)) }
	closePopup := func(m *Manager, _ *adoptionEngine) { m.removeClosedPage("popup") }
	expire := func(m *Manager, _ *adoptionEngine) { m.resolveParked("popup", nil) }
	for _, tc := range []struct {
		name                  string
		before, during, after []step
		// inTab says the download is still listed in the popup's tab.
		inTab bool
	}{
		{name: "all before", before: []step{complete, closePopup}},
		{name: "complete before, close during", before: []step{complete}, during: []step{closePopup}},
		{name: "both during", during: []step{complete, closePopup}},
		{name: "complete during, close after", during: []step{complete}, after: []step{closePopup}},
		{name: "complete before, close after", before: []step{complete}, after: []step{closePopup}},
		{name: "expiry during", before: []step{complete}, during: []step{expire}, inTab: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManager(t)
			if !popupDownload(manager, engine, "popup", "dl-1", opener) {
				t.Fatal("the popup's download was refused")
			}
			manager.downloadProgress(progressOf("dl-1", downloadInProgress, 10))
			for _, s := range tc.before {
				s(manager, engine)
			}
			var wg sync.WaitGroup
			manager.popupListing = func() {
				// On a goroutine, since the listing may hold what they wait
				// for; the pause gives them the chance to run in the window.
				wg.Go(func() {
					for _, s := range tc.during {
						s(manager, engine)
					}
				})
				time.Sleep(50 * time.Millisecond)
			}
			manager.adoptPopup(engine.popup(opener, "popup"))
			wg.Wait()
			for _, s := range tc.after {
				s(manager, engine)
			}
			var got []string
			for _, d := range manager.CompanionState(access.Caller()).Downloads {
				got = append(got, d.State)
			}
			if !slices.Equal(got, []string{downloadStateCompleted}) {
				t.Fatalf("the thread lists downloads %v, want one completed", got)
			}
			if tc.inTab {
				wantDownloads(t, manager, "popup", "dl-1 completed 100")
			}
			if got := engine.cancels(); len(got) != 0 {
				t.Errorf("canceled %v", got)
			}
		})
	}
}

// A popup's tab closed by the user between its listing and the listing of
// its downloads leaves them to its nearest listed opener, as a popup never
// listed does: a fate naming a tab that is gone falls back to the openers.
func TestAPopupClosedPubliclyBeforeItsDownloadsAreListedLeavesThemWithItsOpener(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 100))
	var wg sync.WaitGroup
	manager.popupListing = func() {
		var id string
		for _, p := range manager.ownedPages(access.tb) {
			if p.driver.Handle() == "popup" {
				id = string(p.id)
			}
		}
		wg.Go(func() {
			if err := manager.ClosePage(context.Background(), access, PageID(id)); err != nil {
				t.Errorf("close the popup's tab: %v", err)
			}
		})
		time.Sleep(50 * time.Millisecond)
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wg.Wait()
	if listed(manager, "popup") {
		t.Fatal("the closed popup is still listed")
	}
	wantDownloads(t, manager, opener, "dl-1 completed 100")
}

// A finished download listed only at its popup's adoption is held to the
// per-download cap of an engine whose downloads are AO's.
func TestAParkedFinishedDownloadOverTheCapIsNotKept(t *testing.T) {
	manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	file := engineArtifact(t, manager, "dl-1")
	popupDownload(manager, engine, "popup", "dl-1", opener)
	manager.downloadProgress(downloadProgress{ID: "dl-1", Received: maxDownloadBytes + 1, State: downloadCompleted})
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", fmt.Sprintf("dl-1 canceled %d", maxDownloadBytes+1))
	awaitRemoved(t, file)
}

// A popup's download not listed yet holds its bytes against the artifact
// quota from its start, and one passing the per-download cap is stopped,
// its file gone, before its popup's fate; the fate lists it canceled.
func TestAParkedDownloadIsBoundedBeforeItsFate(t *testing.T) {
	manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	before := manager.artifactBytes.Used()
	file := engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	if got := manager.artifactBytes.Used() - before; got != maxDownloadBytes {
		t.Fatalf("a parked download reserved %d artifact bytes, want %d", got, maxDownloadBytes)
	}
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, maxDownloadBytes+1))
	awaitRemoved(t, file)
	if got := engine.cancels(); !slices.Equal(got, []downloadID{"dl-1"}) {
		t.Fatalf("the engine canceled %q before the popup's fate, want dl-1", got)
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", fmt.Sprintf("dl-1 canceled %d", maxDownloadBytes+1))
	// Its row's discard settles the reservation once its file is gone.
	for deadline := time.Now().Add(hostedTestDeadline); manager.artifactBytes.Used() != before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after the fate %d artifact bytes are reserved, want %d", manager.artifactBytes.Used(), before)
		}
	}
}

// Parked downloads past the artifact quota are refused as they start.
func TestParkedDownloadsStopAtTheArtifactQuota(t *testing.T) {
	manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	manager.artifactBytes.SetUsed(maxArtifactRootBytes - maxDownloadBytes)
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("a parked download within the quota was refused")
	}
	if popupDownload(manager, engine, "popup", "dl-2", opener) {
		t.Fatal("a parked download past the artifact quota was admitted")
	}
}

// A popup's download refused at its fate (no page that opened it is still
// a tab) is listed in no tab, and its file in the jar's download directory
// goes with it, and its reservation: nothing keeps it, so it would sit
// outside every quota.
func TestAParkedDownloadRefusedAtItsFateLeavesNoFile(t *testing.T) {
	for _, state := range []downloadState{downloadCompleted, downloadInProgress} {
		t.Run(state.String(), func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
			// Another tab keeps the jar open.
			mustOpen(t, manager, access, "https://example.test/other")
			before := manager.artifactBytes.Used()
			file := engineArtifact(t, manager, "dl-1")
			if !popupDownload(manager, engine, "popup", "dl-1", opener) {
				t.Fatal("the popup's download was refused")
			}
			manager.downloadProgress(progressOf("dl-1", state, 100))
			manager.removeClosedPage(opener)
			manager.removeClosedPage("popup")
			awaitRemoved(t, file)
			// The reservation settles once the file is gone.
			for deadline := time.Now().Add(hostedTestDeadline); manager.artifactBytes.Used() != before; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("%d artifact bytes are reserved, want %d", manager.artifactBytes.Used(), before)
				}
			}
		})
	}
}

// A popup's download holds its workspace's allowance from its start, as a
// listed one does: one past the workspace's quota is refused before the
// engine writes it, whatever the artifact quota has left.
func TestAParkedDownloadHoldsItsWorkspacesAllowance(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	authority := authorityOf(t, manager, access)
	before := authority.downloadBytes.Used()
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("a parked download within the quotas was refused")
	}
	if got := authority.downloadBytes.Used() - before; got != maxDownloadBytes {
		t.Fatalf("a parked download holds %d of its workspace's bytes, want %d", got, maxDownloadBytes)
	}
	authority.downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
	if popupDownload(manager, engine, "popup", "dl-2", opener) {
		t.Fatal("a parked download past its workspace's quota was admitted")
	}
}

// A parked download's listing takes its reservation over: with the artifact
// quota taken up meanwhile, the download its popup finished is listed as it
// is, not canceled over quota, and holds its size of the quotas after.
func TestAParkedDownloadsListingKeepsItsReservation(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 5))
	// Another download takes everything the artifact quota has left as the
	// listing begins.
	manager.downloadAdmitting = func() {
		if _, err := manager.reserveArtifacts(maxArtifactRootBytes - manager.artifactBytes.Used()); err != nil {
			t.Error("the competing reservation was refused")
		}
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", "dl-1 completed 5")
	if got := manager.artifactBytes.Used(); got != maxArtifactRootBytes-maxDownloadBytes+5 {
		t.Fatalf("%d artifact bytes are reserved, want the competitor's and the file's", got)
	}
	if got := authorityOf(t, manager, access).downloadBytes.Used(); got != 5 {
		t.Fatalf("the workspace holds %d download bytes, want the file's 5", got)
	}
}

// A finished parked download whose jar closed with its last tab is still
// removed with the browser: the download keeps the directory it was
// written in.
func TestAParkedDownloadOfAClosedJarIsRemovedWithTheBrowser(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	before := manager.artifactBytes.Used()
	file := engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 5))
	if err := manager.ClosePage(t.Context(), access, PageID(manager.CompanionState(access.Caller()).Pages[0].ID)); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	open := len(manager.scopes)
	manager.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d jars are open after the last tab closed", open)
	}
	if err := manager.closeBrowser(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the parked download's file survives the browser's close: %v", err)
	}
	if got := manager.artifactBytes.Used(); got != before {
		t.Fatalf("after the close %d artifact bytes are reserved, want %d", got, before)
	}
}

// The downloads of a popup whose fate never comes go to its opener once
// the park times out, with the newest progress reported meanwhile.
func TestParkedDownloadsGoToTheOpenerAfterTheTimeout(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	manager.parkTimeout = 10 * time.Millisecond
	popupDownload(manager, engine, "popup", "dl-1", opener)
	for i := range 50 {
		manager.downloadProgress(progressOf("dl-1", downloadInProgress, float64(i)))
	}
	// The row is listed, then takes its newest progress.
	want := []string{"dl-1 in_progress 49"}
	for deadline := time.Now().Add(hostedTestDeadline); !slices.Equal(downloadsOf(manager, opener), want); {
		if time.Now().After(deadline) {
			t.Fatalf("tab %s lists %q, want %q", opener, downloadsOf(manager, opener), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A popup parks at most maxParkedDownloads downloads, and the Manager at
// most maxParkedPopups popups; past either a download is refused, so its
// engine cancels it.
func TestParkedDownloadsAreBounded(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	for i := range maxParkedDownloads {
		if !popupDownload(manager, engine, "popup", downloadID(fmt.Sprintf("dl-%d", i)), opener) {
			t.Fatalf("download %d was refused", i)
		}
	}
	if popupDownload(manager, engine, "popup", "dl-over", opener) {
		t.Fatal("a download past the popup's bound was taken")
	}
	for i := 1; i < maxParkedPopups; i++ {
		if !popupDownload(manager, engine, ids.PageHandle(fmt.Sprintf("popup-%d", i)), downloadID(fmt.Sprintf("p-%d", i)), opener) {
			t.Fatalf("popup %d's download was refused", i)
		}
	}
	if popupDownload(manager, engine, "popup-over", "p-over", opener) {
		t.Fatal("a download of a popup past the bound was taken")
	}
	manager.parkMu.Lock()
	popups, ids := len(manager.parked), len(manager.parkedIDs)
	manager.parkMu.Unlock()
	if popups != maxParkedPopups || ids != maxParkedDownloads+maxParkedPopups-1 {
		t.Fatalf("parked %d popups, %d downloads", popups, ids)
	}
}

// A popup that goes with no page that opened it still listed refuses its
// downloads: the engine cancels a running one, and a finished one stays
// where it was saved.
func TestParkedDownloadsWithNoListedOpenerAreCanceled(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	// Another thread's page keeps the profile, which cancels.
	if _, err := manager.Open(t.Context(), manager.Bind(Caller{ThreadID: "other", Workspace: access.Workspace()}), "https://example.test/", OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	popupDownload(manager, engine, "popup", "dl-running", opener)
	popupDownload(manager, engine, "popup", "dl-done", opener)
	manager.downloadProgress(progressOf("dl-done", downloadCompleted, 100))
	manager.removeClosedPage(opener)
	manager.removeClosedPage("popup")
	for deadline := time.Now().Add(hostedTestDeadline); len(engine.cancels()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the running download was never canceled")
		}
		time.Sleep(time.Millisecond)
	}
	if got := engine.cancels(); !slices.Equal(got, []downloadID{"dl-running"}) {
		t.Fatalf("canceled %v", got)
	}
	if popupDownload(manager, engine, "popup", "dl-late", opener) {
		t.Fatal("a download of a popup with no listed opener was taken")
	}
}

// The browser's close drops every parked download and every fate.
func TestTheBrowsersCloseDropsParkedDownloads(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	popupDownload(manager, engine, "popup", "dl-1", opener)
	manager.resolveParked("gone", nil, opener)
	if err := manager.closeBrowser(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager.parkMu.Lock()
	defer manager.parkMu.Unlock()
	if len(manager.parked) != 0 || len(manager.parkedIDs) != 0 || manager.popupFates != nil {
		t.Fatalf("parked %d, ids %d, fates %v", len(manager.parked), len(manager.parkedIDs), manager.popupFates)
	}
}

// A headless popup's download the browser's close drops takes its file in
// the jar's download directory with it, finished or not, and its
// reservation: no row lists it, and nothing else would remove it.
func TestTheBrowsersCloseRemovesParkedArtifacts(t *testing.T) {
	for _, state := range []downloadState{downloadCompleted, downloadInProgress} {
		t.Run(state.String(), func(t *testing.T) {
			manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
			before := manager.artifactBytes.Used()
			file := engineArtifact(t, manager, "dl-1")
			popupDownload(manager, engine, "popup", "dl-1", opener)
			manager.downloadProgress(progressOf("dl-1", state, 100))
			if err := manager.closeBrowser(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the parked download's file survives the browser's close: %v", err)
			}
			if got := manager.artifactBytes.Used(); got != before {
				t.Fatalf("after the close %d artifact bytes are reserved, want %d", got, before)
			}
		})
	}
}

package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
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

// A download a popup started before its listing stays in the popup's tab
// when whatever takes the tab out does so between the hold that lists the
// tab with its downloads and the application of their parked ends
// (popupListing), and settles as that leave settles the tab's others: an
// end already received is kept, one parking stopped over the cap stays
// canceled for it, and anything else is canceled with the leave's reason,
// its file removed and its reservation returned. Its engine is asked to
// cancel it by whatever stopped it: the leave, or for one over the cap,
// parking's stop and the stop of the row it is listed as, which retries a
// cancel that failed (stopOverCap).
func TestAPopupsTabTakenOutBeforeItsDownloadsSettleKeepsThem(t *testing.T) {
	removers := []struct {
		name, reason string
		// forgets is set for a remover that takes the thread's history.
		forgets bool
		remove  func(m *Manager, access Access, tab PageID) error
	}{
		{"its tab closed", "page closed", false, func(m *Manager, access Access, tab PageID) error {
			return m.ClosePage(context.Background(), access, tab)
		}},
		{"its thread closed", "page closed", true, func(m *Manager, access Access, _ PageID) error {
			return m.CloseThread(context.Background(), access.ThreadID())
		}},
		{"its thread's jar switched", "page closed", false, func(m *Manager, access Access, _ PageID) error {
			return m.SetJar(context.Background(), access, JarPrivate, JarSwitchConfirmed)
		}},
		{"the browser closed", "the browser closed", false, func(m *Manager, _ Access, _ PageID) error {
			return m.closeBrowser(context.Background())
		}},
	}
	type kind struct {
		name     string
		progress downloadProgress
		// want is the popup's row as "id state bytes cause" once reason's
		// leave settled it.
		want func(reason string, saving downloadSaving) string
		// cancels is what its engine is asked to cancel; kept is whether
		// its file stays.
		cancels []downloadID
		kept    bool
		// artifactsOnly is set for a kind only an engine whose downloads
		// are AO's has.
		artifactsOnly bool
	}
	kinds := []kind{
		{name: "running", progress: progressOf("dl-1", downloadInProgress, 10), cancels: []downloadID{"dl-1"}, want: func(reason string, _ downloadSaving) string {
			return "dl-1 canceled 10 " + reason
		}},
		{name: "completed", progress: progressOf("dl-1", downloadCompleted, 5), kept: true, want: func(_ string, saving downloadSaving) string {
			if saving == downloadsAsArtifacts {
				return "dl-1 completed 5 "
			}
			return "dl-1 completed 5 saved elsewhere"
		}},
		{name: "stopped over the cap while parked", progress: progressOf("dl-1", downloadInProgress, maxDownloadBytes+1), cancels: []downloadID{"dl-1", "dl-1"}, artifactsOnly: true, want: func(string, downloadSaving) string {
			return fmt.Sprintf("dl-1 canceled %d download exceeds %d bytes", maxDownloadBytes+1, maxDownloadBytes)
		}},
	}
	for _, remover := range removers {
		for _, saved := range []struct {
			name   string
			saving downloadSaving
		}{{"saved by its engine", downloadsSavedByEngine}, {"kept as an artifact", downloadsAsArtifacts}} {
			for _, kind := range kinds {
				if kind.artifactsOnly && saved.saving != downloadsAsArtifacts {
					continue
				}
				saving := saved.saving
				t.Run(fmt.Sprintf("%s, %s, %s", remover.name, saved.name, kind.name), func(t *testing.T) {
					manager, engine, access, opener := newAdoptionManagerSaving(t, saving)
					before := manager.artifactBytes.Used()
					var file string
					if saving == downloadsAsArtifacts {
						file = engineArtifact(t, manager, "dl-1")
					}
					if !popupDownload(manager, engine, "popup", "dl-1", opener) {
						t.Fatal("the popup's download was refused")
					}
					manager.downloadProgress(kind.progress)
					if len(kind.cancels) > 1 {
						// Parking's stop: its cancel, then its discard of the
						// file, before the listing.
						awaitRemoved(t, file)
						if got := engine.cancels(); !slices.Equal(got, []downloadID{"dl-1"}) {
							t.Fatalf("parking's stop asked to cancel %q, want dl-1", got)
						}
					}
					want := kind.want(remover.reason, saving)
					manager.mu.Lock()
					openerTab := manager.pageByHandleLocked(opener)
					manager.mu.Unlock()
					var tab *managedPage
					var wg sync.WaitGroup
					manager.popupListing = func() {
						manager.mu.Lock()
						tab = manager.pageByHandleLocked("popup")
						manager.mu.Unlock()
						if tab == nil {
							t.Error("the popup is not listed at its listing")
							return
						}
						wg.Go(func() {
							if err := remover.remove(manager, access, tab.id); err != nil {
								t.Errorf("take the popup's tab out: %v", err)
							}
						})
						// The remover takes the tab out in one m.mu hold,
						// which this polls for every millisecond; what of it
						// waits for this listing finishes after.
						for deadline := time.Now().Add(hostedTestDeadline); listed(manager, "popup"); time.Sleep(time.Millisecond) {
							if time.Now().After(deadline) {
								t.Error("the remover never took the popup's tab out")
								return
							}
						}
					}
					manager.adoptPopup(engine.popup(opener, "popup"))
					wg.Wait()
					if tab == nil {
						t.FailNow()
					}
					// The tab's teardown, which stops what its leave
					// canceled, is done once no jar holds it closing.
					for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
						manager.mu.Lock()
						closing := false
						for _, scope := range manager.scopes {
							closing = closing || scope.closing[tab.id] != nil
						}
						manager.mu.Unlock()
						if !closing {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("the popup's tab never finished closing")
						}
					}
					var got []string
					for _, d := range tab.downloads.list() {
						got = append(got, fmt.Sprintf("%s %s %d %s", d.ID, d.State, d.Bytes, d.Error))
					}
					if !slices.Equal(got, []string{want}) {
						// Not fatal: the count of cancels below says who else
						// settled it.
						t.Errorf("the popup's tab lists %q, want %q", got, want)
					}
					if rows := openerTab.downloads.list(); len(rows) != 0 {
						t.Fatalf("the opener's tab lists %+v", rows)
					}
					manager.mu.Lock()
					pending := len(manager.endPending)
					manager.mu.Unlock()
					if pending != 0 {
						t.Fatalf("the Manager still waits for %d ends", pending)
					}
					history := manager.CompanionState(access.Caller()).Downloads
					switch {
					case remover.forgets && len(history) != 0:
						t.Fatalf("the closed thread's history kept %+v", history)
					case !remover.forgets && (len(history) != 1 || history[0].State != strings.Fields(want)[1]):
						t.Fatalf("the thread's history is %+v, want the download %s", history, strings.Fields(want)[1])
					}
					if saving == downloadsAsArtifacts {
						wantUsed := before
						if kind.kept {
							// The kept file's size, against its quotas.
							wantUsed += 5
						} else {
							awaitRemoved(t, file)
						}
						// Every stop's cancel comes before its discard, and
						// the last discard settles the reservation, so the
						// cancels below are final.
						for deadline := time.Now().Add(hostedTestDeadline); manager.artifactBytes.Used() != wantUsed; time.Sleep(time.Millisecond) {
							if time.Now().After(deadline) {
								t.Fatalf("%d artifact bytes are reserved, want %d", manager.artifactBytes.Used(), wantUsed)
							}
						}
					}
					if got := engine.cancels(); !slices.Equal(got, kind.cancels) {
						t.Fatalf("the engine was asked to cancel %q, want %q", got, kind.cancels)
					}
				})
			}
		}
	}
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
	// Its row's stop settles the reservation once its file is gone, after
	// that stop's own cancel, which retries parking's in case it failed
	// (stopOverCap): two cancels, and none after.
	for deadline := time.Now().Add(hostedTestDeadline); manager.artifactBytes.Used() != before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after the fate %d artifact bytes are reserved, want %d", manager.artifactBytes.Used(), before)
		}
	}
	if got := engine.cancels(); !slices.Equal(got, []downloadID{"dl-1", "dl-1"}) {
		t.Fatalf("the engine was asked to cancel %q, want dl-1 by parking's stop and by its row's", got)
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
	before := authority.pool.Pool().Used()
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("a parked download within the quotas was refused")
	}
	if got := authority.pool.Pool().Used() - before; got != maxDownloadBytes {
		t.Fatalf("a parked download holds %d of its workspace's bytes, want %d", got, maxDownloadBytes)
	}
	authority.pool.Pool().SetUsed(maxWorkspaceDownloadBytes)
	if popupDownload(manager, engine, "popup", "dl-2", opener) {
		t.Fatal("a parked download past its workspace's quota was admitted")
	}
}

// A parked download's listing takes its reservation over: its row holds the
// very reservation the download parked with, so with the artifact quota
// taken up before the adoption, the download its popup finished is
// listed as it is, not canceled over quota, and holds its size of the
// quotas after.
func TestAParkedDownloadsListingKeepsItsReservation(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 5))
	manager.parkMu.Lock()
	parked := manager.parked["popup"].downloads[0].reservation
	manager.parkMu.Unlock()
	if parked == nil {
		t.Fatal("the parked download holds no reservation")
	}
	// Another download takes everything the artifact quota has left.
	if _, err := manager.reserveArtifacts(maxArtifactRootBytes - manager.artifactBytes.Used()); err != nil {
		t.Fatal("the competing reservation was refused")
	}
	manager.popupListing = func() {
		manager.mu.Lock()
		p := manager.pageByHandleLocked("popup")
		manager.mu.Unlock()
		if p == nil {
			t.Error("the popup is not listed at its listing")
			return
		}
		held := p.downloads.hold()
		defer held.release()
		if row := held.entry("dl-1"); row == nil || row.reservation != parked {
			t.Errorf("the listed row holds %+v, not the reservation the download parked with", row)
		}
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", "dl-1 completed 5")
	if got := manager.artifactBytes.Used(); got != maxArtifactRootBytes-maxDownloadBytes+5 {
		t.Fatalf("%d artifact bytes are reserved, want the competitor's and the file's", got)
	}
	if got := authorityOf(t, manager, access).pool.Pool().Used(); got != 5 {
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
	for _, tc := range []struct {
		name     string
		progress downloadProgress
		// holdCancel holds the stop's cancel, and so its discard, until
		// the test ends: the close alone must remove what the download
		// kept.
		holdCancel bool
	}{
		{name: "completed", progress: progressOf("dl-1", downloadCompleted, 100)},
		{name: "inProgress", progress: progressOf("dl-1", downloadInProgress, 100)},
		{name: "completed over the cap", progress: progressOf("dl-1", downloadCompleted, maxDownloadBytes+1)},
		{name: "over the cap, its stop's cancel unanswered", progress: progressOf("dl-1", downloadInProgress, maxDownloadBytes+1), holdCancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
			if tc.holdCancel {
				held, canceling := make(chan struct{}), make(chan struct{}, 1)
				t.Cleanup(func() { close(held) })
				engine.mu.Lock()
				engine.onCancel = func(downloadID) error {
					canceling <- struct{}{}
					<-held
					return nil
				}
				engine.mu.Unlock()
				defer func() {
					select {
					case <-canceling:
					case <-time.After(hostedTestDeadline):
						t.Error("the stop never asked to cancel the download")
					}
				}()
			}
			before := manager.artifactBytes.Used()
			file := engineArtifact(t, manager, "dl-1")
			popupDownload(manager, engine, "popup", "dl-1", opener)
			manager.downloadProgress(tc.progress)
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

// An unraced adoption shows a client each download its popup started
// before its listing, in the first push that lists it, as parking left it:
// a finished one completed, announced once after that push, and a running
// one running with its counts. Its pushes stay within what listing the
// downloads one by one, each with its end, made: for download i an end's
// push and a start's push of i rows, then the tab addition's of all n,
// three of them carrying all n rows.
func TestAnAdoptionPublishesItsParkedDownloadsWithinOnePushEach(t *testing.T) {
	n := maxParkedDownloads
	for _, tc := range []struct {
		name  string
		state downloadState
		// want is a row's state and bytes in the first push that lists it.
		want     string
		received float64
	}{
		{name: "finished", state: downloadCompleted, want: downloadStateCompleted, received: 5},
		{name: "running", state: downloadInProgress, want: downloadStateRunning, received: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, _, opener := newAdoptionManager(t)
			for i := range n {
				id := downloadID(fmt.Sprintf("dl-%d", i+1))
				if !popupDownload(manager, engine, "popup", id, opener) {
					t.Fatalf("the popup's download %s was refused", id)
				}
				manager.downloadProgress(progressOf(id, tc.state, tc.received))
			}
			// events is what a client receives: each push's downloads, or an
			// announcement, in order.
			type event struct {
				push         []ThreadDownload
				announcement *ThreadDownload
			}
			var mu sync.Mutex
			var events []event
			manager.SetEventSink(func(e CompanionEvent) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case e.Kind == "state" && len(e.Downloads) > 0:
					events = append(events, event{push: slices.Clone(e.Downloads)})
				case e.Kind == "download" && e.Download != nil:
					announced := *e.Download
					events = append(events, event{announcement: &announced})
				}
			})
			manager.adoptPopup(engine.popup(opener, "popup"))
			mu.Lock()
			defer mu.Unlock()
			firstShown := map[string]int{}
			pushes, rows, whole := 0, 0, 0
			for i, e := range events {
				if e.push == nil {
					continue
				}
				pushes++
				rows += len(e.push)
				if len(e.push) == n {
					whole++
				}
				for _, d := range e.push {
					if _, seen := firstShown[d.Name]; seen {
						continue
					}
					firstShown[d.Name] = i
					if got := fmt.Sprintf("%s %d", d.State, d.ReceivedBytes); got != fmt.Sprintf("%s %d", tc.want, int64(tc.received)) {
						t.Errorf("the first push that lists %s shows it %s, want %s %d", d.Name, got, tc.want, int64(tc.received))
					}
				}
			}
			announced := map[string]int{}
			for i, e := range events {
				if e.announcement == nil {
					continue
				}
				a := e.announcement
				announced[a.Name]++
				if shown, ok := firstShown[a.Name]; !ok || shown > i {
					t.Errorf("%s was announced before a push listed it", a.Name)
				}
				if a.State != tc.want {
					t.Errorf("%s was announced %s, want %s", a.Name, a.State, tc.want)
				}
			}
			for i := range n {
				name := fmt.Sprintf("dl-%d", i+1)
				if _, ok := firstShown[name]; !ok {
					t.Errorf("no push listed %s", name)
				}
				wantAnnounced := 0
				if tc.state == downloadCompleted {
					wantAnnounced = 1
				}
				if announced[name] != wantAnnounced {
					t.Errorf("%s was announced %d times, want %d", name, announced[name], wantAnnounced)
				}
			}
			// The adoption's one push of its downloads, and the new tab's.
			if pushes != 2 || rows != 2*n || whole != 2 {
				t.Errorf("%d pushes carry %d download rows, %d of them all %d, want 2 pushes carrying %d, each all %d", pushes, rows, whole, n, 2*n, n)
			}
		})
	}
}

// The user's cancel of a download whose end its popup's listing has
// received and not yet applied is declined: the download is listed with
// that end, and its engine is never asked to cancel it.
func TestACancelOfAParkedDownloadWhoseEndIsKnownIsDeclined(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 5))
	manager.popupListing = func() {
		listed := manager.CompanionState(access.Caller()).Downloads
		if len(listed) != 1 {
			t.Errorf("the thread lists %+v at the popup's listing, want its download", listed)
			return
		}
		if err := manager.CancelThreadDownload(access, DownloadKey(listed[0].ID)); err != nil {
			t.Errorf("cancel the download: %v", err)
		}
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", "dl-1 completed 5")
	if got := engine.cancels(); len(got) != 0 {
		t.Fatalf("the engine was asked to cancel %q", got)
	}
}

// adoptPopup announces popup and adopts it in the caller's goroutine, the
// synchronous form of popupOpened.
func (m *Manager) adoptPopup(popup enginePopup) {
	if m.announcePopup(popup) {
		m.runAdoption(popup)
	}
}

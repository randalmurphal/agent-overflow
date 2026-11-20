package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/quota"
)

// A workspace has one download pool while any reservation holds it: with
// its quota held by transfers that outlive its last page, a page of the
// workspace opened again meets the same pool, and a download there is
// refused past it.
func TestAWorkspaceKeepsOnePoolWhileItsDownloadsHoldIt(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	authority := authorityOf(t, manager, access)
	var held []*quota.Reservation
	for range maxWorkspaceDownloadBytes / maxDownloadBytes {
		reservation, err := manager.reserveDownload(authority)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, reservation)
	}
	if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	if authorityOf(t, manager, access) != nil {
		t.Fatal("the workspace kept its authority with no page left")
	}
	again, err := manager.Open(t.Context(), access, "https://example.test/again", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := authorityOf(t, manager, access).downloadBytes; got != authority.downloadBytes {
		t.Fatal("the workspace has a second pool while its first holds reservations")
	}
	frame := paneHandle(manager, access, again.ID)
	if got := manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "fifth", SuggestedName: "fifth.bin"}); got != downloadRefused {
		t.Fatalf("a download past the workspace's held quota answered %d, want refused", got)
	}
	for _, reservation := range held {
		reservation.Release()
	}
	if got := manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "sixth", SuggestedName: "sixth.bin"}); got != downloadAdmitted {
		t.Fatalf("a download once the quota freed answered %d, want admitted", got)
	}
}

// startPopupDownload starts download id in popup, opened by openers,
// nearest first, in the engine's profile, and answers the Manager's
// admission.
func startPopupDownload(m *Manager, e *adoptionEngine, popup ids.PageHandle, id downloadID, openers ...ids.PageHandle) downloadAdmission {
	e.mu.Lock()
	profile := e.profile
	e.mu.Unlock()
	return m.downloadStarted(downloadStart{
		Frame: frameID(popup), ID: id, URL: "https://example.test/" + string(id), SuggestedName: string(id),
		Popup: popup, Openers: openers, Profile: profile,
	})
}

func wantAdmission(t *testing.T, what string, got, want downloadAdmission) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: admission %d, want %d", what, got, want)
	}
}

// parkedState answers how many popups hold downloads, how many downloads
// they hold, and how many park timers run.
func parkedState(m *Manager) (popups, downloads int) {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	return len(m.parked), len(m.parkedIDs)
}

// Each of the Manager's three admission paths answers admitted, refused or
// unowned, and only a known owner's decision is refused: on a listed page,
// for a popup not listed yet, and for a popup whose fate is known.
func TestADownloadStartIsAdmittedRefusedOrUnownedOnEachPath(t *testing.T) {
	t.Run("listed page", func(t *testing.T) {
		manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
		manager.mu.Lock()
		p, _ := manager.livePageLocked(PageID(page.ID))
		manager.mu.Unlock()
		start := func(id downloadID) downloadAdmission {
			return manager.downloadStarted(downloadStart{Frame: frameID(p.driver.Handle()), ID: id, URL: "https://example.test/" + string(id), SuggestedName: string(id)})
		}
		wantAdmission(t, "a download within the quotas", start("a"), downloadAdmitted)
		authorityOf(t, manager, access).downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
		wantAdmission(t, "a download past its workspace's quota", start("b"), downloadRefused)
		wantAdmission(t, "a frame no page owns", manager.downloadStarted(downloadStart{Frame: "nobody", ID: "c"}), downloadUnowned)
	})
	t.Run("parked", func(t *testing.T) {
		manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
		wantAdmission(t, "a popup of a listed opener", startPopupDownload(manager, engine, "popup", "a", opener), downloadAdmitted)
		wantAdmission(t, "a popup whose opener is not listed yet", startPopupDownload(manager, engine, "orphan", "b", "not-yet"), downloadUnowned)
		wrongProfile := manager.downloadStarted(downloadStart{Frame: "other", ID: "c", Popup: "other", Openers: []ids.PageHandle{opener}, Profile: "no-such-jar"})
		wantAdmission(t, "a popup of a jar that is not open", wrongProfile, downloadUnowned)
		authorityOf(t, manager, access).downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
		wantAdmission(t, "a popup past its workspace's quota", startPopupDownload(manager, engine, "full", "d", opener), downloadRefused)
		if popups, downloads := parkedState(manager); popups != 1 || downloads != 1 {
			t.Fatalf("parked %d popups with %d downloads, want only the admitted one", popups, downloads)
		}
	})
	t.Run("fate known", func(t *testing.T) {
		manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
		manager.resolveParked("gone", nil, ids.PageHandle(opener))
		wantAdmission(t, "a popup that went, with its opener listed", startPopupDownload(manager, engine, "gone", "a", opener), downloadAdmitted)
		wantDownloads(t, manager, opener, "a in_progress 0")
		manager.resolveParked("orphaned", nil, "never-listed")
		wantAdmission(t, "a popup that went, with no opener listed", startPopupDownload(manager, engine, "orphaned", "b"), downloadUnowned)
		authorityOf(t, manager, access).downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
		wantAdmission(t, "a popup that went, past its opener's quota", startPopupDownload(manager, engine, "gone", "c", opener), downloadRefused)
	})
}

// A start refused or unowned allocates nothing that outlives the answer:
// no parked popup, no timer, no slot. Past the workspace's quota, starts
// of more popups than the park holds are each refused, and once the quota
// frees a valid start of yet another popup is admitted.
func TestRefusedPopupDownloadsTakeNoParkingSlot(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	authority := authorityOf(t, manager, access)
	authority.downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
	for i := range maxParkedPopups + 1 {
		got := startPopupDownload(manager, engine, ids.PageHandle(fmt.Sprintf("popup-%d", i)), downloadID(fmt.Sprintf("dl-%d", i)), opener)
		wantAdmission(t, fmt.Sprintf("start %d past the quota", i), got, downloadRefused)
	}
	if popups, downloads := parkedState(manager); popups != 0 || downloads != 0 {
		t.Fatalf("refused starts left %d popups and %d downloads parked", popups, downloads)
	}
	authority.downloadBytes.SetUsed(0)
	for i := range maxParkedPopups + 1 {
		got := startPopupDownload(manager, engine, ids.PageHandle(fmt.Sprintf("orphan-%d", i)), downloadID(fmt.Sprintf("o-%d", i)), "not-yet")
		wantAdmission(t, fmt.Sprintf("start %d with no owner", i), got, downloadUnowned)
	}
	if popups, downloads := parkedState(manager); popups != 0 || downloads != 0 {
		t.Fatalf("unowned starts left %d popups and %d downloads parked", popups, downloads)
	}
	wantAdmission(t, "a valid start once the quota frees", startPopupDownload(manager, engine, "valid", "dl-valid", opener), downloadAdmitted)
}

// headlessUnownedRig is a headless profile whose Manager answers every
// start unowned, with a download directory holding the file Chromium
// wrote for the download.
func headlessUnownedRig(t *testing.T, wait time.Duration, lifetime context.Context) (*downloadTracker, string, func() int) {
	t.Helper()
	var mu sync.Mutex
	offers := 0
	p := newTargetTestProfile(t, engineEvents{
		DownloadStarted: func(downloadStart) downloadAdmission {
			mu.Lock()
			offers++
			mu.Unlock()
			return downloadUnowned
		},
		DownloadProgress: func(downloadProgress) {},
	})
	p.engine.ownerWait = wait
	p.downloadDir = t.TempDir()
	file := filepath.Join(p.downloadDir, "G1")
	if err := os.WriteFile(file, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p.downloadTracker(lifetime, newHeadlessTargets(p)), file, func() int {
		mu.Lock()
		defer mu.Unlock()
		return offers
	}
}

// Headless Chromium offers an unowned start again for a bounded time, then
// cancels it and removes the file it wrote once its end arrives; a
// connection that ends first leaves it to the browser's stop. Neither path ever
// treats it as admitted.
func TestAnUnownedHeadlessDownloadIsCanceledWithItsFileOnceItsWaitEnds(t *testing.T) {
	t.Run("the wait passes", func(t *testing.T) {
		d, file, offers := headlessUnownedRig(t, 100*time.Millisecond, t.Context())
		canceled := make(chan downloadID, 1)
		d.cancel = func(id downloadID) { canceled <- id }
		d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: "G1", FrameID: "F-unknown"})
		select {
		case id := <-canceled:
			if id != "G1" {
				t.Fatalf("canceled %s, want G1", id)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the start was never canceled after the owner wait")
		}
		d.progress(cdpbrowser.EventDownloadProgress{GUID: "G1", State: cdpbrowser.DownloadProgressStateCanceled})
		awaitRemoved(t, file)
		settled := offers()
		if settled < 2 {
			t.Fatalf("the start was offered %d times, want it offered again while it waited", settled)
		}
		time.Sleep(20 * requestOwnerPoll)
		if offers() != settled {
			t.Fatal("the start was still offered after it was refused")
		}
	})
	// The connection ends before the browser process has: Chromium may
	// still write the partial file and rename it to its final name, so
	// both stay claimed until the process has exited (stop), and both go
	// then.
	t.Run("the connection ends", func(t *testing.T) {
		lifetime, end := context.WithCancel(t.Context())
		d, file, _ := headlessUnownedRig(t, time.Hour, lifetime)
		partial := file + chromiumPartialSuffix
		if err := os.Rename(file, partial); err != nil {
			t.Fatal(err)
		}
		d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: "G1", FrameID: "F-unknown"})
		end()
		time.Sleep(20 * requestOwnerPoll)
		// The writer, still running, finishes the file in the interval.
		if err := os.Rename(partial, file); err != nil {
			t.Fatalf("the partial file went before the browser stopped: %v", err)
		}
		d.stop()
		for _, name := range []string{partial, file} {
			if _, err := os.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s is still there after the browser stopped: %v", name, err)
			}
		}
	})
}

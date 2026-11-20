package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	if got := authorityOf(t, manager, access).pool.Pool(); got != authority.pool.Pool() {
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
		authorityOf(t, manager, access).pool.Pool().SetUsed(maxWorkspaceDownloadBytes)
		wantAdmission(t, "a download past its workspace's quota", start("b"), downloadRefused)
		wantAdmission(t, "a frame no page owns", manager.downloadStarted(downloadStart{Frame: "nobody", ID: "c"}), downloadUnowned)
	})
	t.Run("parked", func(t *testing.T) {
		manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
		wantAdmission(t, "a popup of a listed opener", startPopupDownload(manager, engine, "popup", "a", opener), downloadAdmitted)
		wantAdmission(t, "a popup whose opener is not listed yet", startPopupDownload(manager, engine, "orphan", "b", "not-yet"), downloadUnowned)
		wrongProfile := manager.downloadStarted(downloadStart{Frame: "other", ID: "c", Popup: "other", Openers: []ids.PageHandle{opener}, Profile: "no-such-jar"})
		wantAdmission(t, "a popup of a jar that is not open", wrongProfile, downloadUnowned)
		authorityOf(t, manager, access).pool.Pool().SetUsed(maxWorkspaceDownloadBytes)
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
		closedJar := manager.downloadStarted(downloadStart{Frame: "gone", ID: "j", Popup: "gone", Openers: []ids.PageHandle{opener}, Profile: "no-such-jar"})
		wantAdmission(t, "a popup that went, of a jar that is not open", closedJar, downloadUnowned)
		authorityOf(t, manager, access).pool.Pool().SetUsed(maxWorkspaceDownloadBytes)
		wantAdmission(t, "a popup that went, past its opener's quota", startPopupDownload(manager, engine, "gone", "c", opener), downloadRefused)
		// The refusal is a row in the opener's tab that says why, as a
		// download starting in that tab past its quota is.
		wantDownloads(t, manager, opener, "a in_progress 0", "c canceled 0")
		for _, row := range listedPage(t, manager, opener).downloads.list() {
			if row.ID == "c" && row.Error == "" {
				t.Fatal("the refused download's row gives no reason")
			}
		}
	})
}

// A start refused or unowned allocates nothing that outlives the answer:
// no parked popup, no timer, no slot. Past the workspace's quota, starts
// of more popups than the park holds are each refused, and once the quota
// frees a valid start of yet another popup is admitted.
func TestRefusedPopupDownloadsTakeNoParkingSlot(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	authority := authorityOf(t, manager, access)
	authority.pool.Pool().SetUsed(maxWorkspaceDownloadBytes)
	for i := range maxParkedPopups + 1 {
		got := startPopupDownload(manager, engine, ids.PageHandle(fmt.Sprintf("popup-%d", i)), downloadID(fmt.Sprintf("dl-%d", i)), opener)
		wantAdmission(t, fmt.Sprintf("start %d past the quota", i), got, downloadRefused)
	}
	if popups, downloads := parkedState(manager); popups != 0 || downloads != 0 {
		t.Fatalf("refused starts left %d popups and %d downloads parked", popups, downloads)
	}
	authority.pool.Pool().SetUsed(0)
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
	room, err := openRoomIn(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(room.path(), "G1")
	if err := os.WriteFile(file, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p.downloadTracker(lifetime, newHeadlessTargets(p), room), file, func() int {
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

// An artifact folder that cannot be measured refuses every artifact
// download with the reason, measures nothing, and is measured again by the
// next reservation once it can be; one that cannot be created refuses the
// same way.
func TestAnArtifactFolderThatCannotBeMeasuredRefusesWithTheReason(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable folder")
	}
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	unreadable := filepath.Join(manager.artifactRoot, "unreadable")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unreadable, "kept.bin"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o700) })
	manager.artifactInitMu.Lock()
	manager.artifactReady = false
	manager.artifactInitMu.Unlock()
	frame := frameID(paneHandle(manager, access, page.ID))
	if manager.downloadStarted(downloadStart{Frame: frame, ID: "refused", SuggestedName: "r.bin"}) != downloadRefused {
		t.Fatal("a download was admitted against an artifact folder that was not measured")
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCanceled || !strings.Contains(got.Error, "measure the artifact folder") {
		t.Fatalf("the refused download lists as %+v, want the reason", got)
	}
	if err := os.Chmod(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if manager.downloadStarted(downloadStart{Frame: frame, ID: "admitted", SuggestedName: "a.bin"}) != downloadAdmitted {
		t.Fatal("a download was refused once the folder could be measured")
	}
	if got := manager.artifactBytes.Used(); got < 5+maxDownloadBytes {
		t.Fatalf("the measure and the reservation charged %d bytes, want at least the kept file's 5 and the reservation", got)
	}

	blocked := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := blocked.Close(); err != nil {
			t.Error(err)
		}
	})
	file := filepath.Join(t.TempDir(), "a file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked.artifactRoot = filepath.Join(file, "artifacts")
	if _, err := blocked.reserveArtifacts(1); err == nil || !strings.Contains(err.Error(), "create the artifact folder") {
		t.Fatalf("a reservation under an artifact folder that cannot be created answered %v", err)
	}
}

// Writes follow a link and the scan would not, so an artifact folder that
// is a link, or holds a link to a folder, is refused rather than measured
// as nearly empty while the folder it reaches is full.
func TestAnArtifactFolderReachedThroughALinkIsRefused(t *testing.T) {
	for _, linked := range []string{"the folder", "a folder inside it"} {
		t.Run(linked, func(t *testing.T) {
			full := t.TempDir()
			sparse, err := os.Create(filepath.Join(full, "full.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if err := sparse.Truncate(maxArtifactRootBytes); err != nil {
				t.Fatal(err)
			}
			if err := sparse.Close(); err != nil {
				t.Fatal(err)
			}
			manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
			t.Cleanup(func() {
				if err := manager.Close(); err != nil {
					t.Error(err)
				}
			})
			link := manager.artifactRoot
			if linked == "a folder inside it" {
				if err := os.MkdirAll(manager.artifactRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(manager.artifactRoot, "finished")
			}
			if err := os.Symlink(full, link); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.reserveArtifacts(1); err == nil || !strings.Contains(err.Error(), "is a link") {
				t.Fatalf("a reservation through %s linked to a full one answered %v, with %d bytes measured", linked, err, manager.artifactBytes.Used())
			}
		})
	}
}

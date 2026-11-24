package browser

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"

	"agent-overflow/internal/browser/ids"
)

// trackerManagerRig is a real Manager keeping downloads as artifacts whose
// jar's downloads come from a headless download tracker, as one Chromium
// process's do: the tracker reports to the Manager, writes into a room in
// the jar's download directory, and the jar's profile cancels through it,
// Chromium accepting every cancel. A sibling tab keeps the jar open when
// the tab the downloads begin in closes. handOver can be held at gate, and
// the admission's answer, after the Manager listed the row, at admitGate.
type trackerManagerRig struct {
	t         *testing.T
	manager   *Manager
	access    Access
	page      PageInfo
	frame     string
	jarDir    string
	tracker   *downloadTracker
	room      *downloadRoom
	gate      chan struct{}
	admitGate chan struct{}
	in        chan struct{}
	cancels   chan downloadID
}

// gatedRoom is a room whose handOver waits at its rig's gate, when set,
// after signaling that it did.
type gatedRoom struct {
	*downloadRoom
	rig *trackerManagerRig
}

func (g gatedRoom) handOver(id downloadID) (string, error) {
	if gate := g.rig.gate; gate != nil {
		g.rig.in <- struct{}{}
		<-gate
	}
	return g.downloadRoom.handOver(id)
}

// trackerProfile is the jar's profile with its cancel the tracker's.
type trackerProfile struct {
	engineProfile
	rig *trackerManagerRig
}

func (p trackerProfile) CancelDownload(id downloadID) error {
	if _, ended := p.rig.tracker.canceling(id); ended {
		return errDownloadEnded
	}
	p.rig.cancels <- id
	return nil
}

func newTrackerManagerRig(t *testing.T) *trackerManagerRig {
	t.Helper()
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	if _, err := manager.Open(t.Context(), access, "https://example.test/sibling", OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	r := &trackerManagerRig{t: t, manager: manager, access: access, page: page, in: make(chan struct{}, 1), cancels: make(chan downloadID, 4)}
	manager.mu.Lock()
	p, scope := manager.livePageLocked(PageID(page.ID))
	r.frame, r.jarDir = string(p.driver.Handle()), scope.downloadDir
	scope.profile = trackerProfile{engineProfile: scope.profile, rig: r}
	manager.mu.Unlock()
	room, err := openRoomIn(r.jarDir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	r.room = room
	r.tracker = newDownloadTracker(engineEvents{
		DownloadStarted: func(start downloadStart) downloadAdmission {
			admission := manager.downloadStarted(start)
			if gate := r.admitGate; gate != nil {
				r.in <- struct{}{}
				<-gate
			}
			return admission
		},
		DownloadProgress: manager.downloadProgress,
	}, func(downloadID) {}, gatedRoom{downloadRoom: room, rig: r})
	t.Cleanup(r.tracker.stop)
	return r
}

// begin starts download guid in the rig's tab, with what Chromium has
// written of it in the room.
func (r *trackerManagerRig) begin(guid string, content []byte) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.room.path(), guid), content, 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.tracker.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: guid, FrameID: cdp.FrameID(r.frame), URL: "https://example.test/" + guid, SuggestedFilename: guid + ".txt"})
	if got := r.listed(); got.State != downloadStateRunning {
		r.t.Fatalf("the started download is listed %+v", got)
	}
}

// listed is the thread's one download.
func (r *trackerManagerRig) listed() ThreadDownload {
	r.t.Helper()
	listed := threadDownloads(r.manager, r.access)
	if len(listed) != 1 {
		r.t.Fatalf("the thread lists %+v, want one download", listed)
	}
	return listed[0]
}

// leftIn lists what is left in dir, the room and its files included.
func leftIn(t *testing.T, dir string) []string {
	t.Helper()
	var left []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir {
			rel, _ := filepath.Rel(dir, path)
			left = append(left, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// A tab closed after the tracker received its download's completion, and
// before the file moved, leaves the row to that end: the download is
// listed completed with its file in the workspace, and nothing of it stays
// in the jar's directory.
func TestATabClosedDuringAFinishedDownloadsHandoffKeepsTheDownload(t *testing.T) {
	r := newTrackerManagerRig(t)
	content := []byte("the whole file")
	r.begin("G-done", content)
	r.gate = make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		r.tracker.progress(cdpbrowser.EventDownloadProgress{GUID: "G-done", ReceivedBytes: float64(len(content)), TotalBytes: float64(len(content)), State: cdpbrowser.DownloadProgressStateCompleted})
	}()
	select {
	case <-r.in:
	case <-time.After(5 * time.Second):
		t.Fatal("the handoff never began")
	}
	if err := r.manager.ClosePage(t.Context(), r.access, PageID(r.page.ID)); err != nil {
		t.Fatal(err)
	}
	close(r.gate)
	<-ended
	r.wantKept("G-done", content)
}

// wantKept checks that download guid is listed completed and openable with
// content in the workspace, nothing of it left in the jar's directory, and
// no cancel sent.
func (r *trackerManagerRig) wantKept(guid string, content []byte) {
	r.t.Helper()
	got := r.listed()
	if got.State != downloadStateCompleted || !got.Openable {
		r.t.Fatalf("the download is listed %+v, want completed and openable", got)
	}
	item, err := r.manager.threadDownload(r.access, DownloadKey(got.ID))
	if err != nil {
		r.t.Fatal(err)
	}
	if saved, err := os.ReadFile(item.path); err != nil || !bytes.Equal(saved, content) {
		r.t.Fatalf("the saved file reads %q, %v", saved, err)
	}
	if left := leftIn(r.t, r.room.path()); len(left) != 0 {
		r.t.Fatalf("the room holds %q", left)
	}
	if _, err := os.Lstat(filepath.Join(r.jarDir, guid)); !os.IsNotExist(err) {
		r.t.Fatalf("the handed-over file stayed in the jar's directory: %v", err)
	}
	select {
	case id := <-r.cancels:
		r.t.Fatalf("the finished download %s was canceled", id)
	default:
	}
}

// A completion Chromium reports while the Manager's admission of the
// download is still being answered, its row already listed, is the
// Manager's from its receipt: a tab closed before the answer leaves the
// row to that completion.
func TestATabClosedDuringItsDownloadsAdmissionKeepsTheFinishedDownload(t *testing.T) {
	r := newTrackerManagerRig(t)
	content := []byte("the whole file")
	if err := os.WriteFile(filepath.Join(r.room.path(), "G-adm"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	r.admitGate = make(chan struct{})
	begun := make(chan struct{})
	go func() {
		defer close(begun)
		r.tracker.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: "G-adm", FrameID: cdp.FrameID(r.frame), URL: "https://example.test/G-adm", SuggestedFilename: "G-adm.txt"})
	}()
	select {
	case <-r.in:
	case <-time.After(5 * time.Second):
		t.Fatal("the admission never began")
	}
	r.tracker.progress(cdpbrowser.EventDownloadProgress{GUID: "G-adm", ReceivedBytes: float64(len(content)), TotalBytes: float64(len(content)), State: cdpbrowser.DownloadProgressStateCompleted})
	if err := r.manager.ClosePage(t.Context(), r.access, PageID(r.page.ID)); err != nil {
		t.Fatal(err)
	}
	close(r.admitGate)
	<-begun
	r.wantKept("G-adm", content)
}

// A tab closed while its download runs cancels it, and its file goes once
// Chromium reports the cancel: listed canceled, nothing left. One that
// finishes after that cancel was accepted is no one's: its file is
// withdrawn from the jar's directory.
func TestATabClosedWhileItsDownloadRunsCancelsIt(t *testing.T) {
	for _, end := range []cdpbrowser.DownloadProgressState{cdpbrowser.DownloadProgressStateCanceled, cdpbrowser.DownloadProgressStateCompleted} {
		t.Run(string(end), func(t *testing.T) {
			r := newTrackerManagerRig(t)
			r.begin("G-run", []byte("half of it"))
			if err := r.manager.ClosePage(t.Context(), r.access, PageID(r.page.ID)); err != nil {
				t.Fatal(err)
			}
			select {
			case id := <-r.cancels:
				if id != "G-run" {
					t.Fatalf("canceled %s", id)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the closed tab's download was never canceled")
			}
			r.tracker.progress(cdpbrowser.EventDownloadProgress{GUID: "G-run", ReceivedBytes: 10, State: end})
			if got := r.listed(); got.State != downloadStateCanceled || got.Openable {
				t.Fatalf("the download is listed %+v, want canceled", got)
			}
			if left := leftIn(t, r.jarDir); len(left) != 1 || left[0] != filepath.Base(r.room.path()) {
				t.Fatalf("the jar's directory holds %q, want only the empty room", left)
			}
		})
	}
}

// A popup's download that passes the cap while parked, whose cancel fails
// with its browser still running, and that Chromium then finishes before
// the popup's adoption, hands its file over into the jar's directory: the
// adopted row, listed canceled over the cap, still owns that file and the
// download's reservation, so the file goes and the quota is released.
func TestAParkedDownloadOverTheCapWhoseCancelFailedLosesTheFileItFinished(t *testing.T) {
	manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	before := manager.artifactBytes.Used()
	manager.mu.Lock()
	var dir string
	for _, scope := range manager.scopes {
		dir = scope.downloadDir
	}
	manager.mu.Unlock()
	room, err := openRoomIn(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	tracker := newDownloadTracker(engineEvents{DownloadStarted: manager.downloadStarted, DownloadProgress: manager.downloadProgress}, func(downloadID) {}, room)
	engine.mu.Lock()
	profile := engine.profile
	engine.mu.Unlock()
	tracker.popup = func(frame frameID) downloadStart {
		if frame != "popup-frame" {
			return downloadStart{}
		}
		return downloadStart{Popup: "popup", Openers: []ids.PageHandle{opener}, Profile: profile}
	}
	t.Cleanup(tracker.stop)
	// The cancel CDP fails on, as headlessProfile.CancelDownload answers
	// it: its mark on the tracker is undone, so Chromium's later finish is
	// not read as AO's cancel.
	engine.mu.Lock()
	engine.onCancel = func(id downloadID) error {
		undo, ended := tracker.canceling(id)
		if ended {
			return errDownloadEnded
		}
		undo()
		return errors.New("the browser did not answer the cancel")
	}
	engine.mu.Unlock()
	// What is at the download's own path in the jar's directory until the
	// stop's discard removes it, which marks that the stop has finished.
	handedOver := filepath.Join(dir, "G-1")
	if err := os.WriteFile(handedOver, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(room.path(), "G-1"), []byte("the whole file"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: "G-1", FrameID: "popup-frame", URL: "https://example.test/G-1", SuggestedFilename: "G-1.txt"})
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		manager.parkMu.Lock()
		_, parked := manager.parkedIDs["G-1"]
		manager.parkMu.Unlock()
		if parked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the popup's download was never parked")
		}
	}
	tracker.progress(cdpbrowser.EventDownloadProgress{GUID: "G-1", ReceivedBytes: maxDownloadBytes + 1, State: cdpbrowser.DownloadProgressStateInProgress})
	awaitRemoved(t, handedOver)
	if got := engine.cancels(); !slices.Equal(got, []downloadID{"G-1"}) {
		t.Fatalf("the stop asked to cancel %q, want G-1", got)
	}
	tracker.progress(cdpbrowser.EventDownloadProgress{GUID: "G-1", ReceivedBytes: maxDownloadBytes + 1, TotalBytes: maxDownloadBytes + 1, State: cdpbrowser.DownloadProgressStateCompleted})
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		if _, err := os.Lstat(handedOver); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Chromium's finish never handed the file over")
		}
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", fmt.Sprintf("G-1 canceled %d", maxDownloadBytes+1))
	awaitRemoved(t, handedOver)
	for deadline := time.Now().Add(hostedTestDeadline); manager.artifactBytes.Used() != before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d artifact bytes are reserved, want %d", manager.artifactBytes.Used(), before)
		}
	}
}

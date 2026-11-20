package browser

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
)

// trackerManagerRig is a real Manager keeping downloads as artifacts whose
// jar's downloads come from a headless download tracker, as one Chromium
// process's do: the tracker reports to the Manager, writes into a room in
// the jar's download directory, and the jar's profile cancels through it,
// Chromium accepting every cancel. A sibling tab keeps the jar open when
// the tab the downloads begin in closes. handOver can be held at gate.
type trackerManagerRig struct {
	t       *testing.T
	manager *Manager
	access  Access
	page    PageInfo
	frame   string
	jarDir  string
	tracker *downloadTracker
	room    *downloadRoom
	gate    chan struct{}
	in      chan struct{}
	cancels chan downloadID
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
	room, err := openDownloadRoom(r.jarDir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	r.room = room
	r.tracker = newDownloadTracker(engineEvents{
		DownloadStarted:  manager.downloadStarted,
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
	got := r.listed()
	if got.State != downloadStateCompleted || !got.Openable {
		t.Fatalf("the download is listed %+v, want completed and openable", got)
	}
	item, err := r.manager.threadDownload(r.access, DownloadKey(got.ID))
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := os.ReadFile(item.path); err != nil || !bytes.Equal(saved, content) {
		t.Fatalf("the saved file reads %q, %v", saved, err)
	}
	if left := leftIn(t, r.room.path()); len(left) != 0 {
		t.Fatalf("the room holds %q", left)
	}
	if _, err := os.Lstat(filepath.Join(r.jarDir, "G-done")); !os.IsNotExist(err) {
		t.Fatalf("the handed-over file stayed in the jar's directory: %v", err)
	}
	select {
	case id := <-r.cancels:
		t.Fatalf("the finished download %s was canceled", id)
	default:
	}
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

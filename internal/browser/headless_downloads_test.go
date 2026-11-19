package browser

import (
	"fmt"
	"sync"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
)

// trackerRig is a download tracker over a recording Manager, cancel and
// file removal.
type trackerRig struct {
	*downloadTracker
	recorder *downloadRecorder
	mu       sync.Mutex
	removed  map[string]int
	canceled chan string
}

func newTrackerRig(allow bool) *trackerRig {
	r := &trackerRig{recorder: newDownloadRecorder(allow), removed: map[string]int{}, canceled: make(chan string, 1024)}
	r.downloadTracker = newDownloadTracker(r.recorder.events(), func(id string) {
		r.recorder.cancel(id)
		r.canceled <- id
	}, func(id string) {
		r.mu.Lock()
		r.removed[id]++
		r.mu.Unlock()
	})
	return r
}

func (r *trackerRig) removals(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removed[id]
}

func (r *trackerRig) begin(guid string) {
	r.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: guid, FrameID: "F1", URL: "https://example.test/a.zip", SuggestedFilename: "a.zip"})
}

func (r *trackerRig) report(guid string, state cdpbrowser.DownloadProgressState, received float64) {
	r.progress(cdpbrowser.EventDownloadProgress{GUID: guid, ReceivedBytes: received, State: state})
}

func (r *trackerRig) awaitCancel(t *testing.T, guid string) {
	t.Helper()
	select {
	case got := <-r.canceled:
		if got != guid {
			t.Fatalf("canceled %s, want %s", got, guid)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("download %s was never canceled", guid)
	}
}

// A download's events, in the order Chromium sends them: the Manager sees
// an admitted download start and settle and the tracker keeps it until its
// end; a refused one is canceled once, and its file is removed once its
// end arrives, never before; progress of a download whose start the
// connection never saw is no one's.
func TestDownloadTrackerFollowsTheWireOrder(t *testing.T) {
	const guid = "G1"
	inProgress, completed, canceled := cdpbrowser.DownloadProgressStateInProgress, cdpbrowser.DownloadProgressStateCompleted, cdpbrowser.DownloadProgressStateCanceled
	type step struct {
		state    cdpbrowser.DownloadProgressState
		received float64
	}
	cases := []struct {
		name      string
		allow     bool
		progress  []step
		wantState string
		wantBytes float64
		wantKept  int
	}{
		{"a completed download", true, []step{{inProgress, 10}, {completed, 20}}, downloadCompleted, 20, 0},
		{"a canceled download", true, []step{{inProgress, 10}, {canceled, 10}}, downloadCanceled, 10, 0},
		{"a download still running", true, []step{{inProgress, 10}, {inProgress, 15}}, downloadInProgress, 15, 1},
		{"a refused download that completes", false, []step{{inProgress, 10}, {completed, 20}}, "refused", 0, 0},
		{"a refused download still running", false, []step{{inProgress, 10}}, "refused", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTrackerRig(tc.allow)
			r.begin(guid)
			if !tc.allow {
				r.awaitCancel(t, guid)
			}
			for _, s := range tc.progress {
				if r.removals(guid) != 0 {
					t.Fatal("the refused download's file was removed before its end")
				}
				r.report(guid, s.state, s.received)
			}
			state, bytes := r.recorder.outcome(guid)
			if state != tc.wantState || bytes != tc.wantBytes {
				t.Fatalf("the Manager saw %q with %.0f bytes, want %q with %.0f", state, bytes, tc.wantState, tc.wantBytes)
			}
			if got := r.retained(); got != tc.wantKept {
				t.Fatalf("kept %d downloads, want %d", got, tc.wantKept)
			}
			wantRemoved := 0
			if !tc.allow && tc.wantKept == 0 {
				wantRemoved = 1
			}
			if got := r.removals(guid); got != wantRemoved {
				t.Fatalf("removed the file %d times, want %d", got, wantRemoved)
			}
		})
	}
	t.Run("progress of a download never begun", func(t *testing.T) {
		r := newTrackerRig(true)
		for i := range orderSlack + 50 {
			r.report(fmt.Sprintf("G%d", i), completed, 1)
		}
		if got := r.retained(); got != 0 {
			t.Fatalf("kept %d downloads no start named", got)
		}
	})
}

// An admitted download is kept until its end however many others start and
// are refused meanwhile: its completion reaches the Manager.
func TestDownloadTrackerKeepsAnAdmittedDownloadToItsEnd(t *testing.T) {
	p := newTargetTestProfile(t, engineEvents{})
	recorder := newDownloadRecorder(false)
	events := recorder.events()
	refuse := events.DownloadStarted
	events.DownloadStarted = func(start downloadStart) downloadAdmission {
		if start.ID == "kept" {
			recorder.mu.Lock()
			recorder.state[start.ID] = downloadInProgress
			recorder.mu.Unlock()
			return downloadAdmitted
		}
		return refuse(start)
	}
	p.engine.events = events
	d := p.downloadTracker(t.Context(), newHeadlessTargets(p))
	d.cancel = func(string) {}
	d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: "kept", FrameID: "F1"})
	for i := range 2*orderSlack + 1 {
		d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: fmt.Sprintf("refused-%d", i), FrameID: "F1"})
	}
	d.progress(cdpbrowser.EventDownloadProgress{GUID: "kept", ReceivedBytes: 5, State: cdpbrowser.DownloadProgressStateCompleted})
	if state, _ := recorder.outcome("kept"); state != downloadCompleted {
		t.Fatalf("the admitted download reads %q at the Manager after its completion", state)
	}
}

// A refused download's file outlives neither its end nor its browser: one
// whose cancel never returns is removed when its completion arrives, one
// still running when the browser stops is removed then, and so is one
// still waiting for its frame's owner; the Manager's own downloads are left
// to it.
func TestDownloadTrackerRemovesWhatNoPageTook(t *testing.T) {
	r := newTrackerRig(false)
	stalled := make(chan struct{})
	defer close(stalled)
	r.cancel = func(string) { <-stalled }
	r.begin("ended")
	r.begin("running")
	if r.removals("ended") != 0 {
		t.Fatal("a refused download's file was removed at its refusal")
	}
	r.report("ended", cdpbrowser.DownloadProgressStateCompleted, 20)
	if r.removals("ended") != 1 {
		t.Fatal("a refused download's file was not removed at its completion while its cancel was unanswered")
	}

	r.recorder.allow = true
	r.begin("admitted")
	r.recorder.allow = false
	r.lifetime, r.ownerWait = t.Context(), time.Hour
	unowned := r.recorder.events()
	r.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		if start.ID == "waiting" {
			return downloadUnowned
		}
		return unowned.DownloadStarted(start)
	}
	r.begin("waiting")
	r.stop()
	for id, want := range map[string]int{"ended": 1, "running": 1, "waiting": 1, "admitted": 0} {
		if got := r.removals(id); got != want {
			t.Errorf("download %s's file was removed %d times after the browser stopped, want %d", id, got, want)
		}
	}
	if got := r.retained(); got != 0 {
		t.Fatalf("the stopped tracker keeps %d downloads", got)
	}
}

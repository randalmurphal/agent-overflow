package browser

import (
	"fmt"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
)

// trackerRig is a download tracker over a recording Manager, cancel and
// download directory.
type trackerRig struct {
	*downloadTracker
	recorder *downloadRecorder
	files    *fakeDownloadFiles
	canceled chan downloadID
}

func newTrackerRig(allow bool) *trackerRig {
	r := &trackerRig{recorder: newDownloadRecorder(allow), files: newFakeDownloadFiles(), canceled: make(chan downloadID, 1024)}
	r.downloadTracker = newDownloadTracker(r.recorder.events(), func(id downloadID) {
		r.recorder.cancel(id)
		r.canceled <- id
	}, r.files)
	return r
}

func (r *trackerRig) begin(guid string) {
	r.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: guid, FrameID: "F1", URL: "https://example.test/a.zip", SuggestedFilename: "a.zip"})
}

func (r *trackerRig) report(guid string, state cdpbrowser.DownloadProgressState, received float64) {
	r.progress(cdpbrowser.EventDownloadProgress{GUID: guid, ReceivedBytes: received, State: state})
}

func (r *trackerRig) awaitCancel(t *testing.T, guid downloadID) {
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

// retained reports how many downloads the tracker keeps.
func (t *downloadTracker) retained() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.downloads)
}

// A download's events, in the order Chromium sends them: the Manager sees
// an admitted download start and settle, its finished file handed over,
// and the tracker keeps it until its end; a refused one is canceled once,
// and its files are removed once its end arrives, never before; progress
// of a download whose start the connection never saw is no one's.
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
		wantFiles string
	}{
		{"a completed download", true, []step{{inProgress, 10}, {completed, 20}}, downloadCompleted.String(), 20, 0, "handOver"},
		{"a download that stopped unasked", true, []step{{inProgress, 10}, {canceled, 10}}, downloadFailed.String(), 10, 0, "discard"},
		{"a download still running", true, []step{{inProgress, 10}, {inProgress, 15}}, downloadInProgress.String(), 15, 1, ""},
		{"a refused download that completes", false, []step{{inProgress, 10}, {completed, 20}}, "refused", 0, 0, "discard"},
		{"a refused download still running", false, []step{{inProgress, 10}}, "refused", 0, 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTrackerRig(tc.allow)
			r.begin(guid)
			if !tc.allow {
				r.awaitCancel(t, guid)
			}
			for _, s := range tc.progress {
				if got := r.files.of(guid); got != "" {
					t.Fatalf("the download's files were settled (%s) before its end", got)
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
			if got := r.files.of(guid); got != tc.wantFiles {
				t.Fatalf("the download's files were settled as %q, want %q", got, tc.wantFiles)
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
			recorder.state[start.ID] = downloadInProgress.String()
			recorder.mu.Unlock()
			return downloadAdmitted
		}
		return refuse(start)
	}
	p.engine.events = events
	d := p.downloadTracker(t.Context(), newHeadlessTargets(p), newFakeDownloadFiles())
	d.cancel = func(downloadID) {}
	d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: "kept", FrameID: "F1"})
	for i := range 2*orderSlack + 1 {
		d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: fmt.Sprintf("refused-%d", i), FrameID: "F1"})
	}
	d.progress(cdpbrowser.EventDownloadProgress{GUID: "kept", ReceivedBytes: 5, State: cdpbrowser.DownloadProgressStateCompleted})
	if state, _ := recorder.outcome("kept"); state != downloadCompleted.String() {
		t.Fatalf("the admitted download reads %q at the Manager after its completion", state)
	}
}

// A download's files outlive neither its end nor its browser: a refused
// one's go when its completion arrives, before any answer to its cancel,
// and once the browser process has stopped the whole directory goes, with
// the files of the Manager's running downloads and of one still waiting
// for its frame's owner, since none of them finished.
func TestDownloadTrackerRemovesWhatNoPageTook(t *testing.T) {
	r := newTrackerRig(false)
	r.begin("ended")
	r.begin("running")
	if got := r.files.of("ended"); got != "" {
		t.Fatalf("a refused download's files were settled at its refusal: %s", got)
	}
	r.report("ended", cdpbrowser.DownloadProgressStateCompleted, 20)
	if got := r.files.of("ended"); got != "discard" {
		t.Fatalf("a refused download's files were settled as %q at its completion, want discarded", got)
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
	if got := r.files.closes(); got != 1 {
		t.Fatalf("the download directory was closed %d times after the browser stopped, want once", got)
	}
	if got := r.retained(); got != 0 {
		t.Fatalf("the stopped tracker keeps %d downloads", got)
	}
}

// One download's report the Manager is slow to take (a completion whose
// file moves on a slow filesystem) holds no lock another download's cancel
// needs: B is marked canceling, so its cancel can be sent, while A's
// completion is still with the Manager, and A's report lands once after.
func TestASlowDownloadReportDoesNotHoldAnotherDownloadsCancel(t *testing.T) {
	r := newTrackerRig(true)
	inA, releaseA := make(chan struct{}), make(chan struct{})
	deliver := r.events.DownloadProgress
	r.events.DownloadProgress = func(progress downloadProgress) {
		if progress.ID == "A" && progress.State == downloadCompleted {
			close(inA)
			<-releaseA
		}
		deliver(progress)
	}
	r.begin("A")
	r.begin("B")
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.report("A", cdpbrowser.DownloadProgressStateCompleted, 20)
	}()
	<-inA
	marked := make(chan struct{})
	go func() {
		r.canceling("B")
		close(marked)
	}()
	select {
	case <-marked:
	case <-time.After(time.Second):
		close(releaseA)
		t.Fatal("B's cancel waited for A's completion to be taken")
	}
	close(releaseA)
	<-done
	if state, _ := r.recorder.outcome("A"); state != downloadCompleted.String() {
		t.Fatalf("A reads %q at the Manager", state)
	}
}

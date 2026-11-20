package browser

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/target"

	"agent-overflow/internal/browser/ids"
)

// A download in a frame no page owns yet and no popup names, a popup's
// subframe before the popup is listed, is admitted once a listed page owns
// the frame within ownerWait, and never cancelled for the wait.
func TestHeadlessDownloadWaitsForItsFramesPage(t *testing.T) {
	const guid, frame = "G1", "F-popup-iframe"
	var mu sync.Mutex
	listed, admitted, canceled := false, 0, 0
	p := newTargetTestProfile(t, engineEvents{
		DownloadStarted: func(start downloadStart) downloadAdmission {
			mu.Lock()
			defer mu.Unlock()
			if start.Frame != frame || !listed {
				return downloadUnowned
			}
			admitted++
			return downloadAdmitted
		},
		DownloadProgress: func(downloadProgress) {},
	})
	p.engine.ownerWait = 5 * time.Second
	d := p.downloadTracker(t.Context(), newHeadlessTargets(p), newFakeDownloadFiles())
	d.cancel = func(downloadID) {
		mu.Lock()
		canceled++
		mu.Unlock()
	}
	d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: guid, FrameID: frame})
	time.Sleep(20 * requestOwnerPoll)
	mu.Lock()
	listed = true
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		gotAdmitted, gotCanceled := admitted, canceled
		mu.Unlock()
		if gotAdmitted > 0 || gotCanceled > 0 || time.Now().After(deadline) {
			if gotAdmitted != 1 || gotCanceled != 0 {
				t.Fatalf("admitted %d times, cancelled %d times; want admitted once and never cancelled", gotAdmitted, gotCanceled)
			}
			return
		}
		time.Sleep(requestOwnerPoll)
	}
}

// A download a popup starts reaches the Manager naming the popup and its
// opener, whichever of downloadWillBegin, targetCreated and
// targetDestroyed is handled first, and is never cancelled for the order.
// A start whose frame no popup ever names is refused once ownerWait passes.
func TestHeadlessPopupDownloadSettlesInAnyOrder(t *testing.T) {
	const popup, opener, guid = "T-popup", "T-opener", "G1"
	type outcome struct {
		admitted bool
		canceled int
	}
	newRig := func(wait time.Duration) (*headlessTargets, *downloadTracker, func() outcome) {
		var mu sync.Mutex
		var got outcome
		p := newTargetTestProfile(t, engineEvents{
			PopupOpened: func(enginePopup) {},
			PageClosed:  func(ids.PageHandle) {},
			DownloadStarted: func(start downloadStart) downloadAdmission {
				ok := start.Popup == popup && slices.Equal(start.Openers, []ids.PageHandle{opener})
				mu.Lock()
				got.admitted = got.admitted || ok
				mu.Unlock()
				switch {
				case ok:
					return downloadAdmitted
				case start.Popup == "":
					return downloadUnowned
				}
				return downloadRefused
			},
			DownloadProgress: func(downloadProgress) {},
		})
		p.engine.ownerWait = wait
		h := newHeadlessTargets(p)
		downloads := p.downloadTracker(t.Context(), h, newFakeDownloadFiles())
		downloads.cancel = func(downloadID) {
			mu.Lock()
			got.canceled++
			mu.Unlock()
		}
		return h, downloads, func() outcome {
			mu.Lock()
			defer mu.Unlock()
			return got
		}
	}
	streams := func(h *headlessTargets, d *downloadTracker) [][]eventStep {
		return [][]eventStep{
			{{"willBegin", func() { d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: guid, FrameID: popup}) }}},
			{{"created", func() {
				h.created(target.EventTargetCreated{TargetInfo: &target.Info{TargetID: popup, Type: "page", OpenerID: opener}})
			}}},
			{{"destroyed", func() { h.destroyed(target.EventTargetDestroyed{TargetID: popup}) }}},
		}
	}
	for i := range interleavings(streams(nil, nil)) {
		h, d, result := newRig(5 * time.Second)
		order := interleavings(streams(h, d))[i]
		for _, step := range order {
			step.run()
		}
		deadline := time.Now().Add(5 * time.Second)
		for !result().admitted && result().canceled == 0 && time.Now().Before(deadline) {
			time.Sleep(requestOwnerPoll)
		}
		if got := result(); !got.admitted || got.canceled != 0 {
			t.Errorf("handled as [%s]: admitted %v, cancelled %d times; want admitted and never cancelled", stepNames(order), got.admitted, got.canceled)
		}
	}
	t.Run("a frame no popup names", func(t *testing.T) {
		_, d, result := newRig(50 * time.Millisecond)
		d.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: guid, FrameID: "F-unknown"})
		deadline := time.Now().Add(5 * time.Second)
		for result().canceled == 0 && time.Now().Before(deadline) {
			time.Sleep(requestOwnerPoll)
		}
		if got := result(); got.admitted || got.canceled != 1 {
			t.Fatalf("admitted %v, cancelled %d times; want refused and cancelled once", got.admitted, got.canceled)
		}
	})
}

// A CDP download's progress carries the size the response announced, which
// the pane shows as how far it is.
func TestCDPDownloadProgressCarriesItsTotal(t *testing.T) {
	got := cdpDownloadProgress(cdpbrowser.EventDownloadProgress{GUID: "G", ReceivedBytes: 10, TotalBytes: 40, State: cdpbrowser.DownloadProgressStateInProgress}, false)
	if want := (downloadProgress{ID: "G", Received: 10, Total: 40, State: downloadInProgress}); got != want {
		t.Fatalf("cdpDownloadProgress = %+v, want %+v", got, want)
	}
}

// A state the pinned protocol does not name fails the download with the
// state as its reason, rather than leaving its row running, a cancel
// pending or not.
func TestCDPDownloadProgressEndsAnUnknownState(t *testing.T) {
	for _, canceling := range []bool{false, true} {
		got := cdpDownloadProgress(cdpbrowser.EventDownloadProgress{GUID: "G", ReceivedBytes: 10, State: "interrupted"}, canceling)
		if got.State != downloadFailed || !strings.Contains(got.Error, `"interrupted"`) {
			t.Fatalf("cdpDownloadProgress(canceling %v) = %+v, want failed naming the state", canceling, got)
		}
	}
}

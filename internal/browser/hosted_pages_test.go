package browser

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/webview2host"
)

// A hosted page's state moves only as the table says: a page being created
// bound once and only before its close, listed once, ending once, a pending
// popup's listing and close in either order ending closing.
func TestAHostedPageStateMovesOnlyByItsTransitions(t *testing.T) {
	for _, tc := range []struct {
		from  hostedPageState
		event hostedPageEvent
		to    hostedPageState
		moved bool
	}{
		{hostedPageCreating, pageBound, hostedPageOpen, true},
		{hostedPageCreating, pageListed, hostedPageCreating, false},
		{hostedPageCreating, pageEnding, hostedPageClosing, true},
		{hostedPopupPending, pageBound, hostedPopupPending, false},
		{hostedPageOpen, pageBound, hostedPageOpen, false},
		{hostedPopupClosing, pageBound, hostedPopupClosing, false},
		{hostedPageClosing, pageBound, hostedPageClosing, false},
		{hostedPopupPending, pageListed, hostedPageOpen, true},
		{hostedPopupPending, pageEnding, hostedPopupClosing, true},
		{hostedPageOpen, pageListed, hostedPageOpen, false},
		{hostedPageOpen, pageEnding, hostedPageClosing, true},
		{hostedPopupClosing, pageListed, hostedPageClosing, true},
		{hostedPopupClosing, pageEnding, hostedPopupClosing, false},
		{hostedPageClosing, pageListed, hostedPageClosing, false},
		{hostedPageClosing, pageEnding, hostedPageClosing, false},
	} {
		if to, moved := tc.from.next(tc.event); to != tc.to || moved != tc.moved {
			t.Errorf("%v by %d: %v %v, want %v %v", tc.from, tc.event, to, moved, tc.to, tc.moved)
		}
	}
	for _, tc := range []struct {
		state         hostedPageState
		admits, popup bool
	}{
		{hostedPageCreating, false, false},
		{hostedPopupPending, true, true},
		{hostedPageOpen, true, false},
		{hostedPopupClosing, false, true},
		{hostedPageClosing, false, false},
	} {
		if tc.state.admits() != tc.admits || tc.state.pendingPopup() != tc.popup {
			t.Errorf("%v admits %v, pending popup %v", tc.state, tc.state.admits(), tc.state.pendingPopup())
		}
	}
}

// blockedPages answers pages whose lanes announce each work on taken and
// hold it until the test sends on proceed.
func blockedPages(t *testing.T) (pages *hostedPages, taken <-chan *acceptedWork, proceed chan<- struct{}) {
	t.Helper()
	works := make(chan *acceptedWork, 64)
	next := make(chan struct{})
	pages = newHostedPages(func(ctx context.Context, w *acceptedWork) {
		works <- w
		select {
		case <-next:
		case <-ctx.Done():
		}
	}, func(<-chan struct{}) {}, func(ids.PageHandle, popupadmit.Observation) {})
	t.Cleanup(func() { pages.stopAll()() })
	return pages, works, next
}

func take(t *testing.T, taken <-chan *acceptedWork) *acceptedWork {
	t.Helper()
	select {
	case w := <-taken:
		return w
	case <-time.After(hostedTestDeadline):
		t.Fatal("no work reached its lane")
		return nil
	}
}

func runningSnapshot(id string, seq uint64) webview2host.DownloadReport {
	return webview2host.DownloadReport{ID: id, Seq: seq, State: webview2host.DownloadRunning, Received: 1}
}

// Receipt records what is true at once: a close seals the page, a second
// close joins the first, a download id is its page's, and an answer
// creates nothing; a pending popup's work runs on its opener's lane until
// it is listed and its work there drains.
func TestHostedPagesReceiveWhatIsTrueAtReceipt(t *testing.T) {
	pages, taken, _ := blockedPages(t)
	openForTest(pages, "opener")
	if !pages.addPopup("popup", "opener", "profile") || pages.addPopup("popup", "opener", "profile") {
		t.Fatal("a popup was recorded twice")
	}
	if got := pages.acceptSnapshot("opener", runningSnapshot("dl-o", 1)); got != snapshotAccepted {
		t.Fatalf("the opener's first snapshot: %v", got)
	}
	take(t, taken)
	if got := pages.acceptSnapshot("popup", runningSnapshot("dl-1", 1)); got != snapshotAccepted {
		t.Fatalf("the popup's first snapshot: %v", got)
	}
	if lane, _ := pages.laneOf("popup"); lane != "opener" {
		t.Fatalf("the pending popup runs on lane %q, want its opener's", lane)
	}
	if got := pages.acceptSnapshot("opener", runningSnapshot("dl-1", 2)); got != snapshotOtherPage {
		t.Fatalf("the popup's download reported by its opener: %v, want refused", got)
	}
	answer := runningSnapshot("dl-9", 1)
	answer.Answer = true
	if got := pages.acceptSnapshot("popup", answer); got != snapshotUnasked {
		t.Fatalf("an answer about no download: %v", got)
	}
	if got := pages.acceptSnapshot("never-was", runningSnapshot("dl-1", 3)); got != snapshotNoPage {
		t.Fatalf("a page with no record: %v", got)
	}

	first := pages.acceptEnd("popup", acceptedClose, "")
	if !first.accepted || !first.pendingPopup || first.retired == nil {
		t.Fatalf("the popup's close: %+v", first)
	}
	if got := pages.acceptSnapshot("popup", runningSnapshot("dl-2", 1)); got != snapshotSealed {
		t.Fatalf("a first snapshot after the close: %v, want sealed", got)
	}
	again := pages.acceptEnd("popup", acceptedDiscard, "")
	if again.accepted || again.retired != first.retired {
		t.Fatalf("a discard after the close: %+v, want it joined to the close", again)
	}
	if _, _, ok := pages.cancel("dl-1"); ok {
		t.Fatal("a closing page's download took a cancel")
	}
	if got := pages.waiting("popup"); len(got) != 2 {
		t.Fatalf("the opener's lane waits on %v, want the popup's snapshot and close", got)
	}
}

// A listed popup's later work runs on its own lane once what it already
// had on its opener's drains.
func TestAListedPopupLeavesItsOpenersLaneOnceItDrains(t *testing.T) {
	pages, taken, proceed := blockedPages(t)
	openForTest(pages, "opener")
	pages.addPopup("popup", "opener", "profile")
	pages.acceptSnapshot("popup", runningSnapshot("dl-1", 1))
	take(t, taken)
	pages.listed("popup")
	pages.acceptSnapshot("popup", webview2host.DownloadReport{ID: "dl-1", Seq: 2, State: webview2host.DownloadCompleted, Path: `C:\x.bin`})
	if lane, _ := pages.laneOf("popup"); lane != "opener" {
		t.Fatalf("a listed popup with work waiting moved to lane %q", lane)
	}
	proceed <- struct{}{}
	take(t, taken)
	proceed <- struct{}{}
	for deadline := time.Now().Add(hostedTestDeadline); pages.busy(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the opener's lane never drained")
		}
	}
	pages.acceptSnapshot("popup", runningSnapshot("dl-2", 1))
	if lane, _ := pages.laneOf("popup"); lane != "popup" {
		t.Fatalf("the listed popup's work after the drain runs on lane %q, want its own", lane)
	}
	take(t, taken)
	proceed <- struct{}{}
}

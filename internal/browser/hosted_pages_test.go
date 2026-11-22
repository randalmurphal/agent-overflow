package browser

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
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
	}, func(<-chan struct{}) {}, func(ids.PageHandle, bool, string, []ids.PageHandle) {})
	t.Cleanup(func() {
		join, ends, paid := pages.stopAll()
		join()
		for _, end := range ends {
			paid(end)
		}
	})
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
	if got := pages.receive("opener", receivedSnapshot(runningSnapshot("dl-o", 1))).outcome; got != receiptQueued {
		t.Fatalf("the opener's first snapshot: %v", got)
	}
	take(t, taken)
	if got := pages.receive("popup", receivedSnapshot(runningSnapshot("dl-1", 1))).outcome; got != receiptQueued {
		t.Fatalf("the popup's first snapshot: %v", got)
	}
	if lane, _ := pages.laneOf("popup"); lane != "opener" {
		t.Fatalf("the pending popup runs on lane %q, want its opener's", lane)
	}
	if got := pages.receive("opener", receivedSnapshot(runningSnapshot("dl-1", 2))).outcome; got != receiptOtherPage {
		t.Fatalf("the popup's download reported by its opener: %v, want refused", got)
	}
	answer := runningSnapshot("dl-9", 1)
	answer.Answer = true
	if got := pages.receive("popup", receivedSnapshot(answer)).outcome; got != receiptUnasked {
		t.Fatalf("an answer about no download: %v", got)
	}
	if got := pages.receive("never-was", receivedSnapshot(runningSnapshot("dl-1", 3))).outcome; got != receiptNoPage {
		t.Fatalf("a page with no record: %v", got)
	}

	first := pages.receive("popup", receivedClose())
	if first.outcome != receiptQueued || first.retired == nil {
		t.Fatalf("the popup's close: %+v", first)
	}
	if got := pages.receive("popup", receivedSnapshot(runningSnapshot("dl-2", 1))).outcome; got != receiptSealed {
		t.Fatalf("a first snapshot after the close: %v, want sealed", got)
	}
	again := pages.receive("popup", receivedDiscard())
	if again.outcome != receiptEnding || again.retired != first.retired {
		t.Fatalf("a discard after the close: %+v, want it joined to the close", again)
	}
	if _, _, ok := pages.cancel("dl-1"); ok {
		t.Fatal("a closing page's download took a cancel")
	}
	if got := pages.waiting("popup"); len(got) != 2 {
		t.Fatalf("the opener's lane waits on %v, want the popup's snapshot and close", got)
	}
}

// heldPages answers pages whose lanes announce each work on taken and hold
// it until the test releases the work's download id (its kind for a page
// end); a close retires its page once released.
func heldPages(t *testing.T) (pages *hostedPages, taken <-chan *acceptedWork, release func(id string)) {
	t.Helper()
	works := make(chan *acceptedWork, 64)
	var mu sync.Mutex
	gates := make(map[string]chan struct{})
	gate := func(id string) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		if gates[id] == nil {
			gates[id] = make(chan struct{})
		}
		return gates[id]
	}
	pages = newHostedPages(func(ctx context.Context, w *acceptedWork) {
		id := string(w.snapshot.ID)
		if w.kind != acceptedSnapshot {
			id = fmt.Sprintf("%s %d", w.page, w.kind)
		}
		works <- w
		select {
		case <-gate(id):
		case <-ctx.Done():
		}
		if w.kind == acceptedClose {
			_, finish := pages.retire(w.page, pageClosedReason)
			finish()
		}
	}, func(<-chan struct{}) {}, func(ids.PageHandle, bool, string, []ids.PageHandle) {})
	t.Cleanup(func() {
		join, ends, paid := pages.stopAll()
		join()
		for _, end := range ends {
			paid(end)
		}
	})
	return pages, works, func(id string) { close(gate(id)) }
}

// takeID takes the next work and fails unless it is of download id.
func takeID(t *testing.T, taken <-chan *acceptedWork, id string) {
	t.Helper()
	if w := take(t, taken); string(w.snapshot.ID) != id {
		t.Fatalf("the lanes took %q (kind %d of %s), want %q", w.snapshot.ID, w.kind, w.page, id)
	}
}

// takesNothing fails if a lane takes work within a moment.
func takesNothing(t *testing.T, taken <-chan *acceptedWork, why string) {
	t.Helper()
	select {
	case w := <-taken:
		t.Fatalf("%s: the lanes took %q of %s", why, w.snapshot.ID, w.page)
	case <-time.After(100 * time.Millisecond):
	}
}

// A listed popup's later work runs on its own lane as soon as its own work
// received before its listing has finished, while its opener's lane still
// has work and takes more.
func TestAListedPopupLeavesItsOpenersLaneOnceItsOwnWorkFinishes(t *testing.T) {
	pages, taken, release := heldPages(t)
	openForTest(pages, "opener")
	pages.addPopup("popup", "opener", "profile")
	pages.receive("opener", receivedSnapshot(runningSnapshot("dl-o1", 1)))
	takeID(t, taken, "dl-o1")
	pages.receive("popup", receivedSnapshot(runningSnapshot("dl-p1", 1)))
	pages.receive("opener", receivedSnapshot(runningSnapshot("dl-o2", 1)))
	pages.listed("popup")
	pages.receive("popup", receivedSnapshot(runningSnapshot("dl-p2", 1)))
	if lane, _ := pages.laneOf("popup"); lane != "popup" {
		t.Fatalf("the listed popup's later work runs on lane %q, want its own", lane)
	}
	release("dl-o1")
	takeID(t, taken, "dl-p1")
	takesNothing(t, taken, "the popup's later work ran before its earlier work finished")
	pages.receive("opener", receivedSnapshot(runningSnapshot("dl-o3", 1)))
	release("dl-p1")
	got := map[string]bool{}
	for range 2 {
		got[string(take(t, taken).snapshot.ID)] = true
	}
	if !got["dl-p2"] || !got["dl-o2"] {
		t.Fatalf("with the opener's dl-o2 held, the lanes took %v, want the popup's dl-p2 beside it", got)
	}
	release("dl-p2")
	release("dl-o2")
	takeID(t, taken, "dl-o3")
	release("dl-o3")
}

// A popup of an unlisted popup whose record went runs its later work on a
// lane of that page id, after its work already on their opener's lane.
func TestAPopupsWorkKeepsItsOrderWhenItsUnlistedOpenerGoes(t *testing.T) {
	pages, taken, release := heldPages(t)
	openForTest(pages, "opener")
	pages.addPopup("popup", "opener", "profile")
	pages.addPopup("child", "popup", "profile")
	pages.receive("opener", receivedSnapshot(runningSnapshot("dl-o1", 1)))
	takeID(t, taken, "dl-o1")
	if pages.receive("popup", receivedClose()).outcome != receiptQueued {
		t.Fatal("the popup's close was not accepted")
	}
	pages.receive("child", receivedSnapshot(runningSnapshot("dl-c1", 1)))
	pages.receive("opener", receivedSnapshot(runningSnapshot("dl-o2", 1)))
	release("dl-o1")
	if w := take(t, taken); w.kind != acceptedClose || w.page != "popup" {
		t.Fatalf("the lanes took %q of %s, want the popup's close", w.snapshot.ID, w.page)
	}
	release(fmt.Sprintf("popup %d", acceptedClose))
	takeID(t, taken, "dl-c1")
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		if _, ok := pages.state("popup"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the popup's record never went")
		}
	}
	pages.receive("child", receivedSnapshot(runningSnapshot("dl-c2", 1)))
	if lane, _ := pages.laneOf("child"); lane != "popup" {
		t.Fatalf("the child's later work runs on lane %q, want its gone opener's id", lane)
	}
	takesNothing(t, taken, "the child's later work ran before its earlier work finished")
	release("dl-c1")
	got := map[string]bool{}
	for range 2 {
		got[string(take(t, taken).snapshot.ID)] = true
	}
	if !got["dl-c2"] || !got["dl-o2"] {
		t.Fatalf("the lanes took %v, want the child's dl-c2 and the opener's dl-o2", got)
	}
	release("dl-c2")
	release("dl-o2")
}

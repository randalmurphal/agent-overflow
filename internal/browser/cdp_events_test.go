package browser

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	jsonv2 "github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// eventStep is one CDP event fed to a handler, named for failure messages.
type eventStep struct {
	name string
	run  func()
}

// interleavings returns every merge of streams that keeps each stream's own
// order: the orders chromedp can hand per-method subscriptions to their
// handlers in, whatever order the browser sent the events in.
func interleavings(streams [][]eventStep) [][]eventStep {
	var out [][]eventStep
	var walk func(prefix []eventStep, rest [][]eventStep)
	walk = func(prefix []eventStep, rest [][]eventStep) {
		done := true
		for i, stream := range rest {
			if len(stream) == 0 {
				continue
			}
			done = false
			next := make([][]eventStep, len(rest))
			copy(next, rest)
			next[i] = stream[1:]
			walk(append(append([]eventStep(nil), prefix...), stream[0]), next)
		}
		if done {
			out = append(out, prefix)
		}
	}
	walk(nil, streams)
	return out
}

func stepNames(steps []eventStep) string {
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = step.name
	}
	return strings.Join(names, ", ")
}

// Network idle must report the same answer whichever of a request's start
// and end is handled first. A redirect reuses the request id for several
// starts before the one end, so the end can be handled between them.
func TestNetworkTrackerSettlesInAnyOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)
	const quiet = 500 * time.Millisecond
	cases := []struct {
		name     string
		streams  func(tr *networkTracker) [][]eventStep
		wantIdle bool
		// wantKept is how many request ids are left once every event is
		// handled: a settled request leaves nothing behind.
		wantKept int
	}{
		{"a finished request", func(tr *networkTracker) [][]eventStep {
			return [][]eventStep{
				{start(tr, "R1", false, now)},
				{end(tr, "R1", now)},
			}
		}, true, 0},
		{"a redirected request", func(tr *networkTracker) [][]eventStep {
			return [][]eventStep{
				{start(tr, "R1", false, now), start(tr, "R1", true, now), start(tr, "R1", true, now)},
				{end(tr, "R1", now)},
			}
		}, true, 0},
		{"two requests", func(tr *networkTracker) [][]eventStep {
			return [][]eventStep{
				{start(tr, "R1", false, now), start(tr, "R2", false, now)},
				{end(tr, "R2", now), end(tr, "R1", now)},
			}
		}, true, 0},
		{"a request still in flight", func(tr *networkTracker) [][]eventStep {
			return [][]eventStep{
				{start(tr, "R1", false, now), start(tr, "R2", false, now)},
				{end(tr, "R2", now)},
			}
		}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The streams are rebuilt per order so each order runs on its
			// own tracker.
			probe := newNetworkTracker(now)
			for i := range interleavings(tc.streams(probe)) {
				tr := newNetworkTracker(now)
				order := interleavings(tc.streams(tr))[i]
				for _, step := range order {
					step.run()
				}
				if got := tr.idle(later, quiet); got != tc.wantIdle {
					t.Errorf("handled as [%s]: idle = %v, want %v", stepNames(order), got, tc.wantIdle)
				}
				if got := tr.retained(); got != tc.wantKept {
					t.Errorf("handled as [%s]: kept %d request ids, want %d", stepNames(order), got, tc.wantKept)
				}
			}
		})
	}
}

func start(tr *networkTracker, id network.RequestID, redirect bool, now time.Time) eventStep {
	name := "start " + string(id)
	if redirect {
		name = "redirect " + string(id)
	}
	return eventStep{name, func() { tr.started(id, redirect, now) }}
}

func end(tr *networkTracker, id network.RequestID, now time.Time) eventStep {
	return eventStep{"end " + string(id), func() { tr.ended(id, now) }}
}

// A page owns a frame from its attach until its detach, whichever of the
// three frame events is handled first. A frame swapped out to another
// process and back is attached twice and detached once.
func TestFrameTrackerSettlesInAnyOrder(t *testing.T) {
	const frame = cdp.FrameID("F1")
	attach := func(tr *frameTracker) eventStep {
		return eventStep{"attached", func() { tr.attached(frame) }}
	}
	navigate := func(tr *frameTracker) eventStep {
		return eventStep{"navigated", func() { tr.navigated(frame) }}
	}
	detach := func(tr *frameTracker) eventStep {
		return eventStep{"detached", func() { tr.detached(frame) }}
	}
	// The frame tree read once the page subscribed names the frame.
	seed := func(tr *frameTracker) eventStep {
		tree := &page.FrameTree{
			Frame:       &cdp.Frame{ID: "main"},
			ChildFrames: []*page.FrameTree{{Frame: &cdp.Frame{ID: frame, ParentID: "main"}}},
		}
		return eventStep{"seeded", func() { tr.seed(tree) }}
	}
	cases := []struct {
		name     string
		streams  func(tr *frameTracker) [][]eventStep
		wantOwns bool
	}{
		{"a detached frame", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{attach(tr)}, {navigate(tr)}, {detach(tr)}}
		}, false},
		{"a live frame", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{attach(tr)}, {navigate(tr)}, {}}
		}, true},
		{"a frame attached before the page subscribed", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{}, {navigate(tr)}, {}}
		}, true},
		{"a frame attached before the page subscribed, then detached", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{}, {navigate(tr)}, {detach(tr)}}
		}, false},
		{"a frame swapped out and back", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{attach(tr), attach(tr)}, {}, {detach(tr)}}
		}, true},
		{"a frame in the frame tree", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{}, {seed(tr)}, {}}
		}, true},
		{"a frame in the frame tree, then detached", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{}, {seed(tr)}, {detach(tr)}}
		}, false},
		{"a frame in the frame tree, swapped out and back", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{attach(tr)}, {seed(tr)}, {detach(tr)}}
		}, true},
		{"a frame in the frame tree, swapped out and back, navigated", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{attach(tr)}, {seed(tr), navigate(tr)}, {detach(tr)}}
		}, true},
		{"a frame in the frame tree, swapped twice, then removed", func(tr *frameTracker) [][]eventStep {
			return [][]eventStep{{attach(tr), attach(tr)}, {seed(tr)}, {detach(tr), detach(tr), detach(tr)}}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count := len(interleavings(tc.streams(newFrameTracker())))
			for i := range count {
				tr := newFrameTracker()
				order := interleavings(tc.streams(tr))[i]
				for _, step := range order {
					step.run()
				}
				if got := tr.owns(frame); got != tc.wantOwns {
					t.Errorf("handled as [%s]: owns = %v, want %v", stepNames(order), got, tc.wantOwns)
				}
			}
		})
	}
}

// downloadRecorder stands in for the Manager's download bookkeeping: like
// downloadProgress, it ignores progress for a download it never saw start.
type downloadRecorder struct {
	mu       sync.Mutex
	allow    bool
	state    map[string]string
	bytes    map[string]float64
	canceled map[string]int
}

func newDownloadRecorder(allow bool) *downloadRecorder {
	return &downloadRecorder{allow: allow, state: map[string]string{}, bytes: map[string]float64{}, canceled: map[string]int{}}
}

func (r *downloadRecorder) events() engineEvents {
	return engineEvents{
		DownloadStarted: func(start downloadStart) downloadAdmission {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.state[start.ID] = downloadInProgress
			if !r.allow {
				r.state[start.ID] = "refused"
				return downloadRefused
			}
			return downloadAdmitted
		},
		DownloadProgress: func(progress downloadProgress) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if state, ok := r.state[progress.ID]; !ok || state == "refused" {
				return
			}
			r.state[progress.ID] = progress.State
			r.bytes[progress.ID] = progress.Received
		},
	}
}

func (r *downloadRecorder) cancel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canceled[id]++
}

func (r *downloadRecorder) outcome(id string) (string, float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state[id], r.bytes[id]
}

// A download reaches the Manager started and then settled, whichever of
// downloadWillBegin and downloadProgress is handled first. A small download
// can be complete before its start is handled.
func TestDownloadTrackerReportsInAnyOrder(t *testing.T) {
	const guid = "G1"
	willBegin := func(tr *downloadTracker) eventStep {
		return eventStep{"willBegin", func() {
			tr.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: guid, FrameID: "F1", URL: "https://example.test/a.zip", SuggestedFilename: "a.zip"})
		}}
	}
	progress := func(tr *downloadTracker, state cdpbrowser.DownloadProgressState, received float64) eventStep {
		return eventStep{fmt.Sprintf("%s %.0f", state, received), func() {
			tr.progress(cdpbrowser.EventDownloadProgress{GUID: guid, ReceivedBytes: received, State: state})
		}}
	}
	cases := []struct {
		name       string
		allow      bool
		progress   func(tr *downloadTracker) []eventStep
		wantState  string
		wantBytes  float64
		wantCancel int
		wantKept   int
	}{
		{"a completed download", true, func(tr *downloadTracker) []eventStep {
			return []eventStep{progress(tr, cdpbrowser.DownloadProgressStateInProgress, 10), progress(tr, cdpbrowser.DownloadProgressStateCompleted, 20)}
		}, downloadCompleted, 20, 0, 0},
		{"a canceled download", true, func(tr *downloadTracker) []eventStep {
			return []eventStep{progress(tr, cdpbrowser.DownloadProgressStateInProgress, 10), progress(tr, cdpbrowser.DownloadProgressStateCanceled, 10)}
		}, downloadCanceled, 10, 0, 0},
		{"a download still running", true, func(tr *downloadTracker) []eventStep {
			return []eventStep{progress(tr, cdpbrowser.DownloadProgressStateInProgress, 10), progress(tr, cdpbrowser.DownloadProgressStateInProgress, 15)}
		}, downloadInProgress, 15, 0, 1},
		{"a refused download", false, func(tr *downloadTracker) []eventStep {
			return []eventStep{progress(tr, cdpbrowser.DownloadProgressStateCompleted, 20)}
		}, "refused", 0, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			streams := func(tr *downloadTracker) [][]eventStep {
				return [][]eventStep{{willBegin(tr)}, tc.progress(tr)}
			}
			count := len(interleavings(streams(newDownloadTracker(engineEvents{}, nil))))
			for i := range count {
				recorder := newDownloadRecorder(tc.allow)
				canceled := make(chan struct{}, 4)
				tr := newDownloadTracker(recorder.events(), func(id string) {
					recorder.cancel(id)
					canceled <- struct{}{}
				})
				order := interleavings(streams(tr))[i]
				for _, step := range order {
					step.run()
				}
				for range tc.wantCancel {
					select {
					case <-canceled:
					case <-time.After(5 * time.Second):
						t.Fatalf("handled as [%s]: the refused download was never cancelled", stepNames(order))
					}
				}
				state, bytes := recorder.outcome(guid)
				if state != tc.wantState || bytes != tc.wantBytes {
					t.Errorf("handled as [%s]: the Manager saw %q with %.0f bytes, want %q with %.0f", stepNames(order), state, bytes, tc.wantState, tc.wantBytes)
				}
				recorder.mu.Lock()
				got := recorder.canceled[guid]
				recorder.mu.Unlock()
				if got != tc.wantCancel {
					t.Errorf("handled as [%s]: cancelled %d times, want %d", stepNames(order), got, tc.wantCancel)
				}
				if got := tr.retained(); got != tc.wantKept {
					t.Errorf("handled as [%s]: kept %d downloads, want %d", stepNames(order), got, tc.wantKept)
				}
			}
		})
	}
}

// popupRecorder stands in for the Manager's page events. Popups and closes
// are reported on goroutines of their own, so it is synchronized and waited
// on.
type popupRecorder struct {
	mu      sync.Mutex
	changed chan struct{}
	opened  map[string]enginePopup
	closed  map[string]int
	infos   map[string][]string
}

func newPopupRecorder() *popupRecorder {
	return &popupRecorder{changed: make(chan struct{}, 64), opened: map[string]enginePopup{}, closed: map[string]int{}, infos: map[string][]string{}}
}

func (r *popupRecorder) events() engineEvents {
	return engineEvents{
		PopupOpened: func(popup enginePopup) {
			r.mu.Lock()
			r.opened[popup.Handle] = popup
			r.mu.Unlock()
			r.changed <- struct{}{}
		},
		PageClosed: func(handle string) {
			r.mu.Lock()
			r.closed[handle]++
			r.mu.Unlock()
			r.changed <- struct{}{}
		},
		PageInfoChanged: func(handle, url, title string, _ uint64) {
			r.mu.Lock()
			r.infos[handle] = append(r.infos[handle], url)
			r.mu.Unlock()
		},
	}
}

// waitFor waits until done reports true of the recorder.
func (r *popupRecorder) waitFor(t *testing.T, what string, done func(r *popupRecorder) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		ok := done(r)
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-r.changed:
		case <-deadline:
			t.Fatalf("never saw %s", what)
		}
	}
}

// newestURL is the URL the Manager ends with: the popup's own, then each
// info change after it.
func (r *popupRecorder) newestURL(handle string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if infos := r.infos[handle]; len(infos) > 0 {
		return infos[len(infos)-1]
	}
	return r.opened[handle].URL
}

func newTargetTestProfile(t *testing.T, events engineEvents) *headlessProfile {
	t.Helper()
	engine := &headlessEngine{
		events: events, logf: t.Logf,
		profiles: make(map[*headlessProfile]struct{}), pageProfile: make(map[string]*headlessProfile),
	}
	p := &headlessProfile{engine: engine, handle: "profile"}
	engine.profiles[p] = struct{}{}
	return p
}

// A popup reaches the Manager open with its newest info, or not at all once
// it is gone, whichever of targetCreated, targetInfoChanged and
// targetDestroyed is handled first. A page the Manager created itself is
// reported closed, and its info changes reach the Manager, whatever the
// order.
func TestHeadlessTargetsSettleInAnyOrder(t *testing.T) {
	const popup, opener, ownPage = "T-popup", "T-opener", "T-own"
	created := func(h *headlessTargets, id, openerID, url string) eventStep {
		return eventStep{"created " + id, func() {
			h.created(target.EventTargetCreated{TargetInfo: &target.Info{TargetID: target.ID(id), Type: "page", OpenerID: target.ID(openerID), URL: url}})
		}}
	}
	changed := func(h *headlessTargets, id, url string) eventStep {
		return eventStep{"infoChanged " + id, func() {
			h.infoChanged(target.EventTargetInfoChanged{TargetInfo: &target.Info{TargetID: target.ID(id), Type: "page", URL: url}})
		}}
	}
	destroyed := func(h *headlessTargets, id string) eventStep {
		return eventStep{"destroyed " + id, func() {
			h.destroyed(target.EventTargetDestroyed{TargetID: target.ID(id)})
		}}
	}
	t.Run("a popup that closed", func(t *testing.T) {
		streams := func(h *headlessTargets) [][]eventStep {
			return [][]eventStep{{created(h, popup, opener, "about:blank")}, {changed(h, popup, "https://example.test/")}, {destroyed(h, popup)}}
		}
		for i := range interleavings(streams(nil)) {
			recorder := newPopupRecorder()
			p := newTargetTestProfile(t, recorder.events())
			h := newHeadlessTargets(p)
			order := interleavings(streams(h))[i]
			for _, step := range order {
				step.run()
			}
			if _, bound := p.engine.profileForPage(popup); bound {
				t.Errorf("handled as [%s]: the destroyed popup is still bound to the profile", stepNames(order))
				continue
			}
			if got := h.retained(); got != 1 {
				t.Errorf("handled as [%s]: kept %d target ids for a popup that is gone, want only its bounded tombstone", stepNames(order), got)
			}
			recorder.waitFor(t, "every reported popup reported closed", func(r *popupRecorder) bool {
				_, opened := r.opened[popup]
				return opened == (r.closed[popup] == 1) && r.closed[popup] <= 1
			})
		}
	})
	t.Run("a popup that stays open", func(t *testing.T) {
		streams := func(h *headlessTargets) [][]eventStep {
			return [][]eventStep{{created(h, popup, opener, "about:blank")}, {changed(h, popup, "https://example.test/")}, {}}
		}
		for i := range interleavings(streams(nil)) {
			recorder := newPopupRecorder()
			p := newTargetTestProfile(t, recorder.events())
			h := newHeadlessTargets(p)
			order := interleavings(streams(h))[i]
			for _, step := range order {
				step.run()
			}
			if _, bound := p.engine.profileForPage(popup); !bound {
				t.Errorf("handled as [%s]: the open popup is not bound to the profile", stepNames(order))
				continue
			}
			recorder.waitFor(t, "the popup reported", func(r *popupRecorder) bool {
				_, opened := r.opened[popup]
				return opened
			})
			if got := recorder.newestURL(popup); got != "https://example.test/" {
				t.Errorf("handled as [%s]: the Manager ends with URL %q, want the newest", stepNames(order), got)
			}
		}
	})
	t.Run("a page the Manager created", func(t *testing.T) {
		streams := func(h *headlessTargets) [][]eventStep {
			return [][]eventStep{{created(h, ownPage, "", "about:blank")}, {}, {destroyed(h, ownPage)}}
		}
		for i := range interleavings(streams(nil)) {
			recorder := newPopupRecorder()
			p := newTargetTestProfile(t, recorder.events())
			p.engine.bindPage(ownPage, p)
			h := newHeadlessTargets(p)
			order := interleavings(streams(h))[i]
			for _, step := range order {
				step.run()
			}
			recorder.waitFor(t, "the page reported closed", func(r *popupRecorder) bool { return r.closed[ownPage] == 1 })
			if _, bound := p.engine.profileForPage(ownPage); bound {
				t.Errorf("handled as [%s]: the destroyed page is still bound", stepNames(order))
			}
			if got := h.retained(); got != 1 {
				t.Errorf("handled as [%s]: kept %d target ids for a page that is gone, want only its bounded tombstone", stepNames(order), got)
			}
		}
	})
	t.Run("a page the Manager created changes", func(t *testing.T) {
		streams := func(h *headlessTargets) [][]eventStep {
			return [][]eventStep{{created(h, ownPage, "", "about:blank")}, {changed(h, ownPage, "https://example.test/")}, {}}
		}
		for i := range interleavings(streams(nil)) {
			recorder := newPopupRecorder()
			p := newTargetTestProfile(t, recorder.events())
			p.engine.bindPage(ownPage, p)
			h := newHeadlessTargets(p)
			order := interleavings(streams(h))[i]
			for _, step := range order {
				step.run()
			}
			if got := recorder.newestURL(ownPage); got != "https://example.test/" {
				t.Errorf("handled as [%s]: the Manager ends with URL %q, want the changed one", stepNames(order), got)
			}
			if got := h.retained(); got != 1 {
				t.Errorf("handled as [%s]: kept %d target ids for one live page", stepNames(order), got)
			}
		}
	})
}

// A headless popup's close and crash before its listing reach the Manager
// at the listing, which acts only on a listed page; the close also at
// once, so what the popup downloaded is listed while its opener still is.
// A page the Manager created hears its crash at once.
func TestHeadlessPopupEndsWaitForTheListing(t *testing.T) {
	const popup, crashed, own = "T-popup", "T-crashed", "T-own"
	var mu sync.Mutex
	var got []string
	p := newTargetTestProfile(t, engineEvents{
		PopupOpened: func(enginePopup) {},
		PageClosed: func(handle string) {
			mu.Lock()
			got = append(got, "closed "+handle)
			mu.Unlock()
		},
		PageCrashed: func(handle, reason string) {
			mu.Lock()
			got = append(got, "crashed "+handle)
			mu.Unlock()
		},
	})
	expect := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(headlessTestDeadline)
		for {
			mu.Lock()
			now := slices.Clone(got)
			mu.Unlock()
			slices.Sort(now)
			if slices.Equal(now, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the Manager heard %q, want %q", now, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	h := newHeadlessTargets(p)
	for _, id := range []target.ID{popup, crashed} {
		h.created(target.EventTargetCreated{TargetInfo: &target.Info{TargetID: id, Type: "page", OpenerID: "T-opener"}})
	}
	p.engine.bindPage(own, p)
	listed := func(handle string) adoptedPage {
		return headlessPopupPage{pageDriver: &handledPage{handle: handle}, engine: p.engine}
	}

	h.destroyed(target.EventTargetDestroyed{TargetID: popup})
	p.targetCrashed(target.EventTargetCrashed{TargetID: crashed})
	p.targetCrashed(target.EventTargetCrashed{TargetID: own})
	expect("closed "+popup, "crashed "+own)
	listed(popup).Adopted()
	listed(crashed).Adopted()
	expect("closed "+popup, "closed "+popup, "crashed "+crashed, "crashed "+own)
	if len(p.engine.popupEnds) != 0 {
		t.Fatalf("the engine still holds %d popups' ends after their listing", len(p.engine.popupEnds))
	}
}

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
	d := p.downloadTracker(t.Context(), newHeadlessTargets(p))
	d.cancel = func(string) {
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

// An info change held until its page's targetCreated is numbered before any
// info change handled after that created, even when the newer one is
// delivered while the held one still is: the Manager, which keeps the
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
			PageClosed:  func(string) {},
			DownloadStarted: func(start downloadStart) downloadAdmission {
				ok := start.Popup == popup && slices.Equal(start.Openers, []string{opener})
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
		downloads := p.downloadTracker(t.Context(), h)
		downloads.cancel = func(string) {
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

// highest order, ends with the newest info.
func TestHeadlessTargetsNumberHeldInfoFirst(t *testing.T) {
	const page = "T-own"
	inHeld, releaseHeld := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	orders := map[string]uint64{}
	events := engineEvents{PageInfoChanged: func(handle, url, title string, order uint64) {
		if url == "https://held.test/" {
			close(inHeld)
			<-releaseHeld
		}
		mu.Lock()
		orders[url] = order
		mu.Unlock()
	}}
	p := newTargetTestProfile(t, events)
	p.engine.bindPage(page, p)
	h := newHeadlessTargets(p)
	h.infoChanged(target.EventTargetInfoChanged{TargetInfo: &target.Info{TargetID: page, Type: "page", URL: "https://held.test/"}})
	createdDone := make(chan struct{})
	go func() {
		defer close(createdDone)
		h.created(target.EventTargetCreated{TargetInfo: &target.Info{TargetID: page, Type: "page", URL: "about:blank"}})
	}()
	select {
	case <-inHeld:
	case <-createdDone:
		t.Fatal("targetCreated returned without delivering the held info")
	}
	// The newer change is delivered while the held one is still being
	// delivered.
	h.infoChanged(target.EventTargetInfoChanged{TargetInfo: &target.Info{TargetID: page, Type: "page", URL: "https://newer.test/"}})
	close(releaseHeld)
	<-createdDone
	mu.Lock()
	defer mu.Unlock()
	held, newer := orders["https://held.test/"], orders["https://newer.test/"]
	if len(orders) != 2 || held == 0 || newer <= held {
		t.Fatalf("the reports were numbered %v, want the held info first and the newer one after", orders)
	}
}

// An end whose start never comes, a request sent before Network.enable, is
// kept only up to orderSlack, and the requests in flight are never dropped
// to make room.
func TestNetworkTrackerBoundsUnmatchedEnds(t *testing.T) {
	now := time.Now()
	tr := newNetworkTracker(now)
	tr.started("live", false, now)
	for i := range orderSlack + 50 {
		tr.ended(network.RequestID(fmt.Sprintf("early-%d", i)), now)
	}
	if got := tr.retained(); got != orderSlack+1 {
		t.Fatalf("kept %d request ids, want the %d newest early ends and the request in flight", got, orderSlack)
	}
	if tr.idle(now.Add(time.Hour), 0) {
		t.Fatal("the request in flight was dropped to make room for early ends")
	}
	// The newest early end is still matched by its late start.
	tr.started(network.RequestID(fmt.Sprintf("early-%d", orderSlack+49)), false, now)
	tr.ended("live", now)
	if !tr.idle(now.Add(time.Hour), 0) {
		t.Fatal("the newest early end no longer settles its late start")
	}
}

// Detached frames are kept only up to orderSlack, and live frames are never
// dropped to make room.
func TestFrameTrackerBoundsDetachedFrames(t *testing.T) {
	tr := newFrameTracker()
	for i := range 10 {
		tr.attached(cdp.FrameID(fmt.Sprintf("live-%d", i)))
	}
	for i := range 2 * orderSlack {
		id := cdp.FrameID(fmt.Sprintf("gone-%d", i))
		tr.attached(id)
		tr.navigated(id)
		tr.detached(id)
	}
	if got := tr.retained(); got != 10+orderSlack {
		t.Fatalf("kept %d frames, want the 10 live ones and %d detached", got, orderSlack)
	}
	for i := range 10 {
		if !tr.owns(cdp.FrameID(fmt.Sprintf("live-%d", i))) {
			t.Fatalf("live frame %d was dropped to make room for detached ones", i)
		}
	}
}

// Progress for downloads that never begin is kept only up to orderSlack.
func TestDownloadTrackerBoundsUnmatchedProgress(t *testing.T) {
	recorder := newDownloadRecorder(true)
	tr := newDownloadTracker(recorder.events(), recorder.cancel)
	for i := range orderSlack + 50 {
		tr.progress(cdpbrowser.EventDownloadProgress{GUID: fmt.Sprintf("G%d", i), State: cdpbrowser.DownloadProgressStateCompleted})
	}
	if got := tr.retained(); got != orderSlack {
		t.Fatalf("kept %d downloads, want %d", got, orderSlack)
	}
}

// Gone targets and info held for targets not yet created are each kept
// only up to orderSlack.
func TestHeadlessTargetsBoundUnmatchedDestroys(t *testing.T) {
	h := newHeadlessTargets(newTargetTestProfile(t, newPopupRecorder().events()))
	for i := range orderSlack + 50 {
		id := target.ID(fmt.Sprintf("T%d", i))
		h.infoChanged(target.EventTargetInfoChanged{TargetInfo: &target.Info{TargetID: id, Type: "page"}})
		h.destroyed(target.EventTargetDestroyed{TargetID: id})
	}
	for i := range orderSlack + 50 {
		h.infoChanged(target.EventTargetInfoChanged{TargetInfo: &target.Info{TargetID: target.ID(fmt.Sprintf("U%d", i)), Type: "page"}})
	}
	if got := h.retained(); got != 2*orderSlack {
		t.Fatalf("kept %d target ids, want %d gone and %d held infos", got, orderSlack, orderSlack)
	}
}

// chromedp.Console joins the console API, uncaught exceptions and the
// browser log in one ordered stream. The log keeps what it showed before
// the join: no exceptions, the page's URL for an API call, the entry's own
// URL for a log entry, and level log for a verbose log entry.
func TestConsoleEntryKeepsTheLogFormat(t *testing.T) {
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	pageURL := func() string { return "https://page.test/" }
	cases := []struct {
		name    string
		message chromedp.ConsoleMessage
		want    ConsoleLog
		wantOK  bool
	}{
		{"an API call", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleWarning, Time: at, URL: "https://script.test/app.js",
			Args: []*cdpruntime.RemoteObject{
				{Type: "string", Value: jsonv2.Value(`"hello"`)}, {Type: "number", Value: jsonv2.Value(`3`)},
				{Type: "object", Description: "Window"}, {Type: "undefined"},
			},
		}, ConsoleLog{Level: "warn", Message: "hello 3 Window undefined", Timestamp: at.Format(time.RFC3339Nano), URL: "https://page.test/"}, true},
		{"a log entry", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleError, Text: "Failed to load resource", Time: at, URL: "https://cdn.test/a.png", Source: "network",
		}, ConsoleLog{Level: "error", Message: "Failed to load resource", Timestamp: at.Format(time.RFC3339Nano), URL: "https://cdn.test/a.png"}, true},
		{"a verbose log entry", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleDebug, Text: "verbose", Time: at, Source: "other",
		}, ConsoleLog{Level: "log", Message: "verbose", Timestamp: at.Format(time.RFC3339Nano)}, true},
		{"a console.debug call", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleDebug, Time: at, Args: []*cdpruntime.RemoteObject{{Type: "string", Value: jsonv2.Value(`"d"`)}},
		}, ConsoleLog{Level: "debug", Message: "d", Timestamp: at.Format(time.RFC3339Nano), URL: "https://page.test/"}, true},
		{"an uncaught exception", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleException, Text: "Uncaught Error: boom", Time: at, Exception: &cdpruntime.ExceptionDetails{Text: "Uncaught"},
		}, ConsoleLog{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := consoleEntry(tc.message, pageURL)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("consoleEntry = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// A CDP download's progress carries the size the response announced, which
// the pane shows as how far it is.
func TestCDPDownloadProgressCarriesItsTotal(t *testing.T) {
	got := cdpDownloadProgress(cdpbrowser.EventDownloadProgress{GUID: "G", ReceivedBytes: 10, TotalBytes: 40, State: cdpbrowser.DownloadProgressStateInProgress})
	if want := (downloadProgress{ID: "G", Received: 10, Total: 40, State: downloadInProgress}); got != want {
		t.Fatalf("cdpDownloadProgress = %+v, want %+v", got, want)
	}
}

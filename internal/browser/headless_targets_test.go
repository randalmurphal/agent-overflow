package browser

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/target"

	"agent-overflow/internal/browser/ids"
)

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

// A headless popup's report, close and crash reach the Manager as their
// target events are handled, before each handler returns and in the order
// handled, with nothing held back for a listing: the popup's admission in
// the Manager holds them (engine_pages.go). A crash of a page the Manager
// created reaches it the same way.
func TestHeadlessReportsAPopupsEndsAsTheyAreHandled(t *testing.T) {
	const popup, crashed, own = "T-popup", "T-crashed", "T-own"
	var mu sync.Mutex
	var got []string
	record := func(what string) {
		mu.Lock()
		got = append(got, what)
		mu.Unlock()
	}
	p := newTargetTestProfile(t, engineEvents{
		PopupOpened: func(opened enginePopup) { record("opened " + string(opened.Handle)) },
		PageClosed:  func(handle ids.PageHandle) { record("closed " + string(handle)) },
		PageCrashed: func(handle ids.PageHandle, reason string) { record("crashed " + string(handle)) },
	})
	heard := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
	h := newHeadlessTargets(p)
	for _, id := range []target.ID{popup, crashed} {
		h.created(target.EventTargetCreated{TargetInfo: &target.Info{TargetID: id, Type: "page", OpenerID: "T-opener"}})
	}
	p.engine.bindPage(own, p)
	h.destroyed(target.EventTargetDestroyed{TargetID: popup})
	p.targetCrashed(target.EventTargetCrashed{TargetID: crashed})
	p.targetCrashed(target.EventTargetCrashed{TargetID: own})
	want := []string{"opened " + popup, "opened " + crashed, "closed " + popup, "crashed " + crashed, "crashed " + own}
	if now := heard(); !slices.Equal(now, want) {
		t.Fatalf("the Manager heard %q as the events were handled, want %q", now, want)
	}
}

// An info change held until its page's targetCreated is numbered before any
// info change handled after that created, even when the newer one is
// delivered while the held one still is: the Manager, which keeps the
// highest order, ends with the newest info.
func TestHeadlessTargetsNumberHeldInfoFirst(t *testing.T) {
	const page = "T-own"
	inHeld, releaseHeld := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	orders := map[string]uint64{}
	events := engineEvents{PageInfoChanged: func(handle ids.PageHandle, url, title string, order uint64) {
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

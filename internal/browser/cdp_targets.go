package browser

import (
	"sync"

	"github.com/chromedp/cdproto/target"
)

// maxKnownTargets bounds the known targets and the opener edges a
// connection keeps. A browser's live targets are far fewer (the launcher
// keeps at most webview2host.MaxHostPages pages, each with its frames and
// workers); past it the oldest go, so a target whose destroy was handled
// before its creation, after its tombstone left the gone record, is not
// kept for the connection's life.
const maxKnownTargets = 4 * orderSlack

// cdpTargets is one CDP connection's record of its targets, kept across
// targetCreated, targetInfoChanged and targetDestroyed, which chromedp
// delivers on three subscriptions in no fixed order (cdp_events.go). A
// target id is known (created handled, destroyed not), gone (destroyed
// handled, whether or not created was), or has its info held (an info
// change handled before anything answers to the target: the newest is
// kept). A created or an info change for a gone target is dropped, so a
// target destroyed before its creation is handled leaves nothing behind.
// A gone popup keeps its opener edge, for a download start Chromium sends
// after the destroy was handled (downloadOwners), also when its creation is
// handled after its destroy.
//
// known and openers hold live targets, and a target whose destroy was
// handled before its creation once its tombstone left gone; both are
// bounded by maxKnownTargets, gone and the held info by orderSlack. The
// record goes with its connection: each connection makes its own.
//
// Every info change is numbered as it is handled, held or not, so a held
// change delivered late is still older than every change handled after it,
// and the Manager, which keeps the highest number, ends with the newest.
// Each method runs the engine's part, when it takes one, under mu, so what
// the engine binds or looks up there is settled against the other events of
// the same target.
type cdpTargets struct {
	mu    sync.Mutex
	known *boundedMap[target.ID, struct{}]
	// openers maps each known page target that has an opener to it, for the
	// browser-wide navigation policy.
	openers *boundedMap[target.ID, target.ID]
	// gone maps each gone target to the opener it had, or to "".
	gone     *boundedMap[target.ID, target.ID]
	heldInfo *boundedMap[target.ID, heldTarget]
	// next numbers the info changes, called under mu
	// (engineEvents.PageInfoChanged).
	next func() uint64
}

// heldTarget is a target's newest info change before anything answers to
// it, with its number.
type heldTarget struct {
	info  *target.Info
	order uint64
}

// newCDPTargets makes a connection's record. next numbers its info
// changes; nil numbers them from one, for an engine whose pages go with
// the connection.
func newCDPTargets(next func() uint64) *cdpTargets {
	if next == nil {
		var order uint64
		next = func() uint64 {
			order++
			return order
		}
	}
	return &cdpTargets{
		next:     next,
		known:    newBoundedMap[target.ID, struct{}](maxKnownTargets),
		openers:  newBoundedMap[target.ID, target.ID](maxKnownTargets),
		gone:     newBoundedMap[target.ID, target.ID](orderSlack),
		heldInfo: newBoundedMap[target.ID, heldTarget](orderSlack),
	}
}

// created records a created target, with its opener when it is a page that
// has one, and answers false for a target already gone. Otherwise then, if
// given, takes and runs with the target's newest info: the info held for
// it, with its number, if there was any. With no then, the held info stays
// for takeHeld.
func (t *cdpTargets) created(info *target.Info, then func(newest *target.Info, held bool, order uint64)) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if opener, gone := t.gone.get(info.TargetID); gone {
		if opener == "" && info.Type == "page" && info.OpenerID != "" {
			t.gone.put(info.TargetID, info.OpenerID)
		}
		return false
	}
	t.known.put(info.TargetID, struct{}{})
	if info.Type == "page" && info.OpenerID != "" {
		t.openers.put(info.TargetID, info.OpenerID)
	}
	if then == nil {
		// The info held for the target stays for the page that comes to
		// answer to it (takeHeld).
		return true
	}
	newest, held := t.heldInfo.take(info.TargetID)
	if !held {
		newest = heldTarget{info: info}
	}
	then(newest.info, held, newest.order)
	return true
}

// changed numbers an info change and answers its number, and whether
// answered (run under mu) says a page answers to the target; otherwise the
// change is held for the target unless it is gone.
func (t *cdpTargets) changed(info *target.Info, answered func() bool) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	order := t.next()
	if answered() {
		return order, true
	}
	if _, gone := t.gone.get(info.TargetID); !gone {
		t.heldInfo.put(info.TargetID, heldTarget{info: info, order: order})
	}
	return order, false
}

// takeHeld takes the info held for id, with its number, for a page that has
// just come to answer to it. The engine makes the page answer first, so a
// change handled after the take is delivered, numbered after this one.
func (t *cdpTargets) takeHeld(id target.ID) (*target.Info, uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	held, ok := t.heldInfo.take(id)
	return held.info, held.order, ok
}

// destroyed forgets a target and keeps it gone, with the opener it had,
// then runs then.
func (t *cdpTargets) destroyed(id target.ID, then func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	opener, _ := t.openers.get(id)
	if kept, _ := t.gone.get(id); opener == "" {
		opener = kept
	}
	t.known.delete(id)
	t.openers.delete(id)
	t.heldInfo.delete(id)
	t.gone.put(id, opener)
	if then != nil {
		then()
	}
}

// isKnownLocked reports whether id is a known target. The caller holds mu,
// in a method's part.
func (t *cdpTargets) isKnownLocked(id target.ID) bool {
	_, known := t.known.get(id)
	return known
}

// owners answers frame followed by the pages that opened it, nearest first,
// when frame is a known popup, and frame alone otherwise. A gone popup's
// edge is no authority for a request.
func (t *cdpTargets) owners(frame string) []string {
	return t.chain(frame, false)
}

// downloadOwners is owners following gone popups' edges too: a download
// Chromium starts in a popup reaches its handler in no fixed order with
// the popup's destroy, and its start names the popup and its openers
// either way.
func (t *cdpTargets) downloadOwners(frame string) []string {
	return t.chain(frame, true)
}

func (t *cdpTargets) chain(frame string, gone bool) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	owners := []string{frame}
	// A popup has one opener, so the chain is at most every recorded popup
	// long; the bound stops a cycle.
	for id := target.ID(frame); len(owners) <= t.openers.len()+t.gone.len(); {
		opener, _ := t.openers.get(id)
		if opener == "" && gone {
			opener, _ = t.gone.get(id)
		}
		if opener == "" {
			break
		}
		owners = append(owners, string(opener))
		id = opener
	}
	return owners
}

// retained reports how many target ids the record keeps.
func (t *cdpTargets) retained() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.known.len() + t.openers.len() + t.gone.len() + t.heldInfo.len()
}

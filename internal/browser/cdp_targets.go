package browser

import (
	"sync"

	"github.com/chromedp/cdproto/target"
)

// cdpTargets is one CDP connection's record of its targets, kept across
// targetCreated, targetInfoChanged and targetDestroyed, which chromedp
// delivers on three subscriptions in no fixed order (cdp_events.go). A
// target id is known (created handled, destroyed not), gone (destroyed
// handled, whether or not created was), or has its info held (an info
// change handled before anything answers to the target: the newest is
// kept). A created or an info change for a gone target is dropped, so a
// target destroyed before its creation is handled leaves nothing behind.
//
// known and openers hold only live targets, so the browser's targets bound
// them; gone and the held info are bounded by orderSlack. The record goes
// with its connection: each connection makes its own.
//
// Every info change is numbered as it is handled, held or not, so a held
// change delivered late is still older than every change handled after it,
// and the Manager, which keeps the highest number, ends with the newest.
// Each method runs the engine's part, when it takes one, under mu, so what
// the engine binds or looks up there is settled against the other events of
// the same target.
type cdpTargets struct {
	mu    sync.Mutex
	known map[target.ID]struct{}
	// openers maps each known page target that has an opener to it, for the
	// browser-wide navigation policy.
	openers  map[target.ID]target.ID
	gone     *boundedMap[target.ID, struct{}]
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
		known:    make(map[target.ID]struct{}),
		openers:  make(map[target.ID]target.ID),
		gone:     newBoundedMap[target.ID, struct{}](orderSlack),
		heldInfo: newBoundedMap[target.ID, heldTarget](orderSlack),
	}
}

// created records a created target, with its opener when it is a page that
// has one, and answers false for a target already gone. Otherwise then runs
// with the target's newest info: the info held for it, with its number, if
// there was any.
func (t *cdpTargets) created(info *target.Info, then func(newest *target.Info, held bool, order uint64)) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, gone := t.gone.get(info.TargetID); gone {
		return false
	}
	t.known[info.TargetID] = struct{}{}
	if info.Type == "page" && info.OpenerID != "" {
		t.openers[info.TargetID] = info.OpenerID
	}
	newest, held := t.heldInfo.take(info.TargetID)
	if !held {
		newest = heldTarget{info: info}
	}
	if then != nil {
		then(newest.info, held, newest.order)
	}
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

// destroyed forgets a target and keeps it gone, then runs then.
func (t *cdpTargets) destroyed(id target.ID, then func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.known, id)
	delete(t.openers, id)
	t.heldInfo.delete(id)
	t.gone.put(id, struct{}{})
	if then != nil {
		then()
	}
}

// isKnownLocked reports whether id is a known target. The caller holds mu,
// in a method's part.
func (t *cdpTargets) isKnownLocked(id target.ID) bool {
	_, known := t.known[id]
	return known
}

// owners answers frame followed by the pages that opened it, nearest first,
// when frame is a known popup, and frame alone otherwise.
func (t *cdpTargets) owners(frame string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	owners := []string{frame}
	// A popup has one opener, so the chain is at most every known popup
	// long; the bound stops a cycle.
	for id := target.ID(frame); len(owners) <= len(t.openers); {
		opener := t.openers[id]
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
	return len(t.known) + len(t.openers) + t.gone.len() + t.heldInfo.len()
}

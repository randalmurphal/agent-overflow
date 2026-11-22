package browser

import (
	"context"
	"fmt"
)

// An operation on a tab that waits (for the structural gate, an engine
// call, a restore) between finding the tab and acting on it acts on the
// tab's incarnation it found, never on the tab's id again: a jar switch, a
// restore, a crash or a re-creation in a new jar replaces the incarnation
// under the same id meanwhile, and what was admitted for the old one (a
// claim's address, a label) does not belong to the new one. tabHold is that
// incarnation, and heldLocked, in the hold that commits, the one check that
// it is still the tab's.

// tabHold is one incarnation of a tab an operation holds across a wait:
// the live page it resolved or created (p) or the suspended record it
// restores (rec), and the claim the page's creation took on its thread's
// browsing, if any. A page object or a record is one incarnation: a jar
// switch suspends a live page into a new record, a restore replaces a
// record with a new page, and a creation finding its jar switched makes a
// new page (freshPendingPageLocked), each under the same id.
type tabHold struct {
	p     *managedPage
	rec   *suspendedPage
	claim *tabClaim
}

// errTabReplaced answers an operation whose tab was replaced by another
// incarnation, or closed, after the operation found it. It is a not found
// of the tab the operation held.
var errTabReplaced = fmt.Errorf("browser: the tab changed while the action waited: %w", errPageNotFound)

// heldLocked answers nil while access may commit on h's incarnation: its
// lifetime is still the thread's, h's page is the live page listed under
// its id, a page of that lifetime in its current jar, or h's record is the
// tab's record, and h's claim still holds. The caller holds m.mu.
func (m *Manager) heldLocked(access Access, h tabHold) error {
	if err := access.admitLocked(); err != nil {
		return err
	}
	switch {
	case h.p != nil:
		if live, _ := m.livePageLocked(h.p.id); live != h.p || h.p.owner != access.tb || !m.jarCurrentLocked(h.p) {
			return errTabReplaced
		}
	case h.rec != nil:
		if m.suspended[h.rec.id] != h.rec || h.rec.owner != access.tb {
			return errTabReplaced
		}
	default:
		return errTabReplaced
	}
	return h.claim.holdsLocked(access.tb)
}

// lockHeld takes m.mu for a structural change to h's tab, after any jar
// switch of access's lifetime (lockStructure), and answers with it held
// while h is still the tab's incarnation (heldLocked); otherwise it answers
// why, with m.mu not held.
func (m *Manager) lockHeld(ctx context.Context, access Access, h tabHold) error {
	if err := m.lockStructure(ctx, access.tb); err != nil {
		return err
	}
	if err := m.heldLocked(access, h); err != nil {
		m.mu.Unlock()
		return err
	}
	return nil
}

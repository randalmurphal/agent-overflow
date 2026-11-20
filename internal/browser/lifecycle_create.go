package browser

import (
	"context"
	"errors"
	"fmt"
	"log"

	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/browser/turnorder"
)

// A new tab's creation (lifecycle.go has the lifecycle). Its admission and
// its pending entry are one m.mu hold through its thread lifetime
// (reserveTabLocked); the engine page is created after it (createReserved),
// admitted again where it is created and listed in the hold that settles
// it.

// tabClaim is what a new tab takes from its thread's browsing besides its
// slot: the address it loads, read from a closed tab it reopens or a tab it
// duplicates. A reopen also takes its closed tab off the history in the
// hold that adds the tab. The claim is void once the browsing it was taken
// from is forgotten (a clear, a jar switch) or, for a reopen, once another
// reopen took its closed tab: the tab is then refused with errClaimVoid.
type tabClaim struct {
	browsing *browsing
	address  string
	label    string
	index    int
	// reopen is set for a reopen, whose closed tab is closed; taken once the
	// hold that added the tab took closed off the history.
	reopen bool
	taken  bool
	closed closedTab
}

// errClaimVoid refuses a tab whose claim on its thread's browsing is void
// (tabClaim).
var errClaimVoid = errors.New("browser: the tab's address was forgotten")

// reopenClaimLocked is the claim of a reopen of tb's most recently closed
// tab; false when it has none. The caller holds m.mu.
func reopenClaimLocked(tb *threadBrowser) (*tabClaim, bool) {
	stack := tb.browsing.closed.tabs
	if len(stack) == 0 {
		return nil, false
	}
	last := stack[len(stack)-1]
	return &tabClaim{
		browsing: tb.browsing, address: last.url, label: last.label, index: last.index,
		reopen: true, closed: last,
	}, true
}

// duplicateClaimLocked is the claim of a duplicate of tb's tab pageID:
// placed right after it, at its address when it has one a new tab can
// load. false when pageID is no tab of tb. The caller holds m.mu.
func (m *Manager) duplicateClaimLocked(tb *threadBrowser, pageID PageID) (*tabClaim, bool) {
	for i, tab := range m.companionTabsLocked(tb) {
		if tab.id != pageID {
			continue
		}
		claim := &tabClaim{browsing: tb.browsing, index: i + 1}
		if tab.pending == nil && reopenable(tab.info.URL) {
			claim.address = tab.info.URL
		}
		return claim, true
	}
	return nil, false
}

// holdsLocked answers nil while claim still holds in tb. The caller holds
// m.mu.
func (c *tabClaim) holdsLocked(tb *threadBrowser) error {
	if c == nil {
		return nil
	}
	if tb.browsing != c.browsing {
		return errClaimVoid
	}
	if stack := c.browsing.closed.tabs; c.reopen && !c.taken && (len(stack) == 0 || stack[len(stack)-1] != c.closed) {
		// Another reopen took it.
		return errClaimVoid
	}
	return nil
}

// admitTabLocked is a new tab's admission in the hold that adds it and
// again in the startMu hold that creates its page: access's thread
// lifetime is still the thread's (thread_life.go), and its claim still
// holds. The caller holds m.mu.
func (m *Manager) admitTabLocked(access Access, claim *tabClaim) error {
	if err := access.admitLocked(); err != nil {
		return err
	}
	return claim.holdsLocked(access.tb)
}

// admissionRefused is a tab admitTabLocked refused where its page is
// created.
type admissionRefused struct{ err error }

func (e *admissionRefused) Error() string { return e.err.Error() }
func (e *admissionRefused) Unwrap() error { return e.err }

// createPage creates a page for access's thread. Its tab appears at once,
// creating, and the call returns once the engine page exists. A tab closed
// meanwhile answers errClosedWhileCreating. This is an agent's tab, opened
// behind the selection; the user's go through createPageAt.
func (m *Manager) createPage(ctx context.Context, access Access) (*managedPage, error) {
	return m.createPageAt(ctx, access, nil, -1, nil)
}

// createPageAt is createPage with the new tab placed at index in the
// thread's tab order from the start; a negative index appends it. A tab the
// user opens has the turn its selection was admitted in (selection.go): it
// is selected and its companion shown before the engine is asked, in the
// hold that adds the tab and commits the turn, and a failure stays on the
// tab. A turn whose pane detached meanwhile opens nothing and answers
// errSelectionPassed. An agent's tab has no turn. claim, when set, is taken in
// the hold that adds the tab (admitTabLocked).
func (m *Manager) createPageAt(ctx context.Context, access Access, turn *turnorder.Turn, index int, claim *tabClaim) (*managedPage, error) {
	if err := m.lockStructure(ctx, access.tb); err != nil {
		return nil, err
	}
	reserved, err := m.reserveTabLocked(ctx, access, turn, index, claim)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return m.createReserved(reserved)
}

// tabReservation is a new tab admitted and pending in its thread's tabs
// (reserveTabLocked), whose engine page createReserved creates.
type tabReservation struct {
	access  Access
	pending *pendingPage
	ctx     context.Context
	user    bool
	claim   *tabClaim
	// savedMoved is whether placing the tab moved a suspended tab's saved
	// position.
	savedMoved bool
}

// reserveTabLocked admits a new tab of access's thread and adds it pending,
// placed at index (negative appends it) and, for a turn's tab (the user's),
// selected with the companion shown, the turn committed. The caller holds
// m.mu, taken by lockStructure, and calls createReserved after releasing
// it.
func (m *Manager) reserveTabLocked(ctx context.Context, access Access, turn *turnorder.Turn, index int, claim *tabClaim) (*tabReservation, error) {
	if err := m.admitTabLocked(access, claim); err != nil {
		return nil, err
	}
	user := turn != nil
	if user && turn.Commit() == turnorder.Passed {
		return nil, errSelectionPassed
	}
	tb := access.tb
	if err := m.admitThreadPageLocked(tb); err != nil {
		return nil, err
	}
	if claim != nil && claim.reopen {
		stack := &tb.browsing.closed
		stack.tabs = stack.tabs[:len(stack.tabs)-1]
		claim.taken = true
	}
	p := newManagedPage(tb, access.Caller())
	p.info = PageInfo{ID: string(p.id), URL: "about:blank"}
	createCtx, cancel := context.WithCancel(ctx)
	pending := &pendingPage{p: p, cancel: cancel, done: make(chan struct{})}
	if m.pending == nil {
		m.pending = make(map[PageID]*pendingPage)
	}
	m.pending[p.id] = pending
	r := &tabReservation{access: access, pending: pending, ctx: createCtx, user: user, claim: claim}
	if index >= 0 {
		r.savedMoved, _ = m.placeTabLocked(tb, p.id, index)
	}
	if user {
		tb.session.ActivePageID, tb.session.Visible = string(p.id), true
	}
	return r, nil
}

// createReserved creates the engine page of a tab reserveTabLocked added.
// The tab is pushed first; the call returns once the engine page exists.
func (m *Manager) createReserved(r *tabReservation) (*managedPage, error) {
	access, pending, user := r.access, r.pending, r.user
	defer pending.cancel()
	defer close(pending.done)
	tb, threadID := access.tb, access.ThreadID()
	p := pending.p
	m.emitThreadState(threadID)
	if r.savedMoved {
		if err := m.persistThreads([]ThreadID{threadID}); err != nil {
			// The suspended tabs keep their new places in memory; only a
			// restart before the next save puts them back.
			log.Printf("browser: place a new tab of thread %s: save the thread's pages: %v", threadID, err)
		}
	}
	if user {
		// The page shown before is hidden while the new tab is created.
		m.syncPanePresentation(threadID)
	}

	admit := func() error {
		if err := m.admitTabLocked(access, r.claim); err != nil {
			return &admissionRefused{err: err}
		}
		return nil
	}
	// One startMu hold spans the engine start and the page's registration, so
	// the idle close cannot stop the engine in between.
	var scope *jarScope
	var err error
	var closed bool
	var outcome listingOutcome
	var verdict popupadmit.Verdict
	for {
		m.startMu.Lock()
		scope, err = m.openPageLocked(r.ctx, access.Caller(), p, admit)
		// One m.mu hold decides the creation, whatever its setup answered,
		// against its close flag, the end its engine reported, the forget
		// fence and its thread's jar as they are now, so none of them can
		// change between reading it and acting on it. A switch writing its
		// saved copy is waited for: the listing is a change of the tabs it
		// captured. Without a cancel: a switch ends with its one write.
		_ = m.lockStructure(context.Background(), tb)
		closed = pending.closed
		verdict = p.unlistedEnd.Verdict(err != nil)
		outcome = m.unlistedOutcomeLocked(p, !closed, scope, verdict)
		if outcome == retryStale {
			// The thread switched jars while the engine created the page in the
			// old one: it is created again, under the same tab, in the new one.
			stale := p
			p = m.freshPendingPageLocked(pending)
			m.mu.Unlock()
			m.startMu.Unlock()
			m.abandonStale(scope, stale, p)
			continue
		}
		m.settleCreationLocked(r, scope, outcome, verdict, err)
		m.mu.Unlock()
		m.startMu.Unlock()
		break
	}
	if outcome == listLive {
		// The tab leaves its creating state.
		m.pageAdded(p)
		return p, nil
	}
	return nil, m.finishUnlisted(r, scope, outcome, verdict, err)
}

// settleCreationLocked applies outcome, the decision on r's page
// (unlistedOutcomeLocked), to its tab in the hold that made it: a live page
// is listed, a crashed one becomes the crashed tab, a user's tab whose
// setup failed keeps the failure as its error, and any other tab leaves,
// except a tab closed while it was created, whose entry holds its
// thread's slot until its engine page is closed (finishUnlisted). The
// thread's selection is settled in the same hold, from its tabs as the
// hold leaves them. setupErr is what the page's setup answered. The caller
// holds m.mu.
func (m *Manager) settleCreationLocked(r *tabReservation, scope *jarScope, outcome listingOutcome, verdict popupadmit.Verdict, setupErr error) {
	pending := r.pending
	p := pending.p
	var refused *admissionRefused
	switch {
	case outcome == listLive:
		delete(m.pending, p.id)
		delete(scope.creating, p)
		m.listPageLocked(scope, p)
	case outcome == listCrashed:
		delete(m.pending, p.id)
		m.listCrashedLocked(p, crashFailure(verdict.CrashReason()))
	case outcome == refuseGone && pending.closed:
	case outcome == refuseFailed && r.user && !errors.As(setupErr, &refused):
		pending.err = truncateUTF8(setupErr.Error(), maxPageErrorBytes)
	default:
		delete(m.pending, p.id)
	}
	// The selection follows the tabs as this hold leaves them.
	m.repairActivePageLocked(p.owner)
}

// finishUnlisted finishes r's creation that listed no live page
// (settleCreationLocked) and answers its error: its engine page is closed
// and its reservation ended (releaseUnlisted), the tab state published,
// and a profile the creation left empty failing to dispose is reported
// whether or not its tab is still there, since a close can take the tab
// at any point from here and then answers with it. A crashed tab answers
// its crash, which a user's tab shows; a page closed by its engine
// errClosedBeforeListing; a tab closed meanwhile, or taken by a forget of
// every page (recordsAdmittedLocked), errClosedWhileCreating.
func (m *Manager) finishUnlisted(r *tabReservation, scope *jarScope, outcome listingOutcome, verdict popupadmit.Verdict, setupErr error) error {
	pending, user := r.pending, r.user
	p, threadID := pending.p, r.access.ThreadID()
	cleanup := m.releaseUnlisted(scope, p)
	if outcome == refuseGone && pending.closed {
		// The engine page is closed before the entry stops holding the
		// thread's slot.
		m.dropPending(pending)
	}
	// The creation's own failure (its cancellation, for a closed tab) is not
	// a teardown failure.
	teardown := errors.Join(teardownPart(setupErr), cleanup)
	if pending.closed || user || outcome != refuseFailed {
		pending.cleanupErr = teardown
		if teardown != nil {
			m.reportTeardown(threadID, p.id, teardown)
		}
	} else if cleanup != nil {
		// The agent that asked hears of it with the failure.
		setupErr = errors.Join(setupErr, asTeardown(cleanup))
	}
	if outcome == refuseGone && pending.closed && setupErr == nil {
		// Closed while the engine created it: the close published the tab.
		return errClosedWhileCreating
	}
	m.emitThreadState(threadID)
	m.syncPanePresentation(threadID)
	var refused *admissionRefused
	switch {
	case outcome == listCrashed:
		if err := m.persistThreads([]ThreadID{threadID}); err != nil {
			// The tab is in memory; only a restart before the next save loses it.
			log.Printf("browser: page %s crashed as it opened: save the thread's pages: %v", p.id, err)
		}
		crashed := errors.New("browser: " + crashFailure(verdict.CrashReason()))
		if user {
			return &shownOnTab{err: crashed}
		}
		return crashed
	case outcome == refuseClosed:
		return errClosedBeforeListing
	case outcome == refuseGone:
		// Closed meanwhile, or crashed while every page is forgotten, which
		// closes the tab with them.
		return errClosedWhileCreating
	case errors.As(setupErr, &refused):
		return refused.err
	case user:
		return &shownOnTab{err: setupErr}
	}
	return setupErr
}

// freshPendingPageLocked replaces a pending tab's page, whose engine page was
// created under a jar its thread has left, with a new page under the same id
// and tab position, and answers it. The caller holds m.mu and abandons the
// old page (abandonStale).
func (m *Manager) freshPendingPageLocked(pending *pendingPage) *managedPage {
	old := pending.p
	p := newManagedPage(old.owner, old.access)
	p.id, p.createdAt, p.info = old.id, old.createdAt, old.cachedInfo()
	p.tabOrder.Store(old.tabOrder.Load())
	p.lastUse.Store(old.lastUse.Load())
	pending.p = p
	return p
}

// abandonStale closes a page created in a jar its thread has left, before it
// was registered; a failure to release that jar's profile reports as the
// teardown of the tab's page.
func (m *Manager) abandonStale(scope *jarScope, stale, tab *managedPage) {
	if err := m.abandonPage(scope, stale); err != nil {
		m.reportTeardown(tab.owner.id, tab.id, err)
	}
}

// shownOnTab is a failed creation the user's tab shows as its Error.
type shownOnTab struct{ err error }

func (e *shownOnTab) Error() string { return e.err.Error() }
func (e *shownOnTab) Unwrap() error { return e.err }

// dropPending forgets a creation entry.
func (m *Manager) dropPending(pending *pendingPage) {
	m.mu.Lock()
	if m.pending[pending.p.id] == pending {
		delete(m.pending, pending.p.id)
	}
	m.mu.Unlock()
}

// admitThreadPageLocked refuses a page past the thread's cap. Every tab of
// tb counts, and so does a page of it still closing or being created after
// its tab was closed. The caller holds m.mu.
func (m *Manager) admitThreadPageLocked(tb *threadBrowser) error {
	if m.threadPageCountLocked(tb) < maxPagesPerThread {
		return nil
	}
	closing := 0
	for _, scope := range m.scopes {
		for _, c := range scope.closing {
			if c.p.owner == tb && m.suspended[c.p.id] == nil {
				closing++
			}
		}
	}
	for _, pending := range m.pending {
		if pending.closed && pending.p.owner == tb {
			closing++
		}
	}
	return fmt.Errorf("browser: page limit reached for thread (%d)%s", maxPagesPerThread, closingHint(closing))
}

// closingHint says why a cap is reached while closed pages still hold it.
func closingHint(closing int) string {
	if closing > 0 {
		return "; a closed page is still shutting down, try again in a moment"
	}
	return "; close a page first"
}

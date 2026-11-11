package browser

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// engine searches; the Manager holds what the engine holds of it: whether a
// search runs on the page, its query and current match, and who asked for
// it. It ends when the page commits another document, stops being the shown
// active page, is suspended or closes, or when the pane mount that asked
// goes; that end is announced as a Kind "find" event so the bar showing the
// search closes. Whether a find bar is open is the frontend's alone.
//
// A search's owner is a pane mount and a sequence: the mount's provider
// numbers its starts in increasing order, and the Manager refuses a start
// whose sequence is not above every start it took from that mount. A step
// names the exact sequence it steps. A stop names a bound: it ends the
// mount's search, on whichever of the thread's pages it runs, exactly when
// that search's sequence is at or below the bound, so a stop made before a
// start never ends it, and one made after a start that was refused, dropped
// or never answered ends whatever search the mount held before it. Every
// answer and end carries the owner with the page (id and incarnation), the
// document the search started on, and a generation. A start replaces the
// page's search; another mount whose search it replaced hears its end. A
// step whose search is
// not the page's, or a stop that finds none at or below its bound, changes
// nothing and answers Foreign, naming the search's real owner ("" for none),
// so it acknowledges nothing of the asker's.
//
// Every find transition of a page holds the page lock (managedPage.mu, the
// lock every engine call already takes) from its precondition check through
// its engine calls to the state it publishes: start, step, stop, every end,
// and the engine's cleanup. An action's deadline starts before it waits for
// the lock, and every engine call under the lock is bounded by it. An end
// raised where waiting is not allowed (the engine's UI thread, a
// presentation change, a mount detaching) is submitted to the page
// (findEnds) and applied by the next transition, or by a transition of its
// own when none is running; each transition applies what was submitted
// before it first and what was submitted during it last, so an end lands
// strictly before or strictly after any action. A commit's end is bound to
// the document epoch stamped where the engine reported it; a presentation
// end rereads what is shown when it is applied. Ends submitted while a long
// page operation holds the lock wait for it: the search's highlights then
// outlive the document or the presentation by that long.
//
// Whether the engine holds a search AO no longer wants (findSession.engine
// with no active session) is state, not a job: every transition settles it
// first and last, retrying a stop that failed before, whoever asks.
//
// Generations come from one counter across the Manager, so their order
// holds across a page's suspend and restore.

const (
	// maxFindQueryBytes bounds a query. A search term is a phrase; anything
	// longer is not one.
	maxFindQueryBytes = 1024
	// maxFindSeedRunes bounds a selection that seeds the engine's own bar,
	// the rule the SPA's find bar applies to its seed.
	maxFindSeedRunes = 200
	// findActionTimeout bounds one find action, from before it waits for the
	// page lock to its last engine call, and one application of submitted
	// ends (Manager.findTimeout).
	findActionTimeout = 10 * time.Second
)

var errFindUnavailable = errors.New("browser: find is not available on this page's browser engine")

var errFindQueryTooLong = fmt.Errorf("browser: find query exceeds %d bytes", maxFindQueryBytes)

// The find actions (Find's kind).
const (
	// FindStart replaces the search with one for the query; in FindModeBar an
	// empty query is a stop. In FindModeEngine it opens the engine's own bar.
	FindStart = "start"
	FindNext  = "next"
	FindPrev  = "previous"
	// FindStop ends the owner's search; in FindModeEngine it closes the
	// engine's bar.
	FindStop = "stop"
)

// findSession is one page's find state. Written only by a transition, which
// holds managedPage.mu; managedPage.findMu guards it for readers.
type findSession struct {
	// gen is the generation of the page's newest start, stop or end.
	gen uint64
	// owner and seq are the pane mount and sequence the running search
	// belongs to; "" and 0 when none runs.
	owner string
	seq   uint64
	// doc is the document epoch the search started on.
	doc uint64
	// active is whether a search runs.
	active bool
	// engine is whether the engine may hold a search AO has not cleared:
	// set by every FindStart, cleared only by a FindStop that succeeded.
	engine bool
	mode   string
	state  FindState
}

// ownedBy reports whether the running search is the owner's, at exactly
// its sequence.
func (s *findSession) ownedBy(owner findOwner) bool {
	return s.active && s.owner == owner.mount && s.seq == owner.seq
}

// heldThrough reports whether the running search is the mount's, at or
// below the bound.
func (s *findSession) heldThrough(mount string, through uint64) bool {
	return s.active && s.owner == mount && s.seq <= through
}

// settled reports whether the page has nothing of a find left to end or to
// stop: no search runs and the engine is owed no stop. Every question of
// whether a page needs a find transition reads it.
func (s *findSession) settled() bool { return !s.active && !s.engine }

// disown ends the search's ownership with it.
func (s *findSession) disown() { s.active, s.owner, s.seq = false, "", 0 }

// findOwner is who a find action is for: a pane mount and a sequence, which
// a stop reads as its bound.
type findOwner struct {
	mount string
	seq   uint64
}

// findEnds are the ends submitted to a page and not yet applied. Bounded:
// a commit's end keeps only the newest epoch, and a presentation end
// rereads what is shown, so one of each is enough. Guarded by its own mu,
// which nothing holds while waiting.
type findEnds struct {
	mu      sync.Mutex
	epoch   uint64
	review  bool
	all     bool
	running bool
}

// identify stamps a state with the search's identity on page p.
func (s *findSession) identify(p *managedPage, state FindState) FindState {
	state.PageID = p.id
	state.Incarnation = p.incarnation
	state.Mount = s.owner
	state.Seq = s.seq
	state.Document = s.doc
	state.Session = s.gen
	return state
}

// snapshot is the search as a reply shows it.
func (s *findSession) snapshot(p *managedPage) FindState {
	if !s.active {
		return s.identify(p, FindState{Current: -1})
	}
	state := s.state
	state.Active = true
	return s.identify(p, state)
}

// foreign is the answer for an action that found no search of its owner's.
func (s *findSession) foreign(p *managedPage) FindState {
	return s.identify(p, FindState{Current: -1, Foreign: true})
}

func pageFinder(p *managedPage) (pageFind, string) {
	finder, ok := p.driver.(pageFind)
	if !ok {
		return nil, ""
	}
	return finder, finder.FindMode()
}

// findModeOf answers the find mode of a live page, "" for none.
func findModeOf(p *managedPage) string {
	if p == nil || p.driver == nil {
		return ""
	}
	_, mode := pageFinder(p)
	return mode
}

// findSeed is a selection usable as a search term: one line, bounded.
func findSeed(selection string) string {
	seed := strings.TrimSpace(selection)
	if seed == "" || strings.ContainsAny(seed, "\r\n") || utf8.RuneCountInString(seed) > maxFindSeedRunes {
		return ""
	}
	return seed
}

// Find runs one find action of a pane mount's on a page of the thread's
// companion. seq is the start's sequence, the sequence a step steps, or a
// stop's bound. start runs only on the shown active page; next, previous and
// stop never restore a suspended page, and stop answers for a page that is
// gone as for one with no search, still ending the mount's search on the
// thread's other pages.
func (m *Manager) Find(ctx context.Context, access Access, pageID, kind, query, mount string, seq uint64) (FindState, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case FindStart, FindNext, FindPrev, FindStop:
	default:
		return FindState{}, fmt.Errorf("browser: find action must be start, next, previous, or stop")
	}
	if len(query) > maxFindQueryBytes {
		return FindState{}, errFindQueryTooLong
	}
	if pageID = strings.TrimSpace(pageID); pageID == "" {
		return FindState{}, fmt.Errorf("browser: page id is required")
	}
	if mount = strings.TrimSpace(mount); mount == "" {
		return FindState{}, fmt.Errorf("browser: a find belongs to a mounted browser pane")
	}
	if seq == 0 {
		return FindState{}, fmt.Errorf("browser: a find action names its sequence")
	}
	owner := findOwner{mount: mount, seq: seq}
	// The action's deadline runs from before the first wait for a page lock,
	// so an action that waited past it, or whose caller went, changes
	// nothing.
	actionCtx, cancel := context.WithTimeout(ctx, m.findTimeout)
	defer cancel()
	if kind == FindStop {
		return m.stopFind(actionCtx, access, pageID, owner)
	}
	var p *managedPage
	var err error
	if kind == FindStart {
		p, _, err = m.resolvePage(ctx, access, pageID, false)
	} else {
		p, _, err = m.lookupOwnedPage(access, pageID)
	}
	if err != nil {
		return FindState{}, err
	}
	finder, mode := pageFinder(p)
	if mode == "" {
		return FindState{}, errFindUnavailable
	}
	return m.findTransition(actionCtx, p, kind, func(opCtx context.Context, _ error) (FindState, error) {
		if kind == FindStart {
			return m.startFind(opCtx, access, p, finder, mode, query, owner)
		}
		return m.stepFind(opCtx, p, finder, mode, kind == FindPrev, owner)
	})
}

// findTransition runs one find action's transition on p under its lock,
// within the action's deadline (actionCtx). A page that closed or suspended
// before its lock was taken answers errPageNotFound and its driver is not
// called. run gets the transition's context and the error of the owed engine
// stop the transition settled first.
func (m *Manager) findTransition(actionCtx context.Context, p *managedPage, kind string, run func(opCtx context.Context, settled error) (FindState, error)) (FindState, error) {
	opCtx, cancel := operationContext(actionCtx, p.ctx, m.findTimeout)
	defer cancel()
	if hook := m.findWaitHook.Load(); hook != nil {
		(*hook)(opCtx)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !m.pageLive(p) {
		return FindState{}, errPageNotFound
	}
	// The action's own context is read too: opCtx learns of its end
	// asynchronously.
	if err := cmp.Or(actionCtx.Err(), opCtx.Err()); err != nil {
		return FindState{}, fmt.Errorf("browser: find %s: %w", kind, err)
	}
	settled := m.beginFindLocked(opCtx, p)
	defer m.endFindTransitionLocked(opCtx, p)
	return run(opCtx, settled)
}

// stopFind ends the mount's search at or below the stop's bound on the named
// page and on every other page of the thread holding one, and answers for
// the named page. A page that is gone answers as one with no search.
func (m *Manager) stopFind(actionCtx context.Context, access Access, pageID string, owner findOwner) (FindState, error) {
	answer := FindState{PageID: pageID, Current: -1, Foreign: true}
	named, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil && !errors.Is(err, errPageNotFound) {
		return FindState{}, err
	}
	if named != nil {
		if _, mode := pageFinder(named); mode == "" {
			return FindState{}, errFindUnavailable
		}
	}
	var errs []error
	for _, p := range m.findStopPages(access.ThreadID, named, owner) {
		finder, mode := pageFinder(p)
		if mode == "" {
			continue
		}
		state, err := m.findTransition(actionCtx, p, FindStop, func(opCtx context.Context, settled error) (FindState, error) {
			state, err := m.stopFindLocked(opCtx, p, finder, owner)
			if err == nil && settled != nil {
				// A stop the engine was owed and refused again: the stop
				// asked for it, so it hears so.
				err = fmt.Errorf("browser: stop find: %w", settled)
			}
			return state, err
		})
		if errors.Is(err, errPageNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
		}
		if p == named && (err == nil || state.PageID != "") {
			answer = state
		}
	}
	return answer, errors.Join(errs...)
}

// findStopPages are the pages a stop runs on: the named page, if it exists,
// and every other live page of the thread whose search the stop ends.
func (m *Manager) findStopPages(threadID string, named *managedPage, owner findOwner) []*managedPage {
	var pages []*managedPage
	if named != nil {
		pages = append(pages, named)
	}
	m.mu.Lock()
	var others []*managedPage
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID && p != named {
				others = append(others, p)
			}
		}
	}
	m.mu.Unlock()
	for _, p := range others {
		p.findMu.Lock()
		held := p.find.heldThrough(owner.mount, owner.seq)
		p.findMu.Unlock()
		if held {
			pages = append(pages, p)
		}
	}
	return pages
}

// pageLive reports whether p is still the live page registered under its id.
// The caller holds p.mu, which closing and suspending a page take before
// releasing its driver.
func (m *Manager) pageLive(p *managedPage) bool {
	if p.driver.Lifetime().Err() != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	live, _ := m.livePageLocked(p.id)
	return live == p
}

// pageShown reports whether pageID is the active page of the thread's shown
// companion with a pane mounted to show it.
func (m *Manager) pageShown(threadID, pageID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[threadID]
	return ok && session.Visible && session.ActivePageID == pageID && m.paneMountedLocked(threadID)
}

// paneMountedLocked reports whether a pane is mounted for the thread, painted
// or not: an occluded pane still shows its find, and no pane shows none. The
// caller holds m.mu.
func (m *Manager) paneMountedLocked(threadID string) bool {
	for _, mount := range m.panes {
		if mount.threadID == threadID {
			return true
		}
	}
	return false
}

// mountOf reports whether mount is a pane mount of the thread's.
func (m *Manager) mountOf(threadID, mount string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pane, ok := m.panes[mount]
	return ok && pane.threadID == threadID
}

var (
	errFindNotShown = errors.New("browser: find runs on the page the browser pane shows")
	errFindNoMount  = errors.New("browser: find runs in a mounted browser pane")
	errFindStale    = errors.New("browser: a find start's sequence must be above every start its pane mount made before")
)

// admitFindStart takes owner's start on the thread's page, or refuses it: the
// mount must be the thread's, the start's sequence above every start the
// mount made before, and the page the shown active one. A start it takes
// raises the mount's sequence.
func (m *Manager) admitFindStart(threadID, pageID string, owner findOwner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pane, ok := m.panes[owner.mount]
	if !ok || pane.threadID != threadID {
		return errFindNoMount
	}
	if owner.seq <= pane.findSeq {
		return fmt.Errorf("%w: %d after %d", errFindStale, owner.seq, pane.findSeq)
	}
	session, ok := m.sessions[threadID]
	if !ok || !session.Visible || session.ActivePageID != pageID {
		return errFindNotShown
	}
	pane.findSeq = owner.seq
	m.panes[owner.mount] = pane
	return nil
}

// beginFindLocked opens a transition: it applies the ends submitted before
// it and settles a stop the engine is owed, answering that stop's error.
// The caller holds p.mu.
func (m *Manager) beginFindLocked(ctx context.Context, p *managedPage) error {
	p.findBusy.Store(true)
	return m.settleFindLocked(ctx, p)
}

// endFindTransitionLocked closes a transition: the ends submitted during it
// are applied after it, and a search it left the engine holding unwanted is
// stopped. The caller holds p.mu.
func (m *Manager) endFindTransitionLocked(ctx context.Context, p *managedPage) {
	_ = m.settleFindLocked(ctx, p)
	p.findBusy.Store(false)
}

// findPoint is a test seam: it runs at each point inside a transition where
// an end could once land between a check and what follows it.
func (m *Manager) findPoint(point string) {
	if hook := m.findPointHook.Load(); hook != nil {
		(*hook)(point)
	}
}

// startFind replaces p's search with owner's. In FindModeBar it searches for
// query, and an empty query is a stop bounded by the start's sequence; in
// FindModeEngine it opens the engine's own bar seeded with query or else the
// page's selection. A start admitFindStart refuses changes nothing; another
// mount's search on the page is then ended and announced to it. The caller
// holds p.mu.
func (m *Manager) startFind(ctx context.Context, access Access, p *managedPage, finder pageFind, mode, query string, owner findOwner) (FindState, error) {
	if mode == FindModeBar && query == "" {
		return m.stopFindLocked(ctx, p, finder, owner)
	}
	if err := m.admitFindStart(access.ThreadID, p.id, owner); err != nil {
		return FindState{}, err
	}
	m.findPoint("checked")
	p.findMu.Lock()
	// The mount's own earlier search is replaced without an end: its
	// provider made this start, and no bar shows the earlier one.
	var taken *FindState
	if p.find.active && p.find.owner != owner.mount {
		state := m.endLocked(p)
		taken = &state
	}
	p.find.gen = m.findGen.Add(1)
	p.find.owner, p.find.seq = owner.mount, owner.seq
	p.find.doc = p.docEpoch.Load()
	p.findMu.Unlock()
	if taken != nil {
		m.emit(CompanionEvent{Kind: "find", ThreadID: p.owner, PageID: p.id, Find: taken})
	}
	if mode == FindModeEngine && query == "" {
		query = findSeed(p.driver.SelectionText(ctx))
	}
	m.findPoint("engine")
	result, err := finder.FindStart(ctx, query)
	p.findMu.Lock()
	p.find.engine = true
	if err != nil {
		p.find.disown()
		p.findMu.Unlock()
		// What the engine kept of a failed start is cleared with it.
		_ = m.clearEngineFind(ctx, p, finder)
		return FindState{}, err
	}
	defer p.findMu.Unlock()
	p.find.active, p.find.mode = true, mode
	state := FindState{Query: query, Current: -1}
	if mode == FindModeBar {
		state.Found = result.Found
		state.Matches = result.Matches
		state.Capped = result.Capped
		if !result.Found {
			state.Matches, state.Capped = 0, false
		} else if result.Matches > 0 {
			state.Current = 0
		}
	}
	p.find.state = state
	return p.find.snapshot(p), nil
}

// stepFind moves the search's current match with a step's answer. The engine
// wraps; past a capped count the position is unknown, so it is not wrapped
// here either. A step for a search that is not the owner's at the step's
// sequence answers Foreign. The caller holds p.mu.
func (m *Manager) stepFind(ctx context.Context, p *managedPage, finder pageFind, mode string, backwards bool, owner findOwner) (FindState, error) {
	if mode == FindModeEngine {
		return FindState{}, fmt.Errorf("browser: this engine steps through matches in its own find bar")
	}
	p.findMu.Lock()
	if !p.find.ownedBy(owner) {
		state := p.find.foreign(p)
		p.findMu.Unlock()
		return state, nil
	}
	p.findMu.Unlock()
	m.findPoint("checked")
	m.findPoint("engine")
	result, err := finder.FindStep(ctx, backwards)
	if err != nil {
		// What the engine still shows is unknown: it is cleared, and the
		// next start begins again.
		p.findMu.Lock()
		p.find.gen = m.findGen.Add(1)
		p.find.disown()
		p.findMu.Unlock()
		_ = m.clearEngineFind(ctx, p, finder)
		return FindState{}, err
	}
	p.findMu.Lock()
	defer p.findMu.Unlock()
	state := &p.find.state
	state.Found, state.Capped = result.Found, result.Capped && result.Found
	state.Matches = result.Matches
	switch {
	case !result.Found:
		state.Matches, state.Current = 0, -1
	case result.Matches <= 0 || state.Current < 0:
		state.Current = -1
	case backwards && state.Current == 0:
		state.Current = result.Matches - 1
		if result.Capped {
			state.Current = -1
		}
	case backwards:
		state.Current = min(state.Current-1, result.Matches-1)
	case state.Current+1 >= result.Matches:
		state.Current = 0
		if result.Capped {
			state.Current = -1
		}
	default:
		state.Current++
	}
	return p.find.snapshot(p), nil
}

// stopFindLocked ends the mount's search on p if its sequence is at or
// below the stop's bound (owner.seq). Another owner's search, a later one of
// the mount's, or none, is left alone and answered Foreign. The caller holds
// p.mu.
func (m *Manager) stopFindLocked(ctx context.Context, p *managedPage, finder pageFind, owner findOwner) (FindState, error) {
	p.findMu.Lock()
	if !p.find.heldThrough(owner.mount, owner.seq) {
		// Not a search this stop ends: nothing is ended, and the answer
		// says whose it is.
		state := p.find.foreign(p)
		p.findMu.Unlock()
		return state, nil
	}
	p.findMu.Unlock()
	m.findPoint("checked")
	p.findMu.Lock()
	p.find.gen = m.findGen.Add(1)
	state := p.find.identify(p, FindState{Current: -1})
	p.find.disown()
	owed := p.find.engine
	p.findMu.Unlock()
	if !owed {
		return state, nil
	}
	if err := m.clearEngineFind(ctx, p, finder); err != nil {
		return state, fmt.Errorf("browser: stop find: %w", err)
	}
	return state, nil
}

// clearEngineFind stops the engine's search and records that it holds none,
// or on failure that the stop is still owed. The caller holds p.mu.
func (m *Manager) clearEngineFind(ctx context.Context, p *managedPage, finder pageFind) error {
	err := finder.FindStop(ctx)
	p.findMu.Lock()
	p.find.engine = err != nil
	p.findMu.Unlock()
	if err != nil && p.ctx.Err() == nil {
		log.Printf("browser: page %s: stop find: %v", p.id, err)
	}
	return err
}

// endLocked ends p's search and answers the end as its owner should see
// it. The caller holds p.mu and p.findMu.
func (m *Manager) endLocked(p *managedPage) FindState {
	p.find.gen = m.findGen.Add(1)
	state := p.find.identify(p, FindState{Current: -1})
	p.find.disown()
	return state
}

// settleFindLocked applies the ends submitted to p, announces the end, and
// makes a stop the engine is owed. A page that is no longer live ends its
// search and asks its closed driver nothing. The caller holds p.mu.
func (m *Manager) settleFindLocked(ctx context.Context, p *managedPage) error {
	p.ends.mu.Lock()
	epoch, review, all := p.ends.epoch, p.ends.review, p.ends.all
	p.ends.epoch, p.ends.review, p.ends.all = 0, false, false
	p.ends.mu.Unlock()
	live := m.pageLive(p)
	p.findMu.Lock()
	ends := false
	if p.find.active {
		switch {
		case !live || all:
			ends = true
		case epoch != 0 && p.find.doc < epoch:
			ends = true
		case review:
			ends = !m.pageShown(p.owner, p.id) || !m.mountOf(p.owner, p.find.owner)
		}
	}
	var state FindState
	if ends {
		state = m.endLocked(p)
	}
	owed := live && !p.find.active && !p.find.settled()
	p.findMu.Unlock()
	if ends {
		m.emit(CompanionEvent{Kind: "find", ThreadID: p.owner, PageID: p.id, Find: &state})
	}
	if !owed {
		return nil
	}
	finder, mode := pageFinder(p)
	if mode == "" {
		return nil
	}
	return m.clearEngineFind(ctx, p, finder)
}

// endFindLocked ends p's search at once, for a caller closing or suspending
// the page under its lock. Those callers close the driver first, so the
// settle asks the engine nothing; it is bounded like every other one. The
// caller holds p.mu.
func (m *Manager) endFindLocked(p *managedPage) {
	p.ends.mu.Lock()
	p.ends.all = true
	p.ends.mu.Unlock()
	ctx, cancel := operationContext(context.Background(), p.ctx, m.findTimeout)
	defer cancel()
	_ = m.settleFindLocked(ctx, p)
}

// submitFindEnd hands p an end to apply, without waiting: the running
// transition applies it when it finishes, or one of its own does. Safe on
// any thread, the engine's UI thread included. Once Close has begun to join
// the find work, an end is no longer applied: the browser's close ended
// every search before it.
func (m *Manager) submitFindEnd(p *managedPage, set func(*findEnds)) {
	p.ends.mu.Lock()
	set(&p.ends)
	start := !p.ends.running && m.admitFindWork()
	if start {
		p.ends.running = true
	}
	p.ends.mu.Unlock()
	if start {
		go m.applyFindEnds(p)
	}
}

// admitFindWork counts one goroutine of find work for Close to join, or
// answers false once Close has begun joining it.
func (m *Manager) admitFindWork() bool {
	m.findGate.Lock()
	defer m.findGate.Unlock()
	if m.findClosed {
		return false
	}
	m.findWork.Add(1)
	return true
}

// applyFindEnds is a transition of its own for submitted ends, repeated
// until none is left.
func (m *Manager) applyFindEnds(p *managedPage) {
	defer m.findWork.Done()
	for {
		p.mu.Lock()
		ctx, cancel := operationContext(context.Background(), p.ctx, m.findTimeout)
		m.findPoint("apply")
		_ = m.settleFindLocked(ctx, p)
		cancel()
		p.mu.Unlock()
		p.ends.mu.Lock()
		if p.ends.epoch == 0 && !p.ends.review && !p.ends.all {
			p.ends.running = false
			p.ends.mu.Unlock()
			return
		}
		p.ends.mu.Unlock()
	}
}

// endFind ends p's search on the Manager's own account: the page closed
// without its lock held by the caller, or the browser closed.
func (m *Manager) endFind(p *managedPage) {
	m.submitFindEnd(p, func(e *findEnds) { e.all = true })
}

// documentCommitted is pageHooks.DocumentCommitted. It runs where the engine
// reports the commit and only stamps the new document epoch, so an action
// that reads the epoch after the commit is bound to the new document. The
// work it returns submits the end of a search started on an earlier
// document; one started on this document or a later one is not its to end.
func (m *Manager) documentCommitted(p *managedPage) func() {
	epoch := p.docEpoch.Add(1)
	return func() {
		m.submitFindEnd(p, func(e *findEnds) { e.epoch = max(e.epoch, epoch) })
	}
}

// endHiddenFinds submits to each of the thread's live pages that holds a
// search or owes the engine a stop, or is in a transition, an end of a
// search not on the shown active page or whose owning pane mount is gone,
// and with it the owed stop. What is shown is read when the end is applied.
// A settled page outside a transition is skipped: a transition starting
// after the change reads it in its own checks.
func (m *Manager) endHiddenFinds(pages []*managedPage) {
	for _, p := range pages {
		p.findMu.Lock()
		settled := p.find.settled()
		p.findMu.Unlock()
		if settled && !p.findBusy.Load() {
			continue
		}
		m.submitFindEnd(p, func(e *findEnds) { e.review = true })
	}
}

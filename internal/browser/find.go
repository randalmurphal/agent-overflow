package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/keybindings"
)

// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// engine searches; the Manager owns the page's find: AO's find bar open on
// it, the running search with its query and current match, and when they
// end. They end when the page commits another document, stops being the
// shown active page, is suspended or closes, or when the pane mount that
// owns them goes; that end is announced as a Kind "find" event so the bar
// showing it closes.
//
// A find's identity is its page (id and incarnation), the pane mount that
// owns it, the document it was opened on, and a generation. Every action,
// answer and end carries all four, and an end, a close or a claim applies
// only to the find whose identity it names: another mount's close, a
// commit older than the find's document, an answer older than an end, or a
// page that is not the same incarnation leaves it alone.
//
// An answer for an action that changed nothing because its find is gone or
// another mount's is marked Foreign and names the find's real owner, "" for
// none: it acknowledges nothing of the asking find's.
//
// Generations come from one counter across the Manager, so their order
// holds across a page's suspend and restore. An engine call runs with the
// page lock held but not the find lock, so an end can land while it runs:
// the call's answer is published only if no end moved the page's
// generation meanwhile, and otherwise the engine's search is stopped again.
//
// Whether the engine holds a search (its highlights, its match selection,
// its own bar) is its own fact (findSession.engine), apart from the
// logical session: an end only owes the engine a stop, and that stop is
// owed until an engine call really clears or replaces the search, whatever
// bookkeeping runs between.

const (
	// maxFindQueryBytes bounds a query. A search term is a phrase; anything
	// longer is not one.
	maxFindQueryBytes = 1024
	// maxFindSeedRunes bounds a selection that seeds the engine's own bar,
	// the rule the SPA's find bar applies to its seed.
	maxFindSeedRunes = 200
	// findTimeout bounds one engine find call.
	findTimeout = 10 * time.Second
)

// findEscape is the key that closes AO's find bar from inside the page:
// engines offer it to keyChord without a modifier.
var findEscape = keybindings.Accelerator{Key: "escape"}

var errFindUnavailable = errors.New("browser: find is not available on this page's browser engine")

// The find actions (Find's kind).
const (
	// FindOpen opens AO's find bar on the page for a pane mount, which
	// claims the page's Escape for `find.close`. FindModeBar only.
	FindOpen = "open"
	// FindStart replaces the search with one for the query; an empty query
	// ends the search and leaves the bar open. In FindModeBar it searches in
	// the mount's open bar, and changes nothing without one. In
	// FindModeEngine it opens the engine's own bar.
	FindStart = "start"
	FindNext  = "next"
	FindPrev  = "previous"
	// FindStop ends the search and leaves the bar open.
	FindStop = "stop"
	// FindClose ends the search and closes the bar.
	FindClose = "close"
)

// findSession is one page's find state. Guarded by managedPage.findMu.
type findSession struct {
	// gen is the generation of the page's newest find action or end.
	gen uint64
	// owner is the pane mount the find belongs to: its bar and its search,
	// and an action in flight. "" when the page has none.
	owner string
	// doc is the document epoch the find was opened on.
	doc uint64
	// bar is whether AO's find bar is open; starting whether an action is
	// in its engine call.
	bar      bool
	starting bool
	// active is whether a search runs; search is its generation.
	active bool
	search uint64
	// engine is whether the engine may hold a search AO has not cleared:
	// set by every FindStart, cleared only by a FindStop that succeeded.
	// Written with the page lock held.
	engine bool
	mode   string
	state  FindState
}

func (s *findSession) idle() bool { return !s.active && !s.bar && !s.starting }

// identify stamps a state with the find's identity on page p.
func (s *findSession) identify(p *managedPage, state FindState) FindState {
	state.PageID = p.id
	state.Incarnation = p.incarnation
	state.Mount = s.owner
	state.Document = s.doc
	return state
}

// snapshot is the find as a reply shows it.
func (s *findSession) snapshot(p *managedPage) FindState {
	if !s.active {
		return s.identify(p, FindState{Session: s.gen, Current: -1})
	}
	state := s.state
	state.Session = s.search
	state.Active = true
	return s.identify(p, state)
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
// companion. open and start run only on the shown active page. stop and
// close never restore a suspended page, answer for a page that is gone as
// for one with no find, and leave alone a find another mount owns.
func (m *Manager) Find(ctx context.Context, access Access, pageID, kind, query, mount string) (FindState, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case FindOpen, FindStart, FindNext, FindPrev, FindStop, FindClose:
	default:
		return FindState{}, fmt.Errorf("browser: find action must be open, start, next, previous, stop, or close")
	}
	if len(query) > maxFindQueryBytes {
		return FindState{}, fmt.Errorf("browser: find query exceeds %d bytes", maxFindQueryBytes)
	}
	if pageID = strings.TrimSpace(pageID); pageID == "" {
		return FindState{}, fmt.Errorf("browser: page id is required")
	}
	if mount = strings.TrimSpace(mount); mount == "" {
		return FindState{}, fmt.Errorf("browser: a find belongs to a mounted browser pane")
	}
	ending := kind == FindStop || kind == FindClose
	gone := FindState{PageID: pageID, Current: -1, Foreign: true}
	var p *managedPage
	var err error
	if kind == FindOpen || kind == FindStart {
		p, _, err = m.resolvePage(ctx, access, pageID, false)
	} else {
		p, _, err = m.lookupOwnedPage(access, pageID)
	}
	if ending && errors.Is(err, errPageNotFound) {
		return gone, nil
	}
	if err != nil {
		return FindState{}, err
	}
	finder, mode := pageFinder(p)
	if mode == "" {
		return FindState{}, errFindUnavailable
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Resolved, the page could have closed or suspended before its lock was
	// taken; its driver is not called then.
	if !m.pageLive(p) {
		if ending {
			return gone, nil
		}
		return FindState{}, errPageNotFound
	}
	opCtx, cancel := operationContext(ctx, p.ctx, findTimeout)
	defer cancel()
	switch kind {
	case FindOpen:
		return m.openFindBar(opCtx, access, p, finder, mode, mount)
	case FindStart:
		return m.startFind(opCtx, access, p, finder, mode, query, mount)
	case FindStop, FindClose:
		return m.stopFindLocked(opCtx, p, finder, kind == FindClose, mount)
	}
	return m.stepFind(opCtx, p, finder, mode, kind == FindPrev, mount)
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
	errFindNoMount  = errors.New("browser: find opens in a mounted browser pane")
)

// reserveFind makes mount the owner of p's find for an action and takes the
// action's generation and document. Another mount's find is ended first,
// announced to it, and its search stopped. The owner is recorded before the
// mount and the presentation are checked, so a detach or a presentation
// change that lands after the check finds the owner and ends the action,
// which publish then sees. The caller holds p.mu.
func (m *Manager) reserveFind(ctx context.Context, access Access, p *managedPage, finder pageFind, mount string) (uint64, error) {
	// A mount that is already gone takes nothing over.
	if !m.mountOf(access.ThreadID, mount) {
		return 0, errFindNoMount
	}
	p.findMu.Lock()
	var taken *FindState
	owed := false
	if !p.find.idle() && p.find.owner != mount {
		state := m.endLocked(p)
		taken = &state
		owed = p.find.engine
	}
	p.find.gen = m.findGen.Add(1)
	mine := p.find.gen
	p.find.owner = mount
	p.find.doc = p.docEpoch.Load()
	p.find.starting = true
	p.findMu.Unlock()
	if taken != nil {
		m.emit(CompanionEvent{Kind: "find", ThreadID: p.owner, PageID: p.id, Find: taken})
		if owed {
			_ = m.clearEngineFind(ctx, p, finder)
		}
	}
	var refused error
	switch {
	case !m.mountOf(access.ThreadID, mount):
		refused = errFindNoMount
	case !m.pageShown(access.ThreadID, p.id):
		refused = errFindNotShown
	}
	if refused != nil {
		p.findMu.Lock()
		if p.find.gen == mine {
			p.find.starting = false
			if !p.find.bar && !p.find.active {
				p.find.owner = ""
			}
		}
		p.findMu.Unlock()
		return 0, refused
	}
	if m.findReserved != nil {
		m.findReserved()
	}
	return mine, nil
}

// openFindBar records AO's bar open on p for mount and claims the page's
// Escape. The caller holds p.mu.
func (m *Manager) openFindBar(ctx context.Context, access Access, p *managedPage, finder pageFind, mode, mount string) (FindState, error) {
	if mode != FindModeBar {
		return FindState{}, fmt.Errorf("browser: this engine shows its own find bar")
	}
	mine, err := m.reserveFind(ctx, access, p, finder, mount)
	if err != nil {
		return FindState{}, err
	}
	p.findMu.Lock()
	defer p.findMu.Unlock()
	// An end since the reservation (the mount detached, the page hid) wins.
	if p.find.gen != mine {
		return FindState{}, errFindNotShown
	}
	p.find.starting = false
	p.find.bar = true
	m.finding.Store(p.driver.Handle(), struct{}{})
	state := p.find.snapshot(p)
	state.Session = mine
	return state, nil
}

// startFind replaces p's search. In FindModeBar it searches for query, and
// an empty query ends the search; in FindModeEngine it opens the engine's
// own bar seeded with query or else the page's selection. The caller holds
// p.mu.
func (m *Manager) startFind(ctx context.Context, access Access, p *managedPage, finder pageFind, mode, query, mount string) (FindState, error) {
	if mode == FindModeBar && query == "" {
		return m.stopFindLocked(ctx, p, finder, false, mount)
	}
	if mode == FindModeBar {
		// The search lives in the mount's bar. A start that waited out the
		// bar's end, or another mount's takeover, does not bring a search
		// back without one.
		p.findMu.Lock()
		barless := p.find.owner != mount || !p.find.bar
		state := p.find.identify(p, FindState{Session: p.find.gen, Current: -1, Foreign: true})
		p.findMu.Unlock()
		if barless {
			return state, nil
		}
	}
	mine, err := m.reserveFind(ctx, access, p, finder, mount)
	if err != nil {
		return FindState{}, err
	}
	if mode == FindModeEngine && query == "" {
		query = findSeed(p.driver.SelectionText(ctx))
	}
	result, err := finder.FindStart(ctx, query)
	p.findMu.Lock()
	p.find.engine = true
	if p.find.gen != mine {
		// An end landed during the call: the search it started is nobody's.
		state := p.find.identify(p, FindState{Session: mine, Current: -1})
		state.Mount = mount
		p.findMu.Unlock()
		_ = m.clearEngineFind(ctx, p, finder)
		return state, nil
	}
	p.find.starting = false
	if err != nil {
		p.find.active = false
		if !p.find.bar {
			p.find.owner = ""
		}
		p.findMu.Unlock()
		// What the engine kept of a failed start is cleared with it.
		_ = m.clearEngineFind(ctx, p, finder)
		return FindState{}, err
	}
	defer p.findMu.Unlock()
	p.find.active, p.find.search, p.find.mode = true, mine, mode
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
// here either. The caller holds p.mu.
func (m *Manager) stepFind(ctx context.Context, p *managedPage, finder pageFind, mode string, backwards bool, mount string) (FindState, error) {
	if mode == FindModeEngine {
		return FindState{}, fmt.Errorf("browser: this engine steps through matches in its own find bar")
	}
	p.findMu.Lock()
	active, search := p.find.active && p.find.owner == mount, p.find.search
	p.findMu.Unlock()
	if !active {
		return FindState{}, fmt.Errorf("browser: no find is in progress on this page")
	}
	result, err := finder.FindStep(ctx, backwards)
	p.findMu.Lock()
	defer p.findMu.Unlock()
	if !p.find.active || p.find.search != search {
		state := p.find.identify(p, FindState{Session: search, Current: -1})
		state.Mount = mount
		return state, nil
	}
	if err != nil {
		// What the engine still shows is unknown: it is cleared, and the
		// next action starts over.
		p.find.active = false
		p.findMu.Unlock()
		_ = m.clearEngineFind(ctx, p, finder)
		p.findMu.Lock()
		return FindState{}, err
	}
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

// stopFindLocked is mount's end of its search, and with closeBar of its bar
// too. A find another mount owns is left alone, and ending what already
// ended answers the same. The caller holds p.mu.
func (m *Manager) stopFindLocked(ctx context.Context, p *managedPage, finder pageFind, closeBar bool, mount string) (FindState, error) {
	p.findMu.Lock()
	if p.find.owner != mount {
		// Not this mount's find: nothing is ended, and the answer says
		// whose it is.
		state := p.find.identify(p, FindState{Session: p.find.gen, Current: -1, Foreign: true})
		p.findMu.Unlock()
		return state, nil
	}
	p.find.gen = m.findGen.Add(1)
	p.find.active = false
	if closeBar {
		p.find.bar = false
		m.finding.Delete(p.driver.Handle())
	}
	state := p.find.snapshot(p)
	if !p.find.bar && !p.find.starting {
		p.find.owner = ""
	}
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

// endLocked ends p's find: the search, the bar and any action in flight, and
// answers the end as its owner should see it. The caller holds p.findMu.
func (m *Manager) endLocked(p *managedPage) FindState {
	p.find.gen = m.findGen.Add(1)
	state := p.find.identify(p, FindState{Session: p.find.gen, Current: -1})
	p.find.active, p.find.bar, p.find.starting = false, false, false
	p.find.owner = ""
	m.finding.Delete(p.driver.Handle())
	return state
}

// endFind ends p's find on the Manager's own account, and announces it.
// stopEngine asks the engine to clear a search left on a page that lives on;
// the call queues behind the page's in-flight operation, off the caller's
// goroutine, and is skipped when a newer action ran first. Callers may hold
// p.mu.
func (m *Manager) endFind(p *managedPage, stopEngine bool) {
	m.endFindIf(p, stopEngine, nil)
}

// endFindIf is endFind when ends answers true for the page's find, nil for
// always.
func (m *Manager) endFindIf(p *managedPage, stopEngine bool, ends func(*findSession) bool) {
	p.findMu.Lock()
	if p.find.idle() || (ends != nil && !ends(&p.find)) {
		p.findMu.Unlock()
		return
	}
	owed := p.find.engine
	state := m.endLocked(p)
	p.findMu.Unlock()
	m.emit(CompanionEvent{Kind: "find", ThreadID: p.owner, PageID: p.id, Find: &state})
	if stopEngine && owed {
		stop := func() { m.stopEngineFind(p) }
		if m.deferEngineStop != nil {
			m.deferEngineStop(stop)
			return
		}
		go stop()
	}
}

// stopEngineFind makes the stop an end owed the engine, unless a search
// started since has replaced the one it owed or the stop was made already.
func (m *Manager) stopEngineFind(p *managedPage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !m.pageLive(p) {
		return
	}
	p.findMu.Lock()
	owed := p.find.engine && !p.find.active
	p.findMu.Unlock()
	if !owed {
		return
	}
	finder, mode := pageFinder(p)
	if mode == "" {
		return
	}
	ctx, cancel := operationContext(context.Background(), p.ctx, findTimeout)
	defer cancel()
	_ = m.clearEngineFind(ctx, p, finder)
}

// documentCommitted is pageHooks.DocumentCommitted. It runs where the engine
// reports the commit and only stamps the new document epoch, so an action
// dispatched after the commit is bound to the new document. The work it
// returns ends a find opened on an earlier document, whenever it runs; a
// find opened on this document or a later one is not its to end.
func (m *Manager) documentCommitted(p *managedPage) func() {
	epoch := p.docEpoch.Add(1)
	return func() {
		m.endFindIf(p, true, func(s *findSession) bool { return s.doc < epoch })
	}
}

// endHiddenFinds ends the finds of the thread's pages that are not the shown
// active page or whose owning pane mount is gone. pages are the thread's
// live pages. Whether a find is shown is read when its end is decided, not
// from the caller's snapshot: a find opened after the snapshot was taken is
// judged by what is true now, and one opened before the change that called
// this is seen by it.
func (m *Manager) endHiddenFinds(threadID string, pages []*managedPage) {
	for _, p := range pages {
		m.endFindIf(p, true, func(s *findSession) bool {
			return !m.pageShown(threadID, p.id) || !m.mountOf(threadID, s.owner)
		})
	}
}

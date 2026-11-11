package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
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
// Every find transition of a page holds the page lock (managedPage.mu, the
// lock every engine call already takes) from its precondition check through
// its engine calls to the state it publishes: open, start, step, stop,
// close, every end, and the engine's cleanup. An end raised where waiting
// is not allowed (the engine's UI thread, a presentation change, a mount
// detaching) is submitted to the page (findEnds) and applied by the next
// transition, or by a transition of its own when none is running; each
// transition applies what was submitted before it first and what was
// submitted during it last, so an end lands strictly before or strictly
// after any action. A commit's end is bound to the document epoch stamped
// where the engine reported it; a presentation end rereads what is shown
// when it is applied. Ends submitted while a long page operation holds the
// lock wait for it: the bar and the page's Escape claim then outlive the
// document or the presentation by that long, and nothing else is out of
// step.
//
// Whether the engine holds a search AO no longer wants (findSession.engine
// with no active session) is state, not a job: every transition settles it
// first, retrying a stop that failed before, whoever owns the bar.
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

// findSession is one page's find state. Written only by a transition, which
// holds managedPage.mu; managedPage.findMu guards it for readers.
type findSession struct {
	// gen is the generation of the page's newest find action or end.
	gen uint64
	// owner is the pane mount the find belongs to: its bar and its search.
	// "" when the page has none.
	owner string
	// doc is the document epoch the find was opened on.
	doc uint64
	// bar is whether AO's find bar is open.
	bar bool
	// active is whether a search runs; search is its generation.
	active bool
	search uint64
	// engine is whether the engine may hold a search AO has not cleared:
	// set by every FindStart, cleared only by a FindStop that succeeded.
	engine bool
	mode   string
	state  FindState
}

func (s *findSession) idle() bool { return !s.active && !s.bar }

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

// foreign is the answer for an action that found no find of its mount's.
func (s *findSession) foreign(p *managedPage) FindState {
	return s.identify(p, FindState{Session: s.gen, Current: -1, Foreign: true})
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
	settled := m.beginFindLocked(opCtx, p)
	defer m.endFindTransitionLocked(opCtx, p)
	switch kind {
	case FindOpen:
		return m.openFindBar(opCtx, access, p, finder, mode, mount)
	case FindStart:
		return m.startFind(opCtx, access, p, finder, mode, query, mount)
	case FindStop, FindClose:
		state, err := m.stopFindLocked(opCtx, p, finder, kind == FindClose, mount)
		if err == nil && settled != nil {
			// A stop the engine was owed and refused again: the stop or close
			// asked for it, so it hears so.
			err = fmt.Errorf("browser: stop find: %w", settled)
		}
		return state, err
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

// beginFindLocked opens a transition: it applies the ends submitted before
// it and settles a stop the engine is owed, answering that stop's error.
// The caller holds p.mu.
func (m *Manager) beginFindLocked(ctx context.Context, p *managedPage) error {
	p.findBusy.Store(true)
	return m.settleFindLocked(ctx, p)
}

// endFindTransitionLocked closes a transition: the ends submitted during it
// are applied after it. The caller holds p.mu.
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

// takeFind makes mount the owner of p's find for an action and answers the
// action's generation. A mount that is gone, or a page not shown, is
// refused before anything changes; another mount's find is then ended,
// announced to it, and its search stopped. The caller holds p.mu.
func (m *Manager) takeFind(ctx context.Context, access Access, p *managedPage, finder pageFind, mount string) (uint64, error) {
	if !m.mountOf(access.ThreadID, mount) {
		return 0, errFindNoMount
	}
	if !m.pageShown(access.ThreadID, p.id) {
		return 0, errFindNotShown
	}
	m.findPoint("checked")
	p.findMu.Lock()
	var taken *FindState
	if !p.find.idle() && p.find.owner != mount {
		state := m.endLocked(p)
		taken = &state
	}
	p.find.gen = m.findGen.Add(1)
	p.find.owner = mount
	p.find.doc = p.docEpoch.Load()
	mine := p.find.gen
	owed := p.find.engine
	p.findMu.Unlock()
	if taken != nil {
		m.emit(CompanionEvent{Kind: "find", ThreadID: p.owner, PageID: p.id, Find: taken})
		if owed {
			_ = m.clearEngineFind(ctx, p, finder)
		}
	}
	return mine, nil
}

// openFindBar records AO's bar open on p for mount and claims the page's
// Escape. Opening searches nothing; the one engine call it can make is the
// stop of a find it takes over. The caller holds p.mu.
func (m *Manager) openFindBar(ctx context.Context, access Access, p *managedPage, finder pageFind, mode, mount string) (FindState, error) {
	if mode != FindModeBar {
		return FindState{}, fmt.Errorf("browser: this engine shows its own find bar")
	}
	mine, err := m.takeFind(ctx, access, p, finder, mount)
	if err != nil {
		return FindState{}, err
	}
	p.findMu.Lock()
	defer p.findMu.Unlock()
	p.find.bar = true
	m.finding.Store(p.driver.Handle(), struct{}{})
	state := p.find.snapshot(p)
	state.Session = mine
	return state, nil
}

// startFind replaces p's search. In FindModeBar it searches for query in
// mount's open bar, and an empty query ends the search; in FindModeEngine
// it opens the engine's own bar seeded with query or else the page's
// selection. The caller holds p.mu.
func (m *Manager) startFind(ctx context.Context, access Access, p *managedPage, finder pageFind, mode, query, mount string) (FindState, error) {
	if mode == FindModeBar && query == "" {
		return m.stopFindLocked(ctx, p, finder, false, mount)
	}
	if mode == FindModeBar {
		// The search lives in the mount's bar; without one it is not
		// brought back.
		p.findMu.Lock()
		barless := p.find.owner != mount || !p.find.bar
		state := p.find.foreign(p)
		p.findMu.Unlock()
		if barless {
			return state, nil
		}
	}
	mine, err := m.takeFind(ctx, access, p, finder, mount)
	if err != nil {
		return FindState{}, err
	}
	if mode == FindModeEngine && query == "" {
		query = findSeed(p.driver.SelectionText(ctx))
	}
	m.findPoint("engine")
	result, err := finder.FindStart(ctx, query)
	p.findMu.Lock()
	p.find.engine = true
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
// here either. A step for a find that is not the mount's answers Foreign;
// one for the mount's bar with no search answers it inactive. The caller
// holds p.mu.
func (m *Manager) stepFind(ctx context.Context, p *managedPage, finder pageFind, mode string, backwards bool, mount string) (FindState, error) {
	if mode == FindModeEngine {
		return FindState{}, fmt.Errorf("browser: this engine steps through matches in its own find bar")
	}
	p.findMu.Lock()
	if p.find.owner != mount {
		state := p.find.foreign(p)
		p.findMu.Unlock()
		return state, nil
	}
	if !p.find.active {
		state := p.find.snapshot(p)
		p.findMu.Unlock()
		return state, nil
	}
	p.findMu.Unlock()
	m.findPoint("checked")
	m.findPoint("engine")
	result, err := finder.FindStep(ctx, backwards)
	if err != nil {
		// What the engine still shows is unknown: it is cleared, and the
		// next action starts over.
		p.findMu.Lock()
		p.find.active = false
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

// stopFindLocked is mount's end of its search, and with closeBar of its bar
// too. A find another mount owns is left alone, and ending what already
// ended answers the same. The caller holds p.mu.
func (m *Manager) stopFindLocked(ctx context.Context, p *managedPage, finder pageFind, closeBar bool, mount string) (FindState, error) {
	p.findMu.Lock()
	if p.find.owner != mount {
		// Not this mount's find: nothing is ended, and the answer says
		// whose it is.
		state := p.find.foreign(p)
		p.findMu.Unlock()
		return state, nil
	}
	p.findMu.Unlock()
	m.findPoint("checked")
	p.findMu.Lock()
	p.find.gen = m.findGen.Add(1)
	p.find.active = false
	if closeBar {
		p.find.bar = false
		m.finding.Delete(p.driver.Handle())
	}
	state := p.find.snapshot(p)
	if !p.find.bar {
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

// endLocked ends p's find, its search and its bar, and answers the end as
// its owner should see it. The caller holds p.mu and p.findMu.
func (m *Manager) endLocked(p *managedPage) FindState {
	p.find.gen = m.findGen.Add(1)
	state := p.find.identify(p, FindState{Session: p.find.gen, Current: -1})
	p.find.active, p.find.bar = false, false
	p.find.owner = ""
	m.finding.Delete(p.driver.Handle())
	return state
}

// settleFindLocked applies the ends submitted to p, announces the end, and
// makes a stop the engine is owed. A page that is no longer live ends its
// find and asks its closed driver nothing. The caller holds p.mu.
func (m *Manager) settleFindLocked(ctx context.Context, p *managedPage) error {
	p.ends.mu.Lock()
	epoch, review, all := p.ends.epoch, p.ends.review, p.ends.all
	p.ends.epoch, p.ends.review, p.ends.all = 0, false, false
	p.ends.mu.Unlock()
	live := m.pageLive(p)
	p.findMu.Lock()
	ends := false
	if !p.find.idle() {
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
	owed := live && p.find.engine && !p.find.active
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

// endFindLocked ends p's find at once, for a caller closing or suspending
// the page under its lock. Those callers close the driver first, so the
// settle asks the engine nothing; it is bounded like every other one. The
// caller holds p.mu.
func (m *Manager) endFindLocked(p *managedPage) {
	p.ends.mu.Lock()
	p.ends.all = true
	p.ends.mu.Unlock()
	ctx, cancel := operationContext(context.Background(), p.ctx, findTimeout)
	defer cancel()
	_ = m.settleFindLocked(ctx, p)
}

// submitFindEnd hands p an end to apply, without waiting: the running
// transition applies it when it finishes, or one of its own does. Safe on
// any thread, the engine's UI thread included. Once Close has begun to join
// the find work, an end is no longer applied: the browser's close ended
// every find before it.
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
		ctx, cancel := operationContext(context.Background(), p.ctx, findTimeout)
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

// endFind ends p's find on the Manager's own account: the page closed
// without its lock held by the caller, or the browser closed.
func (m *Manager) endFind(p *managedPage) {
	m.submitFindEnd(p, func(e *findEnds) { e.all = true })
}

// documentCommitted is pageHooks.DocumentCommitted. It runs where the engine
// reports the commit and only stamps the new document epoch, so an action
// that reads the epoch after the commit is bound to the new document. The
// work it returns submits the end of a find opened on an earlier document;
// a find opened on this document or a later one is not its to end.
func (m *Manager) documentCommitted(p *managedPage) func() {
	epoch := p.docEpoch.Add(1)
	return func() {
		m.submitFindEnd(p, func(e *findEnds) { e.epoch = max(e.epoch, epoch) })
	}
}

// endHiddenFinds submits to each of the thread's live pages that holds a
// find, or is in a transition, an end of a find not on the shown active
// page or whose owning pane mount is gone. What is shown is read when the
// end is applied. A page outside a transition with no find is skipped: a
// transition starting after the change reads it in its own checks.
func (m *Manager) endHiddenFinds(pages []*managedPage) {
	for _, p := range pages {
		p.findMu.Lock()
		quiet := p.find.idle()
		p.findMu.Unlock()
		if quiet && !p.findBusy.Load() {
			continue
		}
		m.submitFindEnd(p, func(e *findEnds) { e.review = true })
	}
}

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
// engine searches; the Manager owns the page's find: whether AO's find bar is
// open on it and for which pane mount, the running search's query and
// current match, and when both end. They end when the page commits another
// document, stops being the shown active page, is suspended or closes, or
// when the bar's pane unmounts; that end is announced as a Kind "find" event
// so the bar showing it closes.
//
// Every find action and every end takes the next number of the page's
// generation (findSession.gen). An engine call runs with the page lock held
// but not the find lock, so an end can land while it runs: the call's answer
// is published only if no end moved the generation meanwhile, and otherwise
// the engine's search is stopped again.

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
	// ends the search and leaves the bar open. In FindModeEngine it opens
	// the engine's own bar.
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
	// gen numbers the page's find actions and ends. An action answers with
	// the number it took (a step with its search's), an end announces the
	// number it moved to, so an end newer than every answer a client holds
	// is recognizably one it has not seen.
	gen uint64
	// bar is the pane mount whose find bar is open on the page, "" for none.
	bar string
	// starting is whether a start is in its engine call.
	starting bool
	// active is whether a search runs; search is its generation.
	active bool
	search uint64
	mode   string
	state  FindState
}

func (s *findSession) snapshot(pageID string) FindState {
	if !s.active {
		return FindState{PageID: pageID, Session: s.gen, Current: -1}
	}
	state := s.state
	state.PageID = pageID
	state.Session = s.search
	state.Active = true
	return state
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

// Find runs one find action on a page of the thread's companion. open and
// start run only on the shown active page, open for a mount of the thread's.
// stop and close never restore a suspended page and answer for a page that
// is gone as for one with no find: there is nothing left to end.
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
	ending := kind == FindStop || kind == FindClose
	var p *managedPage
	var err error
	if kind == FindOpen || kind == FindStart {
		p, _, err = m.resolvePage(ctx, access, pageID, false)
	} else {
		p, _, err = m.lookupOwnedPage(access, pageID)
	}
	if ending && errors.Is(err, errPageNotFound) {
		return FindState{PageID: pageID, Current: -1}, nil
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
			return FindState{PageID: pageID, Current: -1}, nil
		}
		return FindState{}, errPageNotFound
	}
	opCtx, cancel := operationContext(ctx, p.ctx, findTimeout)
	defer cancel()
	switch kind {
	case FindOpen:
		return m.openFindBar(access, p, mode, mount)
	case FindStart:
		return m.startFind(opCtx, access, p, finder, mode, query)
	case FindStop, FindClose:
		return m.stopFindLocked(opCtx, p, finder, kind == FindClose)
	}
	return m.stepFind(opCtx, p, finder, mode, kind == FindPrev)
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

var errFindNotShown = errors.New("browser: find runs on the page the browser pane shows")

// reserveFind takes the next generation for an action on p and checks, after
// taking it, that p is the shown page: a presentation change that ends the
// find moves the generation, so one that lands later is seen at publish.
// The caller holds p.mu.
func (m *Manager) reserveFind(threadID string, p *managedPage) (uint64, error) {
	p.findMu.Lock()
	p.find.gen++
	mine := p.find.gen
	p.find.starting = true
	p.findMu.Unlock()
	if !m.pageShown(threadID, p.id) {
		p.findMu.Lock()
		p.find.starting = false
		p.findMu.Unlock()
		return 0, errFindNotShown
	}
	return mine, nil
}

// openFindBar records AO's bar open on p for mount and claims the page's
// Escape. The caller holds p.mu.
func (m *Manager) openFindBar(access Access, p *managedPage, mode, mount string) (FindState, error) {
	if mode != FindModeBar {
		return FindState{}, fmt.Errorf("browser: this engine shows its own find bar")
	}
	if !m.mountOf(access.ThreadID, mount) {
		return FindState{}, fmt.Errorf("browser: find opens in a mounted browser pane")
	}
	mine, err := m.reserveFind(access.ThreadID, p)
	if err != nil {
		return FindState{}, err
	}
	p.findMu.Lock()
	defer p.findMu.Unlock()
	p.find.starting = false
	// An end since the reservation (the mount detached, the page hid) wins.
	if p.find.gen != mine {
		return FindState{}, errFindNotShown
	}
	p.find.bar = mount
	m.finding.Store(p.driver.Handle(), struct{}{})
	state := p.find.snapshot(p.id)
	state.Session = mine
	return state, nil
}

// mountOf reports whether mount is a pane mount of the thread's.
func (m *Manager) mountOf(threadID, mount string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pane, ok := m.panes[strings.TrimSpace(mount)]
	return ok && pane.threadID == threadID
}

// startFind replaces p's search. In FindModeBar it searches for query, and
// an empty query ends the search; in FindModeEngine it opens the engine's
// own bar seeded with query or else the page's selection. The caller holds
// p.mu.
func (m *Manager) startFind(ctx context.Context, access Access, p *managedPage, finder pageFind, mode, query string) (FindState, error) {
	if mode == FindModeBar && query == "" {
		return m.stopFindLocked(ctx, p, finder, false)
	}
	mine, err := m.reserveFind(access.ThreadID, p)
	if err != nil {
		return FindState{}, err
	}
	if mode == FindModeEngine && query == "" {
		query = findSeed(p.driver.SelectionText(ctx))
	}
	result, err := finder.FindStart(ctx, query)
	p.findMu.Lock()
	p.find.starting = false
	if p.find.gen != mine {
		// An end landed during the call: the search it started is nobody's.
		p.findMu.Unlock()
		if err == nil {
			if stopErr := finder.FindStop(ctx); stopErr != nil && p.ctx.Err() == nil {
				log.Printf("browser: page %s: stop a superseded find: %v", p.id, stopErr)
			}
		}
		return FindState{PageID: p.id, Session: mine, Current: -1}, nil
	}
	if err != nil {
		p.find.active = false
		p.findMu.Unlock()
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
	return p.find.snapshot(p.id), nil
}

// stepFind moves the search's current match with a step's answer. The engine
// wraps; past a capped count the position is unknown, so it is not wrapped
// here either. The caller holds p.mu.
func (m *Manager) stepFind(ctx context.Context, p *managedPage, finder pageFind, mode string, backwards bool) (FindState, error) {
	if mode == FindModeEngine {
		return FindState{}, fmt.Errorf("browser: this engine steps through matches in its own find bar")
	}
	p.findMu.Lock()
	active, search := p.find.active, p.find.search
	p.findMu.Unlock()
	if !active {
		return FindState{}, fmt.Errorf("browser: no find is in progress on this page")
	}
	result, err := finder.FindStep(ctx, backwards)
	p.findMu.Lock()
	defer p.findMu.Unlock()
	if !p.find.active || p.find.search != search {
		return FindState{PageID: p.id, Session: search, Current: -1}, nil
	}
	if err != nil {
		// What the engine still shows is unknown, so the next action starts
		// over.
		p.find.active = false
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
	return p.find.snapshot(p.id), nil
}

// stopFindLocked is a caller's end of the search, and with closeBar of the
// bar too. Ending what already ended answers the same. The caller holds p.mu.
func (m *Manager) stopFindLocked(ctx context.Context, p *managedPage, finder pageFind, closeBar bool) (FindState, error) {
	p.findMu.Lock()
	p.find.gen++
	searching := p.find.active
	p.find.active = false
	if closeBar && p.find.bar != "" {
		p.find.bar = ""
		m.finding.Delete(p.driver.Handle())
	}
	state := p.find.snapshot(p.id)
	p.findMu.Unlock()
	if !searching {
		return state, nil
	}
	if err := finder.FindStop(ctx); err != nil {
		return state, fmt.Errorf("browser: stop find: %w", err)
	}
	return state, nil
}

// endFind ends p's find on the Manager's own account: the search, the bar
// and any action in flight. An end that ended something is announced.
// stopEngine asks the engine to clear a search left on a page that lives on;
// the call queues behind the page's in-flight operation, off the caller's
// goroutine, and is skipped when a newer action ran first. Callers may hold
// p.mu.
func (m *Manager) endFind(p *managedPage, stopEngine bool) {
	m.endFindIf(p, stopEngine, nil)
}

// endFindIf is endFind when ends answers true for the page's session, nil
// for always.
func (m *Manager) endFindIf(p *managedPage, stopEngine bool, ends func(*findSession) bool) {
	p.findMu.Lock()
	idle := !p.find.active && p.find.bar == "" && !p.find.starting
	if idle || (ends != nil && !ends(&p.find)) {
		p.findMu.Unlock()
		return
	}
	p.find.gen++
	ended := p.find.gen
	searching, open := p.find.active, p.find.bar != ""
	p.find.active, p.find.bar = false, ""
	m.finding.Delete(p.driver.Handle())
	state := p.find.snapshot(p.id)
	p.findMu.Unlock()
	if !searching && !open {
		return
	}
	m.emit(CompanionEvent{Kind: "find", ThreadID: p.owner, PageID: p.id, Find: &state})
	if stopEngine && searching {
		go m.stopEngineFind(p, ended)
	}
}

func (m *Manager) stopEngineFind(p *managedPage, ended uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !m.pageLive(p) {
		return
	}
	p.findMu.Lock()
	current := p.find.gen
	p.findMu.Unlock()
	if current != ended {
		return
	}
	finder, mode := pageFinder(p)
	if mode == "" {
		return
	}
	ctx, cancel := operationContext(context.Background(), p.ctx, findTimeout)
	defer cancel()
	if err := finder.FindStop(ctx); err != nil && p.ctx.Err() == nil {
		log.Printf("browser: page %s: stop find: %v", p.id, err)
	}
}

// documentCommitted ends p's find once its main frame commits another
// document (pageHooks.DocumentCommitted); a same-document navigation keeps it.
func (m *Manager) documentCommitted(p *managedPage) {
	m.endFind(p, true)
}

// endHiddenFinds ends the finds of the thread's pages that are not the shown
// active page, and a bar whose pane mount is gone. pages are the thread's
// live pages; shown is whether the thread has a session and a mounted pane;
// mounts are the thread's pane mounts.
func (m *Manager) endHiddenFinds(pages []*managedPage, session SessionInfo, shown bool, mounts map[string]bool) {
	for _, p := range pages {
		if shown && session.Visible && p.id == session.ActivePageID {
			m.endFindIf(p, true, func(s *findSession) bool { return s.bar != "" && !mounts[s.bar] })
			continue
		}
		m.endFind(p, true)
	}
}

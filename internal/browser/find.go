package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Find in the browser pane (docs/architecture/browser-tools.md, "Find"). The
// engine searches; the Manager owns the session: which page it runs on, its
// query, the current match, and when it ends. A session ends when its page
// navigates to another document, stops being the shown active page, is
// suspended, or closes; that end is announced as a Kind "find" event so the
// bar showing it closes.

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

var errFindUnavailable = errors.New("browser: find is not available on this page's browser engine")

// findSession is one page's find state. Guarded by managedPage.findMu.
type findSession struct {
	seq    uint64
	active bool
	mode   string
	// doc is the address the session searched, without its fragment: a
	// different one is another document.
	doc   string
	state FindState
}

func (s *findSession) snapshot(pageID string) FindState {
	state := s.state
	state.PageID = pageID
	state.Session = s.seq
	state.Active = s.active
	if !s.active {
		state = FindState{PageID: pageID, Session: s.seq, Current: -1}
	}
	return state
}

func pageFinder(p *managedPage) (pageFind, string) {
	finder, ok := p.driver.(pageFind)
	if !ok {
		return nil, ""
	}
	return finder, finder.FindMode()
}

// findMode answers the find mode of a live page, "" for none.
func findModeOf(p *managedPage) string {
	if p == nil || p.driver == nil {
		return ""
	}
	_, mode := pageFinder(p)
	return mode
}

func documentOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	return parsed.String()
}

// findSeed is a selection usable as a search term: one line, bounded.
func findSeed(selection string) string {
	seed := strings.TrimSpace(selection)
	if seed == "" || strings.ContainsAny(seed, "\r\n") || utf8.RuneCountInString(seed) > maxFindSeedRunes {
		return ""
	}
	return seed
}

// Find runs one find action on the page the thread's companion shows: start
// (replace the search with query; an empty query ends it), next, previous, or
// stop. In FindModeEngine a start opens the engine's own bar, seeded with
// query or else the page's selection, and the engine steps in it.
func (m *Manager) Find(ctx context.Context, access Access, pageID, kind, query string) (FindState, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "start", "next", "previous", "stop":
	default:
		return FindState{}, fmt.Errorf("browser: find action must be start, next, previous, or stop")
	}
	if len(query) > maxFindQueryBytes {
		return FindState{}, fmt.Errorf("browser: find query exceeds %d bytes", maxFindQueryBytes)
	}
	if pageID = strings.TrimSpace(pageID); pageID == "" {
		return FindState{}, fmt.Errorf("browser: page id is required")
	}
	p, _, err := m.resolvePage(ctx, access, pageID, false)
	if err != nil {
		return FindState{}, err
	}
	finder, mode := pageFinder(p)
	if mode == "" {
		return FindState{}, errFindUnavailable
	}
	if kind == "start" && !m.pageShown(access.ThreadID, p.id) {
		return FindState{}, fmt.Errorf("browser: find runs on the page the browser pane shows")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, findTimeout)
	defer cancel()
	switch kind {
	case "stop":
		return m.stopFindLocked(opCtx, p, finder)
	case "start":
		if mode == FindModeEngine && query == "" {
			query = findSeed(p.driver.SelectionText(opCtx))
		} else if query == "" {
			return m.stopFindLocked(opCtx, p, finder)
		}
		result, err := finder.FindStart(opCtx, query)
		if err != nil {
			m.failFind(p)
			return FindState{}, err
		}
		return m.beginFind(p, mode, query, result), nil
	}
	if mode == FindModeEngine {
		return FindState{}, fmt.Errorf("browser: this engine steps through matches in its own find bar")
	}
	p.findMu.Lock()
	active := p.find.active
	p.findMu.Unlock()
	if !active {
		return FindState{}, fmt.Errorf("browser: no find is in progress on this page")
	}
	result, err := finder.FindStep(opCtx, kind == "previous")
	if err != nil {
		m.failFind(p)
		return FindState{}, err
	}
	return m.stepFind(p, kind == "previous", result), nil
}

// pageShown reports whether pageID is the active page of the thread's shown
// companion.
func (m *Manager) pageShown(threadID, pageID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[threadID]
	return ok && session.Visible && session.ActivePageID == pageID
}

// beginFind records a new session from a start's answer. The caller holds
// p.mu.
func (m *Manager) beginFind(p *managedPage, mode, query string, result findResult) FindState {
	p.findMu.Lock()
	defer p.findMu.Unlock()
	p.find.seq++
	p.find.active = true
	p.find.mode = mode
	p.find.doc = documentOf(p.cachedInfo().URL)
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
	return p.find.snapshot(p.id)
}

// stepFind moves the session's current match with a step's answer. The
// engine wraps; past a capped count the position is unknown, so it is not
// wrapped here either. The caller holds p.mu.
func (m *Manager) stepFind(p *managedPage, backwards bool, result findResult) FindState {
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
	return p.find.snapshot(p.id)
}

// stopFindLocked is a caller's stop. The caller holds p.mu.
func (m *Manager) stopFindLocked(ctx context.Context, p *managedPage, finder pageFind) (FindState, error) {
	p.findMu.Lock()
	active := p.find.active
	if active {
		p.find.active = false
		p.find.seq++
	}
	state := p.find.snapshot(p.id)
	p.findMu.Unlock()
	if !active {
		return state, nil
	}
	if err := finder.FindStop(ctx); err != nil {
		return state, fmt.Errorf("browser: stop find: %w", err)
	}
	return state, nil
}

// failFind ends a session whose engine call failed; what the engine still
// shows is unknown, so the next action starts over. The caller holds p.mu.
func (m *Manager) failFind(p *managedPage) {
	p.findMu.Lock()
	if p.find.active {
		p.find.active = false
		p.find.seq++
	}
	p.findMu.Unlock()
}

// endFind ends p's session on the Manager's own account and announces it.
// stopEngine asks the engine to clear what the find left on a page that
// lives on; the call queues behind the page's in-flight operation, off the
// caller's goroutine, and is skipped when a newer session started first.
// Callers may hold p.mu.
func (m *Manager) endFind(p *managedPage, stopEngine bool) {
	p.findMu.Lock()
	if !p.find.active {
		p.findMu.Unlock()
		return
	}
	p.find.active = false
	p.find.seq++
	ended := p.find.seq
	state := p.find.snapshot(p.id)
	p.findMu.Unlock()
	m.emit(CompanionEvent{Kind: "find", ThreadID: p.owner, PageID: p.id, Find: &state})
	if stopEngine {
		go m.stopEngineFind(p, ended)
	}
}

func (m *Manager) stopEngineFind(p *managedPage, ended uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return
	}
	p.findMu.Lock()
	current := p.find.seq
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

// followFindDocument ends p's session once the page shows another document.
func (m *Manager) followFindDocument(p *managedPage) {
	p.findMu.Lock()
	moved := p.find.active && p.find.doc != documentOf(p.cachedInfo().URL)
	p.findMu.Unlock()
	if moved {
		m.endFind(p, true)
	}
}

// endHiddenFinds ends the sessions of the thread's pages that are not the
// shown active page. pages are the thread's live pages.
func (m *Manager) endHiddenFinds(pages []*managedPage, session SessionInfo, shown bool) {
	for _, p := range pages {
		if shown && session.Visible && p.id == session.ActivePageID {
			continue
		}
		m.endFind(p, true)
	}
}

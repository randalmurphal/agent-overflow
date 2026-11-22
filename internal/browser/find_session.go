package browser

import (
	"strings"
	"unicode/utf8"
)

// A page's find state (findSession), whom a find action is for
// (findOwner), how an answer names the search, and the page's finder.
// find.go owns the actions and the rules they follow.

// findSession is one page's find state. Written only by a transition, which
// holds managedPage.mu; managedPage.findMu guards it for readers.
type findSession struct {
	// gen is the generation of the page's newest start, stop or end.
	gen uint64
	// owner and seq are the pane mount and sequence the running search
	// belongs to; "" and 0 when none runs.
	owner MountID
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
func (s *findSession) heldThrough(mount MountID, through uint64) bool {
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
	mount MountID
	seq   uint64
}

// identify stamps a state with the search's identity on page p.
func (s *findSession) identify(p *managedPage, state FindState) FindState {
	state.PageID = string(p.id)
	state.Incarnation = p.incarnation
	state.Mount = string(s.owner)
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

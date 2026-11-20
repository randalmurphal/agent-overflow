package trackerapp

import (
	"fmt"
	"slices"

	"agent-overflow/internal/tracker"
)

// A row can name a status, user or sprint no structure read explains: an
// issue edited today that still names a sprint closed before the history
// window, a user the tracker no longer lists. A structure read for such a
// reference changes nothing, so the service keeps, per scope, the
// references the last structure read left unexplained, and a cascade
// reads the structure only for a reference outside that set. The set is
// in memory, rebuilt by every structure read of the scope's board (a
// subscribe's first), and dropped when its connection stops being used.

const (
	// maxUnexplained bounds the references one scope's set keeps. A scope
	// whose set is full no longer reads the structure for a sprint: an
	// unlisted sprint is then most likely old, not new.
	maxUnexplained = 256
	// maxUnexplainedScopes bounds the scopes with a set; the least
	// recently written goes first, which costs it at most one structure
	// read to learn its set again.
	maxUnexplainedScopes = 64
)

// refs are statuses, users and sprints by ID.
type refs struct {
	statuses, users, sprints map[string]bool
}

func newRefs() *refs {
	return &refs{statuses: map[string]bool{}, users: map[string]bool{}, sprints: map[string]bool{}}
}

// add gathers what the rows of changed issues name. changed's lists are
// sorted.
func (n *refs) add(rows []tracker.Row, changed Changes) {
	for _, row := range rows {
		c := row.Card
		if _, ok := slices.BinarySearch(changed.Cards, c.ID); !ok {
			if _, ok := slices.BinarySearch(changed.Rows, c.ID); !ok {
				continue
			}
		}
		if c.Status != "" {
			n.statuses[string(c.Status)] = true
		}
		if c.Assignee != "" {
			n.users[string(c.Assignee)] = true
		}
		for _, sp := range c.Sprints {
			n.sprints[string(sp)] = true
		}
	}
}

func (n *refs) size() int { return len(n.statuses) + len(n.users) + len(n.sprints) }

func (n *refs) empty() bool { return n.size() == 0 }

// union adds o's references to n.
func (n *refs) union(o *refs) {
	for _, kind := range [][2]map[string]bool{{n.statuses, o.statuses}, {n.users, o.users}, {n.sprints, o.sprints}} {
		for id := range kind[1] {
			kind[0][id] = true
		}
	}
}

// remove takes o's references out of n.
func (n *refs) remove(o *refs) {
	for _, kind := range [][2]map[string]bool{{n.statuses, o.statuses}, {n.users, o.users}, {n.sprints, o.sprints}} {
		for id := range kind[1] {
			delete(kind[0], id)
		}
	}
}

// clone is a copy of n, cut to at most limit references, statuses first
// and sprints last; cut reports whether any were left out.
func (n *refs) clone(limit int) (out *refs, cut bool) {
	out = newRefs()
	for _, kind := range [][2]map[string]bool{{out.statuses, n.statuses}, {out.users, n.users}, {out.sprints, n.sprints}} {
		for _, id := range sortedKeys(kind[1]) {
			if limit == 0 {
				return out, true
			}
			kind[0][id] = true
			limit--
		}
	}
	return out, false
}

// unexplainedSet is one scope's set: full when references were left out
// for the bound; used orders the sets for eviction.
type unexplainedSet struct {
	refs *refs
	full bool
	used uint64
}

// unexplainedOf is a copy of scope's set and whether it is full.
func (s *Service) unexplainedOf(scope Scope) (*refs, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.unexplained[scope]
	if set == nil {
		return newRefs(), false
	}
	out, _ := set.refs.clone(maxUnexplained)
	return out, set.full
}

// setUnexplained makes set scope's set, cut to maxUnexplained.
func (s *Service) setUnexplained(scope Scope, set *refs) {
	kept, cut := set.clone(maxUnexplained)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if kept.empty() {
		delete(s.unexplained, scope)
		return
	}
	if _, ok := s.unexplained[scope]; !ok && len(s.unexplained) >= maxUnexplainedScopes {
		oldest := Scope{}
		var at uint64
		for sc, v := range s.unexplained {
			if at == 0 || v.used < at {
				oldest, at = sc, v.used
			}
		}
		delete(s.unexplained, oldest)
	}
	s.unexplainedUse++
	s.unexplained[scope] = &unexplainedSet{refs: kept, full: cut, used: s.unexplainedUse}
}

// forgetUnexplainedLocked drops the sets of connection id's scopes. The
// caller holds mu.
func (s *Service) forgetUnexplainedLocked(id string) {
	for sc := range s.unexplained {
		if sc.Connection == id {
			delete(s.unexplained, sc)
		}
	}
}

// cascade reads board's structure when the rows a unit stored, n, name a
// status, user or sprint the mirror lacks, unless the scope's last
// structure read left it unexplained or the tick has read the structure
// already; either way what is still lacking joins the scope's set.
func (r *run) cascade(board tracker.BoardID, n *refs, out *UnitResult) error {
	if n.empty() {
		return nil
	}
	lack, err := r.lacking(board, n)
	if err != nil || lack.empty() {
		return err
	}
	known, full := r.s.unexplainedOf(r.scope)
	lack.remove(known)
	if full {
		clear(lack.sprints)
	}
	if lack.empty() {
		return nil
	}
	if r.structured {
		known.union(lack)
		r.s.setUnexplained(r.scope, known)
		return nil
	}
	ch, err := r.structure(board)
	out.Changes.add(ch)
	if err != nil {
		return err
	}
	return r.reexplain(board, lack)
}

// reexplain makes the scope's set what, of it and of also, the mirror
// still lacks after a structure read of board.
func (r *run) reexplain(board tracker.BoardID, also *refs) error {
	known, _ := r.s.unexplainedOf(r.scope)
	known.union(also)
	if !known.empty() {
		still, err := r.lacking(board, known)
		if err != nil {
			return fmt.Errorf("trackerapp: check the references board %s left unexplained: %w", board, err)
		}
		known = still
	}
	r.s.setUnexplained(r.scope, known)
	return nil
}

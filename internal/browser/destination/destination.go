// Package destination owns a native engine's download destination
// decisions while they are open: from WebKit asking where a download goes,
// on the UI thread, until the decision is completed or canceled there,
// exactly once.
//
// Naming the staged file is file system work the UI thread must not wait
// on, so the decision is handed to a worker and completed on the UI thread
// afterwards. Three parties can end it: the worker's completion, the
// profile's close, and the UI loop going away. Decisions is the one record
// of the open decisions, each taken by exactly one of them; Claim is the
// staged file the worker named, released exactly once unless a completed
// decision handed it on; Settle is the worker.
//
// Stdlib-only, apart from internal/browser/uicall.
package destination

import (
	"sync"

	"agent-overflow/internal/browser/uicall"
)

// Decisions is a profile's open destination decisions by download id. A
// decision is held as WebKit hands it over, on the UI thread, and taken
// once: by its completion (Take) or by the profile's close (Close), which
// also refuses every later Hold.
type Decisions[ID comparable, D any] struct {
	mu     sync.Mutex
	closed bool
	open   map[ID]D
}

// NewDecisions answers an empty, open set.
func NewDecisions[ID comparable, D any]() *Decisions[ID, D] {
	return &Decisions[ID, D]{open: make(map[ID]D)}
}

// Hold keeps decision d under id and answers true, or answers false once
// the set is closed or while id already holds an open decision, which
// stays held: the caller then cancels d itself.
func (s *Decisions[ID, D]) Hold(id ID, d D) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if _, open := s.open[id]; open {
		return false
	}
	s.open[id] = d
	return true
}

// Take removes and answers the decision held under id; ok is false when
// none is, because Close took it or it was never held.
func (s *Decisions[ID, D]) Take(id ID) (d D, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok = s.open[id]
	delete(s.open, id)
	return d, ok
}

// Close refuses every later Hold and answers the decisions still open,
// for the caller to cancel. A second Close answers none.
func (s *Decisions[ID, D]) Close() []D {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	taken := make([]D, 0, len(s.open))
	for id, d := range s.open {
		taken = append(taken, d)
		delete(s.open, id)
	}
	return taken
}

// Claim is the staged file a worker named for a download: released once,
// unless a completed decision handed it to the download's owner first.
type Claim struct {
	once    sync.Once
	release func()
}

// NewClaim answers the claim release ends. A nil release is a claim on
// nothing.
func NewClaim(release func()) *Claim {
	return &Claim{release: release}
}

// HandOff ends the claim without releasing it: the download's owner now
// holds the file. Later calls, and Release, do nothing.
func (c *Claim) HandOff() { c.once.Do(func() {}) }

// Release releases the claim unless it was handed off; later calls do
// nothing.
func (c *Claim) Release() {
	c.once.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
}

// Staged is what a worker's staging named: the staged file's path and its
// claim, or the error that left none.
type Staged struct {
	Path  string
	Claim *Claim
	Err   error
}

// Settle is a decision's worker. It runs stage off the UI thread, then
// complete on thread, until complete ran or gone says it never will (the
// UI loop is gone, or the profile closed, which cancels the decision
// itself). complete answers whether it took the decision; a claim no taken
// decision handed off is released here, without the UI loop or the claim's
// owner.
func Settle(thread *uicall.Thread, gone func() bool, stage func() Staged, complete func(Staged) bool) {
	staged := stage()
	claim := staged.Claim
	if claim == nil {
		claim = NewClaim(nil)
	}
	defer claim.Release()
	for {
		taken := false
		if thread.Run(func() { taken = complete(staged) }) == uicall.Ran {
			if taken {
				claim.HandOff()
			}
			return
		}
		if gone() {
			return
		}
	}
}

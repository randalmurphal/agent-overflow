// Package procutiltest supplies the reapers tests hand to code that refuses to
// start a process without one. Production code must not import it.
package procutiltest

import (
	"slices"
	"sync"

	"agent-overflow/internal/procutil"
)

// Reaper returns a reaper that watches nothing. A test whose subject must be
// given a reaper passes this one, so a change to what a launch requires lands
// here and not at every call site. A test asserting what is watched uses a
// Recorder.
func Reaper() procutil.Reaper { return nopReaper{} }

type nopReaper struct{}

func (nopReaper) Watch(int)   {}
func (nopReaper) Release(int) {}

// Recorder is a reaper that records the groups it is told to watch and
// release.
type Recorder struct {
	mu       sync.Mutex
	watched  []int
	released []int
}

func (r *Recorder) Watch(pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watched = append(r.watched, pgid)
}

func (r *Recorder) Release(pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, pgid)
}

// Calls returns the groups watched and released so far, in order.
func (r *Recorder) Calls() (watched, released []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.watched), slices.Clone(r.released)
}

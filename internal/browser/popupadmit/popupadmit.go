// Package popupadmit is the one record of a popup between its engine's
// report and the Manager's decision on it: whether it becomes a tab, and
// what ended it before that.
//
// An engine reports a page it opened by itself (Announce), and the
// Manager attaches and readies it on a goroutine of its own before it
// lists it as a tab. The page can close or crash meanwhile, and the engine
// reports that by its handle (Observe) whatever the Manager is doing with
// it, even before the announcement reaches the registry. The Manager acts
// on a close or crash only for a listed tab, so the observation waits on
// the popup's admission until the listing's commit (Commit) reads it: a
// popup that closed is never listed, one that only crashed is listed and
// then handled as a crashed tab. Every popup's admission ends once, listed
// or ended (Commit, Discard).
//
// A handle is an engine's page handle, unique for the engine's life: a
// headless or hosted target or page id, or a WebKit engine's page number.
//
// An admission also keeps the popup's opener, after its end too within a
// bound, so a download the popup or a popup of it starts is attributed to
// the nearest ancestor still listed (Ancestors), even through a parent
// that was never listed.
//
// Stdlib-only.
package popupadmit

import "sync"

// Handle is an engine page handle: the identity a popup is reported,
// adopted and listed under.
type Handle string

// Disposition is where a popup's admission stands.
type Disposition int

const (
	// Pending: announced, neither listed nor ended yet.
	Pending Disposition = iota + 1
	// Listed: the popup is a tab; what ends it now is the tab's.
	Listed
	// Ended: the popup will never be a tab: it closed, or the Manager
	// discarded it.
	Ended
)

func (d Disposition) String() string {
	switch d {
	case Pending:
		return "pending"
	case Listed:
		return "listed"
	case Ended:
		return "ended"
	}
	panic("popupadmit: unknown disposition")
}

// Observation is a terminal fact an engine reports of a page by handle.
// Build one with Closed or Crashed.
type Observation struct {
	closed bool
	crash  string
}

// Closed is the page's own close (window.close(), its target going).
func Closed() Observation { return Observation{closed: true} }

// Crashed is the page's process ending, for reason.
func Crashed(reason string) Observation {
	if reason == "" {
		reason = "crashed"
	}
	return Observation{crash: reason}
}

// ending is what a popup's observations add up to: a close ends it
// whatever crashed before or after.
type ending struct {
	closed bool
	crash  string
}

func (e ending) with(o Observation) ending {
	e.closed = e.closed || o.closed
	if e.crash == "" {
		e.crash = o.crash
	}
	return e
}

// Observed is what Observe did with an observation.
type Observed int

const (
	// Held: the popup's admission is pending and keeps the observation
	// for its commit.
	Held Observed = iota + 1
	// Unheld: no pending admission has the handle, so the observation is
	// the caller's to act on (a listed tab, or no popup at all).
	Unheld
)

// VerdictKind is what a commit decided.
type VerdictKind int

const (
	// List: list the popup as a live tab.
	List VerdictKind = iota + 1
	// ListCrashed: list the popup and handle it as a crashed tab.
	ListCrashed
	// Refuse: do not list the popup: it closed, or its admission had
	// already ended.
	Refuse
)

// Verdict is a commit's decision.
type Verdict struct {
	kind  VerdictKind
	crash string
}

// Kind is the decision.
func (v Verdict) Kind() VerdictKind { return v.kind }

// CrashReason is the reason its process ended, for ListCrashed.
func (v Verdict) CrashReason() string { return v.crash }

type admission struct {
	opener      Handle
	disposition Disposition
	ending      ending
}

// to moves a to next and answers whether that is a transition an
// admission may make: only a pending one moves, to Listed or Ended.
func (a *admission) to(next Disposition) bool {
	if a.disposition != Pending || next == Pending {
		return false
	}
	a.disposition = next
	return true
}

// Registry is the admissions of one Manager's popups. Pending admissions
// are kept until they end; ended and listed ones, and observations of
// handles not announced yet, are kept up to the limit New was given,
// oldest first out.
type Registry struct {
	mu      sync.Mutex
	limit   int
	popups  map[Handle]*admission
	settled []Handle
	early   map[Handle]ending
	order   []Handle
}

// New answers an empty registry keeping limit settled admissions and
// limit early observations.
func New(limit int) *Registry {
	if limit < 1 {
		limit = 1
	}
	return &Registry{limit: limit, popups: make(map[Handle]*admission), early: make(map[Handle]ending)}
}

// Announce starts the admission of popup h, opened by opener, and answers
// false when h was announced before. What was observed of h before its
// announcement joins its admission.
func (r *Registry) Announce(h, opener Handle) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.popups[h]; known {
		return false
	}
	a := &admission{opener: opener, disposition: Pending, ending: r.early[h]}
	delete(r.early, h)
	r.popups[h] = a
	return true
}

// Observe gives o to the pending admission of h. With none, it is kept a
// while for an announcement still on its way, and answered Unheld.
func (r *Registry) Observe(h Handle, o Observation) Observed {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.popups[h]
	if a != nil && a.disposition == Pending {
		a.ending = a.ending.with(o)
		return Held
	}
	if a == nil {
		if _, kept := r.early[h]; !kept {
			r.order = append(r.order, h)
		}
		r.early[h] = r.early[h].with(o)
		for len(r.order) > r.limit {
			delete(r.early, r.order[0])
			r.order = r.order[1:]
		}
	}
	return Unheld
}

// Commit ends the admission of h at its listing and answers whether to
// list it: a popup that closed is refused, one that crashed is listed
// crashed. The caller holds the lock under which it lists pages, so no
// listing of h races this.
func (r *Registry) Commit(h Handle) Verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.popups[h]
	if a == nil || a.disposition != Pending {
		return Verdict{kind: Refuse}
	}
	if a.ending.closed {
		r.settleLocked(h, a, Ended)
		return Verdict{kind: Refuse}
	}
	r.settleLocked(h, a, Listed)
	if a.ending.crash != "" {
		return Verdict{kind: ListCrashed, crash: a.ending.crash}
	}
	return Verdict{kind: List}
}

// Discard ends the pending admission of h without a listing; it does
// nothing to one that already ended.
func (r *Registry) Discard(h Handle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.popups[h]; a != nil && a.disposition == Pending {
		r.settleLocked(h, a, Ended)
	}
}

// Disposition answers where the admission of h stands; ok is false when
// none is known.
func (r *Registry) Disposition(h Handle) (d Disposition, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.popups[h]; a != nil {
		return a.disposition, true
	}
	return 0, false
}

// Ancestors answers the openers of h as far as the registry knows them,
// nearest first: h's opener, that opener's own if it was a popup, and so
// on, each once.
func (r *Registry) Ancestors(h Handle) []Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	var chain []Handle
	seen := map[Handle]bool{h: true}
	for a := r.popups[h]; a != nil && a.opener != "" && !seen[a.opener]; a = r.popups[a.opener] {
		seen[a.opener] = true
		chain = append(chain, a.opener)
	}
	return chain
}

// settleLocked moves a to next and keeps it among the settled, the oldest
// beyond the limit forgotten. The caller holds r.mu.
func (r *Registry) settleLocked(h Handle, a *admission, next Disposition) {
	if !a.to(next) {
		panic("popupadmit: an admission ended twice")
	}
	r.settled = append(r.settled, h)
	for len(r.settled) > r.limit {
		delete(r.popups, r.settled[0])
		r.settled = r.settled[1:]
	}
}

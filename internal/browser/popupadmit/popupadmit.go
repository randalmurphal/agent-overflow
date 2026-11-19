// Package popupadmit is the one record of a popup between its engine's
// report and the Manager's decision on it, whether it becomes a tab and
// what ended it before that, and of the opener chain its downloads are
// attributed through for as long as anything depends on it.
//
// An engine reports a page it opened by itself (Announce) before it
// reports anything else of it, and the Manager attaches and readies it on
// a goroutine of its own before it lists it as a tab. The page can close
// or crash meanwhile, and the engine reports that by its handle (Observe)
// whatever the Manager is doing with it. The Manager acts on a close or
// crash only for a listed tab, so the observation waits on the popup's
// admission until the listing's commit (Commit, or CommitUnattached when
// its attach failed) reads it: a popup that closed is never listed, one
// that only crashed is listed as the crashed tab it is. Every popup's
// admission ends once, listed or ended (Commit, CommitUnattached, Discard),
// and a listed one ends when its tab leaves (Leave).
//
// A handle is an engine's page handle, unique for the engine's life: a
// headless or hosted target or page id, or a WebKit engine's page number.
//
// Admissions are held in two containers with two policies. A live one is
// one still pending or listed, or the opener of a live one: it is never
// dropped, so a download the popup or a popup of it starts is attributed
// to the nearest ancestor still listed (Ancestors) for as long as any
// descendant is live, even through a parent that was never listed or has
// gone. What is live is bounded by what the Manager can have pending and
// listed. An admission that is no longer live goes to a bounded history,
// oldest out first; an admission leaves before its opener does, so a
// handle the history still has keeps its whole chain. A handle in neither
// has an unknown ancestry, which Ancestors says, and which its caller
// treats as "no owner known" rather than as "no opener".
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
	// Listed: the popup is a live tab; what ends it now is the tab's.
	Listed
	// Ended: the popup is not a live tab and will not be one: it closed,
	// the Manager discarded it, it was listed crashed, or its tab left.
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
	// ListCrashed: list the popup as a crashed tab, never as a live one.
	ListCrashed
	// Refuse: do not list the popup: it closed, its attach failed with no
	// crash to show, or its admission had already ended.
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

// Ancestry is what the registry knows of a popup's openers.
type Ancestry struct {
	known bool
	chain []Handle
}

// Known says whether the popup's admission is still known. An unknown
// ancestry names no opener at all, which is not the same as having none.
func (a Ancestry) Known() bool { return a.known }

// Chain is the popup's openers, nearest first: its opener, that opener's
// own if it was a popup, and so on, each once. Nil when unknown.
func (a Ancestry) Chain() []Handle { return a.chain }

type admission struct {
	opener      Handle
	disposition Disposition
	ending      ending
}

// to moves a to next and answers whether that is a transition an
// admission may make: a pending one is listed or ends, a listed one ends.
func (a *admission) to(next Disposition) bool {
	switch {
	case a.disposition == Pending && (next == Listed || next == Ended):
	case a.disposition == Listed && next == Ended:
	default:
		return false
	}
	a.disposition = next
	return true
}

// Registry is the admissions of one Manager's popups.
type Registry struct {
	mu sync.Mutex
	// live are the admissions pending or listed, and those that are the
	// opener of a live one.
	live map[Handle]*admission
	// children counts the live admissions whose opener is each handle,
	// whether or not that handle is a popup.
	children map[Handle]int
	history  history
}

// New answers an empty registry whose history keeps limit admissions no
// longer live.
func New(limit int) *Registry {
	if limit < 1 {
		limit = 1
	}
	return &Registry{
		live: make(map[Handle]*admission), children: make(map[Handle]int),
		history: history{limit: limit, entries: make(map[Handle]historyEntry)},
	}
}

// Announce starts the admission of popup h, opened by opener, and answers
// false when h was announced before and is still known.
func (r *Registry) Announce(h, opener Handle) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live[h] != nil {
		return false
	}
	if _, known := r.history.get(h); known {
		return false
	}
	r.live[h] = &admission{opener: opener, disposition: Pending}
	r.pinLocked(opener)
	return true
}

// Observe gives o to the pending admission of h. With none, it is the
// caller's: an engine announces a popup before it reports anything else
// of it, so an observation of a handle with no pending admission is of a
// listed tab, a popup that already ended, or a page that was never a
// popup, and nothing keeps it.
func (r *Registry) Observe(h Handle, o Observation) Observed {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.live[h]; a != nil && a.disposition == Pending {
		a.ending = a.ending.with(o)
		return Held
	}
	return Unheld
}

// Commit ends the admission of h at its listing, its page attached, and
// answers whether to list it: a popup that closed is refused, one that
// crashed is listed crashed. The caller holds the lock under which it
// lists pages, so no listing of h races this.
func (r *Registry) Commit(h Handle) Verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.live[h]
	if a == nil || a.disposition != Pending {
		return Verdict{kind: Refuse}
	}
	switch {
	case a.ending.closed:
		r.settleLocked(h, a, Ended)
		return Verdict{kind: Refuse}
	case a.ending.crash != "":
		r.settleLocked(h, a, Ended)
		return Verdict{kind: ListCrashed, crash: a.ending.crash}
	}
	r.settleLocked(h, a, Listed)
	return Verdict{kind: List}
}

// CommitUnattached ends the admission of h whose page could not be
// attached: a popup whose process ended first is still listed crashed, as
// its crash would list it attached, unless it also closed; any other is
// refused.
func (r *Registry) CommitUnattached(h Handle) Verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.live[h]
	if a == nil || a.disposition != Pending {
		return Verdict{kind: Refuse}
	}
	r.settleLocked(h, a, Ended)
	if a.ending.crash != "" && !a.ending.closed {
		return Verdict{kind: ListCrashed, crash: a.ending.crash}
	}
	return Verdict{kind: Refuse}
}

// Discard ends the pending admission of h without a listing; it does
// nothing to one that already ended or is listed.
func (r *Registry) Discard(h Handle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.live[h]; a != nil && a.disposition == Pending {
		r.settleLocked(h, a, Ended)
	}
}

// Leave ends the admission of h, whose tab left; it does nothing to a
// handle that is not a listed popup.
func (r *Registry) Leave(h Handle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.live[h]; a != nil && a.disposition == Listed {
		r.settleLocked(h, a, Ended)
	}
}

// Disposition answers where the admission of h stands; ok is false when
// none is known.
func (r *Registry) Disposition(h Handle) (d Disposition, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.lookupLocked(h); a != nil {
		return a.disposition, true
	}
	return 0, false
}

// Ancestors answers the openers of h, nearest first, or that its
// admission is no longer known.
func (r *Registry) Ancestors(h Handle) Ancestry {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.lookupLocked(h)
	if a == nil {
		return Ancestry{}
	}
	var chain []Handle
	seen := map[Handle]bool{h: true}
	for ; a != nil && a.opener != "" && !seen[a.opener]; a = r.lookupLocked(a.opener) {
		seen[a.opener] = true
		chain = append(chain, a.opener)
	}
	return Ancestry{known: true, chain: chain}
}

// lookupLocked answers the admission of h, live or in the history, or
// nil. The caller holds r.mu.
func (r *Registry) lookupLocked(h Handle) *admission {
	if a := r.live[h]; a != nil {
		return a
	}
	a, _ := r.history.get(h)
	return a
}

// settleLocked moves a to next, and lets it go from the live ones when
// nothing keeps it there. The caller holds r.mu.
func (r *Registry) settleLocked(h Handle, a *admission, next Disposition) {
	if !a.to(next) {
		panic("popupadmit: an admission moved from " + a.disposition.String() + " to " + next.String())
	}
	r.releaseLocked(h)
}

// pinLocked counts one more live admission opened by h. An opener that
// already went to the history is live again while it has one, so its
// chain is kept with it. The caller holds r.mu.
func (r *Registry) pinLocked(h Handle) {
	if h == "" {
		return
	}
	r.children[h]++
	if r.children[h] > 1 || r.live[h] != nil {
		return
	}
	if a, ok := r.history.take(h); ok {
		r.live[h] = a
		r.pinLocked(a.opener)
	}
}

// releaseLocked moves the admission of h to the history once it is
// neither pending, listed nor the opener of a live one, and so on up its
// chain. The caller holds r.mu.
func (r *Registry) releaseLocked(h Handle) {
	for h != "" {
		a := r.live[h]
		if a == nil || a.disposition != Ended || r.children[h] > 0 {
			return
		}
		delete(r.live, h)
		r.history.put(h, a)
		opener := a.opener
		if opener == "" {
			return
		}
		if r.children[opener]--; r.children[opener] > 0 {
			return
		}
		delete(r.children, opener)
		h = opener
	}
}

// history is the admissions no longer live, the newest limit of them.
type history struct {
	limit   int
	next    uint64
	entries map[Handle]historyEntry
	// order is the handles put, oldest first, each with the number it was
	// put under; one taken or put again since is skipped.
	order []historyKey
}

type historyEntry struct {
	admission *admission
	seq       uint64
}

type historyKey struct {
	handle Handle
	seq    uint64
}

func (h *history) get(k Handle) (*admission, bool) {
	e, ok := h.entries[k]
	return e.admission, ok
}

func (h *history) take(k Handle) (*admission, bool) {
	e, ok := h.entries[k]
	delete(h.entries, k)
	return e.admission, ok
}

func (h *history) put(k Handle, a *admission) {
	h.next++
	h.entries[k] = historyEntry{admission: a, seq: h.next}
	h.order = append(h.order, historyKey{handle: k, seq: h.next})
	for len(h.entries) > h.limit {
		oldest := h.order[0]
		h.order[0] = historyKey{}
		h.order = h.order[1:]
		if e, ok := h.entries[oldest.handle]; ok && e.seq == oldest.seq {
			delete(h.entries, oldest.handle)
		}
	}
	if len(h.order) > 2*h.limit {
		kept := h.order[:0]
		for _, key := range h.order {
			if e, ok := h.entries[key.handle]; ok && e.seq == key.seq {
				kept = append(kept, key)
			}
		}
		h.order = kept
	}
}

// retained answers how many admissions are live and how many the history
// keeps.
func (r *Registry) retained() (live, kept int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live), len(r.history.entries)
}

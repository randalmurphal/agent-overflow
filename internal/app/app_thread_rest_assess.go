package app

import (
	"log"
	"maps"
	"sync"

	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/store"
)

// A decision on rest that cannot be taken back, a held request settling
// `finished` (heldThreadAtRest) or the retention sweep clearing a spawn
// past the floor (sweepThreadRequestsBefore), reads the rest of the thread
// in question and of every thread a local spawn link with a run of its
// caller's open reaches below it (store.ReadSubtreeRest),
// from those threads' own sources (docs/specs/standing-agents.md, "Owned
// workers"). It reads neither liveWorkers nor the open_below marks, which
// only draw rows, frames and summaries, and writes neither.
//
// The decision is made twice. Before its write it reads the subtree and its
// durable sources (store.ReadSubtreeRest), among them each thread's
// latest turn row and its workflow runs' holds, notes the reached threads'
// revisions (restRevisions), then reads what memory holds of them: a turn
// triage holds, a queued message, a prompt, a provider timer, a call parked
// on a send or ask, Codex's background work. The write
// carries the rest as its precondition (store.RestGuard): inside its own
// transaction the store reads the subtree and its durable sources again,
// and the write happens only if they read at rest, no thread is reached
// that the first read did not reach, and no reached thread's revision
// moved. A durable change is so either read there or ordered after the
// write; a change to memory is covered by its owner's bump. Only a change
// to memory between that comparison and the commit is outside it.

// restRevisions is the revision of what memory holds of a thread's rest,
// for the threads a decision in flight watches: a memory owner names the
// thread and bumps it after each change it makes (noteThreadRestChange).
// It is a leaf: its lock is held across no call and no store read, so a
// write's transaction may read it. A thread nobody watches costs a bump a
// map miss and keeps nothing.
type restRevisions struct {
	mu      sync.Mutex
	next    uint64
	watched map[string]*restRevision
}

// restRevision is one watched thread's revision and how many decisions
// watch it.
type restRevision struct {
	rev      uint64
	watchers int
}

func (r *restRevisions) bump(threadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if watched := r.watched[threadID]; watched != nil {
		r.next++
		watched.rev = r.next
	}
}

// watch starts watching threads and returns each one's revision now.
func (r *restRevisions) watch(threads []string) map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.watched == nil {
		r.watched = map[string]*restRevision{}
	}
	noted := make(map[string]uint64, len(threads))
	for _, id := range threads {
		watched := r.watched[id]
		if watched == nil {
			watched = &restRevision{}
			r.watched[id] = watched
		}
		watched.watchers++
		noted[id] = watched.rev
	}
	return noted
}

// unwatch ends a watch watch started.
func (r *restRevisions) unwatch(noted map[string]uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range noted {
		if watched := r.watched[id]; watched != nil {
			if watched.watchers--; watched.watchers == 0 {
				delete(r.watched, id)
			}
		}
	}
}

// moved names the threads of noted whose revision moved since.
func (r *restRevisions) moved(noted map[string]uint64) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for id, rev := range noted {
		if watched := r.watched[id]; watched == nil || watched.rev != rev {
			out[id] = true
		}
	}
	return out
}

// noteThreadRestChange bumps threadID's revision: something memory holds of
// its rest changed. Its owner calls it after the change.
func (a *App) noteThreadRestChange(threadID string) {
	a.restRevisions.bump(threadID)
}

// restDecision is one decision on the rest of some roots: the first read,
// held until done, and the check its write carries (guard).
type restDecision struct {
	app *App
	// reached is each root's subtree as the first read walked it.
	reached map[string][]string
	// unarmed is, by thread, the open sends and asks with no wake armed
	// whose parked calls the first read checked.
	unarmed map[string]map[string]bool
	atRest  map[string]bool
	noted   map[string]uint64
	once    sync.Once
}

// readRest reads whether each of roots, and every thread a local spawn link
// reaches below it, is at rest by its own sources and has no open work
// outstanding. The decision is always returned, to be done; a root whose
// read failed is not at rest, and the error names each such thread.
func (a *App) readRest(roots []string) (*restDecision, error) {
	d := &restDecision{app: a, atRest: map[string]bool{}, unarmed: map[string]map[string]bool{}}
	subtree, err := a.store.ReadSubtreeRest(roots)
	if err != nil {
		return d, err
	}
	d.reached = subtree.Reached
	threads := make([]string, 0, len(subtree.Sources))
	for id := range subtree.Sources {
		threads = append(threads, id)
	}
	// Noted before memory is read: a change after the note bumps it.
	d.noted = a.restRevisions.watch(threads)
	rests := make(map[string]bool, len(threads))
	for _, id := range threads {
		src := subtree.Sources[id]
		checked := make(map[string]bool, len(src.Unarmed))
		for _, token := range src.Unarmed {
			checked[token] = true
		}
		d.unarmed[id] = checked
		rests[id] = a.restsWithin(id, src)
	}
	for _, root := range roots {
		at := len(subtree.Reached[root]) > 0
		for _, id := range subtree.Reached[root] {
			at = at && rests[id]
		}
		d.atRest[root] = at
	}
	if hook := a.restReadForTest; hook != nil {
		hook(roots)
	}
	return d, nil
}

// restsWithin reports whether a reached thread is at rest and has no open
// work outstanding: no request it made that is open work, no send or ask
// of it a call is parked on, and its own sources at rest (ownRest).
func (a *App) restsWithin(threadID string, src store.ThreadRestSources) bool {
	if src.OpenWork {
		return false
	}
	for _, token := range src.Unarmed {
		if a.parkedOn(token) {
			return false
		}
	}
	return a.ownRest(threadID, src).unrest == ""
}

// rests reports whether root read at rest; false for no read.
func (d *restDecision) rests(root string) bool {
	return d != nil && d.atRest[root]
}

// guard is the precondition the write for roots carries: those of them
// that read at rest, read again inside the write's transaction (stillAtRest).
func (d *restDecision) guard(roots ...string) *store.RestGuard {
	var at []string
	for _, root := range roots {
		if d.rests(root) {
			at = append(at, root)
		}
	}
	return &store.RestGuard{Roots: at, AtRest: d.stillAtRest}
}

// stillAtRest is the guard's check, inside the write's transaction, of the
// subtrees and durable sources that transaction read: a root is still at
// rest when every thread it reaches was reached by the first read, reads at
// rest by its durable sources (no open turn row, no source restWaits
// counts) with no open work and no open send or ask the first read did not
// check for a parked call, and kept its revision. It reads only the
// revision record.
func (d *restDecision) stillAtRest(subtree store.SubtreeRest) []string {
	moved := d.app.restRevisions.moved(d.noted)
	var out []string
	for root, reached := range subtree.Reached {
		at := d.rests(root)
		for _, id := range reached {
			if !at {
				break
			}
			_, read := d.noted[id]
			src := subtree.Sources[id]
			waits, _ := restWaits(src, memoryWaits{}, nil)
			at = read && !moved[id] && !src.TurnOpen && !src.OpenWork && waits.unrest == ""
			for _, token := range src.Unarmed {
				at = at && d.unarmed[id][token]
			}
		}
		if at {
			out = append(out, root)
		}
	}
	return out
}

// done ends the decision; its threads are no longer watched for it.
func (d *restDecision) done() {
	if d != nil {
		d.once.Do(func() { d.app.restRevisions.unwatch(d.noted) })
	}
}

// sweepThreadRequestsBefore deletes the requests past floor, and clears the
// spawns past it whose threads here, and every thread below them, read at
// rest, read again inside the sweep's write transaction (RestGuard). A
// thread whose read failed keeps its spawns whole; the failure is logged
// and the next sweep reads again.
func (a *App) sweepThreadRequestsBefore(floor int64) (swept int64, released []string, err error) {
	roots, err := a.store.SpawnThreadsPastFloor(floor)
	if err != nil {
		logThreadRequestSweep("list the spawns past retention", err)
	}
	var rest *store.RestGuard
	if len(roots) > 0 {
		decision, err := a.readRest(roots)
		defer decision.done()
		if err != nil {
			logThreadRequestSweep("read the rest of the threads of spawns past retention", err)
		}
		rest = decision.guard(roots...)
	}
	if rest == nil {
		rest = &store.RestGuard{}
	}
	if !buildvariant.RemoteAccess || a.backends == nil {
		return a.store.DeleteThreadRequestsBefore(floor, rest)
	}
	// There is no peer to reconcile after an explicit unpair. Its historical
	// rows still identify the surviving worker until ordinary retention.
	computers, err := a.store.OwnerComputers()
	if err != nil {
		return 0, nil, err
	}
	for _, id := range computers {
		unlock := a.lockComputerPairing(id)
		paired, err := a.pairingSaved(id)
		if err != nil {
			unlock()
			return 0, nil, err
		}
		if paired {
			unlock()
		} else {
			defer unlock()
			rest.ForgottenComputers = append(rest.ForgottenComputers, id)
		}
	}
	o := &a.ownerUpdates
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, p := range o.peers {
		if p.legacy || p.ready {
			if !p.legacy {
				select {
				case <-p.disconnected:
					continue
				default:
				}
			}
			rest.ReconciledComputers = append(rest.ReconciledComputers, id)
			for token := range p.owed {
				rest.OwedRemoteTokens = append(rest.OwedRemoteTokens, token)
			}
		}
	}
	return a.store.DeleteThreadRequestsBefore(floor, rest)
}

// noteRestRead records how a held request's attempt to settle on
// threadID's rest went: one whose read failed is read again by the request
// sweep's next pass (retryFailedRestReads), which recording it wakes,
// however long no change comes; one that completed, settled or found the
// thread not at rest, ends that.
func (a *App) noteRestRead(threadID string, err error) {
	r := &a.threadRequests
	r.mu.Lock()
	if err == nil {
		delete(r.rest.failed, threadID)
		r.mu.Unlock()
		return
	}
	if r.rest.failed == nil {
		r.rest.failed = map[string]bool{}
	}
	r.rest.failed[threadID] = true
	r.mu.Unlock()
	a.nudgeThreadRequestSweep()
}

// retryFailedRestReads reads again, as one pass, the rest of the threads
// whose held requests' last attempt failed. Each stays owed until its
// attempt completes (noteRestRead); a thread none of whose requests is
// still held is dropped. It runs on the request sweep, one at a time.
func (a *App) retryFailedRestReads() {
	r := &a.threadRequests
	r.mu.Lock()
	if len(r.rest.failed) == 0 || r.rest.barred {
		r.mu.Unlock()
		return
	}
	threads := maps.Clone(r.rest.failed)
	r.mu.Unlock()
	a.settleHeldReceiptsAtRest(threads)
	r.mu.Lock()
	defer r.mu.Unlock()
	for thread := range threads {
		if !r.holdsReceiptLocked(thread) {
			delete(r.rest.failed, thread)
		}
	}
}

// restReadsFailed reports whether a held request's read of rest failed and
// waits for the sweep to read it again.
func (a *App) restReadsFailed() bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	return len(a.threadRequests.rest.failed) > 0
}

// logRestRead logs a held request's failed read of rest.
func logRestRead(threadID, token string, err error) {
	log.Printf("thread tools: read whether %s is at rest for request %s: %v", threadID, token, err)
}

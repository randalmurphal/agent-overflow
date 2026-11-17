package app

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"agent-overflow/internal/store"
)

// A decision on rest that cannot be taken back, a held request settling
// `finished` (heldThreadAtRest) or the retention sweep clearing a spawn
// past the floor (restedSpawnsPastFloor), reads the rest of the thread in
// question and of every thread a local spawn link reaches below it, from
// those threads' own sources (docs/specs/standing-agents.md, "Owned
// workers"): one snapshot of their durable sources (store.ReadSubtreeRest),
// the rule each thread's own state answers by (ownRest), and the open work
// each has outstanding below it. It reads neither liveWorkers nor the
// open_below marks, which only draw rows, frames and summaries, and writes
// nothing.
//
// Work can be admitted to a reached thread after its sources were read.
// Every owner of a source signals each change after its write (the rest
// change notes below, raised where the Agents rows' drain is told), and a
// read takes the change count before it reads: the write that settles or
// clears is made only when no change to a reached thread was signalled
// since (restRead.stillAtRest). Otherwise the decision is taken again on
// the next pass, which the change itself asks for. A change elsewhere on
// the computer does not stop it.

// restChanges counts the signals of a change to what keeps a thread from
// rest. While a read is under way it keeps, by key, the count at each
// key's last change: a thread, a request's token or a workflow run, the
// last two named by the thread they belong to when a read compares
// (store.ThreadsOfRestChanges). With no read under way it keeps nothing.
type restChanges struct {
	mu      sync.Mutex
	count   uint64
	readers int
	threads map[string]uint64
	tokens  map[string]uint64
	runs    map[string]uint64
}

func (c *restChanges) begin() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readers++
	return c.count
}

func (c *restChanges) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readers--; c.readers == 0 {
		c.threads, c.tokens, c.runs = nil, nil, nil
	}
}

// note counts one change to key in set.
func (c *restChanges) note(set *map[string]uint64, key string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	if c.readers == 0 {
		return
	}
	if *set == nil {
		*set = map[string]uint64{}
	}
	(*set)[key] = c.count
}

// since lists the keys changed after count.
func (c *restChanges) since(count uint64) (threads map[string]bool, tokens, runs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	threads = map[string]bool{}
	for id, at := range c.threads {
		if at > count {
			threads[id] = true
		}
	}
	for token, at := range c.tokens {
		if at > count {
			tokens = append(tokens, token)
		}
	}
	for run, at := range c.runs {
		if at > count {
			runs = append(runs, run)
		}
	}
	return threads, tokens, runs
}

// noteThreadRestChange signals a change to one of threadID's sources.
func (a *App) noteThreadRestChange(threadID string) {
	a.restChanges.note(&a.restChanges.threads, threadID)
}

// noteRequestRestChange signals a change to the request token: its state,
// its wake, or a call parked on it.
func (a *App) noteRequestRestChange(token string) {
	a.restChanges.note(&a.restChanges.tokens, token)
}

// noteRunRestChange signals a change to the workflow run itemID: its state
// or the report it owes.
func (a *App) noteRunRestChange(itemID string) {
	a.restChanges.note(&a.restChanges.runs, itemID)
}

// restRead is one read of the rest of the threads reached from some roots,
// held open until done so the changes since it began are kept.
type restRead struct {
	app     *App
	since   uint64
	reached map[string][]string
	atRest  map[string]bool
	once    sync.Once
}

// readRest reads whether each of roots, and every thread a local spawn link
// reaches below it, is at rest by its own sources and has no open work
// outstanding. The read is always returned, to be done; a root whose read
// failed is not at rest, and the error names each such thread.
func (a *App) readRest(roots []string) (*restRead, error) {
	read := &restRead{app: a, since: a.restChanges.begin(), atRest: map[string]bool{}}
	subtree, err := a.store.ReadSubtreeRest(roots, a.workflowApplication().OwedReports())
	if err != nil {
		return read, err
	}
	read.reached = subtree.Reached
	threads := map[string]bool{}
	var errs []error
	for _, root := range roots {
		rests := true
		for _, id := range subtree.Reached[root] {
			at, known := threads[id]
			if !known {
				var err error
				if at, err = a.restsWithin(id, subtree.Sources[id]); err != nil {
					errs = append(errs, fmt.Errorf("read whether %s, below %s, is at rest: %w", id, root, err))
				}
				threads[id] = at
			}
			if rests = at; !rests {
				break
			}
		}
		read.atRest[root] = rests
	}
	if hook := a.restReadForTest; hook != nil {
		hook(roots)
	}
	return read, errors.Join(errs...)
}

// restsWithin reports whether a reached thread is at rest and has no open
// work outstanding: no request it made that is open work, no send or ask
// of it a call is parked on, and its own sources at rest (ownRest).
func (a *App) restsWithin(threadID string, src store.ThreadRestSources) (bool, error) {
	if src.OpenWork {
		return false, nil
	}
	for _, token := range src.Unarmed {
		if a.parkedOn(token) {
			return false, nil
		}
	}
	rest, err := a.ownRest(threadID, src)
	if err != nil {
		return false, err
	}
	return rest.unrest == "", nil
}

// rests reports whether root read at rest; false for no read.
func (r *restRead) rests(root string) bool {
	return r != nil && r.atRest[root]
}

// stillAtRest lists those of roots that read at rest and whose reached
// threads no change was signalled to since the read began. The changed
// requests and workflow runs are named by their threads in one read.
func (r *restRead) stillAtRest(roots ...string) ([]string, error) {
	changed, tokens, runs := r.app.restChanges.since(r.since)
	threads, err := r.app.store.ThreadsOfRestChanges(tokens, runs)
	if err != nil {
		return nil, fmt.Errorf("read which threads the changes since the read of rest reach: %w", err)
	}
	for _, id := range threads {
		changed[id] = true
	}
	var out []string
	for _, root := range roots {
		still := r.atRest[root] && !changed[root]
		for _, id := range r.reached[root] {
			still = still && !changed[id]
		}
		if still {
			out = append(out, root)
		}
	}
	return out, nil
}

// unchanged reports whether root read at rest and still does.
func (r *restRead) unchanged(root string) (bool, error) {
	if !r.rests(root) {
		return false, nil
	}
	still, err := r.stillAtRest(root)
	return len(still) == 1, err
}

// done ends the read; the changes since it began are no longer kept for it.
func (r *restRead) done() {
	if r != nil {
		r.once.Do(r.app.restChanges.end)
	}
}

// restedSpawnsPastFloor reads the rest of the threads of the spawns past
// floor and every thread below them, for the retention sweep: the spawns
// it may clear, named at the write's moment by those whose threads read at
// rest and took no work since (store.RestedSpawns). A spawn whose read
// failed is not named; the failure is logged and the next sweep reads
// again. done ends the read once the sweep's write is made.
func (a *App) restedSpawnsPastFloor(floor int64) (rested store.RestedSpawns, done func()) {
	spawns, err := a.store.SpawnsPastFloor(floor)
	if err != nil {
		logThreadRequestSweep("list the spawns past retention", err)
		return nil, func() {}
	}
	if len(spawns) == 0 {
		return nil, func() {}
	}
	var roots []string
	seen := map[string]bool{}
	for _, spawn := range spawns {
		if !seen[spawn.ThreadID] {
			seen[spawn.ThreadID] = true
			roots = append(roots, spawn.ThreadID)
		}
	}
	read, err := a.readRest(roots)
	if err != nil {
		logThreadRequestSweep("read the rest of the threads of spawns past retention", err)
	}
	return func() ([]string, error) {
		still, err := read.stillAtRest(roots...)
		if err != nil {
			return nil, err
		}
		at := map[string]bool{}
		for _, id := range still {
			at[id] = true
		}
		var tokens []string
		for _, spawn := range spawns {
			if at[spawn.ThreadID] {
				tokens = append(tokens, spawn.Token)
			}
		}
		return tokens, nil
	}, read.done
}

// noteRestRead records how a held request's read of threadID's rest went:
// one that failed is read again by the request sweep's next pass
// (retryFailedRestReads), however long no change comes; one that read
// ends that.
func (a *App) noteRestRead(threadID string, err error) {
	r := &a.threadRequests
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.rest.failed, threadID)
		return
	}
	if r.rest.failed == nil {
		r.rest.failed = map[string]bool{}
	}
	r.rest.failed[threadID] = true
}

// retryFailedRestReads reads again, as one pass, the rest of the threads
// whose held requests' last read failed. A thread none of whose requests
// is still held is dropped. It runs on the request sweep, one at a time.
func (a *App) retryFailedRestReads() {
	r := &a.threadRequests
	r.mu.Lock()
	if len(r.rest.failed) == 0 || r.rest.barred {
		r.mu.Unlock()
		return
	}
	threads := r.rest.failed
	r.rest.failed = nil
	r.mu.Unlock()
	a.settleHeldReceiptsAtRest(threads)
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

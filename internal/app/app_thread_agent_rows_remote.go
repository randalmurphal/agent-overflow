package app

import (
	"context"
	"errors"
	"log"
	"reflect"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A pane reads the last tree for each remote spawn and schedules a bounded
// detail read. Owner changes invalidate that tree independently of its age.

const (
	// threadAgentPeerListsInFlight bounds the agent_rows calls in flight
	// at once, across every listing.
	threadAgentPeerListsInFlight = 8
	// threadAgentPeerListsPerComputer bounds those calls to one computer.
	threadAgentPeerListsPerComputer = 2
	// threadAgentPeerRefreshEvery is the shortest gap between two reads of
	// one spawn's rows a pane's reads start: the frame an answer emits
	// brings the pane back, and that read must not ask again.
	threadAgentPeerRefreshEvery = time.Second
	// threadAgentPeerCacheLimit bounds the remembered answers and the
	// spawns waiting to be asked. The oldest answer goes first; a spawn
	// still listed is asked again.
	threadAgentPeerCacheLimit = 256
	// A computer whose call timed out waits threadAgentPeerBackoffFirst
	// before it is asked again, doubling on each timeout up to
	// threadAgentPeerBackoffMax, until it answers.
	threadAgentPeerBackoffFirst = threadAgentPeerListTimeout
	threadAgentPeerBackoffMax   = time.Minute
)

// threadAgentPeerCache is the last agent_rows answer for each remote spawn,
// by the spawn's token, and the asking of them.
type threadAgentPeerCache struct {
	mu      sync.Mutex
	entries map[string]*threadAgentPeerEntry
	// waiting is the spawns to ask about, oldest first; queued holds
	// their tokens, and running the tokens being asked now. again holds the
	// running spawns a refresh asked about meanwhile: a change may have
	// landed after their call read the rows, so each is asked once more.
	waiting []store.ThreadRequest
	queued  map[string]bool
	running map[string]bool
	again   map[string]store.ThreadRequest
	// inFlight counts the calls running, and perComputer those to each
	// computer.
	inFlight    int
	perComputer map[string]int
	backoff     map[string]threadAgentPeerBackoff
	// rereadCallers are the callers of spawns dropped from a full queue:
	// each is told to read again once there is room.
	rereadCallers map[string]struct{}
	// nudgesTaken holds, by spawn token, the ended nudge runs recorded here
	// from the latest answer about the spawn, which its next read names so
	// its computer forgets them (threadAgentPeerArgs.NudgesTaken). A read
	// whose answer is lost names them again; naming one twice is harmless.
	nudgesTaken map[string]peerNudgesTaken
}

// peerNudgesTaken is the ended nudge runs recorded here from one answer of
// computer.
type peerNudgesTaken struct {
	computer string
	tokens   []string
}

// noteNudgesTaken records the ended nudge runs of spawn's latest answer
// that were recorded here, replacing those of the answer before: that
// computer reported no run it had already forgotten.
func (c *threadAgentPeerCache) noteNudgesTaken(spawn store.ThreadRequest, taken []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(taken) == 0 {
		delete(c.nudgesTaken, spawn.Token)
		return
	}
	if c.nudgesTaken == nil {
		c.nudgesTaken = map[string]peerNudgesTaken{}
	}
	c.nudgesTaken[spawn.Token] = peerNudgesTaken{computer: spawn.TargetComputerID, tokens: taken}
}

// nudgesTakenOf is what spawn's next read names as taken.
func (c *threadAgentPeerCache) nudgesTakenOf(spawn string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.nudgesTaken[spawn].tokens)
}

type threadAgentPeerEntry struct {
	spawn   store.ThreadRequest
	listing ThreadAgentRows
	err     error
	at      time.Time
}

type threadAgentPeerBackoff struct {
	until time.Time
	next  time.Duration
}

// threadAgentPeerAnswer is one agent_rows answer: the rows, or why there
// are none.
type threadAgentPeerAnswer struct {
	listing ThreadAgentRows
	err     error
}

// cachedPeerAgentRows is the last answer for a remote spawn, and whether
// one has arrived.
func (a *App) cachedPeerAgentRows(spawn store.ThreadRequest) (threadAgentPeerAnswer, bool) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[spawn.Token]
	if !ok {
		return threadAgentPeerAnswer{}, false
	}
	return threadAgentPeerAnswer{listing: entry.listing, err: entry.err}, true
}

// cachedPeerSpawnFor finds the remote spawn whose last answer lists token.
func (a *App) cachedPeerSpawnFor(token string) (store.ThreadRequest, bool) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		for _, row := range entry.listing.Rows {
			if row.Token == token {
				return entry.spawn, true
			}
		}
	}
	return store.ThreadRequest{}, false
}

// refreshPairedPeerAgentRows queues a paired spawn unless it is already
// queued, its answer is newer than minAge, or its computer is backing off.
// A spawn being asked now is marked to be asked once more when its call
// returns. A full queue drops its oldest spawn, whose caller is told to
// read again once there is room.
func (a *App) refreshPairedPeerAgentRows(spawn store.ThreadRequest, minAge time.Duration) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.lifeCtx().Err() != nil || c.queued[spawn.Token] {
		return
	}
	if entry, ok := c.entries[spawn.Token]; ok && time.Since(entry.at) < minAge {
		return
	}
	if c.running[spawn.Token] {
		c.again[spawn.Token] = spawn
		return
	}
	a.queuePeerAgentRowsLocked(spawn)
}

// queuePeerAgentRowsLocked queues one spawn and starts what there is room
// for, unless the app is shutting down or its computer is backing off.
func (a *App) queuePeerAgentRowsLocked(spawn store.ThreadRequest) {
	c := &a.threadAgentPeers
	if a.lifeCtx().Err() != nil || c.backingOffLocked(spawn.TargetComputerID) {
		return
	}
	if c.queued == nil {
		c.queued, c.running, c.again = map[string]bool{}, map[string]bool{}, map[string]store.ThreadRequest{}
		c.perComputer, c.backoff = map[string]int{}, map[string]threadAgentPeerBackoff{}
		c.rereadCallers = map[string]struct{}{}
	}
	if len(c.waiting) >= threadAgentPeerCacheLimit {
		dropped := c.waiting[0]
		c.waiting = append(c.waiting[:0], c.waiting[1:]...)
		delete(c.queued, dropped.Token)
		if len(c.rereadCallers) < threadAgentPeerCacheLimit {
			c.rereadCallers[dropped.CallerThreadID] = struct{}{}
		}
	}
	c.waiting = append(c.waiting, spawn)
	c.queued[spawn.Token] = true
	a.startPeerAgentRowsLocked()
}

func (c *threadAgentPeerCache) backingOffLocked(computerID string) bool {
	backoff, ok := c.backoff[computerID]
	return ok && time.Now().Before(backoff.until)
}

// startPeerAgentRowsLocked starts the waiting calls there is room for, in
// queue order, skipping a computer at its own bound and dropping a spawn
// whose computer is backing off.
func (a *App) startPeerAgentRowsLocked() {
	c := &a.threadAgentPeers
	if a.lifeCtx().Err() != nil {
		return
	}
	kept := c.waiting[:0]
	for _, spawn := range c.waiting {
		computer := spawn.TargetComputerID
		switch {
		case c.backingOffLocked(computer):
			delete(c.queued, spawn.Token)
		case c.inFlight >= threadAgentPeerListsInFlight || c.perComputer[computer] >= threadAgentPeerListsPerComputer:
			kept = append(kept, spawn)
		default:
			delete(c.queued, spawn.Token)
			if !a.threadRequestsWG.Go(func() { a.askPeerAgentRows(spawn) }) {
				// Shutting down: nothing more starts.
				continue
			}
			c.running[spawn.Token] = true
			c.inFlight++
			c.perComputer[computer]++
		}
	}
	clear(c.waiting[len(kept):])
	c.waiting = kept
}

// askPeerAgentRows runs one queued call and records its answer.
func (a *App) askPeerAgentRows(spawn store.ThreadRequest) {
	a.readPeerAgentRows(a.lifeCtx(), spawn, threadAgentRowDepth-1)
	c := &a.threadAgentPeers
	c.mu.Lock()
	c.inFlight--
	if c.perComputer[spawn.TargetComputerID]--; c.perComputer[spawn.TargetComputerID] <= 0 {
		delete(c.perComputer, spawn.TargetComputerID)
	}
	delete(c.running, spawn.Token)
	again, askAgain := c.again[spawn.Token]
	delete(c.again, spawn.Token)
	var reread []string
	if len(c.waiting) < threadAgentPeerCacheLimit {
		for caller := range c.rereadCallers {
			reread = append(reread, caller)
		}
		clear(c.rereadCallers)
	}
	if askAgain {
		a.queuePeerAgentRowsLocked(again)
	}
	a.startPeerAgentRowsLocked()
	c.mu.Unlock()
	for _, caller := range reread {
		a.noteThreadRequestChanged(store.ThreadRequestChange{CallerThreadID: caller})
	}
}

// readPeerAgentRows orders pane and forwarded detail reads with the owner's
// state reads. A reply superseded while it was in flight uses the current tree.
func (a *App) readPeerAgentRows(ctx context.Context, spawn store.ThreadRequest, depth int) threadAgentPeerAnswer {
	read := a.ownerReads.start(ctx, spawn.TargetComputerID, []string{spawn.Token})
	defer read.close()
	listing, origin, err := a.peerThreadAgentRows(ctx, spawn, depth)
	answer := threadAgentPeerAnswer{listing: listing, err: err}
	if ctx.Err() != nil {
		return answer
	}
	c := &a.threadAgentPeers
	unlock := a.lockComputerPairing(spawn.TargetComputerID)
	paired, lookupErr := a.peerPaired(spawn.TargetComputerID)
	if lookupErr != nil {
		unlock()
		return threadAgentPeerAnswer{err: lookupErr}
	}
	if !paired {
		unlock()
		return threadAgentPeerAnswer{err: errThreadAgentPeerUnpaired}
	}
	changed := false
	var prompts <-chan error
	commit := func() error {
		if err == nil || threadAgentPeerRefused(err) {
			if _, writeErr := a.store.SetThreadRequestNestedOpen(spawn.Token, err == nil && peerListingOpen(listing)); writeErr != nil {
				return writeErr
			}
			if err == nil {
				if writeErr := a.endRemoteRunsAtRest(spawn, listing); writeErr != nil {
					return writeErr
				}
			}
			prompts = a.noteRemotePrompts(read.delivery(), origin, spawn, listing, err)
		}
		c.mu.Lock()
		changed = c.noteAnswerLocked(spawn, listing, err, depth <= 0 && listing.Open != nil)
		entry := c.entries[spawn.Token]
		answer = threadAgentPeerAnswer{listing: entry.listing, err: entry.err}
		c.mu.Unlock()
		return nil
	}
	applyErr := read.commit(origin, "", err, commit)
	unlock()
	if applyErr != nil {
		if errors.Is(applyErr, errReadSuperseded) || errors.Is(applyErr, errPeerReadInvalidated) {
			if kept, ok := a.cachedPeerAgentRows(spawn); ok {
				return kept
			}
		}
		return threadAgentPeerAnswer{err: applyErr}
	}
	if prompts != nil {
		select {
		case err := <-prompts:
			if err != nil {
				return threadAgentPeerAnswer{err: err}
			}
		case <-ctx.Done():
			return threadAgentPeerAnswer{err: ctx.Err()}
		}
	}
	if changed {
		a.projectPeerRows(spawn.CallerThreadID)
	}
	return answer
}

// peerPaired reports whether computerID is paired here, false with the
// error when the lookup fails.
func (a *App) peerPaired(computerID string) (bool, error) {
	if a.backends == nil {
		return false, nil
	}
	_, found, err := a.backends.Lookup(computerID)
	return err == nil && found, err
}

// peerListingOpen reports whether an agent_rows answer says the spawn's
// thread is open: not at rest, or with open work at any depth below it.
// Every computer that answers agent_rows says so.
func peerListingOpen(listing ThreadAgentRows) bool {
	return listing.Open != nil && *listing.Open
}

// threadAgentPeerRefused reports whether an agent_rows call was refused as
// asking about a thread that is not the spawn's there: that computer's
// answer that it holds nothing of it, never a failure to answer.
func threadAgentPeerRefused(err error) bool {
	code, _, ok := errorsx.PublicDetails(err)
	return ok && code == threadtools.CodeNotYours
}

// noteAnswerLocked caches one answer, keeps the cache to its bound by
// dropping the oldest other answers, and moves the computer's backoff: a
// timeout once the last backoff passed lengthens it, an answer clears it.
// An answer of the thread's state alone (stateOnly) keeps the rows the
// last one listed, and their age, so a read that nests them asks for them
// again on its own pace. It reports whether the answer changed what the
// caller's pane shows.
func (c *threadAgentPeerCache) noteAnswerLocked(spawn store.ThreadRequest, listing ThreadAgentRows, err error, stateOnly bool) bool {
	if c.entries == nil {
		c.entries = map[string]*threadAgentPeerEntry{}
	}
	c.noteBackoffLocked(spawn.TargetComputerID, err)
	entry, ok := c.entries[spawn.Token]
	if !ok {
		entry = &threadAgentPeerEntry{}
		c.entries[spawn.Token] = entry
	}
	if stateOnly && ok && err == nil {
		listing.Rows, listing.Hidden = entry.listing.Rows, entry.listing.Hidden
	}
	changed := !ok || errorText(entry.err) != errorText(err) ||
		(err == nil && !reflect.DeepEqual(entry.listing, listing))
	// A failed call keeps the last answer, which the row still shows; a
	// refusal of the thread as not this spawn's is an answer, and leaves
	// nothing of it.
	entry.spawn, entry.err = spawn, err
	if !stateOnly {
		entry.at = time.Now()
	}
	if err == nil {
		entry.listing = listing
	} else if threadAgentPeerRefused(err) {
		entry.listing = ThreadAgentRows{}
	}
	for len(c.entries) > threadAgentPeerCacheLimit {
		oldest := ""
		for token, other := range c.entries {
			if token != spawn.Token && (oldest == "" || other.at.Before(c.entries[oldest].at)) {
				oldest = token
			}
		}
		delete(c.entries, oldest)
	}
	return changed
}

// noteBackoffLocked moves a computer's backoff for one call's outcome.
func (c *threadAgentPeerCache) noteBackoffLocked(computer string, err error) {
	if c.backoff == nil {
		c.backoff = map[string]threadAgentPeerBackoff{}
	}
	switch {
	case err == nil:
		delete(c.backoff, computer)
	case errors.Is(err, errThreadAgentPeerTimedOut) && !c.backingOffLocked(computer):
		// Calls that were in flight together time out together: one
		// outage lengthens the backoff once.
		backoff := c.backoff[computer]
		if backoff.next == 0 {
			backoff.next = threadAgentPeerBackoffFirst
		}
		backoff.until = time.Now().Add(backoff.next)
		backoff.next = min(2*backoff.next, threadAgentPeerBackoffMax)
		c.backoff[computer] = backoff
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// forgetPeerAgentRows forgets a computer no longer paired: its answers,
// its spawns waiting to be asked and its backoff here, nested_open on
// every spawn to it, so a settled one leaves its caller's Agents segment
// by the settled window and the sweep stops asking, and the prompts its
// workers put to threads here (forgetRemotePrompts). A call to it still in
// flight records nothing when it returns.
func (a *App) forgetPeerAgentRows(computerID string) {
	c := &a.threadAgentPeers
	a.ownerReads.mu.Lock()
	a.endOwnerGenerationLocked(computerID)
	c.mu.Lock()
	for token, entry := range c.entries {
		if entry.spawn.TargetComputerID == computerID {
			delete(c.entries, token)
		}
	}
	for token, taken := range c.nudgesTaken {
		if taken.computer == computerID {
			delete(c.nudgesTaken, token)
		}
	}
	kept := c.waiting[:0]
	for _, spawn := range c.waiting {
		if spawn.TargetComputerID == computerID {
			delete(c.queued, spawn.Token)
			continue
		}
		kept = append(kept, spawn)
	}
	clear(c.waiting[len(kept):])
	c.waiting = kept
	delete(c.backoff, computerID)
	c.mu.Unlock()
	callers, err := a.store.ForgetRemoteSpawnsOnComputer(computerID)
	// Behind every reconcile an answer recorded before this queued, and
	// ahead of none: no answer from it is recorded from here on.
	a.workerPrompts.run(func() { a.forgetRemotePrompts(computerID) })
	a.ownerReads.mu.Unlock()
	if err != nil {
		// The sweep clears what this could not: it drops a spawn whose
		// computer is no longer paired.
		log.Printf("agent rows: forget open work on computer %s: %v", computerID, err)
		return
	}
	for _, caller := range callers {
		a.noteThreadRequestChanged(store.ThreadRequestChange{CallerThreadID: caller})
	}
}

// forgetUnpairedPeerAgentRows forgets every computer this cache knows that
// is no longer paired: the backstop for a set change that names no
// machine. Spawns no answer here mentions are left to the sweep.
func (a *App) forgetUnpairedPeerAgentRows() {
	if a.backends == nil {
		return
	}
	c := &a.threadAgentPeers
	c.mu.Lock()
	known := map[string]bool{}
	for _, entry := range c.entries {
		known[entry.spawn.TargetComputerID] = true
	}
	for _, spawn := range c.waiting {
		known[spawn.TargetComputerID] = true
	}
	for computer := range c.backoff {
		known[computer] = true
	}
	c.mu.Unlock()
	for computer := range known {
		if _, found, err := a.backends.Lookup(computer); err != nil {
			log.Printf("agent rows: look up computer %s: %v", computer, err)
		} else if !found {
			a.forgetPeerAgentRows(computer)
		}
	}
}

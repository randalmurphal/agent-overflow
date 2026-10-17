package app

import (
	"context"
	"errors"
	"log"
	"reflect"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// The Agents rows a spawn on a paired computer nests, as this computer last
// heard them. A pane's read never waits on another computer: it nests the
// last agent_rows answer for each remote spawn and asks again in the
// background, and an answer that changes anything emits
// `thread:agent_rows_changed` for the spawn's caller, so the pane reads
// again. Every answer also records on the spawn's row whether that
// computer listed open work (`nested_open`), which keeps a settled remote
// spawn listed while it does; the poller asks again at its own pace while
// a row says so.
//
// The asking is bounded whatever the number of spawns or computers: one
// queue of spawns waiting to be asked, each once, at most
// threadAgentPeerCacheLimit of them; at most threadAgentPeerListsInFlight
// calls at once, threadAgentPeerListsPerComputer of them to one computer,
// so a computer that does not answer holds two slots, not all of them; and
// a computer whose last call timed out is not asked again until its
// backoff passes. A computer no longer paired is forgotten here and on its
// spawns' rows.

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
	// recording is held across an answer's pairing check and its record,
	// and across a forget, so an answer whose computer was forgotten while
	// its call ran is never recorded. It is taken before mu.
	recording sync.Mutex
	mu        sync.Mutex
	entries   map[string]*threadAgentPeerEntry
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
	// sweepAfter is the last spawn the sweep asked about; the next sweep
	// starts after it, and from the start once a page comes back short.
	sweepAfter store.ThreadRequest
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

// refreshPeerAgentRows queues the spawn to be asked about in the
// background, unless its computer is no longer paired.
func (a *App) refreshPeerAgentRows(spawn store.ThreadRequest, minAge time.Duration) {
	if paired, err := a.peerPaired(spawn.TargetComputerID); err != nil {
		log.Printf("agent rows: look up computer %s: %v", spawn.TargetComputerID, err)
	} else if paired {
		a.refreshPairedPeerAgentRows(spawn, minAge)
	}
}

// refreshPairedPeerAgentRows is refreshPeerAgentRows for a spawn whose
// computer the caller found paired. It queues the spawn unless it is
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
	listing, err := a.peerThreadAgentRows(a.lifeCtx(), spawn.TargetComputerID, spawn.MadeIn(),
		spawn.TargetThreadID, threadAgentRowDepth-1)
	shutdown := errors.Is(err, context.Canceled) && a.lifeCtx().Err() != nil
	c := &a.threadAgentPeers
	c.recording.Lock()
	record := !shutdown && a.peerStillPaired(spawn.TargetComputerID)
	if record {
		a.recordPeerOpenWork(spawn, listing, err)
	}
	c.mu.Lock()
	c.inFlight--
	if c.perComputer[spawn.TargetComputerID]--; c.perComputer[spawn.TargetComputerID] <= 0 {
		delete(c.perComputer, spawn.TargetComputerID)
	}
	delete(c.running, spawn.Token)
	again, askAgain := c.again[spawn.Token]
	delete(c.again, spawn.Token)
	changed := false
	if record {
		changed = c.noteAnswerLocked(spawn, listing, err)
	}
	var reread []string
	if len(c.waiting) < threadAgentPeerCacheLimit {
		for caller := range c.rereadCallers {
			reread = append(reread, caller)
		}
		clear(c.rereadCallers)
	}
	if askAgain && record {
		a.queuePeerAgentRowsLocked(again)
	}
	a.startPeerAgentRowsLocked()
	c.mu.Unlock()
	c.recording.Unlock()
	if changed {
		a.noteThreadRequestChanged(store.ThreadRequestChange{CallerThreadID: spawn.CallerThreadID})
	}
	for _, caller := range reread {
		a.noteThreadRequestChanged(store.ThreadRequestChange{CallerThreadID: caller})
	}
}

// notePeerAgentRows records an answer a live listing read itself.
func (a *App) notePeerAgentRows(spawn store.ThreadRequest, listing ThreadAgentRows, err error) {
	if errors.Is(err, context.Canceled) && a.lifeCtx().Err() != nil {
		return
	}
	c := &a.threadAgentPeers
	c.recording.Lock()
	if !a.peerStillPaired(spawn.TargetComputerID) {
		c.recording.Unlock()
		return
	}
	a.recordPeerOpenWork(spawn, listing, err)
	c.mu.Lock()
	changed := c.noteAnswerLocked(spawn, listing, err)
	c.mu.Unlock()
	c.recording.Unlock()
	if changed {
		a.noteThreadRequestChanged(store.ThreadRequestChange{CallerThreadID: spawn.CallerThreadID})
	}
}

// peerStillPaired reports whether an answer from computerID may be
// recorded. The caller holds recording, and every forget runs once the
// computer's profile is gone, so a computer found here is forgotten only
// after this answer is recorded. An answer whose lookup fails is dropped;
// the next read asks again.
func (a *App) peerStillPaired(computerID string) bool {
	paired, err := a.peerPaired(computerID)
	if err != nil {
		log.Printf("agent rows: look up computer %s: %v", computerID, err)
	}
	return paired
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

// recordPeerOpenWork writes whether an answer listed open work on the
// spawn's row. It runs before the answer is cached, so a read that nests
// the rows finds the spawn kept for them. A computer that did not answer
// leaves the last record.
func (a *App) recordPeerOpenWork(spawn store.ThreadRequest, listing ThreadAgentRows, err error) {
	if err != nil {
		return
	}
	open := false
	for _, row := range listing.Rows {
		open = open || row.SettledAt == 0
	}
	if _, setErr := a.store.SetThreadRequestNestedOpen(spawn.Token, open); setErr != nil {
		log.Printf("agent rows: record open work under %s: %v", spawn.Token, setErr)
	}
}

// noteAnswerLocked caches one answer, keeps the cache to its bound by
// dropping the oldest other answers, and moves the computer's backoff: a
// timeout once the last backoff passed lengthens it, an answer clears it. It reports whether the answer
// changed what the caller's pane shows.
func (c *threadAgentPeerCache) noteAnswerLocked(spawn store.ThreadRequest, listing ThreadAgentRows, err error) bool {
	if c.entries == nil {
		c.entries = map[string]*threadAgentPeerEntry{}
	}
	if c.backoff == nil {
		c.backoff = map[string]threadAgentPeerBackoff{}
	}
	computer := spawn.TargetComputerID
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
	entry, ok := c.entries[spawn.Token]
	if !ok {
		entry = &threadAgentPeerEntry{}
		c.entries[spawn.Token] = entry
	}
	changed := !ok || errorText(entry.err) != errorText(err) ||
		(err == nil && !reflect.DeepEqual(entry.listing, listing))
	entry.spawn, entry.at, entry.err = spawn, time.Now(), err
	if err == nil {
		entry.listing = listing
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

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// forgetPeerAgentRows forgets a computer no longer paired: its answers,
// its spawns waiting to be asked and its backoff here, and nested_open on
// every spawn to it, so a settled one leaves its caller's Agents segment
// by the settled window and the sweep stops asking. A call to it still in
// flight records nothing when it returns.
func (a *App) forgetPeerAgentRows(computerID string) {
	c := &a.threadAgentPeers
	c.recording.Lock()
	c.mu.Lock()
	for token, entry := range c.entries {
		if entry.spawn.TargetComputerID == computerID {
			delete(c.entries, token)
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
	callers, err := a.store.ClearThreadRequestsNestedOpenForComputer(computerID)
	c.recording.Unlock()
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

// refreshNestedOpenRemoteSpawns asks again about every settled remote spawn
// whose computer last listed open work, at the poll's pace, until it lists
// none. A spawn whose computer is no longer paired is forgotten instead.
func (a *App) refreshNestedOpenRemoteSpawns() {
	if a.backends == nil {
		return
	}
	c := &a.threadAgentPeers
	c.mu.Lock()
	after := c.sweepAfter
	c.mu.Unlock()
	rows, err := a.store.ListNestedOpenRemoteSpawns(after.SettledAt, after.Token, threadRequestPollBatch)
	if err != nil {
		logThreadRequestSweep("read remote spawns with open work", err)
		return
	}
	next := store.ThreadRequest{}
	if len(rows) == threadRequestPollBatch {
		next = rows[len(rows)-1]
	}
	c.mu.Lock()
	c.sweepAfter = store.ThreadRequest{SettledAt: next.SettledAt, Token: next.Token}
	c.mu.Unlock()
	paired := map[string]bool{}
	for _, row := range rows {
		known, seen := paired[row.TargetComputerID]
		if !seen {
			_, found, err := a.backends.Lookup(row.TargetComputerID)
			if err != nil {
				logThreadRequestSweep("look up a remote spawn's computer", err)
				continue
			}
			known, paired[row.TargetComputerID] = found, found
			if !found {
				a.forgetPeerAgentRows(row.TargetComputerID)
			}
		}
		if known {
			a.refreshPairedPeerAgentRows(row, a.threadPollNormal())
		}
	}
}

package app

import (
	"context"
	"errors"
	"log"
	"reflect"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// The Agents rows a spawn on a paired computer nests, as this computer last
// heard them. A pane's read never waits on another computer: it nests the
// last agent_rows answer for each remote spawn and asks again in the
// background, and an answer that changes anything emits
// `thread:agent_rows_changed` for the spawn's caller, so the pane reads
// again. Every answer also records on the spawn's row whether that
// computer said the spawn's thread is open, not at rest or with open work
// at any depth below it (`nested_open`, peerListingOpen), which keeps a
// settled remote spawn listed while it is;
// the poller asks again at its own pace while a row says so. The poll that
// collects a remote spawn's answer asks for its rows too, so a thread that
// keeps working after it answered is recorded as it settles. Nothing there
// tells this computer when a thread that rested is continued, so the sweep
// also asks about the settled remote spawns at rest, one page a
// threadAgentRestPassEvery from a cursor, each once a cycle, those the
// retention floor reduced to their lineage included: an answer that finds
// one still at rest is kept nowhere and frames nothing, one that finds it
// working records it as any answer does, and a refusal of its thread as
// gone deletes a spawn's lineage.
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
	// restAfter is sweepAfter for the spawns at rest, and restPassAt when
	// that pass last read a page. Whether there are any is the ledger's
	// (store.ThreadRequestSchedule.RestingRemote).
	restAfter  store.ThreadRequest
	restPassAt time.Time
	// probes are the queued or running spawns only the rest pass asked
	// about: an answer for one that finds it still at rest is not kept.
	probes map[string]bool
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
	// A read that wants the answer kept is no probe.
	delete(c.probes, spawn.Token)
	if c.running[spawn.Token] {
		c.again[spawn.Token] = spawn
		return
	}
	a.queuePeerAgentRowsLocked(spawn)
}

// probePairedPeerAgentRows queues a spawn the rest pass found at rest, for
// a computer the caller found paired, unless it is already queued or being
// asked about. One with an answer kept, which a pane or the poll asked for,
// is refreshed as they refresh it.
func (a *App) probePairedPeerAgentRows(spawn store.ThreadRequest) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	_, kept := c.entries[spawn.Token]
	busy := c.queued[spawn.Token] || c.running[spawn.Token]
	c.mu.Unlock()
	if busy {
		return
	}
	if kept {
		a.refreshPairedPeerAgentRows(spawn, a.threadAgentRestPassEvery())
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.lifeCtx().Err() != nil || c.queued[spawn.Token] || c.running[spawn.Token] {
		return
	}
	if c.probes == nil {
		c.probes = map[string]bool{}
	}
	c.probes[spawn.Token] = true
	a.queuePeerAgentRowsLocked(spawn)
	if !c.queued[spawn.Token] && !c.running[spawn.Token] {
		// Not queued: its computer is backing off, or the app is closing.
		delete(c.probes, spawn.Token)
	}
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
		if c.probes[dropped.Token] {
			// No pane is waiting on a probe: the next cycle asks again.
			delete(c.probes, dropped.Token)
		} else if len(c.rereadCallers) < threadAgentPeerCacheLimit {
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
			delete(c.probes, spawn.Token)
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
	listing, err := a.peerThreadAgentRows(a.lifeCtx(), spawn, threadAgentRowDepth-1)
	shutdown := errors.Is(err, context.Canceled) && a.lifeCtx().Err() != nil
	c := &a.threadAgentPeers
	c.recording.Lock()
	c.mu.Lock()
	// A probe that finds the thread still at rest, or gets no answer, has
	// nothing to record: the spawn's row already says it is at rest. A
	// refusal is recorded: the thread there is gone.
	probe := c.probes[spawn.Token] && !threadAgentPeerRefused(err) && (err != nil || !peerListingOpen(listing))
	delete(c.probes, spawn.Token)
	c.mu.Unlock()
	record := !shutdown && a.peerStillPaired(spawn.TargetComputerID)
	if record {
		a.applyPeerAgentRows(spawn, listing, err, probe)
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
	switch {
	case record && probe:
		c.noteBackoffLocked(spawn.TargetComputerID, err)
	case record:
		changed = c.noteAnswerLocked(spawn, listing, err, false)
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

// notePeerAgentRows records an answer a live listing read itself, stateOnly
// when it is the thread's state alone, and returns what the listing nests
// for the spawn: the answer, or for a call that failed the answer kept for
// it, as a pane's read nests it (cachedPeerAgentRows): the last one with
// the failure, and for a refusal none. An answer that is not recorded, at
// shutdown or from a computer no longer paired, is nested as it came.
func (a *App) notePeerAgentRows(spawn store.ThreadRequest, listing ThreadAgentRows, err error, stateOnly bool) threadAgentPeerAnswer {
	answer := threadAgentPeerAnswer{listing: listing, err: err}
	if errors.Is(err, context.Canceled) && a.lifeCtx().Err() != nil {
		return answer
	}
	c := &a.threadAgentPeers
	c.recording.Lock()
	if !a.peerStillPaired(spawn.TargetComputerID) {
		c.recording.Unlock()
		return answer
	}
	a.applyPeerAgentRows(spawn, listing, err, false)
	c.mu.Lock()
	changed := c.noteAnswerLocked(spawn, listing, err, stateOnly)
	if err != nil {
		kept := c.entries[spawn.Token]
		answer = threadAgentPeerAnswer{listing: kept.listing, err: kept.err}
	}
	c.mu.Unlock()
	c.recording.Unlock()
	if changed {
		a.noteThreadRequestChanged(store.ThreadRequestChange{CallerThreadID: spawn.CallerThreadID})
	}
	return answer
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

// applyPeerAgentRows is what every answer recorded here does, whoever
// asked: it records whether the spawn's thread is open, unless a probe
// found it still at rest (recordPeerOpenWork), and takes up the prompts its
// worker routed here (noteRemotePrompts). The caller holds recording, so an
// answer from a computer forgotten meanwhile does neither.
func (a *App) applyPeerAgentRows(spawn store.ThreadRequest, listing ThreadAgentRows, err error, probe bool) {
	if !probe {
		a.recordPeerOpenWork(spawn, listing, err)
	}
	a.noteRemotePrompts(spawn, listing, err)
}

// recordPeerOpenWork writes whether an answer said the spawn's thread is
// open (peerListingOpen) on the spawn's row. It runs before
// the answer is cached, so a read that nests the rows finds the spawn kept
// for them. A computer that did not answer
// leaves the last record. One that refused the thread as not this spawn's
// (its thread, and so its receipt, is gone there) will never list it
// again, so it records none, and a spawn past the retention floor, kept
// here only as that thread's link, is deleted.
func (a *App) recordPeerOpenWork(spawn store.ThreadRequest, listing ThreadAgentRows, err error) {
	open := false
	switch {
	case threadAgentPeerRefused(err):
		if _, delErr := a.store.DeleteRemoteSpawnLineage(spawn.Token); delErr != nil {
			log.Printf("agent rows: delete the lineage of %s: %v", spawn.Token, delErr)
		}
	case err != nil:
		return
	default:
		open = peerListingOpen(listing)
	}
	if _, setErr := a.store.SetThreadRequestNestedOpen(spawn.Token, open); setErr != nil {
		log.Printf("agent rows: record open work under %s: %v", spawn.Token, setErr)
	}
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
			delete(c.probes, spawn.Token)
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
// none, then about the ones at rest at the rest pass's
// (probeRestingRemoteSpawns). A spawn whose computer is no longer paired is
// forgotten instead.
func (a *App) refreshNestedOpenRemoteSpawns() {
	if a.backends == nil {
		return
	}
	defer a.probeRestingRemoteSpawns()
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

// threadAgentRestPassEvery is the rest pass's pace: one page of the remote
// spawns at rest every six poll intervals, 30 s at the normal poll.
func (a *App) threadAgentRestPassEvery() time.Duration {
	return 6 * a.threadPollNormal()
}

// probeRestingRemoteSpawns asks, once threadAgentRestPassEvery has passed
// since its last page, about the next page of settled remote spawns whose
// computer last listed nothing open there, from where the last page ended
// and from the start once a page comes back short. Its cost is one indexed
// page read a pass (idx_thread_requests_remote_rest) and at most
// threadRequestPollBatch agent_rows calls, through the same queue and
// bounds as every other: with n spawns at rest each is asked once every
// ceil(n/threadRequestPollBatch) passes. A computer that runs an older
// version answers that it cannot list, which records nothing.
func (a *App) probeRestingRemoteSpawns() {
	c := &a.threadAgentPeers
	c.mu.Lock()
	if time.Since(c.restPassAt) < a.threadAgentRestPassEvery() {
		c.mu.Unlock()
		return
	}
	c.restPassAt = time.Now()
	after := c.restAfter
	c.mu.Unlock()
	rows, err := a.store.ListRestingRemoteSpawns(after.SettledAt, after.Token, threadRequestPollBatch)
	if err != nil {
		logThreadRequestSweep("read remote spawns at rest", err)
		return
	}
	next := store.ThreadRequest{}
	if len(rows) == threadRequestPollBatch {
		next = rows[len(rows)-1]
	}
	c.mu.Lock()
	c.restAfter = store.ThreadRequest{SettledAt: next.SettledAt, Token: next.Token}
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
		}
		if known {
			a.probePairedPeerAgentRows(row)
		}
	}
}

// nextRestPass is when the rest pass reads its next page.
func (a *App) nextRestPass() time.Time {
	c := &a.threadAgentPeers
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restPassAt.Add(a.threadAgentRestPassEvery())
}

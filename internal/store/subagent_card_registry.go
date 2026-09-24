package store

import (
	"maps"
	"slices"
	"sync"
)

// subagentCards is the store's registry of card accumulators, one entry
// per thread with open cards or unflushed values.
type subagentCards struct {
	mu      sync.Mutex
	threads map[string]*cardThread
}

type cardThread struct {
	id string
	// Guarded by subagentCards.mu: refs counts open cards and operations
	// in flight; stale holds the stamps a recompute rewrote since the
	// last drain, invalid reports that one ran and all that it covered
	// the whole thread.
	refs    int
	stale   map[string]struct{}
	invalid bool
	all     bool
	// relive names the anchors whose agents may have started or stopped
	// since the last drain; reliveAll covers every card of the thread.
	relive    map[string]struct{}
	reliveAll bool

	// mu serializes the thread's item writes that hold it, and the card
	// resolves and flushes. It is taken before the writer connection,
	// never while holding it.
	mu      sync.Mutex
	stamps  map[string]*cardStamp
	handles map[*SubagentCard]struct{}
	// seeds are anchors a note left for the next flush to recompute that
	// no accumulator holds: the carrier a new resume prompt names.
	seeds map[string]struct{}
}

func (c *subagentCards) acquire(threadID string, create bool) *cardThread {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[threadID]
	if t == nil {
		if !create {
			return nil
		}
		if c.threads == nil {
			c.threads = make(map[string]*cardThread)
		}
		// The maps wait for the first card (OpenSubagentCard): an item
		// write without one holds the entry only for its lock.
		t = &cardThread{id: threadID}
		c.threads[threadID] = t
	}
	t.refs++
	return t
}

// release drops one reference; the caller holds t.mu. The last reference
// to a thread that holds nothing drops its entry.
func (c *subagentCards) release(t *cardThread) {
	idle := len(t.handles) == 0 && len(t.seeds) == 0
	for _, st := range t.stamps {
		if !idle {
			break
		}
		idle = !st.pending()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.refs--
	if t.refs == 0 && idle && c.threads[t.id] == t {
		delete(c.threads, t.id)
	}
}

// invalidate records, inside the transaction of a recompute, that the
// stamps ids were rewritten. The next card operation on the thread drops
// their accumulators and makes every card read its chain again. It takes
// only the registry lock, so a writer holding the connection can call it.
func (c *subagentCards) invalidate(threadID string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[threadID]
	if t == nil {
		return
	}
	t.invalid = true
	if t.stale == nil {
		t.stale = make(map[string]struct{}, len(ids))
	}
	for _, id := range ids {
		t.stale[id] = struct{}{}
	}
}

// invalidateThread is invalidate for every stamp of the thread: a writer
// rebuilt it (restampSubagentAggregatesTx).
func (c *subagentCards) invalidateThread(threadID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.threads[threadID]; t != nil {
		t.invalid, t.all = true, true
	}
}

// holds reports whether the registry has an entry for the thread: open
// cards, an operation in flight, or accumulators not flushed.
func (c *subagentCards) holds(threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threads[threadID] != nil
}

// relive records that the agents of anchors ids may have started or
// stopped running, or with ids nil that any agent of the thread may
// have: the next card operation makes the cards whose liveness they
// decide read their chain again (SubagentCard.live). It takes only the
// registry lock, so a writer holding the connection can call it; a
// transaction that rolls back after it costs a resolve, never a stale
// card.
func (c *subagentCards) relive(threadID string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[threadID]
	if t == nil {
		return
	}
	if ids == nil {
		t.reliveAll = true
		return
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if t.relive == nil {
			t.relive = make(map[string]struct{})
		}
		t.relive[id] = struct{}{}
	}
}

// resetAll is invalidateThread for every thread: the rows under the
// accumulators were replaced (RestoreFrom).
func (c *subagentCards) resetAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.threads {
		t.invalid, t.all = true, true
	}
}

// drain applies the invalidations recorded since the last operation. The
// caller holds t.mu and has begun its transaction, so every recompute it
// drains has committed or rolled back, but it cannot tell which: a
// retired accumulator holding values not flushed yet recomputes at its
// next flush (retire).
func (c *subagentCards) drain(t *cardThread) {
	c.mu.Lock()
	relive, reliveAll := t.relive, t.reliveAll
	t.relive, t.reliveAll = nil, false
	if !t.invalid {
		c.mu.Unlock()
		t.reresolve(relive, reliveAll)
		return
	}
	stale, all := t.stale, t.all
	t.stale, t.invalid, t.all = nil, false, false
	c.mu.Unlock()
	t.reresolve(relive, reliveAll)
	if all {
		stale = make(map[string]struct{}, len(t.stamps))
		for id := range t.stamps {
			stale[id] = struct{}{}
		}
	}
	t.retire(slices.Collect(maps.Keys(stale)), false)
}

// retire drops the accumulators of stamps a recompute rewrote and makes
// every card read its chain again. committed reports that the caller saw
// the recompute commit. Otherwise an accumulator holding values not
// flushed yet is kept to recompute at the next flush: if the recompute
// rolled back, its rows are in no stamp. The caller holds t.mu.
func (t *cardThread) retire(ids []string, committed bool) {
	for _, id := range ids {
		st := t.stamps[id]
		if st == nil {
			continue
		}
		if !committed && st.pending() {
			st.state, st.reached = cardRecompute, false
			continue
		}
		delete(t.stamps, id)
	}
	for _, st := range t.stamps {
		if st.round != nil && t.stamps[st.round.id] != st.round {
			st.round, st.state = nil, cardRecompute
		}
	}
	t.unresolve()
}

// unresolve makes every card of the thread read its chain at its next
// write. The caller holds t.mu.
func (t *cardThread) unresolve() {
	for h := range t.handles {
		h.resolved = false
	}
}

// reresolve makes the cards whose liveness an anchor in ids decides, or
// every card with all, read their chain at their next write. The caller
// holds t.mu.
func (t *cardThread) reresolve(ids map[string]struct{}, all bool) {
	if all {
		t.unresolve()
		return
	}
	for h := range t.handles {
		if _, named := ids[h.liveAnchor]; named && h.liveAnchor != "" {
			h.resolved = false
		}
	}
}

// snapshot records what applying a resolve, a write's notes and a flush
// can change, for a transaction that applies them before it commits
// (cardWrite.settle): the returned func puts them back. c is the write's
// card, or nil. The caller holds t.mu.
func (t *cardThread) snapshot(c *SubagentCard) func() {
	stamps, seeds := maps.Clone(t.stamps), maps.Clone(t.seeds)
	saved := make(map[*cardStamp]cardStamp, len(t.stamps))
	for _, st := range t.stamps {
		saved[st] = *st
	}
	card := func() {}
	if c != nil {
		levels, tray, orphan, resolved := slices.Clone(c.levels), c.tray, c.orphan, c.resolved
		live, liveAnchor := c.live, c.liveAnchor
		card = func() {
			c.levels, c.tray, c.orphan, c.resolved = levels, tray, orphan, resolved
			c.live, c.liveAnchor = live, liveAnchor
		}
	}
	return func() {
		for st, was := range saved {
			*st = was
		}
		t.stamps, t.seeds = stamps, seeds
		card()
	}
}

// uncovered reports an accumulator or seed holding changes no boot pass
// would recover once the agents stops names have stopped: one no live
// card reaches, the cards those agents kept live not counted. all
// reports that any agent of the thread may have stopped. The caller
// holds t.mu.
func (t *cardThread) uncovered(stops []string, all bool) bool {
	if !t.pending() {
		return false
	}
	if all || len(t.seeds) > 0 {
		return true
	}
	for _, st := range t.stamps {
		if st.pending() && !t.covered(st, stops) {
			return true
		}
	}
	return false
}

// covered reports whether a live card whose agent is not in stops reaches
// st: the boot pass recovers the stamps a live card reaches
// (SubagentCard.live). The caller holds t.mu.
func (t *cardThread) covered(st *cardStamp, stops []string) bool {
	for h := range t.handles {
		if !h.resolved || !h.live || h.orphan || slices.Contains(stops, h.liveAnchor) {
			continue
		}
		if h.tray == st {
			return true
		}
		for _, level := range h.levels {
			if level == st || level.round == st {
				return true
			}
		}
	}
	return false
}

// pending reports accumulators or seeds a flush would write. The caller
// holds t.mu.
func (t *cardThread) pending() bool {
	if len(t.seeds) > 0 {
		return true
	}
	for _, st := range t.stamps {
		if st.pending() {
			return true
		}
	}
	return false
}

// collect drops the accumulators no card reaches that hold nothing to
// flush. The caller holds t.mu.
func (t *cardThread) collect() {
	reached := make(map[*cardStamp]struct{})
	for h := range t.handles {
		for _, st := range h.levels {
			reached[st] = struct{}{}
			if st.round != nil {
				reached[st.round] = struct{}{}
			}
		}
		if h.tray != nil {
			reached[h.tray] = struct{}{}
		}
	}
	for id, st := range t.stamps {
		if _, ok := reached[st]; !ok && !st.pending() {
			delete(t.stamps, id)
		}
	}
}

// put stores st as the thread's accumulator for its id, in place when one
// is held, so every card that reaches it sees the new values.
func (t *cardThread) put(st *cardStamp) {
	if held := t.stamps[st.id]; held != nil {
		*held = *st
		return
	}
	t.stamps[st.id] = st
}

// linkRounds points each resumed root at its last round's carrier
// accumulator. A recompute gives every stored anchorable carrier one, so
// a root left without is one whose last round names a carrier not stored
// as an anchorable row: its rows count toward its transcript alone, as
// the recompute counts them, until the carrier's insert recomputes the
// family (cardWrite.inserted).
func (t *cardThread) linkRounds() {
	for _, st := range t.stamps {
		if st.roundID == "" {
			continue
		}
		st.round = t.stamps[st.roundID]
		st.roundID = ""
	}
}

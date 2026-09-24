package triage

import (
	"log"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// Row emission and the anchor refresh behind it.
//
// Every upsert or patch the router pushes from a write goes through
// emitItemUpsert / emitItemPatch. (A proposed plan is pushed from its
// own decorated read, GetThreadProposedPlanItem, by payload_items.go and
// the app layer; its stamped set is itself, so it has nothing to
// refresh.) Two rules keep every row a client holds at a revision it can
// prove (docs/architecture/thread-replica-sync.md §3.1):
//
//   - a row whose page read is decorated (store.ItemReadNeedsDecoration:
//     an anchor no clean stamp serves, a completion sibling, a plan) is
//     pushed as written but marked unstamped, and
//     noted so the next refresh pushes its page read. The write never
//     reads the decorated row itself: that read can walk the anchor's
//     descendants, and the writes that land here arrive at tens per
//     second on the provider event path. A field patch to such a row is
//     pushed the same way, because a patch replaces the client's meta
//     wholesale and the stored meta is not the decorated one. A stamped
//     anchor's stored row is its page read and goes out as written;
//   - a write stamps rows it did not touch (the parent anchor, resume
//     carriers, completion siblings), and the rows written under an agent
//     reach its card's stamp only when the thread's cards are flushed
//     (subagent_cards.go). So after a burst of writes the cards are
//     flushed, and the rows whose revision moved without a push of their
//     own, with the anchors whose stamp the flush changed, are read as a
//     page would and pushed again. That happens at quiet points, never
//     per write: wireRefreshQuiet after the last push on the thread, at
//     most wireRefreshMaxWait after the first, and synchronously at turn
//     completion and session teardown. The one exception is an agent's
//     first row: the cards are flushed and the anchor whose card it opens
//     (store.ListFirstChildWireAnchors) is pushed with it, so the card
//     appears with the agent's first activity.
//
// A wire row the emitter altered on purpose (blankedStreamingWireRow)
// carries store.UnstampedItemRev and takes neither path: the settle patch
// is what makes the client's copy a stored row again.
//
// The refresh timers are tracked by r.refreshWG, not r.settleWG: the
// settle drain runs on in-turn paths (persistOrUpdateCompletedTextItem)
// and must not sit out a quiet period. DrainWireItemRefresh is the
// shutdown drain.
const (
	wireRefreshQuiet   = time.Second
	wireRefreshMaxWait = 5 * time.Second
)

// wireItemRefresh is a thread's pending refresh: the rows pushed since
// the last flush, each at the revision the client was pushed, and the
// timer that will flush them. Held in r.wireRefresh under r.mu from the
// first note until a flush takes it.
type wireItemRefresh struct {
	emitted  map[string]int64
	timer    *time.Timer
	deadline time.Time
}

func (r *Router) emitItemUpsert(item store.Item) {
	r.emitItemUpserts(item.ThreadID, []store.Item{item})
}

// emitItemUpserts pushes rows written in one thread. A row with a
// decorated page read goes out as written but marked unstamped, and is
// noted at that revision so the next refresh reads it: the client never
// holds a provable revision for a row it saw undecorated.
func (r *Router) emitItemUpserts(threadID string, items []store.Item) {
	for _, item := range items {
		if item.Rev == store.UnstampedItemRev {
			r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(item))
			r.emitFirstChildAnchors(item)
			continue
		}
		if r.itemReadNeedsDecoration(item) {
			item.Rev = store.UnstampedItemRev
		}
		r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(item))
		r.noteWireItemEmitted(threadID, item.ID, item.Rev)
		r.emitFirstChildAnchors(item)
	}
}

// emitFirstChildAnchors pushes the anchors whose card the written child
// just opened. The probe runs once per parent in a session: once a
// parent's first child has been pushed, no later child of it can open its
// card, so later children cost no read. A prompt row is always probed,
// because it can open a resumed round's carrier under a parent seen
// before; there is one per round. A failed read leaves the anchors to the
// quiet-point refresh.
func (r *Router) emitFirstChildAnchors(child store.Item) {
	if child.ParentID == "" {
		return
	}
	if child.Kind != itemKindUserText && !r.claimFirstChildProbe(child.ThreadID, child.ParentID) {
		return
	}
	// The child is in its card, not yet in the stamp the probe reads.
	r.flushSubagentCards(child.ThreadID)
	anchors, err := r.store.ListFirstChildWireAnchors(child)
	if err != nil {
		log.Printf("triage: read first child anchors of %s/%s: %v", child.ThreadID, child.ID, err)
		return
	}
	for _, anchor := range anchors {
		r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(anchor))
		r.noteWireItemEmitted(anchor.ThreadID, anchor.ID, anchor.Rev)
	}
}

// claimFirstChildProbe records that parentID's first-child probe ran in
// this session and reports whether this call claimed it. The set is
// bounded like the tool-call links: at the bound it starts over, which
// costs one read per parent seen again. A stopped thread is not given
// state back.
func (r *Router) claimFirstChildProbe(threadID, parentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id := r.identityIfPresent(threadID); id != nil && id.stopped {
		return false
	}
	st := r.state(threadID)
	if _, probed := st.firstChildProbed[parentID]; probed {
		return false
	}
	if st.firstChildProbed == nil || len(st.firstChildProbed) >= maxToolCallLinksPerThread {
		st.firstChildProbed = make(map[string]struct{})
	}
	st.firstChildProbed[parentID] = struct{}{}
	return true
}

// itemReadNeedsDecoration is the store's probe with a failed probe
// folded into "needed": the row then goes out unstamped and the refresh
// decides.
func (r *Router) itemReadNeedsDecoration(item store.Item) bool {
	needs, err := r.store.ItemReadNeedsDecoration(item)
	if err != nil {
		log.Printf("triage: probe wire row %s/%s: %v", item.ThreadID, item.ID, err)
		return true
	}
	return needs
}

func (r *Router) emitItemRemove(threadID, itemID, kind string) {
	r.emit(eventchan.ProviderItemEvent, newItemStreamRemove(threadID, itemID, kind))
}

// emitItemPatch sends a lightweight patch event carrying only the fields
// that changed. The frontend merges the patch into the existing item in
// place, avoiding re-transmission of immutable structural fields and the
// potentially large summary text.
//
// rev is a separate parameter rather than a field the caller fills in
// because it is the one value on a patch that does not come from the
// caller's own intent: it is what the WRITE produced, read inside the
// write's transaction. Taking it here means a new patch emitter has to go
// and get it (ItemPatch.Rev explains why a client needs it).
func (r *Router) emitItemPatch(threadID, itemID, kind string, rev int64, patch ItemPatchFields) {
	r.emit(eventchan.ProviderItemEvent, newItemStreamPatch(threadID, itemID, kind, rev, patch))
	r.noteWireItemEmitted(threadID, itemID, rev)
}

// persistItemFieldsAndPatch writes a targeted UPDATE for the specified
// fields and pushes the change. Use instead of persistItem when the row
// already exists and only a narrow set of fields changed (e.g.,
// streaming settle: status + meta + updatedAt). A row with a decorated
// page read is pushed as an unstamped upsert instead of a patch (see the
// file comment); item must therefore be the whole row.
//
// The push carries the row as a read serves it, not the caller's fields:
// the stored meta holds no card key, a read merges an anchor's stamp into
// it (subagent_aggregate_stamps.go), and the client must hold what a read
// returns at that revision.
func (r *Router) persistItemFieldsAndPatch(item store.Item, update store.ItemPartialUpdate) error {
	var stored store.Item
	err := r.withSubagentCard(item.ThreadID, item.ParentID, func(card *store.SubagentCard) error {
		update.SubagentCard = card
		var err error
		stored, err = r.store.UpdateItemFields(item.ThreadID, item.ID, update)
		return err
	})
	if err != nil {
		return err
	}
	if r.itemReadNeedsDecoration(stored) {
		stored.Rev = store.UnstampedItemRev
		r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(stored))
		r.noteWireItemEmitted(stored.ThreadID, stored.ID, stored.Rev)
		return nil
	}
	patch := patchFromPartial(update)
	if update.Meta != nil {
		patch.Meta = &stored.Meta
	}
	r.emitItemPatch(stored.ThreadID, stored.ID, stored.Kind, stored.Rev, patch)
	return nil
}

func patchFromPartial(u store.ItemPartialUpdate) ItemPatchFields {
	return ItemPatchFields{
		Status:    u.Status,
		Summary:   u.Summary,
		Meta:      u.Meta,
		Decision:  u.Decision,
		UpdatedAt: u.UpdatedAt,
	}
}

// noteWireItemEmitted records that the client now holds itemID at rev
// and arms the thread's refresh.
//
// Every thread takes the debounced refresh, including one with no live
// state (a write from an app method, or a host-synthesized settle after
// teardown). Such a thread used to run its refresh here, synchronously,
// so that no deferred refresh could race its next write. That refresh is
// the decorated page read of every anchor the write stamped, and on the
// provider event path it cost a descendant walk per write (about 180 ms
// on a thread with a large agent). A deferred refresh reads after the
// writes that armed it, and a write that lands while one is running
// arms the next, so the thread converges on its stored rows the same way
// a live thread does.
func (r *Router) noteWireItemEmitted(threadID, itemID string, rev int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wireRefresh == nil {
		r.wireRefresh = make(map[string]*wireItemRefresh)
	}
	pending := r.wireRefresh[threadID]
	if pending == nil {
		pending = &wireItemRefresh{emitted: make(map[string]int64)}
		r.wireRefresh[threadID] = pending
	}
	// An unstamped note means "read this again"; otherwise the newest
	// push wins, whatever order the emitting goroutines ran in.
	if prev, seen := pending.emitted[itemID]; !seen || rev == store.UnstampedItemRev || prev < rev {
		pending.emitted[itemID] = rev
	}
	now := time.Now()
	if pending.timer == nil {
		pending.deadline = now.Add(wireRefreshMaxWait)
		r.refreshWG.Add(1)
		pending.timer = time.AfterFunc(wireRefreshQuiet, func() {
			defer r.refreshWG.Done()
			r.refreshWireItems(threadID, r.takeWireRefresh(threadID, pending, false))
		})
		return
	}
	// A refresh that already fired is queued behind r.mu and will take
	// this note with the rest; only a timer that has not fired is pushed
	// back, and never past the deadline.
	if pending.timer.Stop() {
		pending.timer.Reset(min(wireRefreshQuiet, pending.deadline.Sub(now)))
	}
}

// takeWireRefresh hands a pending set to its flush. The timer's own
// callback passes its entry and cancel=false: it takes the entry only if
// no synchronous flush took it first, and its refreshWG slot is released
// by the callback. A synchronous flush passes nil (whatever is pending)
// and cancel=true, and releases the slot itself when it beat the timer;
// a timer that already fired keeps the slot and finds nothing to do.
func (r *Router) takeWireRefresh(threadID string, want *wireItemRefresh, cancel bool) map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.wireRefresh[threadID]
	if pending == nil || (want != nil && pending != want) {
		return nil
	}
	delete(r.wireRefresh, threadID)
	if cancel && pending.timer != nil && pending.timer.Stop() {
		r.refreshWG.Done()
	}
	return pending.emitted
}

// flushWireItemRefresh runs the thread's pending refresh now: the turn
// just completed or the session is being torn down, so nothing quieter
// is coming.
func (r *Router) flushWireItemRefresh(threadID string) {
	r.refreshWireItems(threadID, r.takeWireRefresh(threadID, nil, true))
}

// DrainWireItemRefresh runs every pending refresh now and waits for the
// ones already running, so a caller about to close the store knows no
// refresh will read it afterwards. A settle still in flight can arm a
// new timer behind it; callers drain settles first (Wait does).
func (r *Router) DrainWireItemRefresh() {
	if r == nil {
		return
	}
	r.flushAllWireItemRefresh()
	r.refreshWG.Wait()
}

// flushAllWireItemRefresh runs every thread's pending refresh now.
func (r *Router) flushAllWireItemRefresh() {
	r.mu.Lock()
	threadIDs := make([]string, 0, len(r.wireRefresh))
	for threadID := range r.wireRefresh {
		threadIDs = append(threadIDs, threadID)
	}
	r.mu.Unlock()
	for _, threadID := range threadIDs {
		r.flushWireItemRefresh(threadID)
	}
}

func (r *Router) refreshWireItems(threadID string, emitted map[string]int64) {
	// The push follows the flush: the anchors whose stamp it changed are
	// read again with the rest.
	for _, id := range r.flushSubagentCards(threadID) {
		if emitted == nil {
			emitted = make(map[string]int64)
		}
		emitted[id] = store.UnstampedItemRev
	}
	if len(emitted) == 0 {
		return
	}
	rows, err := r.store.ListWireItemsBehind(threadID, emitted)
	if err != nil {
		// The client keeps its copies; their revisions are behind the
		// store's, so a reopen pages instead of proving them fresh.
		log.Printf("triage: refresh wire rows for %s: %v", threadID, err)
		return
	}
	for _, row := range rows {
		r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(row))
	}
}

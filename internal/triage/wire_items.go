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
// refresh.) Two rules keep a pushed row a copy of what a page would read
// at the revision it claims (docs/architecture/thread-replica-sync.md
// §3.1):
//
//   - a row whose page read is decorated (store.ItemReadNeedsDecoration:
//     an anchor with children, a carrier, a completion sibling, a plan)
//     is pushed from that read, never from the write's own read-back,
//     and a field patch to such a row is pushed as that read too,
//     because a patch replaces the client's meta wholesale and the
//     stored meta is not the decorated one;
//   - a write stamps rows it did not touch (the parent anchor, resume
//     carriers, completion siblings), so after a burst of writes the
//     rows whose revision moved without a push of their own are read as
//     a page would and pushed again. That happens at quiet points, never
//     per write: one second after the last push on the thread, at most
//     wireRefreshMaxWait after the first, and synchronously at turn
//     completion and session teardown.
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
// timer that will flush them. Guarded by r.mu. It lives on threadState;
// the timer's callback holds the state pointer, so a flush that runs
// after cleanupThread dropped the entry still finds its own set.
type wireItemRefresh struct {
	emitted  map[string]int64
	timer    *time.Timer
	deadline time.Time
}

func (r *Router) emitItemUpsert(item store.Item) {
	r.emitItemUpserts(item.ThreadID, []store.Item{item})
}

// emitItemUpserts pushes rows written in one thread, reading the ones
// with a decorated page read together. When that read fails they go out
// as written but marked unstamped, and are noted at that revision so
// the next refresh reads them again: the client never holds a provable
// revision for a row it saw undecorated. A row deleted between its write
// and the read is pushed as written; whoever deleted it pushes the remove.
func (r *Router) emitItemUpserts(threadID string, items []store.Item) {
	needsRead := make(map[string]bool, len(items))
	for _, item := range items {
		if item.Rev != store.UnstampedItemRev && r.itemReadNeedsDecoration(item) {
			needsRead[item.ID] = true
		}
	}
	r.emitItemUpsertsReading(threadID, items, needsRead)
}

// itemReadNeedsDecoration is the store's probe with a failed probe
// folded into "needed": the page read then decides, or fails and sends
// the row unstamped.
func (r *Router) itemReadNeedsDecoration(item store.Item) bool {
	needs, err := r.store.ItemReadNeedsDecoration(item)
	if err != nil {
		log.Printf("triage: probe wire row %s/%s: %v", item.ThreadID, item.ID, err)
		return true
	}
	return needs
}

func (r *Router) emitItemUpsertsReading(threadID string, items []store.Item, needsRead map[string]bool) {
	ids := make([]string, 0, len(needsRead))
	for _, item := range items {
		if needsRead[item.ID] {
			ids = append(ids, item.ID)
		}
	}
	var read map[string]store.Item
	readFailed := false
	if len(ids) > 0 {
		rows, err := r.store.ListWireItems(threadID, ids)
		if err != nil {
			log.Printf("triage: read wire rows %s %v: %v", threadID, ids, err)
			readFailed = true
		} else {
			read = make(map[string]store.Item, len(rows))
			for _, row := range rows {
				read[row.ID] = row
			}
		}
	}
	for _, item := range items {
		if item.Rev == store.UnstampedItemRev {
			r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(item))
			continue
		}
		if needsRead[item.ID] {
			if row, ok := read[item.ID]; ok {
				item = row
			} else if readFailed {
				item.Rev = store.UnstampedItemRev
			}
		}
		r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(item))
		r.noteWireItemEmitted(threadID, item.ID, item.Rev)
	}
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
// page read is pushed as that read instead of a patch (see the file
// comment).
func (r *Router) persistItemFieldsAndPatch(item store.Item, update store.ItemPartialUpdate) error {
	rev, err := r.store.UpdateItemFields(item.ThreadID, item.ID, update)
	if err != nil {
		return err
	}
	// The written row: what the probe below judges, and the fallback
	// push when the page read fails.
	if update.Status != nil {
		item.Status = *update.Status
	}
	if update.Summary != nil {
		item.Summary = *update.Summary
	}
	if update.Meta != nil {
		item.Meta = *update.Meta
	}
	if update.Decision != nil {
		item.Decision = *update.Decision
	}
	if update.UpdatedAt != nil {
		item.UpdatedAt = *update.UpdatedAt
	}
	item.Rev = rev
	if r.itemReadNeedsDecoration(item) {
		r.emitItemUpsertsReading(item.ThreadID, []store.Item{item}, map[string]bool{item.ID: true})
		return nil
	}
	r.emitItemPatch(item.ThreadID, item.ID, item.Kind, rev, patchFromPartial(update))
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
// and arms the thread's refresh. A thread with no live state has no
// quiet point coming, so its refresh runs here, before the caller
// pushes anything else: a deferred one would race the next write and
// push a row the write's own upsert already carried.
func (r *Router) noteWireItemEmitted(threadID, itemID string, rev int64) {
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		r.mu.Unlock()
		r.refreshWireItems(threadID, map[string]int64{itemID: rev})
		return
	}
	defer r.mu.Unlock()
	pending := &st.wireRefresh
	if pending.emitted == nil {
		pending.emitted = make(map[string]int64)
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
			r.refreshWireItems(threadID, r.takeWireRefresh(st, false))
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

// takeWireRefresh hands the pending set to its flush. The timer's own
// callback passes cancel=false: its refreshWG slot is released by the
// callback. A synchronous flush passes cancel=true and releases the slot
// itself when it beat the timer; a timer that already fired keeps the
// slot and finds nothing to do.
func (r *Router) takeWireRefresh(st *threadState, cancel bool) map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := &st.wireRefresh
	if cancel && pending.timer != nil && pending.timer.Stop() {
		r.refreshWG.Done()
	}
	emitted := pending.emitted
	*pending = wireItemRefresh{}
	return emitted
}

// flushWireItemRefresh runs the thread's pending refresh now: the turn
// just completed or the session is being torn down, so nothing quieter
// is coming.
func (r *Router) flushWireItemRefresh(threadID string) {
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	r.mu.Unlock()
	if st == nil {
		return
	}
	r.refreshWireItems(threadID, r.takeWireRefresh(st, true))
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
	threadIDs := make([]string, 0, len(r.threads))
	for threadID, st := range r.threads {
		if st.wireRefresh.timer != nil {
			threadIDs = append(threadIDs, threadID)
		}
	}
	r.mu.Unlock()
	for _, threadID := range threadIDs {
		r.flushWireItemRefresh(threadID)
	}
}

func (r *Router) refreshWireItems(threadID string, emitted map[string]int64) {
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

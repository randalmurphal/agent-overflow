package transport

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
)

// Per-connection lifecycle leases (docs/specs/remote-access.md §"The phone
// client", "Lifecycle").
//
// A client may tell its connection whether it is in the foreground with a
// `lease` frame (frame.go, conn.go handleLease). While the lease says
// `background` the backend spends less of a sleeping phone's radio and
// battery on it: highlight seeds are withheld, and transcript deltas are
// merged into one frame per row per leaseDeltaWindow. Everything else —
// turns, approvals, errors, thread rows, notifications — flows exactly as
// it always has, which is what keeps the push mapping and the sidebar badge
// carriers working for a client nobody is looking at.
//
// Four properties make this safe, and all four are load-bearing:
//
//   - **It is the WHOLE-CLIENT native lifecycle.** The phone shell's
//     pause/resume, and nothing else. Never a pane, never document
//     visibility, never focus. Off-view work shedding is a rejected design
//     in this codebase for the reason event_entity.go states about panes: a
//     surface that stops receiving renders wrongly the moment it is looked
//     at again. A whole client that the OS has paused is the one case where
//     nothing is being looked at at all.
//   - **Active until told otherwise.** A connection that never sends the
//     frame behaves exactly as it did before the frame existed, which is
//     every desktop client, every browser client, and every Go client in
//     the tree. The lease survives nothing: a reconnect starts `active` and
//     the client restates its state after hello, the same way it restates
//     its watch set.
//   - **Withholding happens BEFORE gap accounting.** A withheld seed is not
//     a frame this connection lost, so it must not flag the channel and
//     must not mint a `gap:true` marker — the same ordering rule the entity
//     filter beside it obeys (eventbus.go Subscriber.deliver). Reversing it
//     would turn a resume into a resync storm.
//   - **A merged frame carries the LAST merged frame's seq**, so the
//     client's per-channel cursor advances past every frame merged away.
//     The cursor never goes BACKWARDS either: see deltaCoalescer.
//
// Nothing here is authorization. A lease reduces what a client is sent; what
// it is ALLOWED to be sent stays the origin, grant and audience filters,
// which do not consult it.

// Lease states. The closed set: anything else is a bad_params refusal that
// leaves the connection's lease exactly as it was, because "unrecognised"
// and "the client meant active" are different facts and only a refusal
// keeps them apart.
const (
	leaseStateActive     = "active"
	leaseStateBackground = "background"
)

// leaseDeltaWindow bounds how often ONE row's streaming text reaches a
// backgrounded client: at most one frame per (thread, item) per window.
// 250ms is the spec's figure and matches triage's own streamPersistInterval,
// so a backgrounded connection lands near the cadence SQLite is written at
// rather than the cadence the provider produces at.
const leaseDeltaWindow = 250 * time.Millisecond

// backgroundWithheldChannel reports whether a backgrounded connection stops
// receiving this channel entirely.
//
// ONE member today, and the membership rule is what keeps it one: the
// channel must be a pure cache WARMER whose consumers already have a
// working path without it. `highlight:seed` is per-growth-step span
// metadata for a streaming fence; its consumers (liveCodeSeeds,
// codeSpanCache) fall back to the highlight RPC, which is what they do for
// every fence that mounts after the seed anyway. A backgrounded client is
// rendering nothing, so the seeds it is being handed warm a cache for code
// nobody is looking at — the largest frames on the wire buying the least.
//
// A channel whose absence a consumer cannot recover from does NOT belong
// here, however cheap it looks. That is the whole difference between this
// list and dropping frames.
var backgroundWithheldChannels = map[string]bool{
	string(eventchan.HighlightSeed): true,
}

func backgroundWithheldChannel(channel string) bool {
	return backgroundWithheldChannels[channel]
}

// leaseItemFrame is the `provider:item_event` payload, decoded down to the
// fields a merge needs. Deliberately a LOCAL shape rather than an import of
// triage.ItemStreamEvent: transport is the plumbing under that package and
// must not grow a dependency on the store types behind it.
// TestLeaseItemFrameMatchesItemStreamEvent pins the two shapes together, so
// a field rename in triage fails there rather than silently producing merged
// frames a client cannot read.
//
// It is both the decode and the ENCODE shape, which is why every optional
// field is `omitempty`: a merged frame must be indistinguishable from the
// delta triage would have emitted for the same text.
type leaseItemFrame struct {
	Action    string             `json:"action"`
	ThreadID  string             `json:"threadId"`
	Item      *leaseItemIdentity `json:"item,omitempty"`
	ItemID    string             `json:"itemId,omitempty"`
	Kind      string             `json:"kind,omitempty"`
	Delta     string             `json:"delta,omitempty"`
	UpdatedAt int64              `json:"updatedAt,omitempty"`
}

// leaseItemIdentity is as much of an upsert's `item` as the coalescer reads:
// its id, so an upsert flushes the pending deltas for the row it re-states.
// The rest of the row is skipped by the decoder rather than allocated.
type leaseItemIdentity struct {
	ID string `json:"id"`
}

// itemStreamActionDelta is the one action the coalescer merges. Spelled here
// rather than imported for the same reason leaseItemFrame is local; the
// drift test pins it.
const itemStreamActionDelta = "delta"

// rowKey names the row a frame is about: its thread and its item. An upsert
// carries the id inside `item`; every other action carries `itemId`.
func (f *leaseItemFrame) rowKey() deltaKey {
	if f.ItemID != "" {
		return deltaKey{threadID: f.ThreadID, itemID: f.ItemID}
	}
	if f.Item != nil {
		return deltaKey{threadID: f.ThreadID, itemID: f.Item.ID}
	}
	return deltaKey{threadID: f.ThreadID}
}

// deltaKey is the coalescing key: one streaming row.
type deltaKey struct {
	threadID string
	itemID   string
}

// pendingDelta accumulates one row's merged text for the current window.
type pendingDelta struct {
	kind string
	text strings.Builder
	// updatedAt is the LAST merged frame's stamp — the merged frame claims
	// the freshness of the newest text it carries, never the oldest.
	updatedAt int64
	// seq is the LAST merged frame's seq, so a client that applies the
	// merged frame has its cursor past every frame merged away and asks for
	// no replay of them.
	seq uint64
	// gap rides through a merge. deliver stamps the loss announcement on
	// whichever frame it delivers and forgets it; dropping the flag here
	// would swallow the one resync instruction the client had coming.
	gap       bool
	entityKey string
	channel   string
}

// deltaCoalescer merges a backgrounded connection's transcript deltas.
//
// Owned by the single pumpEvents goroutine, like coalesceBuffer beside it,
// and holding nothing until a frame is actually merged — a connection that
// never leases background allocates none of this.
//
// **Ordering is the whole design.** A client drops any frame whose seq is
// at or below its per-channel cursor (wsClient handleEventEntry), so this
// may never emit a channel's frames out of seq order. Two rules produce
// that, and neither is optional:
//
//   - Every pass-through frame on the coalesced channel flushes ALL pending
//     rows first, not just its own. Flushing only its own row would leave a
//     lower-seq merge behind a higher-seq pass-through, and the client would
//     drop it as a duplicate — losing text, not just delaying it.
//   - Pending rows flush in LAST-ARRIVAL order (append moves a row to the
//     tail on every delta), which is ascending last-seq order because seqs
//     only increase.
//
// The stronger flush rule is a superset of "flush this row first", which is
// what preserves the frontend's contract that a row's deltas land before the
// meta or patch that re-states it (triage item_events.go).
type deltaCoalescer struct {
	pending map[deltaKey]*pendingDelta
	// order holds every pending key in last-arrival order; see above.
	order  []deltaKey
	timer  *time.Timer
	armed  bool
	window time.Duration
	// emit hands a merged frame onward, which is the pump's ordinary
	// coalescing buffer. Set once at pump start.
	emit func(Event)
}

// intercept decides what the pump does with one event while the lease says
// background. Reports true when the event was absorbed into a pending merge
// and must not be written; false means "write it as-is" — and by then any
// pending merges that had to precede it have already been emitted.
//
// Only the coalesced channel is decoded, and a payload that does not decode
// passes through untouched: fail-open, the same rule the entity filter's
// empty key follows. A shape change costs a wire-cost regression, never a
// missing row.
func (c *deltaCoalescer) intercept(e Event) bool {
	if e.Channel != string(eventchan.ProviderItemEvent) {
		return false
	}
	var frame leaseItemFrame
	if err := json.Unmarshal(e.Data, &frame); err != nil {
		c.flushAll()
		return false
	}
	key := frame.rowKey()
	if frame.Action != itemStreamActionDelta || key.threadID == "" || key.itemID == "" {
		// Upsert, meta, patch, or a delta this build cannot key: it goes out
		// unchanged, behind everything already held for this channel.
		c.flushAll()
		return false
	}
	c.append(key, &frame, e)
	return true
}

// append merges one delta into its row's pending window and arms the timer
// if this is the window's first frame — a throttle, not a debounce, so a row
// under continuous deltas still reaches the client every window rather than
// only when the stream pauses.
func (c *deltaCoalescer) append(key deltaKey, frame *leaseItemFrame, e Event) {
	p := c.pending[key]
	if p == nil {
		if c.pending == nil {
			c.pending = make(map[deltaKey]*pendingDelta)
		}
		p = &pendingDelta{kind: frame.Kind, channel: e.Channel, entityKey: e.EntityKey}
		c.pending[key] = p
		c.order = append(c.order, key)
	} else {
		c.moveToTail(key)
	}
	p.text.WriteString(frame.Delta)
	p.updatedAt = frame.UpdatedAt
	p.seq = e.Seq
	p.gap = p.gap || e.Gap
	if c.armed {
		return
	}
	if c.timer == nil {
		c.timer = time.NewTimer(c.window)
	} else {
		c.timer.Reset(c.window)
	}
	c.armed = true
}

// moveToTail keeps `order` sorted by each row's newest seq. Linear, over a
// slice holding the rows streaming concurrently on one connection inside one
// window — one or two in practice, and a map of them would allocate more
// than the scan costs.
func (c *deltaCoalescer) moveToTail(key deltaKey) {
	for i := range c.order {
		if c.order[i] == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, key)
}

// flushAll emits every pending merge, in seq order, and disarms the window.
// A no-op when nothing is pending, which is what makes a stale timer tick
// harmless.
func (c *deltaCoalescer) flushAll() {
	// Empty is the hot case — the pump calls this on every pass-through
	// event, including on connections that never leased anything — and an
	// empty `order` implies a disarmed window, because `armed` is only ever
	// set by an append that also appends a key.
	if len(c.order) == 0 {
		return
	}
	if c.timer != nil {
		c.timer.Stop()
	}
	c.armed = false
	for _, key := range c.order {
		p := c.pending[key]
		delete(c.pending, key)
		if p == nil {
			continue
		}
		if merged, ok := mergedDeltaEvent(key, p); ok {
			c.emit(merged)
		}
	}
	c.order = c.order[:0]
}

// timerC is the window's channel for the pump's select. nil while nothing is
// pending, which blocks forever — the correct behavior when there is nothing
// to flush, and the reason an inactive lease costs the pump nothing.
func (c *deltaCoalescer) timerC() <-chan time.Time {
	if c.timer == nil {
		return nil
	}
	return c.timer.C
}

// stop flushes whatever the window still holds and releases the timer. Runs
// on pump teardown, ahead of the batch buffer's own stop, so text held for a
// client that is still there when its socket closes is written rather than
// dropped.
func (c *deltaCoalescer) stop() {
	c.flushAll()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

// mergedDeltaEvent builds the replacement frame. A NEW Event with new bytes,
// never a mutation of the one the bus handed out: Data and WireBytes are
// shared across every subscriber, and writing through them would rewrite
// another connection's frame.
func mergedDeltaEvent(key deltaKey, p *pendingDelta) (Event, bool) {
	payload, err := json.Marshal(leaseItemFrame{
		Action:    itemStreamActionDelta,
		ThreadID:  key.threadID,
		ItemID:    key.itemID,
		Kind:      p.kind,
		Delta:     p.text.String(),
		UpdatedAt: p.updatedAt,
	})
	if err != nil {
		log.Printf("transport: marshal coalesced delta: %v", err)
		return Event{}, false
	}
	merged := Event{
		Channel:   p.channel,
		Seq:       p.seq,
		Data:      payload,
		Gap:       p.gap,
		EntityKey: p.entityKey,
	}
	wire, err := encodeEventFrame(merged)
	if err != nil {
		log.Printf("transport: encode coalesced delta: %v", err)
		return Event{}, false
	}
	merged.WireBytes = wire
	return merged, true
}

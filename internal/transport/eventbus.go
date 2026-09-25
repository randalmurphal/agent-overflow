package transport

import (
	"encoding/json"
	"log"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/eventchan"
)

// DefaultRingCapacity and RingByteBudget bound a channel's replay ring,
// and together they are the reconnect budget: a client that reconnects
// within 30 s of losing its connection replays every channel without a
// gap while a thread runs 100 agents at the rate the CLI writes them. The
// harness measures that burst at about 150 frames/s on the busiest
// channel, provider:item_event (e2e/tests/transport-replay-burst.spec.ts),
// so the ring holds 55 s of it. One ring serves every thread, so threads
// bursting at once share the window. Tests can configure smaller rings to
// exercise overflow.
const DefaultRingCapacity = 8192

// RingByteBudget bounds the wire bytes one ring retains, so a channel of
// large frames (terminal output) keeps what fits in it rather than
// DefaultRingCapacity of them. The newest frame is always retained.
const RingByteBudget = 16 << 20

// RingRetainFor bounds how long a RetentionDefault ring retains a frame.
// The ring serves reconnects, and the client's reconnect backoff caps at
// 5 s on loopback and 30 s remote (frontend/src/lib/transport/wsClient.ts
// RECONNECT_MAX_LOCAL_MS and RECONNECT_MAX_REMOTE_MS), so an older frame
// serves no reconnect the client attempts on its own. A cursor below a
// released frame replays as gap:true. A latest-only ring's frame does not
// age: it is the channel's current value.
const RingRetainFor = 5 * time.Minute

// RingSweepEvery is how often the bus releases aged frames from every
// ring, so a channel that stops emitting does not keep its last burst.
const RingSweepEvery = 30 * time.Second

// WatermarkEvery is how often a connection's pump sends watermarks for
// the frames its subscriber withheld (Subscriber.takeWithheld). When a
// connection drops, the client's cursor trails the newest frame withheld
// from it by at most WatermarkEvery. Of RingRetainFor's 5 minutes that
// leaves 4.5 minutes for the outage and the reconnect backoff (capped at
// 30 s for a remote client, 5 s for a local one), within which the
// reconnect replays without an age gap.
const WatermarkEvery = 30 * time.Second

// DefaultSubscriberBuffer sizes a subscriber's delivery channel: how far
// a connected client may fall behind live fanout before the channel is
// gapped for it. The replay ring serves a client that is not connected.
const DefaultSubscriberBuffer = 1024

// EventBus is the in-memory fanout for server-pushed events. Events
// are tagged with a monotonically increasing per-channel sequence and
// stored in a bounded circular buffer per channel; subscribers
// receive every new event live and can replay missed events when they
// reconnect.
//
// The ring is intentionally in-memory and bounded — it's a network
// replay buffer, not a persistent event store. CLAUDE.md principle 3
// (SQLite is a history cache, not an event store) prohibits us from
// persisting the stream; out-of-window gaps surface as a single
// {gap:true} marker so the client can re-fetch via list endpoints.
//
// Concurrency model:
//   - rings + subs maps are guarded by mu (RWMutex).
//   - Emit takes mu (writer) to bump the seq, append into the ring, and
//     fan out through each subscriber's non-blocking deliver. Fanout stays
//     inside the same critical section as sequence assignment. Provider
//     sessions emit concurrently onto one channel, so unlocking between
//     those operations lets seq N+1 reach a subscriber before seq N. The
//     client must treat that as a dropped event and the late seq N as a
//     duplicate, corrupting the stream even though the server lost nothing.
//   - Replay holds an RLock for the duration of the per-channel walk.
//   - The sweeper goroutine takes mu every RingSweepEvery to release
//     aged frames from every ring. Close stops it and waits for it.
//   - subList is a maintained slice mirroring subs map membership so
//     Emit doesn't allocate-and-copy a snapshot per call. Updated
//     inside Subscribe / Subscriber.Close under the same mu. Slice
//     mutations are copy-on-remove (and append-grow on add), so a
//     snapshot taken under Lock stays safe to iterate after Unlock —
//     subsequent Subscribe / Close don't touch the snapshot's
//     backing array slots.
//   - Subscribers' deliver() drops to a non-blocking select; a slow
//     consumer drops events. The drop is recorded per (subscriber,
//     channel) in Subscriber.gapped, and the next event that DOES fit is
//     stamped Gap:true (re-encoded per subscriber), so the client learns
//     about the loss even when the dropped events were the channel's
//     tail. The announcement names the entity keys of the dropped frames
//     (Event.GapThreads) when it can, so the client recovers only those
//     threads. The client's forward-skip detection (wsClient.ts
//     handleEventEntry) still covers the mid-stream case on its own; the
//     sticky flag exists because that detection needs a later same-channel
//     delivery to fire, which sustained traffic can delay for tens of seconds
//     (incident 2026-08-29: 30-40s standing timeline truncation under a
//     subagent fan-out storm). Other gapped channels piggyback: any
//     successful delivery first flushes standalone {gap:true} markers
//     (same shape Replay emits) for every other gapped channel that has
//     buffer room. Detection stays scoped to one connection client-side,
//     because across a reconnect a legitimate skip is expected (ephemeral
//     and latest-only channels — see event_visibility.go) and Replay is
//     the authority.
type EventBus struct {
	mu       sync.RWMutex
	rings    map[string]*ring
	subs     map[*Subscriber]struct{}
	subList  []*Subscriber
	capacity int
	// ringBytes is each ring's byte budget (RingByteBudget).
	ringBytes int
	subBuf    int
	closed    atomic.Bool
	// now is the bus clock, time.Now outside tests. epoch is its reading
	// at construction; ring stamps are durations since it (clock).
	now   func() time.Time
	epoch time.Time
	// stopSweep ends the sweeper; sweepDone closes when it has returned.
	stopSweep chan struct{}
	sweepDone chan struct{}
}

// ring is a bounded circular buffer keyed by per-channel seq.
// head + count slide around backing[]; no copies on overflow. The
// backing array starts small and doubles up to capacity as events
// arrive — most channels never see more than a handful of events, so
// preallocating capacity slots per channel would waste the bulk of
// the bus's steady-state memory. It evicts its oldest entries to stay
// within capacity entries and byteBudget wire bytes, and an aging ring
// also releases entries older than RingRetainFor.
type ring struct {
	head     int
	count    int
	seq      uint64
	capacity int
	// bytes is the WireBytes the retained entries hold; byteBudget bounds it.
	bytes      int
	byteBudget int
	backing    []Event
	// stamps[i] is the emit time of backing[i] (EventBus.clock). The two
	// share length and head: grow copies both, and releaseAged frees both.
	stamps []int64
	// ages is whether entries older than RingRetainFor are released:
	// RetentionDefault channels only.
	ages bool
	// dropped is the head at the last DropRetained. A cursor below it
	// missed nothing: that history was discarded, not evicted.
	dropped uint64
	// envPrefix is the fixed leading portion of every live event
	// envelope on this channel: `{"type":"event","channel":<escaped>,"seq":`.
	// Cached so Emit can assemble WireBytes with plain appends under the
	// bus mutex instead of a reflection marshal (see appendEventWire).
	envPrefix []byte
}

// ringInitialCapacity is the first backing allocation for a non-empty
// ring. Small on purpose: quiet channels stay small forever.
const ringInitialCapacity = 16

// newRing sizes channel's ring from its retention (event_channels.go):
// capacity is the depth of a RetentionDefault ring.
func newRing(channel string, capacity, byteBudget int) *ring {
	r := &ring{capacity: capacity, byteBudget: byteBudget}
	switch channelRetention(channel) {
	case RetentionEphemeral:
		// Seed-style cache warmers and one-shot directives: sequence
		// tracking only, no replay retention.
		r.capacity = 0
	case RetentionLatestOnly:
		// Whole-state frames: the newest one supersedes all prior ones,
		// so retain exactly it. It never ages, because it is the
		// channel's current value and Replay serves it instead of a gap.
		r.capacity = 1
	case RetentionDefault:
		// Full-depth reconnect buffer.
		r.ages = true
	}
	// json.Marshal of a string cannot fail (invalid UTF-8 is replaced),
	// and matches exactly how the reflection encoder renders the
	// ServerFrame Channel field, HTML escaping included.
	name, _ := json.Marshal(channel)
	prefix := make([]byte, 0, len(eventFramePrefix)+len(name)+len(eventSeqKey))
	prefix = append(prefix, eventFramePrefix...)
	prefix = append(prefix, name...)
	prefix = append(prefix, eventSeqKey...)
	r.envPrefix = prefix
	return r
}

const (
	eventFramePrefix = `{"type":"` + frameTypeEvent + `","channel":`
	eventSeqKey      = `,"seq":`
	eventDataKey     = `,"data":`
	// maxUint64Digits is the worst-case decimal width of a seq.
	maxUint64Digits = 20
)

// appendEventWire assembles the pre-encoded ServerFrame envelope for a
// live event in one exact-size allocation: envPrefix + seq + `,"data":`
// + data + `}`. Byte-identical to encodeEventFrame's reflection marshal
// for Emit-shaped events (seq >= 1, non-empty Data — itself compact,
// HTML-escaped json.Marshal output — and Gap always false, so every
// omitempty branch is fixed); TestEventBus_PreEncodedWireBytesMatchEnvelope
// and TestEventBus_EmitWireBytesByteIdentical pin the equivalence. Runs
// under the bus mutex, so no reflection walk or encoder buffer growth
// is paid inside the critical section.
func (r *ring) appendEventWire(seq uint64, data []byte) []byte {
	wire := make([]byte, 0, len(r.envPrefix)+maxUint64Digits+len(eventDataKey)+len(data)+1)
	wire = append(wire, r.envPrefix...)
	wire = strconv.AppendUint(wire, seq, 10)
	wire = append(wire, eventDataKey...)
	wire = append(wire, data...)
	return append(wire, '}')
}

// append stores e, emitted at now (EventBus.clock), at the tail. It first
// releases aged entries, then evicts the oldest entries until e fits
// within capacity and the byte budget, and grows the backing up to
// capacity. Amortized O(1). The newest entry is kept even when it alone
// exceeds the byte budget. A zero-capacity ring (ephemeral channels)
// tracks sequence only and retains nothing.
func (r *ring) append(e Event, now int64) {
	if r.capacity == 0 {
		return
	}
	r.releaseAged(now)
	size := len(e.WireBytes)
	for r.count > 0 && (r.count == r.capacity || r.bytes+size > r.byteBudget) {
		r.evictOldest()
	}
	if r.count == len(r.backing) {
		r.grow()
	}
	slot := (r.head + r.count) % len(r.backing)
	r.backing[slot] = e
	r.stamps[slot] = now
	r.count++
	r.bytes += size
	r.seq = e.Seq
}

// releaseAged evicts the entries emitted more than RingRetainFor before
// now, oldest first, stopping at the first younger one. An aging ring
// left empty also frees its backing arrays, so a burst's grown backing
// is not kept; grow starts over from a nil backing. seq and dropped are
// unchanged: a cursor below a released entry missed it, and replayAfter
// answers that cursor with a gap.
func (r *ring) releaseAged(now int64) {
	if !r.ages {
		return
	}
	cutoff := now - int64(RingRetainFor)
	for r.count > 0 && r.stamps[r.head] < cutoff {
		r.evictOldest()
	}
	if r.count == 0 && r.backing != nil {
		r.backing, r.stamps, r.head = nil, nil, 0
	}
}

// evictOldest drops the head entry and releases its frame.
func (r *ring) evictOldest() {
	r.bytes -= len(r.backing[r.head].WireBytes)
	r.backing[r.head] = Event{}
	r.head = (r.head + 1) % len(r.backing)
	r.count--
}

// grow re-linearizes the ring into larger backing and stamp arrays (head
// back to 0). Doubling bounded by capacity; at most log2(capacity)
// growths between releases of an empty backing.
func (r *ring) grow() {
	newCap := min(max(2*len(r.backing), ringInitialCapacity), r.capacity)
	fresh := make([]Event, newCap)
	stamps := make([]int64, newCap)
	for i := range r.count {
		j := (r.head + i) % len(r.backing)
		fresh[i] = r.backing[j]
		stamps[i] = r.stamps[j]
	}
	r.backing, r.stamps = fresh, stamps
	r.head = 0
}

// replayAfter walks the ring and returns every event with seq > lastSeq.
// Returns hadGap=true when lastSeq fell outside the ring — either the
// eviction end (the events the client wanted are gone) or the head end
// (the client's cursor is ahead of ours) — so the caller can emit a
// single gap marker instead of partial history.
func (r *ring) replayAfter(lastSeq uint64) (events []Event, hadGap bool) {
	if lastSeq < r.dropped {
		lastSeq = r.dropped
	}
	if lastSeq > r.seq {
		// The client's cursor is above our head, so its sequence space
		// is not ours: a restarted backend re-seeds every channel from
		// 1. Reporting "nothing missed" would leave the client dropping
		// every live event below its stale cursor forever (wsClient
		// handleEventEntry dedups on seq), so the invalid state must
		// surface as a gap the client resyncs from. Deliberately ahead
		// of the empty-ring check: a zero-capacity (ephemeral) or
		// not-yet-filled ring says nothing about whose sequence space
		// the cursor belongs to.
		return nil, true
	}
	if r.count == 0 {
		// An ephemeral ring retains nothing, so its frames were never
		// history. A retaining ring is empty after DropRetained, whose
		// floor already raised lastSeq to the head, or after aging
		// released every frame above the cursor.
		return nil, r.capacity > 0 && lastSeq < r.seq
	}
	oldest := r.backing[r.head].Seq
	if lastSeq+1 < oldest {
		return nil, true
	}
	cap := len(r.backing)
	out := make([]Event, 0, r.count)
	for i := 0; i < r.count; i++ {
		e := r.backing[(r.head+i)%cap]
		if e.Seq > lastSeq {
			out = append(out, e)
		}
	}
	return out, false
}

// Event is one frame on the wire (encoded by encodeEventFrame).
// Data is kept as json.RawMessage so the bus can store the encoded
// bytes once and broadcast them to N subscribers without re-marshalling.
//
// WireBytes is the pre-encoded event envelope (type="event",
// channel, seq, data, gap?) assembled once at Emit time. The conn
// event-pump writes WireBytes verbatim — single events and spliced
// batch frames alike — so a multi-subscriber LAN bind doesn't pay
// N×marshal cost. On live-fanout events Data is a subslice of
// WireBytes (the payload bytes inside the envelope), so a queued event
// holds ONE payload copy, not two, across up-to-1024-deep subscriber
// buffers; Data stays populated because non-wire subscribers (the
// harness workflow waiter) decode payloads from it.
//
// EntityKey is the id of the entity this frame is addressed to — a thread
// id today — derived ONCE by the emitter (internal/app's emit funnel, which
// shares the derivation with the replay log) rather than per subscriber. It
// is the input to per-thread subscription narrowing on EntityFiltered
// channels (event_entity.go) and is empty on every other frame, including
// one whose payload the extractor could not attribute. Empty means
// "deliver": see event_entity.go for why that direction is the safe one.
//
// EntityScope is the frame's second attribution on a
// TranscriptScopeFiltered channel: the transcript scope root (the row's
// `parentId`) inside the EntityKey thread, derived once by the same
// emitter. Empty means a root-scope row, or a payload the extractor could
// not attribute; both take the thread rule (event_entity.go). Ignored on
// every other channel.
//
// GapThreads, on a Gap frame that announces a subscriber-buffer drop, is
// the sorted entity keys of the frames dropped. Nil means the loss is not
// attributed and the client recovers everything: a dropped frame had no
// key, the keys outnumbered MaxWatchThreads, or the frame is a replay
// marker, whose lost frames the ring no longer holds. It never names
// scopes: a loss recovers every surface of the thread.
//
// Watermark marks a per-connection frame with no Data (watermarkEvent): it
// tells the client that every frame on Channel at or below Seq that it is
// entitled to has been delivered, so the client may advance its cursor to
// Seq. It is never emitted on the bus or retained in a ring.
type Event struct {
	Channel     string
	Seq         uint64
	Data        json.RawMessage
	Gap         bool
	GapThreads  []string
	Watermark   bool
	WireBytes   []byte
	EntityKey   string
	EntityScope string
}

// NewEventBus returns a new bus with the given per-channel capacity.
// Pass 0 to use DefaultRingCapacity. The bus runs a sweeper goroutine
// until Close.
func NewEventBus(capacity int) *EventBus {
	return newEventBus(capacity, time.Now)
}

// newEventBus is NewEventBus reading the given clock; tests inject one.
func newEventBus(capacity int, now func() time.Time) *EventBus {
	if capacity <= 0 {
		capacity = DefaultRingCapacity
	}
	b := &EventBus{
		rings:     make(map[string]*ring),
		subs:      make(map[*Subscriber]struct{}),
		subList:   nil,
		capacity:  capacity,
		ringBytes: RingByteBudget,
		subBuf:    DefaultSubscriberBuffer,
		now:       now,
		epoch:     now(),
		stopSweep: make(chan struct{}),
		sweepDone: make(chan struct{}),
	}
	go b.runSweeper()
	return b
}

// clock reads the bus clock as nanoseconds since epoch. Readings of
// time.Now carry the monotonic clock, so their difference is elapsed
// time and a wall-clock step does not age frames early or hold them late.
func (b *EventBus) clock() int64 {
	return int64(b.now().Sub(b.epoch))
}

// runSweeper releases aged frames every RingSweepEvery until Close.
func (b *EventBus) runSweeper() {
	defer close(b.sweepDone)
	tick := time.NewTicker(RingSweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-b.stopSweep:
			return
		case <-tick.C:
			b.sweepAged()
		}
	}
}

// sweepAged releases every ring's frames older than RingRetainFor. Only
// aging rings hold frames it can release.
func (b *EventBus) sweepAged() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	for _, r := range b.rings {
		r.releaseAged(now)
	}
}

// Emit publishes a new event on the given channel. The channel is an
// eventchan.Channel rather than a string so a call site cannot typo a
// name or invent one without adding both its constant and its
// ChannelPolicy row; the three paths that legitimately publish a
// caller-named channel (HarnessEmit, harness.Replayer, and the harness
// replay-log republisher) spell an explicit eventchan.Channel(name)
// conversion and land on the fail-closed loopback-only default.
// Returns the event so
// callers can synchronously inspect the seq if needed (replay tests
// exercise this). Emitting after Close is a silent no-op — late
// publishers shouldn't crash the app during shutdown.
//
// Wire pre-encoding: the ServerFrame envelope (type="event", channel,
// seq, data) is assembled once here and reused by every subscriber.
// The payload marshal (reflection, arbitrary size) happens before the
// lock; only seq assignment, the cheap append-splice of the envelope
// (appendEventWire), and the ring append run under the bus-wide mutex,
// so concurrent emitters and Replay/Subscribe don't serialize behind a
// reflection walk. Seq assignment and ring append stay atomic per
// channel. Live fanout runs before unlock so subscribers observe that same
// order even when several goroutines emit onto one channel concurrently.
func (b *EventBus) Emit(typedChannel eventchan.Channel, payload any) (Event, error) {
	return b.EmitScoped(typedChannel, "", "", payload)
}

// ChannelSequence returns the last published sequence without creating a ring.
// A destructive mutation can carry this boundary in both its event and RPC reply.
func (b *EventBus) ChannelSequence(channel eventchan.Channel) uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if r := b.rings[string(channel)]; r != nil {
		return r.seq
	}
	return 0
}

// EmitEntity is Emit for a caller that has already derived the frame's
// entity key (event_entity.go). Emit is the same call with an empty key,
// which delivers to every subscriber exactly as it always has.
//
// The key is a PARAMETER rather than something this package extracts,
// because the one caller that needs it also needs it for the NDJSON replay
// log, and the extraction is a reflect walk with a JSON round-trip fallback
// — paying it twice per emit to keep the signature shorter would be the
// wrong trade on the transcript-stream hot path. internal/app's emit funnel
// is where the single derivation lives.
func (b *EventBus) EmitEntity(typedChannel eventchan.Channel, entityKey string, payload any) (Event, error) {
	return b.EmitScoped(typedChannel, entityKey, "", payload)
}

// EmitScoped is EmitEntity with the frame's scope attribution as well: the
// transcript scope root inside entityKey's thread (Event.EntityScope). Same
// reason for taking it as a parameter; internal/app derives both once.
func (b *EventBus) EmitScoped(typedChannel eventchan.Channel, entityKey, entityScope string, payload any) (Event, error) {
	if b.closed.Load() {
		return Event{}, nil
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}

	// The wire spelling. Free — string and eventchan.Channel share a
	// representation — and it is what every map below is keyed by, since
	// replay cursors and subscribe filters arrive from the peer as
	// strings.
	channel := string(typedChannel)

	b.mu.Lock()
	// Close marks the bus before taking mu so an Emit already spending time in
	// payload marshaling can observe shutdown here. Without this second check,
	// that in-flight call could recreate a ring after Close cleared the bus.
	if b.closed.Load() {
		b.mu.Unlock()
		return Event{}, nil
	}
	r, ok := b.rings[channel]
	if !ok {
		if _, registered := policyForChannel(channel); !registered {
			// Once per channel (ring creation), not per frame. Loud on
			// purpose: fail-closed visibility silently withholds an
			// unregistered channel from remote clients, and this line is
			// how that omission surfaces instead of presenting as a
			// remote-only "events stopped arriving" mystery.
			log.Printf("transport: event channel %q has no ChannelPolicy row; delivering to loopback connections only (add a row in event_channels.go)", channel)
		}
		r = newRing(channel, b.capacity, b.ringBytes)
		b.rings[channel] = r
	}
	r.seq++
	wire := r.appendEventWire(r.seq, data)
	// Alias Data to the payload bytes inside WireBytes (full slice
	// expression so an accidental append can't clobber the envelope's
	// closing brace) — one payload copy per queued event instead of
	// two, and the standalone marshal buffer is released immediately.
	dataEnd := len(wire) - 1
	evt := Event{
		Channel:     channel,
		Seq:         r.seq,
		Data:        json.RawMessage(wire[dataEnd-len(data) : dataEnd : dataEnd]),
		WireBytes:   wire,
		EntityKey:   entityKey,
		EntityScope: entityScope,
	}
	// The ring retains WireBytes only: replay splices it verbatim into
	// its batch frames (or writes it through writeEventFrame's fast
	// path when a chunk holds one event). Data is dropped to keep ring
	// entries to the one WireBytes reference. EntityKey and EntityScope
	// stay, because replay applies the same watch filter live delivery
	// does and has nothing else to read the frame's address from.
	ringEvt := evt
	ringEvt.Data = nil
	// Stamped under mu, so stamps rise in ring order and releaseAged can
	// stop at the first young frame.
	r.append(ringEvt, b.clock())

	// deliver is non-blocking. Keeping fanout under mu makes sequence
	// assignment, ring append, and live delivery one ordered operation. A
	// subscriber that cannot accept immediately still uses the existing
	// seq-gap recovery contract; a healthy subscriber can no longer see a
	// false gap caused by two Emit goroutines racing after the lock.
	for _, s := range b.subList {
		s.deliver(evt)
	}
	b.mu.Unlock()
	return evt, nil
}

// encodeEventFrame marshals the event envelope
// for evt. Replay's gap markers (which carry Gap:true and null Data —
// shapes appendEventWire deliberately doesn't handle) pre-encode
// through here, and it doubles as the reference encoding the
// appendEventWire fast path is pinned against
// (see TestEventBus_PreEncodedWireBytesMatchEnvelope).
func encodeEventFrame(evt Event) ([]byte, error) {
	// Event sequences are required, including a restart marker's zero.
	// ServerFrame is a shared RPC/event union; its optional fields must
	// not decide what an event omits on the wire.
	frame := struct {
		Type       string          `json:"type"`
		Channel    string          `json:"channel"`
		Seq        uint64          `json:"seq"`
		Data       json.RawMessage `json:"data,omitempty"`
		Gap        bool            `json:"gap,omitempty"`
		GapThreads []string        `json:"gapThreads,omitempty"`
		Watermark  bool            `json:"watermark,omitempty"`
	}{
		Type:       frameTypeEvent,
		Channel:    evt.Channel,
		Seq:        evt.Seq,
		Data:       evt.Data,
		Gap:        evt.Gap,
		GapThreads: evt.GapThreads,
		Watermark:  evt.Watermark,
	}
	return json.Marshal(frame)
}

// Subscribe registers a subscriber and returns it. The caller must
// invoke Subscriber.Close when finished. Buffered channel sized for
// burst absorption; if a subscriber falls behind, deliveries drop
// silently and the client detects the resulting per-channel seq skip to
// trigger a re-fetch (see the EventBus doc comment).
//
// The bus does NOT replay history on subscribe. Callers wanting replay
// invoke Replay(lastSeqByChannel) which returns a slice of missed
// events plus gap markers.
func (b *EventBus) Subscribe() *Subscriber {
	s, _ := b.subscribe(false)
	return s
}

// SubscribeWithReplayBaseline captures the channel heads at the same instant
// delivery begins. Zero is meaningful: a channel's first event may occur
// while the client is disconnected. Reading heads after subscribing would
// let the baseline skip buffered events; reading before would leave a gap.
func (b *EventBus) SubscribeWithReplayBaseline() (*Subscriber, map[string]uint64) {
	return b.subscribe(true)
}

func (b *EventBus) subscribe(captureBaseline bool) (*Subscriber, map[string]uint64) {
	s := &Subscriber{
		ch:     make(chan Event, b.subBuf),
		done:   make(chan struct{}),
		gapped: make(map[string]map[string]struct{}),
	}
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		s.close()
		return s, nil
	}
	var baseline map[string]uint64
	if captureBaseline {
		baseline = make(map[string]uint64, len(channelPolicies))
		for _, policy := range channelPolicies {
			channel := string(policy.Channel)
			var seq uint64
			if r := b.rings[channel]; r != nil {
				seq = r.seq
			}
			baseline[channel] = seq
		}
	}
	b.subs[s] = struct{}{}
	b.subList = append(b.subList, s)
	s.bus = b
	b.mu.Unlock()
	return s, baseline
}

// SubscriberCount returns the current number of registered subscribers.
// Exposed so tests can wait for the connHandler's goroutine to attach
// without reaching into private fields.
func (b *EventBus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// ChannelSubscriberCount returns the number of subscribers that explicitly
// opted into channel filtering and selected channel. Default all-channel SPA
// subscribers are deliberately excluded so service-presence checks do not
// mistake an ordinary webview for a dedicated bridge consumer.
func (b *EventBus) ChannelSubscriberCount(channel string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := 0
	for _, subscriber := range b.subList {
		if subscriber.explicitlySubscribes(channel) {
			count++
		}
	}
	return count
}

// RemoteReceiverCount returns the number of live subscribers that are
// BOTH off this machine and granted the scope this channel takes. It is
// the "is anybody out there who could use this" question, and it exists
// for work a backend should not do at all when the answer is nobody: the
// dev-server scan (app_preview.go) polls every 3 seconds, and a
// desktop-only install must never pay for it.
//
// Channel SUBSCRIPTION is deliberately not consulted, unlike
// ChannelSubscriberCount. An SPA subscriber takes every channel by
// default, so subscription answers "is a client attached", not "does a
// client want this". The grant does answer it: a session that never
// asked for the capability cannot receive the frame either way.
//
// A subscriber with no origin recorded (the harness waiter and every
// other non-connection subscriber) is not remote and does not count. Nor
// is one whose scope filter is absent or inactive: both mean no session
// named its grants, and this asks about a session that did.
func (b *EventBus) RemoteReceiverCount(channel string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := 0
	for _, subscriber := range b.subList {
		if subscriber.closed.Load() {
			continue
		}
		loopback := subscriber.loopback.Load()
		if loopback == nil || *loopback {
			continue
		}
		scopes := subscriber.scopes.Load()
		if scopes == nil || !scopes.active || !scopes.allows(channel) {
			continue
		}
		count++
	}
	return count
}

// LocalScreenPresence answers what the BACKEND MACHINE's own screen is
// already showing, for the OS-notification gate and for nothing else
// (presence.go states the doctrine; internal/app notifyOS is the caller).
//
// focused is true when any client on this machine has window focus.
// threadVisible is true when any of them has threadID on screen. An empty
// threadID asks only the first question, which is what a notification with
// no thread behind it (a signed-out provider, an update notice, a workflow
// item) has to ask.
//
// ORed over subscribers rather than answered by one, because "this machine's
// screen" is not one connection: the embedded webview and a `--connect`
// browser tab beside it are two, and either one being looked at is a person
// looking. Loopback is what makes a subscriber local — the same flag
// eventVisibleToOrigin reads, so the two cannot disagree about which
// connections are this machine's own. A subscriber with no origin recorded
// (the harness waiter and every other non-connection subscriber) is not a
// screen and does not count, and neither does a remote one: a phone in
// another room being focused must not silence the desk.
func (b *EventBus) LocalScreenPresence(threadID string) (focused, threadVisible bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, subscriber := range b.subList {
		if subscriber.closed.Load() {
			continue
		}
		loopback := subscriber.loopback.Load()
		if loopback == nil || !*loopback {
			continue
		}
		presence := subscriber.presence.Load()
		if presence == nil {
			continue
		}
		if presence.focused {
			focused = true
		}
		if threadID != "" {
			if _, ok := presence.threads[threadID]; ok {
				threadVisible = true
			}
		}
		if focused && (threadVisible || threadID == "") {
			break
		}
	}
	return focused, threadVisible
}

// Replay returns the events the client missed since lastSeqByChannel.
// For each channel:
//   - If lastSeq is older than the oldest event in the ring, a single
//     {gap:true, seq:<current>} marker is emitted so the client knows
//     to re-fetch via list endpoints. Actual events in the ring are
//     NOT returned in this case — the client is on the wrong side of
//     the buffer and shouldn't act on partial history.
//   - If lastSeq is within the ring, every event with seq > lastSeq
//     is returned in order.
//   - If lastSeq equals or exceeds the current head, returns nothing
//     for that channel.
//
// Channels not present in lastSeqByChannel are skipped. A channel the
// bus has NO RING for is answered with a gap marker at seq 0 whenever
// the cursor is non-zero: rings are created lazily at the first Emit,
// so "no ring" after a restart means this cursor belongs to the
// previous process's sequence space and every frame the new one is
// about to send would be dropped as a duplicate. Seq 0 is below every
// seq this bus can mint, so the client resets to it and the next live
// frame passes. A zero cursor asks for nothing and gets nothing.
//
// Caller must cap the input map size before invoking — see
// MaxReplayChannels in frame.go for the wire-level cap.
// DropRetained discards every ring's history and keeps each channel's
// sequence: a cursor from before the drop missed nothing, later emits
// continue the same sequence space, and only they replay. A harness
// reset uses it: a retained activation from one test would otherwise
// reach the next test's fresh loopback page, which replays that channel
// from zero.
func (b *EventBus) DropRetained() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.rings {
		clear(r.backing)
		r.head, r.count, r.bytes = 0, 0, 0
		r.dropped = r.seq
	}
}

func (b *EventBus) Replay(lastSeqByChannel map[string]uint64) []Event {
	if len(lastSeqByChannel) == 0 {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]Event, 0)
	for channel, lastSeq := range lastSeqByChannel {
		r, ok := b.rings[channel]
		if !ok {
			// No ring: nothing has been emitted on this channel in THIS
			// process. A non-zero cursor therefore names a sequence space
			// that is not ours, which is exactly the condition
			// replayAfter's above-head branch exists for and cannot reach
			// here because there is no ring to ask. Answer the marker
			// itself, at seq 0, so the client's cursor drops below every
			// seq this bus can mint (Emit pre-increments, so the first
			// frame is 1). Without it the client silently discards every
			// live frame on this channel until the new sequence overtakes
			// the stale cursor.
			if lastSeq > 0 {
				out = append(out, replayGapMarker(channel, 0))
			}
			continue
		}
		evts, hadGap := r.replayAfter(lastSeq)
		if hadGap && lastSeq <= r.seq && channelRetention(channel) == RetentionLatestOnly {
			// Whole-state channel: frames the ring evicted are
			// superseded state, not lost history — deliver the newest
			// frame instead of a gap marker (a gap would make clients
			// log/recover for data the next frame replaces anyway).
			// Only for an eviction-side gap (lastSeq <= r.seq): a
			// cursor ABOVE our head keeps its gap marker, because the
			// newest frame carries a seq the stale cursor would
			// discard as a duplicate — the marker is what resets it.
			evts, hadGap = r.replayAfter(r.seq - 1)
		}
		if hadGap {
			out = append(out, replayGapMarker(channel, r.seq))
			continue
		}
		out = append(out, evts...)
	}
	return out
}

// replayGapMarker builds the {gap:true} frame Replay answers a
// re-fetch-needed cursor with. One constructor for both callers, so the
// ring-absent marker and the out-of-ring one cannot differ in shape.
//
// Pre-encoded here so the conn pump can write it without falling back to
// marshal: the live-pump path is the hot path, but replay stays
// symmetric with it.
func replayGapMarker(channel string, seq uint64) Event {
	gap := Event{
		Channel: channel,
		Seq:     seq,
		Gap:     true,
		Data:    json.RawMessage(`null`),
	}
	if wire, err := encodeEventFrame(gap); err == nil {
		gap.WireBytes = wire
	}
	return gap
}

// watermarkEvent builds the per-connection frame that advances a client's
// cursor on channel to seq: {"type":"event","channel":…,"seq":…,
// "watermark":true}, with no data. Pre-encoded like replayGapMarker, so
// the pump's single-event fast path and spliceBatchFrame carry it as is.
func watermarkEvent(channel string, seq uint64) Event {
	mark := Event{Channel: channel, Seq: seq, Watermark: true}
	if wire, err := encodeEventFrame(mark); err == nil {
		mark.WireBytes = wire
	}
	return mark
}

// Close stops accepting new emissions, stops the sweeper and waits for it
// to return, and signals every subscriber. Idempotent — late callers see
// closed==true and bail.
func (b *EventBus) Close() {
	if !b.closed.CompareAndSwap(false, true) {
		return
	}
	// Outside mu: a sweep in progress holds it until it finishes.
	close(b.stopSweep)
	<-b.sweepDone
	b.mu.Lock()
	subs := b.subList
	b.subs = nil
	b.subList = nil
	b.mu.Unlock()
	for _, s := range subs {
		s.close()
	}
}

// Subscriber is a per-WS event channel. The subscriber owns the
// channel and the goroutine that pumps it; the bus only knows it
// exists.
//
// We deliberately do NOT close the events channel on shutdown — that
// would race deliver() and panic on send-after-close. Instead, a Done
// channel is closed which the consumer selects on alongside Events().
type Subscriber struct {
	ch       chan Event
	done     chan struct{}
	bus      *EventBus
	closed   atomic.Bool
	channels atomic.Pointer[subscriberChannelFilter]
	// loopback, when set, applies the per-origin channel-visibility
	// filter (event_visibility.go) at enqueue time so frames the
	// connection could never emit don't consume buffer slots — a burst
	// of invisible traffic (terminal:output toward a remote peer,
	// highlight:seed toward loopback) could otherwise force drops of
	// visible events and gap-driven re-fetches. nil means unfiltered
	// (non-conn subscribers like the harness workflow waiter).
	loopback atomic.Pointer[bool]
	// scopes, when set, applies the per-GRANT channel filter at the same
	// point and for the same reason (event_visibility.go): a
	// session-carrying connection must not spend buffer slots on channels
	// its grants never open. nil means unfiltered, which is what every
	// non-conn subscriber and every connection naming no session gets.
	scopes atomic.Pointer[eventScopeFilter]
	// watched, when set, narrows EntityFiltered channels to the entities
	// this connection named in a `watch` frame (event_entity.go). nil
	// means wildcard, which is what every subscriber gets until the first
	// such frame arrives and what every non-SPA client keeps forever.
	watched atomic.Pointer[subscriberWatchFilter]
	// background is this connection's lease (lease.go): true while the
	// client says its whole app is paused. A plain bool rather than the
	// pointer shape beside it, because "never leased" and "leased active"
	// want the same behavior — the frame states a lifecycle, not a filter
	// that latches out of wildcard.
	background atomic.Bool
	// presence is what this connection's screen is already showing
	// (presence.go): nil until the first `presence` frame, which reads as
	// "not attended". Never consulted by deliver — it changes what the OS
	// notification gate RAISES, never what this subscriber is sent.
	presence atomic.Pointer[subscriberPresence]
	// gapped records the channels this subscriber has dropped events on
	// since it last learned about the loss, each with the entity keys of
	// the frames dropped (nil when the loss is not attributed, see
	// noteDrop). Written only inside deliver, which runs under the bus
	// mutex (Emit's fanout is its sole call site), so no extra locking.
	// See the EventBus doc comment for the announce protocol.
	gapped map[string]map[string]struct{}
	// withheld maps a channel to the newest seq deliver withheld from this
	// subscriber by the connection's own election (its watch set or its
	// background lease) since the pump last took it. The pump turns each
	// entry into a watermark (takeWithheld). deliver writes it under the
	// bus mutex and the pump takes it without, hence withheldMu.
	withheldMu sync.Mutex
	withheld   map[string]uint64
}

type subscriberChannelFilter map[string]struct{}

// Events returns the receive-only event channel. Callers must select
// on Events() AND Done() simultaneously — Events never closes, Done
// closes on shutdown.
func (s *Subscriber) Events() <-chan Event { return s.ch }

// Done returns a channel that's closed when the subscriber has been
// shut down. Receive returns immediately when closed.
func (s *Subscriber) Done() <-chan struct{} { return s.done }

// SetChannels opts this subscriber into a narrow event set. Before this is
// called a subscriber receives every channel, preserving the existing SPA
// wire contract.
func (s *Subscriber) SetChannels(channels []string) {
	filter := make(subscriberChannelFilter, len(channels))
	for _, channel := range channels {
		filter[channel] = struct{}{}
	}
	s.channels.Store(&filter)
}

// SetWatch narrows this subscriber's EntityFiltered channels to the given
// entity ids, and its TranscriptScopeFiltered channels further to the given
// transcript scopes (event_entity.go). Both sets are ABSOLUTE and replace
// the previous ones together, and an EMPTY slice is a legal, meaningful
// value meaning "watching nothing", which is what a client with no panes
// open has.
//
// Like SetChannels this is a ONE-WAY LATCH out of wildcard: once a
// subscriber has a set, every later call replaces it, and there is no call
// that restores the nil. A client that wants wildcard again reconnects.
// Deliberate — "" as a wildcard sentinel inside the set, or a nil slice
// meaning "unset" while an empty one means "none", are both shapes where a
// client bug reads as a silent full-stream subscription.
//
// scopes is the one place a nil slice and an empty one differ. Nil states
// no scope set: every scope of a watched thread is admitted, which is what
// a client that does not narrow by scope receives. The latch argument above
// does not apply to it, because the widest reading it allows is the watched
// threads' own rows; a client bug that drops the field costs wire bytes for
// threads it named, never rows a surface needs. scopeThreads completes a
// stated set with the threads whose every scope is admitted
// (frame.go ClientFrame.ScopeThreads).
func (s *Subscriber) SetWatch(entityIDs []string, scopes []WatchScope, scopeThreads []string) {
	filter := subscriberWatchFilter{threads: make(map[string]struct{}, len(entityIDs))}
	for _, id := range entityIDs {
		filter.threads[id] = struct{}{}
	}
	if scopes != nil {
		filter.scopes = make(map[WatchScope]struct{}, len(scopes))
		for _, scope := range scopes {
			filter.scopes[scope] = struct{}{}
		}
		filter.scopeThreads = make(map[string]struct{}, len(scopeThreads))
		for _, id := range scopeThreads {
			filter.scopeThreads[id] = struct{}{}
		}
	}
	s.watched.Store(&filter)
}

// SetBackground records this subscriber's lease state (lease.go). Unlike
// SetChannels and SetWatch this is NOT a latch: a client that
// backgrounds and resumes says so both times, and `active` restores exactly
// the delivery it had. Read on the fanout path, so it is stored as an
// atomic and never read under a lock.
func (s *Subscriber) SetBackground(background bool) {
	s.background.Store(background)
}

// SetPresence records what this connection's screen is showing (presence.go).
// Like SetBackground and unlike SetChannels / SetWatch this is NOT a
// latch: each call replaces both halves at once, because they describe one
// instant and a focus bit paired with a stale thread set is a fact that was
// never true.
func (s *Subscriber) SetPresence(focused bool, threadIDs []string) {
	next := &subscriberPresence{focused: focused}
	if len(threadIDs) > 0 {
		next.threads = make(map[string]struct{}, len(threadIDs))
		for _, id := range threadIDs {
			next.threads[id] = struct{}{}
		}
	}
	s.presence.Store(next)
}

// SetOriginLoopback arms enqueue-time origin-visibility filtering for a
// connection-owned subscriber. Call it before any event matters (the
// conn handler sets it between Subscribe and starting the pump); the
// pump keeps its own visibility check as the correctness gate, this one
// only decides what may occupy buffer slots.
func (s *Subscriber) SetOriginLoopback(isLoopback bool) {
	s.loopback.Store(&isLoopback)
}

// SetScopeFilter arms the grant half of the same enqueue-time filtering.
// Same timing contract as SetOriginLoopback: set it before any event
// matters, and the pump's own check stays the correctness gate.
func (s *Subscriber) SetScopeFilter(filter eventScopeFilter) {
	s.scopes.Store(&filter)
}

func (s *Subscriber) accepts(channel string) bool {
	filter := s.channels.Load()
	if filter == nil {
		return true
	}
	_, ok := (*filter)[channel]
	return ok
}

// watches reports whether this subscriber's watch set admits a frame on
// channel addressed to entityKey, in transcript scope entityScope. True for
// every subscriber that never sent a watch frame, every channel the
// registry does not mark EntityFiltered, and every frame with no entity key
// (event_entity.go argues that last one). The scope applies only on a
// TranscriptScopeFiltered channel (subscriberWatchFilter.admits).
//
// The check order is the hot path's: the empty-key test is free and answers
// almost every frame in the process today, the atomic load is next, and the
// registry probe runs only once a filter is actually armed.
func (s *Subscriber) watches(channel, entityKey, entityScope string) bool {
	if entityKey == "" {
		return true
	}
	filter := s.watched.Load()
	if filter == nil {
		return true
	}
	if !channelEntityFiltered(channel) {
		return true
	}
	return filter.admits(channel, entityKey, entityScope)
}

func (s *Subscriber) explicitlySubscribes(channel string) bool {
	filter := s.channels.Load()
	if filter == nil {
		return false
	}
	_, ok := (*filter)[channel]
	return ok
}

// deliver pushes an event into the subscriber's buffered channel. A
// full channel drops the event and records the channel in s.gapped; the
// next event that does fit on that channel is stamped Gap:true so the
// client resyncs even when nothing else ever arrives to expose a seq
// skip. Runs only from Emit's fanout, under the bus mutex.
func (s *Subscriber) deliver(e Event) {
	if s.closed.Load() {
		return
	}
	if !s.accepts(e.Channel) {
		return
	}
	if lb := s.loopback.Load(); lb != nil && !eventVisibleToOrigin(e.Channel, *lb) {
		return
	}
	if scopes := s.scopes.Load(); scopes != nil && !scopes.allows(e.Channel) {
		return
	}
	// Ahead of every gap concern, exactly like the three filters above it:
	// a frame this connection was never addressed by is not a frame it
	// lost, so it must not mark the channel gapped and must not trigger the
	// announce protocol. The client's own forward-skip detection is
	// exempted for these channels for the same reason
	// (frontend/src/lib/transport/entityFilteredChannels.ts). A frame
	// withheld by scope takes this branch too.
	//
	// This branch and the lease branch below are the only withholds that
	// record a watermark (noteWithheld): the connection is entitled to the
	// channel and elected not to receive this frame now. The three filters
	// above mark nothing, because their channels never reach this
	// connection's cursor: hello's baseline and handleReplay apply the same
	// visibility filter, and a connection that subscribed to named channels
	// keeps cursors for those channels only.
	if !s.watches(e.Channel, e.EntityKey, e.EntityScope) {
		s.noteWithheld(e)
		return
	}
	// The last of the withholding filters, and ahead of gap accounting for
	// the same reason all of them are: a cache warmer this paused client
	// was not sent is not a frame it lost, so it must not flag the channel
	// (lease.go). The atomic is read FIRST because it is false on every
	// connection that never leased background, which short-circuits the
	// map probe away on the fanout hot path.
	if s.background.Load() && backgroundWithheldChannel(e.Channel) {
		s.noteWithheld(e)
		return
	}
	if len(s.gapped) > 0 {
		s.flushGapMarkers(e.Channel)
	}
	out := e
	if threads, isGapped := s.gapped[e.Channel]; isGapped {
		// Ride the loss announcement on this frame rather than spending
		// a buffer slot on a standalone marker. Per-subscriber re-encode:
		// WireBytes is shared across subscribers and must not be mutated.
		stamped := e
		stamped.Gap = true
		stamped.GapThreads = gapThreadList(threads)
		if wire, err := encodeEventFrame(stamped); err == nil {
			stamped.WireBytes = wire
			out = stamped
		}
	}
	// Delivered or dropped, this frame's seq is above the channel's mark:
	// a delivered one moves the client's cursor past the mark itself, and
	// a dropped one must not be passed by it.
	s.clearWithheld(e.Channel)
	select {
	case s.ch <- out:
		if out.Gap {
			delete(s.gapped, e.Channel)
		}
	case <-s.done:
	default:
		// Drop. A whole-state channel's next frame supersedes the lost
		// one (the same reasoning as Replay's latest-only carve-out), so
		// only deeper retentions need the loss announced.
		if channelRetention(e.Channel) != RetentionLatestOnly {
			s.noteDrop(e)
		}
	}
}

// noteWithheld records e as the newest frame on its channel this
// subscriber elected not to receive, for the pump's next watermark. Runs
// only from deliver, under the bus mutex, so s.gapped is safe to read.
//
// A watermark must never advance the cursor past a frame this connection
// LOST. A dropped frame is recovered by replay from the cursor, and a mark
// past it would skip it silently once this subscriber, and the gapped set
// that still announces the loss, is gone. So nothing is recorded while the
// channel has an unannounced loss, and deliver clears the mark on every
// frame that reaches the enqueue attempt (clearWithheld), delivered or
// dropped.
func (s *Subscriber) noteWithheld(e Event) {
	if _, lost := s.gapped[e.Channel]; lost {
		return
	}
	s.withheldMu.Lock()
	if s.withheld == nil {
		s.withheld = make(map[string]uint64)
	}
	s.withheld[e.Channel] = e.Seq
	s.withheldMu.Unlock()
}

// clearWithheld drops channel's mark; see noteWithheld.
func (s *Subscriber) clearWithheld(channel string) {
	s.withheldMu.Lock()
	delete(s.withheld, channel)
	s.withheldMu.Unlock()
}

// takeWithheld hands the pump every mark recorded since the last call and
// starts a new set. Nil when there are none, without allocating.
func (s *Subscriber) takeWithheld() map[string]uint64 {
	s.withheldMu.Lock()
	marks := s.withheld
	s.withheld = nil
	s.withheldMu.Unlock()
	if len(marks) == 0 {
		return nil
	}
	return marks
}

// noteDrop records a frame this subscriber could not take. The channel's
// set collects the dropped frames' entity keys so the announcement can
// name the threads that lost frames. A frame with no key cannot be
// attributed, and neither can more keys than a watch set may hold, so
// either leaves the channel's loss unattributed (a nil set) until it is
// announced. An armed watch filter keeps the set within the threads it
// names, directly or through a scope: deliver drops only frames it would
// have sent. A scoped frame is recorded by its thread, never its scope.
func (s *Subscriber) noteDrop(e Event) {
	threads, gapped := s.gapped[e.Channel]
	switch {
	case gapped && threads == nil:
		// Already unattributed.
	case e.EntityKey == "":
		s.gapped[e.Channel] = nil
	case !gapped:
		s.gapped[e.Channel] = map[string]struct{}{e.EntityKey: {}}
	default:
		threads[e.EntityKey] = struct{}{}
		if len(threads) > MaxWatchThreads {
			s.gapped[e.Channel] = nil
		}
	}
}

// gapThreadList is the wire form of a gapped channel's set: sorted, and
// nil for an unattributed loss.
func gapThreadList(threads map[string]struct{}) []string {
	if threads == nil {
		return nil
	}
	list := make([]string, 0, len(threads))
	for id := range threads {
		list = append(list, id)
	}
	slices.Sort(list)
	return list
}

// flushGapMarkers enqueues a standalone {gap:true} marker — the same
// shape Replay emits — for every gapped channel other than the one
// being delivered, which announces its own loss by riding the delivered
// frame. Non-blocking: a marker that doesn't fit stays flagged and is
// retried on the next delivery. Runs under the bus mutex (see deliver),
// which is what makes the s.bus.rings read safe.
func (s *Subscriber) flushGapMarkers(deliveringChannel string) {
	for channel, threads := range s.gapped {
		if channel == deliveringChannel {
			continue
		}
		r, ok := s.bus.rings[channel]
		if !ok {
			delete(s.gapped, channel)
			continue
		}
		marker := Event{
			Channel:    channel,
			Seq:        r.seq,
			Gap:        true,
			GapThreads: gapThreadList(threads),
			Data:       json.RawMessage(`null`),
		}
		wire, err := encodeEventFrame(marker)
		if err != nil {
			continue
		}
		marker.WireBytes = wire
		select {
		case s.ch <- marker:
			delete(s.gapped, channel)
		default:
			// Still full; the flag survives for the next delivery.
		}
	}
}

// Close detaches the subscriber from the bus and signals Done. Safe
// to call multiple times. The events channel is intentionally not
// closed to avoid send-on-closed races in deliver().
func (s *Subscriber) Close() {
	s.close()
	if s.bus != nil {
		s.bus.mu.Lock()
		delete(s.bus.subs, s)
		s.bus.subList = removeSub(s.bus.subList, s)
		s.bus.mu.Unlock()
	}
}

// removeSub returns subs with the first occurrence of target removed,
// preserving order. Tens of subscribers in a deployment make linear
// scan cheap relative to allocation; the swap-with-tail pattern would
// shuffle delivery order across reconnects without buying anything.
// Returns the input slice unchanged when target is not present.
func removeSub(subs []*Subscriber, target *Subscriber) []*Subscriber {
	for i, s := range subs {
		if s == target {
			out := make([]*Subscriber, 0, len(subs)-1)
			out = append(out, subs[:i]...)
			out = append(out, subs[i+1:]...)
			return out
		}
	}
	return subs
}

func (s *Subscriber) close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.done)
}

package transport

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
)

// DefaultRingCapacity is the per-channel event buffer size. Tuned to
// cover a few seconds of streaming bursts (item deltas peak around
// 500/frame) plus a margin for momentary network hiccups. Tests can
// configure smaller rings to exercise overflow.
const DefaultRingCapacity = 1000

// DefaultSubscriberBuffer sizes a subscriber's delivery channel.
// Matches DefaultRingCapacity so a slow subscriber and a slow ring
// drop at the same time — asymmetric values would let the subscriber
// gap before the ring did, defeating in-window replay on reconnect.
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
//   - Emit takes mu (writer) briefly to bump the seq, append into the
//     ring, and read the maintained subscriber slice; fanout happens
//     after the lock is released.
//   - Replay holds an RLock for the duration of the per-channel walk.
//   - subList is a maintained slice mirroring subs map membership so
//     Emit doesn't allocate-and-copy a snapshot per call. Updated
//     inside Subscribe / Subscriber.Close under the same mu. Slice
//     mutations are copy-on-remove (and append-grow on add), so a
//     snapshot taken under Lock stays safe to iterate after Unlock —
//     subsequent Subscribe / Close don't touch the snapshot's
//     backing array slots.
//   - Subscribers' deliver() drops to a non-blocking select; a slow
//     consumer drops events. The wsClient (Phase B) detects per-channel
//     seq gaps and re-fetches via list endpoints.
type EventBus struct {
	mu       sync.RWMutex
	rings    map[string]*ring
	subs     map[*Subscriber]struct{}
	subList  []*Subscriber
	capacity int
	subBuf   int
	closed   atomic.Bool
}

// ring is a bounded circular buffer keyed by per-channel seq.
// head + count slide around backing[]; no copies on overflow. The
// backing array starts small and doubles up to capacity as events
// arrive — most channels never see more than a handful of events, so
// preallocating capacity slots per channel would waste the bulk of
// the bus's steady-state memory.
type ring struct {
	head     int
	count    int
	seq      uint64
	capacity int
	backing  []Event
	// envPrefix is the fixed leading portion of every live event
	// envelope on this channel: `{"type":"event","channel":<escaped>,"seq":`.
	// Cached so Emit can assemble WireBytes with plain appends under the
	// bus mutex instead of a reflection marshal (see appendEventWire).
	envPrefix []byte
}

// ringInitialCapacity is the first backing allocation for a non-empty
// ring. Small on purpose: quiet channels stay small forever.
const ringInitialCapacity = 16

func newRing(channel string, capacity int) *ring {
	// json.Marshal of a string cannot fail (invalid UTF-8 is replaced),
	// and matches exactly how the reflection encoder renders the
	// ServerFrame Channel field, HTML escaping included.
	name, _ := json.Marshal(channel)
	prefix := make([]byte, 0, len(eventFramePrefix)+len(name)+len(eventSeqKey))
	prefix = append(prefix, eventFramePrefix...)
	prefix = append(prefix, name...)
	prefix = append(prefix, eventSeqKey...)
	return &ring{capacity: capacity, envPrefix: prefix}
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

// append stores e at the tail, growing the backing up to capacity and
// evicting the oldest entry once full. Amortized O(1). A zero-capacity
// ring (ephemeral channels) tracks sequence only and retains nothing.
func (r *ring) append(e Event) {
	if r.capacity == 0 {
		return
	}
	if r.count == len(r.backing) && r.count < r.capacity {
		r.grow()
	}
	cap := len(r.backing)
	if r.count < cap {
		r.backing[(r.head+r.count)%cap] = e
		r.count++
	} else {
		r.backing[r.head] = e
		r.head = (r.head + 1) % cap
	}
	r.seq = e.Seq
}

// grow re-linearizes the ring into a larger backing array (head back
// to 0). Doubling bounded by capacity; at most log2(capacity) growths
// over a channel's lifetime.
func (r *ring) grow() {
	newCap := min(max(2*len(r.backing), ringInitialCapacity), r.capacity)
	fresh := make([]Event, newCap)
	for i := range r.count {
		fresh[i] = r.backing[(r.head+i)%len(r.backing)]
	}
	r.backing = fresh
	r.head = 0
}

// replayAfter walks the ring and returns every event with seq > lastSeq.
// Returns hadGap=true when lastSeq fell outside the ring — either the
// eviction end (the events the client wanted are gone) or the head end
// (the client's cursor is ahead of ours) — so the caller can emit a
// single gap marker instead of partial history.
func (r *ring) replayAfter(lastSeq uint64) (events []Event, hadGap bool) {
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
		return nil, false
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

// Event is one frame on the wire (encoded as ServerFrame{Type:"event"}).
// Data is kept as json.RawMessage so the bus can store the encoded
// bytes once and broadcast them to N subscribers without re-marshalling.
//
// WireBytes is the pre-encoded ServerFrame envelope (type="event",
// channel, seq, data, gap?) assembled once at Emit time. The conn
// event-pump writes WireBytes verbatim — single events and spliced
// batch frames alike — so a multi-subscriber LAN bind doesn't pay
// N×marshal cost. On live-fanout events Data is a subslice of
// WireBytes (the payload bytes inside the envelope), so a queued event
// holds ONE payload copy, not two, across up-to-1024-deep subscriber
// buffers; Data stays populated because non-wire subscribers (the
// harness workflow waiter) decode payloads from it.
type Event struct {
	Channel   string
	Seq       uint64
	Data      json.RawMessage
	Gap       bool
	WireBytes []byte
}

// NewEventBus returns a new bus with the given per-channel capacity.
// Pass 0 to use DefaultRingCapacity.
func NewEventBus(capacity int) *EventBus {
	if capacity <= 0 {
		capacity = DefaultRingCapacity
	}
	return &EventBus{
		rings:    make(map[string]*ring),
		subs:     make(map[*Subscriber]struct{}),
		subList:  nil,
		capacity: capacity,
		subBuf:   DefaultSubscriberBuffer,
	}
}

// Emit publishes a new event on the given channel. Returns the event so
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
// channel — ring order is exact; fanout runs after unlock, as before.
func (b *EventBus) Emit(channel string, payload any) (Event, error) {
	if b.closed.Load() {
		return Event{}, nil
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}

	b.mu.Lock()
	r, ok := b.rings[channel]
	if !ok {
		capacity := b.capacity
		if ephemeralEventChannels[channel] {
			// Seed-style cache warmers: sequence tracking only, no
			// replay retention (see event_visibility.go).
			capacity = 0
		} else if latestOnlyEventChannels[channel] {
			// Whole-state frames: the newest one supersedes all prior
			// ones, so retain exactly it (see event_visibility.go).
			capacity = 1
		}
		r = newRing(channel, capacity)
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
		Channel:   channel,
		Seq:       r.seq,
		Data:      json.RawMessage(wire[dataEnd-len(data) : dataEnd : dataEnd]),
		WireBytes: wire,
	}
	// The ring retains WireBytes only: replay splices it verbatim into
	// its batch frames (or writes it through writeEventFrame's fast
	// path when a chunk holds one event). Data is dropped to keep ring
	// entries to the one WireBytes reference.
	ringEvt := evt
	ringEvt.Data = nil
	r.append(ringEvt)

	// Snapshot the maintained slice under the same lock. Subs join /
	// leave through Subscribe / Close, so this is a single bounded
	// copy rather than a map-walk-and-allocate per Emit.
	subs := b.subList
	b.mu.Unlock()

	for _, s := range subs {
		s.deliver(evt)
	}
	return evt, nil
}

// encodeEventFrame marshals the ServerFrame{type:"event", ...} envelope
// for evt. Replay's gap markers (which carry Gap:true and null Data —
// shapes appendEventWire deliberately doesn't handle) pre-encode
// through here, and it doubles as the reference encoding the
// appendEventWire fast path is pinned against
// (see TestEventBus_PreEncodedWireBytesMatchEnvelope).
func encodeEventFrame(evt Event) ([]byte, error) {
	frame := ServerFrame{
		Type:    frameTypeEvent,
		Channel: evt.Channel,
		Seq:     evt.Seq,
		Data:    evt.Data,
		Gap:     evt.Gap,
	}
	return json.Marshal(frame)
}

// Subscribe registers a subscriber and returns it. The caller must
// invoke Subscriber.Close when finished. Buffered channel sized for
// burst absorption; if a subscriber falls behind, deliveries drop
// silently and the wsClient (Phase B) detects per-channel seq gaps to
// trigger a re-fetch.
//
// The bus does NOT replay history on subscribe. Callers wanting replay
// invoke Replay(lastSeqByChannel) which returns a slice of missed
// events plus gap markers.
func (b *EventBus) Subscribe() *Subscriber {
	s := &Subscriber{
		ch:   make(chan Event, b.subBuf),
		done: make(chan struct{}),
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.subList = append(b.subList, s)
	s.bus = b
	b.mu.Unlock()
	return s
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
// Channels not present in lastSeqByChannel are skipped. Channels in the
// map that the bus has never seen are silently ignored. Caller must
// cap the input map size before invoking — see MaxReplayChannels in
// frame.go for the wire-level cap.
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
			continue
		}
		evts, hadGap := r.replayAfter(lastSeq)
		if hadGap && lastSeq <= r.seq && latestOnlyEventChannels[channel] {
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
			gap := Event{
				Channel: channel,
				Seq:     r.seq,
				Gap:     true,
				Data:    json.RawMessage(`null`),
			}
			// Pre-encode the gap marker so the conn pump can write
			// it without falling back to marshal — the live-pump path
			// is the hot path but we keep replay symmetric.
			if wire, err := encodeEventFrame(gap); err == nil {
				gap.WireBytes = wire
			}
			out = append(out, gap)
			continue
		}
		out = append(out, evts...)
	}
	return out
}

// Close stops accepting new emissions and signals every subscriber.
// Idempotent — late callers see closed==true and bail.
func (b *EventBus) Close() {
	if !b.closed.CompareAndSwap(false, true) {
		return
	}
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

// SetOriginLoopback arms enqueue-time origin-visibility filtering for a
// connection-owned subscriber. Call it before any event matters (the
// conn handler sets it between Subscribe and starting the pump); the
// pump keeps its own visibility check as the correctness gate, this one
// only decides what may occupy buffer slots.
func (s *Subscriber) SetOriginLoopback(isLoopback bool) {
	s.loopback.Store(&isLoopback)
}

func (s *Subscriber) accepts(channel string) bool {
	filter := s.channels.Load()
	if filter == nil {
		return true
	}
	_, ok := (*filter)[channel]
	return ok
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
// full channel drops the event silently — the wsClient detects the
// per-channel seq gap on the next received event and re-fetches via
// list endpoints. Falling behind is the subscriber's problem to detect.
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
	select {
	case s.ch <- e:
	case <-s.done:
	default:
		// Drop. Client recovers via per-channel seq-gap detection.
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

package harnessclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The event side of a connection: what arrived, what a caller is still
// waiting for, and the consume bit that makes two identical waits
// observe two occurrences. Split from client.go because the RPC half and
// the assertion half share only the mutex.

// defaultEventLogCap bounds the in-memory event log, matching the TS
// client. The log is an assertion surface, not a history store: a run
// long enough to overflow it is a soak, and a soak asserts on the
// evidence files instead.
const defaultEventLogCap = 10_000

// logShedDivisor decides how much of the log is dropped when it fills:
// the oldest 1/N of the cap, in one move, rather than one event per
// event. Shifting by one on every arrival is O(cap) per event and so
// quadratic over a sustained stream — with the default cap that is a
// 10,000-element memmove per frame, on the read loop, under the mutex.
// Shedding a quarter turns it into one memmove per 2,500 frames. The cost
// is that a Count taken just after a shed sees fewer historical events
// than the cap implies, which the log's own contract already allows: it
// is an assertion surface over recent traffic, not a history store.
const logShedDivisor = 4

// Event is one pushed frame as a caller sees it.
type Event struct {
	Channel string          `json:"channel"`
	Seq     uint64          `json:"seq"`
	Data    json.RawMessage `json:"data"`
	// Gap marks a replay cursor the server could not satisfy from its
	// ring: the client should re-read through list endpoints rather than
	// trust the history it holds.
	Gap bool `json:"gap,omitempty"`
}

// SequenceGap records a forward jump in one channel's event sequence. The
// transport assigns sequences per channel, so a single global cursor would
// manufacture gaps whenever two channels interleave.
type SequenceGap struct {
	Channel  string    `json:"channel"`
	Expected uint64    `json:"expected"`
	Observed uint64    `json:"observed"`
	At       time.Time `json:"at"`
	Replay   bool      `json:"replay,omitempty"`
}

// SequenceFault records an event that did not advance a channel's sequence.
// A duplicate or rewind is distinct from a forward gap. Both invalidate a
// stream oracle, but they need different investigation paths.
type SequenceFault struct {
	Channel  string    `json:"channel"`
	Previous uint64    `json:"previous"`
	Observed uint64    `json:"observed"`
	At       time.Time `json:"at"`
	Replay   bool      `json:"replay,omitempty"`
}

type channelSequence struct {
	last uint64
	seen bool
}

type replaySequence struct {
	baseline uint64
	observed map[uint64]struct{}
	gap      bool
}

// logEntry is an Event plus the consume bit. Consumption is what makes
// two identical waits observe two occurrences instead of the same one
// twice — multi-turn assertions depend on it, so it is a property of the
// log rather than of any one waiter.
type logEntry struct {
	Event
	consumed bool
}

type waiter struct {
	channel string
	match   func(Event) bool
	out     chan Event
}

// WaitOptions tunes which occurrence a wait is allowed to settle on.
// The zero value is the historical behaviour: scan everything already
// received, oldest first, then park.
type WaitOptions struct {
	// Newest scans the retained history NEWEST-first. The default picks
	// the OLDEST unconsumed match, which is right for a long-lived client
	// stepping through a stream in order and wrong for a one-shot caller
	// that just pulled a replay ring and wants the latest occurrence.
	Newest bool
	// MinSeq ignores events at or below this server sequence. Zero means
	// no floor — and an event whose own Seq is zero is never filtered,
	// because that is what a locally-injected frame looks like.
	MinSeq uint64
	// SkipHistory parks a waiter without looking at what already arrived,
	// so only events pushed AFTER the call can satisfy it.
	SkipHistory bool
}

// Awaiting is a wait that is already PARKED: Await registers the waiter
// and returns, so a caller can then do the thing that produces the event
// without a window in which the answer could arrive unobserved. That is
// the difference between `send` and `send --wait`: a mock can complete a
// turn inside the SendMessage round trip.
type Awaiting struct {
	c       *Client
	key     int
	w       *waiter
	channel string
	// err records a client that was already closed when the wait was
	// parked, so Wait answers instead of blocking on a dead connection.
	err error
}

// Await parks a wait for the next matching event and returns the handle
// to block on. Every Await must be finished with Wait or Close, or its
// waiter stays in the map for the life of the connection.
func (c *Client) Await(channel string, match func(Event) bool) *Awaiting {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.awaitLocked(channel, match)
}

// awaitLocked is Await with c.mu already held, which is what lets
// WaitForEventOpts scan history and park in ONE critical section. Doing
// the two under separate locks would drop an event that arrived in
// between: it would file as unconsumed with nobody waiting, and the
// waiter parked a moment later would never see it.
func (c *Client) awaitLocked(channel string, match func(Event) bool) *Awaiting {
	if c.closed {
		err := c.closeErr
		if err == nil {
			err = errors.New("connection closed")
		}
		return &Awaiting{c: c, channel: channel, err: fmt.Errorf("wait for %s: %w", channel, err)}
	}
	w := &waiter{channel: channel, match: match, out: make(chan Event, 1)}
	c.nextHook++
	key := c.nextHook
	c.waiters[key] = w
	return &Awaiting{c: c, key: key, w: w, channel: channel}
}

// retire removes the waiter AND drains anything dispatch already handed
// it, in one critical section.
//
// Both halves are load-bearing. dispatch marks the log entry consumed
// and hands the event to the waiter's channel while holding c.mu, so a
// timeout that only deleted the waiter would destroy the sole copy of an
// event that DID arrive: consumed in the log, unread in a channel nobody
// holds any more. Draining under the same lock makes "dispatch got there
// first" and "the deadline got there first" a decision rather than a
// race with two losers.
func (a *Awaiting) retire() (Event, bool) {
	if a.w == nil {
		return Event{}, false
	}
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	delete(a.c.waiters, a.key)
	select {
	case event := <-a.w.out:
		return event, true
	default:
		return Event{}, false
	}
}

// Close retires an Await the caller is abandoning without blocking on
// it. An event dispatch had already handed over is dropped, so this is
// for an error path that is giving up on the wait, never a substitute
// for Wait.
func (a *Awaiting) Close() { a.retire() }

// Wait blocks until the parked wait settles. A deadline that expires
// AFTER dispatch handed the event over still returns the event: it
// arrived, and reporting a timeout would both lie and consume it.
func (a *Awaiting) Wait(ctx context.Context) (Event, error) {
	if a.err != nil {
		return Event{}, a.err
	}
	select {
	case event := <-a.w.out:
		a.retire()
		return event, nil
	case <-ctx.Done():
		if event, ok := a.retire(); ok {
			return event, nil
		}
		return Event{}, fmt.Errorf("wait for %s: %w (recent channels: %s)", a.channel, ctx.Err(), a.c.recentChannels(20))
	case <-a.c.readDone:
		if event, ok := a.retire(); ok {
			return event, nil
		}
		return Event{}, fmt.Errorf("wait for %s: connection closed", a.channel)
	}
}

// WaitForEvent blocks until an event on channel matches, CONSUMING it:
// waiting twice for the same shape observes two distinct occurrences
// rather than the first one twice. Events already received are scanned
// first, so a fast backend cannot win the race against the caller.
//
// A nil match accepts any event on the channel. The predicate sees the
// whole Event rather than only its payload, because a replay gap marker
// is an event on the channel that carries no data and must not satisfy a
// wait for real traffic.
func (c *Client) WaitForEvent(ctx context.Context, channel string, match func(Event) bool) (Event, error) {
	return c.WaitForEventOpts(ctx, channel, WaitOptions{}, match)
}

// WaitForEventOpts is WaitForEvent with the history scan's direction and
// floor under the caller's control. A one-shot CLI that just pulled a
// replay ring wants the NEWEST match; a test stepping through a stream
// wants the oldest, which is the default.
func (c *Client) WaitForEventOpts(ctx context.Context, channel string, opts WaitOptions, match func(Event) bool) (Event, error) {
	accept := func(ev Event) bool {
		if opts.MinSeq > 0 && ev.Seq != 0 && ev.Seq <= opts.MinSeq {
			return false
		}
		return match == nil || match(ev)
	}

	c.mu.Lock()
	if !opts.SkipHistory {
		found := -1
		for i := range c.log {
			entry := &c.log[i]
			if entry.consumed || entry.Channel != channel {
				continue
			}
			if !accept(entry.Event) {
				continue
			}
			found = i
			if !opts.Newest {
				break
			}
		}
		if found >= 0 {
			c.log[found].consumed = true
			event := c.log[found].Event
			c.mu.Unlock()
			return event, nil
		}
	}
	a := c.awaitLocked(channel, accept)
	c.mu.Unlock()
	return a.Wait(ctx)
}

// Count reports how many events on a channel matched, consumed or not.
// It is the absence half of an event assertion: a wait can only prove
// something happened. The twin of `countEvents` in e2e/src/harness.ts —
// kept in step with it deliberately, so an assertion written against one
// client reads the same against the other.
func (c *Client) Count(channel string, match func(Event) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for i := range c.log {
		if c.log[i].Channel != channel {
			continue
		}
		if match != nil && !match(c.log[i].Event) {
			continue
		}
		n++
	}
	return n
}

// Clear drops the remembered events. Call it after a reset so stale
// matches cannot satisfy a later wait.
func (c *Client) Clear() {
	c.mu.Lock()
	c.log = nil
	c.mu.Unlock()
}

// SequenceGaps returns a snapshot of transport sequence gaps observed on
// this connection. It includes explicit replay gap markers and live forward
// jumps. The slice is safe for callers to retain.
func (c *Client) SequenceGaps() []SequenceGap {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SequenceGap(nil), c.seqGaps...)
}

// SequenceFaults returns duplicate or backwards sequence observations on
// this connection. A replay can legitimately arrive out of order, so replay
// observations are marked and callers may decide whether to reject them.
func (c *Client) SequenceFaults() []SequenceFault {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SequenceFault(nil), c.seqFaults...)
}

// ClearSequenceObservations starts a fresh stream oracle without dropping
// the event log. This is useful after a deliberate backend reset when the
// caller does not want the old connection's sequence space in its receipt.
func (c *Client) ClearSequenceObservations() {
	c.mu.Lock()
	c.sequences = make(map[string]channelSequence)
	c.seqGaps = nil
	c.seqFaults = nil
	c.mu.Unlock()
}

// Events snapshots the log in arrival order.
func (c *Client) Events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.log))
	for i := range c.log {
		out[i] = c.log[i].Event
	}
	return out
}

// Listen registers a callback invoked for every event as it arrives, and
// returns the function that removes it.
//
// The callback runs ON THE READ LOOP: it must not call back into this
// client (a Call would wait for a response the blocked read loop can
// never deliver). Print, count, or hand the event to a channel.
func (c *Client) Listen(fn func(Event)) (cancel func()) {
	c.mu.Lock()
	c.nextHook++
	key := c.nextHook
	c.listeners[key] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.listeners, key)
		c.mu.Unlock()
	}
}

func (c *Client) dispatch(event Event) {
	c.mu.Lock()
	c.observeSequenceLocked(event)
	entry := logEntry{Event: event}
	// Offer to waiters before filing, exactly as the TS client's
	// synchronous notification does. Waiter keys come off a monotonic
	// counter, so the LOWEST matching key is the longest-waiting caller:
	// picking it makes "which of two identical waits observes this event"
	// a rule instead of whatever order Go's randomized map iteration
	// produced this time. The scan runs to the end rather than breaking
	// early — waiter predicates are pure by contract, and there are only
	// ever a handful parked.
	var (
		chosen    *waiter
		chosenKey int
	)
	for key, w := range c.waiters {
		if w.channel != event.Channel {
			continue
		}
		if w.match != nil && !w.match(event) {
			continue
		}
		if chosen == nil || key < chosenKey {
			chosen, chosenKey = w, key
		}
	}
	if chosen != nil {
		entry.consumed = true
		delete(c.waiters, chosenKey)
		chosen.out <- event
	}
	c.log = append(c.log, entry)
	if len(c.log) > c.logCap {
		drop := c.logCap / logShedDivisor
		if drop < 1 {
			drop = 1
		}
		if drop > len(c.log) {
			drop = len(c.log)
		}
		kept := copy(c.log, c.log[drop:])
		// Clear the vacated tail: those entries still hold the event
		// payloads, and a backing array that keeps them alive would make
		// the shed free only in wall time, not in memory.
		clear(c.log[kept:])
		c.log = c.log[:kept]
	}
	listeners := make([]func(Event), 0, len(c.listeners))
	for _, fn := range c.listeners {
		listeners = append(listeners, fn)
	}
	c.mu.Unlock()
	for _, fn := range listeners {
		fn(event)
	}
}

func (c *Client) observeSequenceLocked(event Event) {
	if c.sequences == nil {
		c.sequences = make(map[string]channelSequence)
	}
	// A replay gap marker is an instruction to resync. Its sequence is the
	// server's current head and must become the cursor. If it is ignored,
	// every live event after a backend restart looks like a second gap and a
	// caller can never establish the new sequence space.
	if event.Gap {
		c.markReplayGapLocked(event.Channel)
		state := c.sequences[event.Channel]
		expected := uint64(0)
		if state.seen {
			expected = state.last + 1
		}
		c.seqGaps = append(c.seqGaps, SequenceGap{
			Channel: event.Channel, Expected: expected, Observed: event.Seq,
			At: time.Now().UTC(), Replay: true,
		})
		if event.Seq > 0 {
			c.sequences[event.Channel] = channelSequence{last: event.Seq, seen: true}
		}
		return
	}
	if event.Seq == 0 {
		// Locally injected frames have no transport sequence and cannot prove
		// anything about the server's sequence space.
		return
	}
	state := c.sequences[event.Channel]
	if c.replayInFlightLocked(event.Channel) {
		// Replay and live delivery share the server write lock but not one
		// ordering. A live seq 12 can arrive before replayed seq 10 and 11.
		// Keep the highest cursor and defer strict continuity until the replay
		// completion marker. Treating 10 as a rewind here manufactures a
		// stream fault for a delivery order the wire explicitly permits.
		if !state.seen || event.Seq > state.last {
			c.sequences[event.Channel] = channelSequence{last: event.Seq, seen: true}
		}
		c.recordReplaySequenceLocked(event.Channel, event.Seq)
		return
	}
	if !state.seen {
		c.sequences[event.Channel] = channelSequence{last: event.Seq, seen: true}
		return
	}
	switch {
	case event.Seq == state.last+1:
		state.last = event.Seq
		c.sequences[event.Channel] = state
	case event.Seq > state.last+1:
		c.seqGaps = append(c.seqGaps, SequenceGap{
			Channel: event.Channel, Expected: state.last + 1, Observed: event.Seq,
			At: time.Now().UTC(),
		})
		state.last = event.Seq
		c.sequences[event.Channel] = state
	default:
		c.seqFaults = append(c.seqFaults, SequenceFault{
			Channel: event.Channel, Previous: state.last, Observed: event.Seq,
			At: time.Now().UTC(),
		})
	}
}

func (c *Client) recordReplaySequenceLocked(channel string, seq uint64) {
	for _, channels := range c.replaySequences {
		if replay, ok := channels[channel]; ok && !replay.gap {
			replay.observed[seq] = struct{}{}
		}
	}
}

func (c *Client) markReplayGapLocked(channel string) {
	for _, channels := range c.replaySequences {
		if replay, ok := channels[channel]; ok {
			replay.gap = true
		}
	}
}

func (c *Client) reconcileReplayLocked(id string) {
	channels := c.replaySequences[id]
	for channel, replay := range channels {
		if replay.gap || len(replay.observed) == 0 {
			continue
		}
		max := uint64(0)
		for seq := range replay.observed {
			if seq > max {
				max = seq
			}
		}
		if max <= replay.baseline || replay.baseline == ^uint64(0) {
			continue
		}
		for expected := replay.baseline + 1; ; expected++ {
			if _, ok := replay.observed[expected]; !ok {
				c.seqGaps = append(c.seqGaps, SequenceGap{
					Channel: channel, Expected: expected, Observed: max,
					At: time.Now().UTC(), Replay: true,
				})
				break
			}
			if expected == max {
				break
			}
		}
	}
}

func (c *Client) replayInFlightLocked(channel string) bool {
	for _, channels := range c.replayChannels {
		if _, ok := channels[channel]; ok {
			return true
		}
	}
	return false
}

func (c *Client) recentChannels(n int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := len(c.log) - n
	if start < 0 {
		start = 0
	}
	// Distinct names, newest first. A repeated channel says nothing the
	// first mention did not, and the whole point of the list is "here is
	// what DID arrive while you waited for something else".
	names := make([]string, 0, len(c.log)-start)
	seen := make(map[string]bool, len(c.log)-start)
	for i := len(c.log) - 1; i >= start; i-- {
		name := c.log[i].Channel
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

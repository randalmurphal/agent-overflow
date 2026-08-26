package harnessclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	c.mu.Lock()
	for i := range c.log {
		entry := &c.log[i]
		if entry.consumed || entry.Channel != channel {
			continue
		}
		if match != nil && !match(entry.Event) {
			continue
		}
		entry.consumed = true
		event := entry.Event
		c.mu.Unlock()
		return event, nil
	}
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		if err == nil {
			err = errors.New("connection closed")
		}
		return Event{}, fmt.Errorf("wait for %s: %w", channel, err)
	}
	w := &waiter{channel: channel, match: match, out: make(chan Event, 1)}
	c.nextHook++
	key := c.nextHook
	c.waiters[key] = w
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.waiters, key)
		c.mu.Unlock()
	}()

	select {
	case event := <-w.out:
		return event, nil
	case <-ctx.Done():
		return Event{}, fmt.Errorf("wait for %s: %w (recent channels: %s)", channel, ctx.Err(), c.recentChannels(20))
	case <-c.readDone:
		return Event{}, fmt.Errorf("wait for %s: connection closed", channel)
	}
}

// Count reports how many events on a channel matched, consumed or not.
// It is the absence half of an event assertion: a wait can only prove
// something happened.
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
	entry := logEntry{Event: event}
	// Offer to waiters before filing: the first matching waiter consumes
	// it, exactly as the TS client's synchronous notification does.
	for key, w := range c.waiters {
		if w.channel != event.Channel {
			continue
		}
		if w.match != nil && !w.match(event) {
			continue
		}
		entry.consumed = true
		delete(c.waiters, key)
		w.out <- event
		break
	}
	c.log = append(c.log, entry)
	if len(c.log) > c.logCap {
		c.log = append(c.log[:0], c.log[1:]...)
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

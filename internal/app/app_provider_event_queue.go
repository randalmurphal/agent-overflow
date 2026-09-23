package app

import (
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

// Provider events reach triage through a per-thread FIFO instead of on the
// provider's own goroutine. A provider read loop also carries the control
// channel: Claude's control_responses (the answer to Interrupt, a model
// switch, a permission-mode change) are read and delivered by the same loop
// that calls onEvent, and control requests the provider answers itself are
// written from it. Handling events on that loop would hold every later
// control_response behind the slowest event, long enough to fail a control
// request on its timeout. The read loop only enqueues; control requests and
// responses are handled by the provider and never enter this queue.
//
// One worker per thread drains the queue, so a thread's events are handled
// one at a time in exactly the order the provider emitted them, across
// consecutive sessions of the same thread as well. The worker is started by
// the first event of a burst and exits when the queue is empty, so no
// goroutine outlives the work. The queue itself lives while the thread's
// session does: it is retired once the session's final event (its
// "disconnected" status) has been handled and nothing is pending.
//
// Bounds. A full queue blocks the producer, which is the provider's read
// loop (or, for Codex, whichever goroutine emits under its event lock). That
// is backpressure onto the CLI's stdout: the unread lines wait in the pipe
// and the CLI's own write buffer, as they would if each event were handled
// before the next line is read, and the CLI slows to the rate AO persists.
// Nothing is dropped and nothing is reordered; the only alternatives are
// unbounded memory here or losing history. While the producer is blocked,
// control traffic behind it waits too, so the bounds are sized to make that
// the exception.
//
//   - providerEventQueueMaxEvents absorbs a burst. Triage can spend on the
//     order of 100 ms on one tool event in a large thread while a subagent
//     fan-out emits hundreds of events a second, so 1024 events keeps the
//     read loop free for several seconds of such a burst. A queued slot is
//     under 300 bytes, so a full ring costs about 300 KiB beyond the
//     events' own data.
//   - providerEventQueueMaxBytes bounds the event data the queue retains. It
//     counts every variable-length field an event carries (Content, Meta,
//     StructuredOutput, Raw), not only Content, because all of them stay
//     alive until the event is handled. Streaming deltas are tens of bytes
//     and most tool results kilobytes; an image or a large file read can
//     reach megabytes. 8 MiB holds a burst of ordinary events plus a few
//     large ones and caps a thread that emits many. A single event larger
//     than the bound is admitted when nothing else is pending, so it cannot
//     wedge the queue.
const (
	providerEventQueueMaxEvents = 1024
	providerEventQueueMaxBytes  = 8 << 20

	// providerEventDrainTimeout bounds how long a session stop or app
	// shutdown waits for events already read from the provider to be
	// handled. It matches the settle drain and SQLite busy_timeout (5 s):
	// a queue that cannot drain in that time is stuck behind something no
	// amount of waiting fixes, and the stop must still complete.
	providerEventDrainTimeout = 5 * time.Second

	// providerEventIdleRing is the ring capacity an idle queue keeps, so a
	// live session's steady stream reuses it. A larger ring, grown by a
	// burst, is released when the burst ends.
	providerEventIdleRing = 64
)

// providerEventQueues is the registry of per-thread provider event queues.
// Its zero value is ready to use.
type providerEventQueues struct {
	mu     sync.Mutex
	queues map[string]*providerEventQueue
	// drainTimeout overrides providerEventDrainTimeout when non-zero.
	drainTimeout time.Duration
}

type queuedProviderEvent struct {
	evt    provider.ProviderEvent
	handle func(provider.ProviderEvent)
	size   int
}

// providerEventQueue is one thread's FIFO. The ring holds queued events;
// bytes also counts the event the worker is handling, so the bound covers
// everything the queue keeps alive.
type providerEventQueue struct {
	threadID string

	mu      sync.Mutex
	ring    []queuedProviderEvent
	head    int
	n       int
	bytes   int
	running bool
	// retireWhenIdle is set once the session's final event is handled; the
	// worker then retires the queue when it next finds it empty.
	retireWhenIdle bool
	// retired is set when the queue leaves the registry. A producer that
	// looked the queue up just before must look again, or its event would
	// land in a queue no later producer or drainer can see.
	retired bool
	// enqueued and handled are sequence counters. A drain waits for
	// handled to reach the enqueued value it observed on entry.
	enqueued uint64
	handled  uint64
	// changed is closed when handled advances or room is freed. Created on
	// demand by a waiter, so the common case allocates nothing.
	changed chan struct{}
}

// enqueue appends evt to the thread's queue and returns once it is queued,
// blocking while the queue is full. handle runs on the thread's worker.
func (qs *providerEventQueues) enqueue(threadID string, evt provider.ProviderEvent, handle func(provider.ProviderEvent)) {
	item := queuedProviderEvent{evt: evt, handle: handle, size: providerEventSize(evt)}
	for {
		q := qs.queueFor(threadID)
		q.mu.Lock()
		if q.retired {
			q.mu.Unlock()
			continue
		}
		for !q.admitsLocked(item.size) {
			wait := q.waitLocked()
			q.mu.Unlock()
			<-wait
			q.mu.Lock()
		}
		q.pushLocked(item)
		q.enqueued++
		// An event queued after the retire mark belongs to a replacement
		// session; that session's own end re-arms the mark.
		q.retireWhenIdle = false
		start := !q.running
		q.running = true
		q.mu.Unlock()
		if start {
			go qs.run(q)
		}
		return
	}
}

// drain waits until every event enqueued for threadID before the call has
// been handled. It returns an error when the drain bound passes first; the
// events stay queued and are still handled, in order, after drain returns.
func (qs *providerEventQueues) drain(threadID string) error {
	q := qs.lookup(threadID)
	if q == nil {
		return nil
	}
	return q.waitHandled(qs.drainBound())
}

// drainAll drains every thread's queue within one shared drain bound and
// reports the threads that did not finish.
func (qs *providerEventQueues) drainAll() error {
	timeout := qs.drainBound()
	qs.mu.Lock()
	queues := make([]*providerEventQueue, 0, len(qs.queues))
	for _, q := range qs.queues {
		queues = append(queues, q)
	}
	qs.mu.Unlock()
	deadline := time.Now().Add(timeout)
	var stuck []string
	for _, q := range queues {
		if err := q.waitHandled(time.Until(deadline)); err != nil {
			stuck = append(stuck, q.threadID)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("provider events still queued after %s for threads %v", timeout, stuck)
	}
	return nil
}

// retireWhenIdle marks the thread's queue for retirement once it is empty.
// The session event handler calls it after handling the session's final
// event, on the worker itself.
func (qs *providerEventQueues) retireWhenIdle(threadID string) {
	q := qs.lookup(threadID)
	if q == nil {
		return
	}
	q.mu.Lock()
	q.retireWhenIdle = true
	q.mu.Unlock()
}

func (qs *providerEventQueues) drainBound() time.Duration {
	if qs.drainTimeout > 0 {
		return qs.drainTimeout
	}
	return providerEventDrainTimeout
}

func (qs *providerEventQueues) queueFor(threadID string) *providerEventQueue {
	qs.mu.Lock()
	defer qs.mu.Unlock()
	if q := qs.queues[threadID]; q != nil {
		return q
	}
	if qs.queues == nil {
		qs.queues = make(map[string]*providerEventQueue)
	}
	q := &providerEventQueue{threadID: threadID}
	qs.queues[threadID] = q
	return q
}

func (qs *providerEventQueues) lookup(threadID string) *providerEventQueue {
	qs.mu.Lock()
	defer qs.mu.Unlock()
	return qs.queues[threadID]
}

// run is the thread's worker. It handles queued events in order and exits
// when the queue is empty; the next enqueue starts a new worker.
func (qs *providerEventQueues) run(q *providerEventQueue) {
	for {
		q.mu.Lock()
		item, ok := q.popLocked()
		if !ok {
			q.running = false
			retire := q.retireWhenIdle
			q.mu.Unlock()
			if retire {
				qs.retireIfIdle(q)
			}
			return
		}
		q.mu.Unlock()

		item.handle(item.evt)

		q.mu.Lock()
		q.handled++
		q.bytes -= item.size
		q.broadcastLocked()
		q.mu.Unlock()
	}
}

// retireIfIdle removes an empty queue whose session has ended from the
// registry, so a thread without a session holds nothing. A producer that
// raced in first keeps the queue alive.
func (qs *providerEventQueues) retireIfIdle(q *providerEventQueue) {
	qs.mu.Lock()
	defer qs.mu.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running || q.n > 0 || q.bytes > 0 || !q.retireWhenIdle {
		return
	}
	if qs.queues[q.threadID] == q {
		delete(qs.queues, q.threadID)
	}
	q.retired = true
}

func (q *providerEventQueue) waitHandled(timeout time.Duration) error {
	q.mu.Lock()
	target := q.enqueued
	q.mu.Unlock()
	timer := time.NewTimer(max(timeout, 0))
	defer timer.Stop()
	for {
		q.mu.Lock()
		if q.handled >= target {
			q.mu.Unlock()
			return nil
		}
		pending := target - q.handled
		wait := q.waitLocked()
		q.mu.Unlock()
		select {
		case <-wait:
		case <-timer.C:
			return fmt.Errorf("%d provider event(s) for thread %s not handled within %s", pending, q.threadID, timeout)
		}
	}
}

func (q *providerEventQueue) admitsLocked(size int) bool {
	if q.n == 0 && q.bytes == 0 {
		return true
	}
	return q.n < providerEventQueueMaxEvents && q.bytes+size <= providerEventQueueMaxBytes
}

func (q *providerEventQueue) pushLocked(item queuedProviderEvent) {
	if q.n == len(q.ring) {
		grown := make([]queuedProviderEvent, min(max(2*len(q.ring), 16), providerEventQueueMaxEvents))
		for i := range q.n {
			grown[i] = q.ring[(q.head+i)%len(q.ring)]
		}
		q.ring, q.head = grown, 0
	}
	q.ring[(q.head+q.n)%len(q.ring)] = item
	q.n++
	q.bytes += item.size
}

func (q *providerEventQueue) popLocked() (queuedProviderEvent, bool) {
	if q.n == 0 {
		// An idle queue keeps a small ring for the session's steady
		// stream but does not pin the capacity of its largest burst.
		if len(q.ring) > providerEventIdleRing {
			q.ring = nil
		}
		q.head = 0
		return queuedProviderEvent{}, false
	}
	item := q.ring[q.head]
	q.ring[q.head] = queuedProviderEvent{}
	q.head = (q.head + 1) % len(q.ring)
	q.n--
	// Room was freed for a blocked producer.
	q.broadcastLocked()
	return item, true
}

func (q *providerEventQueue) waitLocked() <-chan struct{} {
	if q.changed == nil {
		q.changed = make(chan struct{})
	}
	return q.changed
}

func (q *providerEventQueue) broadcastLocked() {
	if q.changed != nil {
		close(q.changed)
		q.changed = nil
	}
}

// providerEventSize is what an event costs the queue's byte bound: the
// variable-length fields it keeps alive until it is handled.
func providerEventSize(evt provider.ProviderEvent) int {
	return len(evt.Content) + len(evt.Meta) + len(evt.StructuredOutput) + len(evt.Raw)
}

// drainProviderEvents waits, up to the stop bound, for the thread's events
// already read from its provider to be handled. A timeout is logged: the
// remaining events are still handled in order, after the caller moves on.
func (a *App) drainProviderEvents(threadID, reason string) {
	if err := a.providerEvents.drain(threadID); err != nil {
		log.Printf("app: %s: %v", reason, err)
	}
}

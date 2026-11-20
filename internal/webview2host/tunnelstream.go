package webview2host

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Each end of the CDP tunnel reads every stream's data from its one
// WebSocket on one goroutine. That reader never writes a stream's socket
// itself: a peer that stops reading would park it, and with it every other
// stream's data and control frames and the tunnel's own teardown. Each
// stream's data goes through a TunnelStreamWriter instead, a bounded queue
// with its own writer goroutine, so one stalled stream ends alone.

// TunnelStreamBounds bound one stream's writes to its socket: Backlog the
// bytes it may have queued, WriteTimeout one write. A stream past either
// has a peer that stopped reading, and it closes.
type TunnelStreamBounds struct {
	Backlog      int
	WriteTimeout time.Duration
}

// DefaultTunnelStreamBounds are both ends' bounds. A peer reading at
// loopback speed keeps the backlog near empty.
var DefaultTunnelStreamBounds = TunnelStreamBounds{Backlog: 4 << 20, WriteTimeout: 30 * time.Second}

// errTunnelStreamBacklog reports a stream whose queued data passed its
// backlog bound.
var errTunnelStreamBacklog = errors.New("the stream's peer stopped reading: its queued data passed the bound")

// errTunnelStreamStopped reports data for a stream whose writer stopped.
var errTunnelStreamStopped = errors.New("the stream is closed")

// TunnelStreamWriter writes one stream's data to its socket, in order, on
// its own goroutine (Run). Write only queues, so the tunnel's reader never
// waits for a socket. A failure, a full queue or a write past its timeout,
// is handed once to the fail func given at construction, which closes the
// stream; Stop ends the writer, and the stream's close calls it.
type TunnelStreamWriter struct {
	conn   net.Conn
	bounds TunnelStreamBounds
	fail   func(error)

	mu      sync.Mutex
	queue   [][]byte
	queued  int
	stopped bool
	// end is the peer's close of the stream, run once the data queued
	// before it is written (End).
	end func()

	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	failOnce sync.Once
}

// NewTunnelStreamWriter answers the writer of conn within bounds; fail
// closes the stream it writes for. The caller runs Run on a goroutine it
// joins and calls Stop as the stream closes.
func NewTunnelStreamWriter(conn net.Conn, bounds TunnelStreamBounds, fail func(error)) *TunnelStreamWriter {
	if conn == nil || bounds.Backlog <= 0 || bounds.WriteTimeout <= 0 || fail == nil {
		panic("webview2host: a tunnel stream writer needs a socket, positive bounds and a fail func")
	}
	return &TunnelStreamWriter{conn: conn, bounds: bounds, fail: fail, wake: make(chan struct{}, 1), stop: make(chan struct{})}
}

// Write queues data, which the writer owns from here, for the socket. It
// never waits: data past the backlog bound fails the stream instead.
func (w *TunnelStreamWriter) Write(data []byte) error {
	w.mu.Lock()
	if w.stopped || w.end != nil {
		w.mu.Unlock()
		return errTunnelStreamStopped
	}
	if w.queued+len(data) > w.bounds.Backlog {
		w.mu.Unlock()
		err := fmt.Errorf("%w (%d bytes)", errTunnelStreamBacklog, w.bounds.Backlog)
		w.failWith(err)
		return err
	}
	w.queue = append(w.queue, data)
	w.queued += len(data)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

// End runs closeStream, the peer's close of the stream, on the writer once
// the data queued before it is written, so a close never overtakes data
// the peer sent first; with the writer stopped, it runs at once. Data
// after it is refused. A stalled write still ends at its timeout.
func (w *TunnelStreamWriter) End(closeStream func()) {
	w.mu.Lock()
	if w.stopped || w.end != nil {
		w.mu.Unlock()
		closeStream()
		return
	}
	w.end = closeStream
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run writes the queued data until Stop, a failure, or End's close once
// the data before it is written.
func (w *TunnelStreamWriter) Run() {
	for {
		data, ok := w.next()
		if !ok {
			return
		}
		if err := w.conn.SetWriteDeadline(time.Now().Add(w.bounds.WriteTimeout)); err != nil {
			w.failWith(fmt.Errorf("bound the stream's write: %w", err))
			return
		}
		_, err := w.conn.Write(data)
		w.mu.Lock()
		if !w.stopped {
			w.queued -= len(data)
		}
		w.mu.Unlock()
		if err != nil {
			w.failWith(fmt.Errorf("write the stream's socket: %w", err))
			return
		}
	}
}

// next answers the oldest queued data, waiting for some, and false once
// the writer stopped or its End came due, whose close it runs.
func (w *TunnelStreamWriter) next() ([]byte, bool) {
	for {
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return nil, false
		}
		if len(w.queue) > 0 {
			data := w.queue[0]
			w.queue[0] = nil
			w.queue = w.queue[1:]
			w.mu.Unlock()
			return data, true
		}
		if end := w.end; end != nil {
			w.mu.Unlock()
			end()
			return nil, false
		}
		w.mu.Unlock()
		select {
		case <-w.wake:
		case <-w.stop:
		}
	}
}

// Stop ends the writer and drops what it still queues. It does not close
// the socket, which its stream owns; a write under way returns as the
// stream closes it, or at its timeout.
func (w *TunnelStreamWriter) Stop() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopped = true
		w.queue, w.queued = nil, 0
		w.mu.Unlock()
		close(w.stop)
	})
}

// failWith hands the stream's first failure to fail, once.
func (w *TunnelStreamWriter) failWith(err error) {
	w.failOnce.Do(func() { w.fail(err) })
}

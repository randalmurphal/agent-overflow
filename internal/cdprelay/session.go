package cdprelay

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/coder/websocket"
)

// One tunnel connection's streams (session): opening a stream for a
// loopback connection, pumping its bytes, and the control and data frames
// the launcher sends back. The Endpoint in endpoint.go installs and
// replaces sessions.

// session is one tunnel connection's stream table. It dies with the
// connection: there is no resume, so nothing here outlives ServeCDPTunnel.
type session struct {
	endpoint *Endpoint
	conn     *websocket.Conn
	ctx      context.Context

	writeMu sync.Mutex

	mu      sync.Mutex
	streams map[uint32]*stream
	nextID  uint32
	closed  bool
}

// stream is one loopback connection bound to one tunnel stream id, and the
// writer of the data the launcher sends it (TunnelStreamWriter), which runs
// until the stream finishes.
type stream struct {
	id     uint32
	local  net.Conn
	writer *webview2host.TunnelStreamWriter

	openOnce sync.Once
	opened   chan struct{}
	doneOnce sync.Once
	done     chan struct{}

	failMu sync.Mutex
	fail   string
}

func (st *stream) markOpened() { st.openOnce.Do(func() { close(st.opened) }) }

// finish closes the local socket, which is what unblocks the pump's Read
// and a write under way, and ends the stream's writer.
func (st *stream) finish(detail string) {
	st.doneOnce.Do(func() {
		st.failMu.Lock()
		st.fail = detail
		st.failMu.Unlock()
		close(st.done)
		st.writer.Stop()
		_ = st.local.Close()
	})
}

func (st *stream) failure() string {
	st.failMu.Lock()
	defer st.failMu.Unlock()
	return st.fail
}

// openStream reserves an id, asks the launcher to dial, and waits for
// `opened`. The local socket is bound to the stream BEFORE the request
// goes out, so a launcher that answers and immediately pumps cannot race
// its first bytes against the binding.
func (s *session) openStream(local net.Conn) (*stream, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrTunnelDown
	}
	if len(s.streams) >= webview2host.MaxTunnelStreams {
		s.mu.Unlock()
		return nil, fmt.Errorf("cdprelay: %d tunnel streams already open", webview2host.MaxTunnelStreams)
	}
	s.nextID++
	st := &stream{id: s.nextID, local: local, opened: make(chan struct{}), done: make(chan struct{})}
	st.writer = webview2host.NewTunnelStreamWriter(local, s.endpoint.streamBounds, func(err error) {
		// The pump sends the launcher the close as the socket ends.
		s.endpoint.logf("cdprelay: close stream %d: %v", st.id, err)
		st.finish("local write failed")
	})
	s.streams[st.id] = st
	s.endpoint.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.endpoint.wg.Done()
		st.writer.Run()
	}()

	fail := func(err error) (*stream, error) {
		st.finish(err.Error())
		s.retire(st)
		return nil, err
	}
	if err := s.sendControl(webview2host.TunnelControl{Op: webview2host.TunnelOpen, StreamID: st.id}); err != nil {
		return fail(err)
	}

	timer := time.NewTimer(s.endpoint.openTimeout)
	defer timer.Stop()
	select {
	case <-st.opened:
		return st, nil
	case <-st.done:
		detail := st.failure()
		if detail == "" {
			detail = "tunnel closed the stream"
		}
		return fail(fmt.Errorf("cdprelay: stream %d: %s", st.id, detail))
	case <-s.ctx.Done():
		return fail(ErrTunnelDown)
	case <-timer.C:
		_, err := fail(fmt.Errorf("cdprelay: stream %d: launcher did not answer open within %s", st.id, s.endpoint.openTimeout))
		s.closeRemote(st.id)
		return nil, err
	}
}

// pump copies the local socket into data frames until either end finishes,
// then tells the launcher the stream is over.
//
// Writes are chunked at TunnelChunkBytes, comfortably under the 1MiB frame
// limit both ends enforce — the backend owes that bound just as the
// launcher does (internal/webview2host/AGENTS.md).
func (s *session) pump(st *stream) {
	buf := make([]byte, webview2host.TunnelChunkBytes)
	frame := make([]byte, 0, 4+webview2host.TunnelChunkBytes)
	for {
		n, err := st.local.Read(buf)
		if n > 0 {
			frame = webview2host.EncodeTunnelData(frame, st.id, buf[:n])
			if writeErr := s.write(websocket.MessageBinary, frame); writeErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	st.finish("local connection ended")
	if s.retire(st) {
		s.closeRemote(st.id)
	}
}

// retire removes st from the table, but only when the slot still holds it.
// Reports whether this call is the one that removed it, which is what
// decides who owes the launcher a close frame.
func (s *session) retire(st *stream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams[st.id] != st {
		return false
	}
	delete(s.streams, st.id)
	return !s.closed
}

func (s *session) lookup(id uint32) *stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *session) handleControl(payload []byte) {
	var control webview2host.TunnelControl
	if err := json.Unmarshal(payload, &control); err != nil {
		s.endpoint.logf("cdprelay: ignore malformed control frame: %v", err)
		return
	}
	switch control.Op {
	case webview2host.TunnelOpened:
		if st := s.lookup(control.StreamID); st != nil {
			st.markOpened()
		}
	// The launcher's end of a stream finishes it once the data the
	// launcher sent before it is written (TunnelStreamWriter.End).
	case webview2host.TunnelError:
		if st := s.lookup(control.StreamID); st != nil {
			detail := webview2host.TruncateDetail(control.Detail)
			st.writer.End(func() { st.finish(detail) })
		}
	case webview2host.TunnelClose:
		if st := s.lookup(control.StreamID); st != nil {
			st.writer.End(func() { st.finish("launcher closed the stream") })
		}
	default:
		// `open` is the backend's own verb and anything else is version
		// skew. Both are dropped rather than guessed at: the tunnel's whole
		// safety story is that no control frame names an address, and
		// acting on an unknown op is how that stops being true.
		s.endpoint.logf("cdprelay: ignore control op %q on stream %d", control.Op, control.StreamID)
	}
}

func (s *session) handleData(payload []byte) {
	id, data, err := webview2host.DecodeTunnelData(payload)
	if err != nil {
		s.endpoint.logf("cdprelay: %v", err)
		return
	}
	st := s.lookup(id)
	if st == nil {
		// A late frame for a stream that just ended is ordinary: the close
		// raced the launcher's last write.
		return
	}
	// Queued, never written here: this is the connection's one reader, and
	// each Read answers a message of its own to keep. A failed or stalled
	// write finishes the stream (TunnelStreamWriter), which wakes the pump,
	// and the pump sends the close.
	_ = st.writer.Write(data)
}

func (s *session) sendControl(control webview2host.TunnelControl) error {
	payload, err := json.Marshal(control)
	if err != nil {
		return fmt.Errorf("cdprelay: encode control %q: %w", control.Op, err)
	}
	return s.write(websocket.MessageText, payload)
}

func (s *session) closeRemote(id uint32) {
	if err := s.sendControl(webview2host.TunnelControl{Op: webview2host.TunnelClose, StreamID: id}); err != nil {
		s.endpoint.logf("cdprelay: close stream %d: %v", id, err)
	}
}

func (s *session) write(kind websocket.MessageType, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(ctx, kind, payload)
}

// shutdown drops every stream on this connection. Idempotent: a replaced
// session is shut down by whoever replaced it and again by its own reader
// when it notices.
func (s *session) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	streams := s.streams
	s.streams = make(map[uint32]*stream)
	s.mu.Unlock()
	for _, st := range streams {
		st.finish("tunnel disconnected")
	}
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}

package webview2host

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// stalledListener accepts connections and never reads them, standing in
// for a CDP socket whose reader stopped.
func stalledListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
	})
	return listener.Addr().String()
}

// startStallTunnel runs a tunnel whose first stream dials stalled and
// every other the echo port, within bounds, and answers its cancel and a
// channel closed as Run returns.
func startStallTunnel(t *testing.T, stub *backendStub, stalled string, echo int, bounds TunnelStreamBounds) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	var logMu sync.Mutex
	tunnel, err := NewCDPTunnel(CDPTunnelConfig{
		WSURL:       stub.wsURL(),
		Token:       "tunnel-token",
		CDPPort:     echo,
		Logf:        func(format string, args ...any) { logMu.Lock(); defer logMu.Unlock(); t.Logf(format, args...) },
		MinBackoff:  time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewCDPTunnel: %v", err)
	}
	tunnel.streamBounds = bounds
	var dials sync.Mutex
	first := true
	tunnel.dialCDP = func(ctx context.Context, addr string) (net.Conn, error) {
		dials.Lock()
		if first {
			addr = stalled
		}
		first = false
		dials.Unlock()
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); tunnel.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-finished })
	stub.waitConnected(t)
	return cancel, finished
}

// stall sends n chunks for stream id, whose CDP socket is never read: far
// more than loopback socket buffers hold, so the stream's write parks.
func (s *backendStub) stall(t *testing.T, id uint32, n int) {
	t.Helper()
	chunk := make([]byte, TunnelChunkBytes)
	for range n {
		s.sendData(t, id, chunk)
	}
}

// open opens stream id and waits for its opened.
func (s *backendStub) open(t *testing.T, id uint32) {
	t.Helper()
	s.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: id})
	if got := s.nextControl(t); got.Op != TunnelOpened || got.StreamID != id {
		t.Fatalf("control = %#v, want opened on stream %d", got, id)
	}
}

// A stream whose CDP socket stopped reading holds no other stream's data,
// and the tunnel's cancel returns while that stream's write is parked.
func TestCDPTunnelAStalledStreamHoldsNoOtherStream(t *testing.T) {
	stub := newBackendStub(t, "tunnel-token")
	cancel, finished := startStallTunnel(t, stub, stalledListener(t), echoListener(t), TunnelStreamBounds{Backlog: 1 << 30, WriteTimeout: time.Hour})
	stub.open(t, 1)
	stub.open(t, 2)

	stub.stall(t, 1, 1024)
	stub.sendData(t, 2, []byte("still-flowing"))
	select {
	case frame := <-stub.data:
		id, payload, err := DecodeTunnelData(frame)
		if err != nil || id != 2 || string(payload) != "echo:still-flowing" {
			t.Fatalf("stream %d answered %q (%v), want stream 2's echo", id, payload, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled stream held another stream's data")
	}

	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel's cancel did not return while a stream's write was parked")
	}
}

// A stream whose CDP socket stopped reading closes, the backend told, once
// its queued data passes the backlog bound or one write passes its
// timeout.
func TestCDPTunnelClosesAStreamItsReaderStopped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bounds TunnelStreamBounds
	}{
		{"backlog", TunnelStreamBounds{Backlog: 1 << 20, WriteTimeout: time.Hour}},
		{"write timeout", TunnelStreamBounds{Backlog: 1 << 30, WriteTimeout: 50 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newBackendStub(t, "tunnel-token")
			startStallTunnel(t, stub, stalledListener(t), echoListener(t), tc.bounds)
			stub.open(t, 1)
			stub.stall(t, 1, 512)
			if got := stub.nextControl(t); got.Op != TunnelClose || got.StreamID != 1 {
				t.Fatalf("control = %#v, want a close of stream 1", got)
			}
		})
	}
}

// A stream the backend closed while its write to the CDP socket is parked
// stays the session's until that writer finishes: it still counts toward
// the stream cap, and the tunnel's cancel closes it at once, well before
// the write's own timeout.
func TestCDPTunnelADrainingStreamIsOwnedUntilItsWriterFinishes(t *testing.T) {
	stub := newBackendStub(t, "tunnel-token")
	cancel, finished := startStallTunnel(t, stub, stalledListener(t), echoListener(t), TunnelStreamBounds{Backlog: 1 << 30, WriteTimeout: 10 * time.Second})
	stub.open(t, 1)
	stub.stall(t, 1, 1024)
	stub.sendControl(t, TunnelControl{Op: TunnelClose, StreamID: 1})
	// Frames are read in order, so stream 2's opened follows stream 1's
	// close.
	for id := uint32(2); id <= MaxTunnelStreams; id++ {
		stub.open(t, id)
	}
	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: MaxTunnelStreams + 1})
	if got := stub.nextControl(t); got.Op != TunnelError || got.StreamID != MaxTunnelStreams+1 {
		t.Errorf("control = %#v, want the open past the cap refused while stream 1 drains", got)
	}

	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the tunnel's cancel waited for a draining stream's parked write")
	}
}

// A data frame with no data is refused at the decoder, so it never takes a
// place in a stream's queue, whose bound counts only bytes.
func TestCDPTunnelQueuesNoEmptyDataFrame(t *testing.T) {
	local, peer := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
	})
	writer := NewTunnelStreamWriter(local, TunnelStreamBounds{Backlog: 1, WriteTimeout: time.Hour}, func(error) {})
	session := &tunnelSession{
		tunnel:  &CDPTunnel{logf: func(string, ...any) {}},
		streams: map[uint32]*tunnelStream{7: {id: 7, conn: local, writer: writer}},
	}
	for range 4096 {
		session.handleData(EncodeTunnelData(nil, 7, nil))
	}
	session.handleData(EncodeTunnelData(nil, 7, []byte("x")))
	writer.mu.Lock()
	queued, entries := writer.queued, len(writer.queue)
	writer.mu.Unlock()
	if queued != 1 || entries != 1 {
		t.Fatalf("the stream queued %d entries of %d bytes, want the one byte alone", entries, queued)
	}
}

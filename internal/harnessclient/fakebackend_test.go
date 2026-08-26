package harnessclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"agent-overflow/internal/transport"
)

// fakeBackend is a transport-shaped server: it decodes what the client
// sends through transport.ClientFrame and answers with
// transport.ServerFrame, so these tests fail if this package's private
// frame structs ever drift from the real ones. It does NOT boot an App —
// the point is to exercise the client's frame handling in isolation,
// which a full harness boot would only make slower and less specific.
type fakeBackend struct {
	t     *testing.T
	srv   *httptest.Server
	token string

	// handle answers one rpc frame. Returning a nil error means the
	// result is delivered; returning one delivers the error envelope.
	handle func(frame transport.ClientFrame) (json.RawMessage, *transport.FrameError)

	mu         sync.Mutex
	conn       *websocket.Conn
	connReady  chan struct{}
	subscribed []string
	replayIDs  []string
	seq        uint64
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	b := &fakeBackend{t: t, token: "test-token", connReady: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", b.serveWS)
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// wsURL is the endpoint a client dials, token included.
func (b *fakeBackend) wsURL() string {
	return "ws" + strings.TrimPrefix(b.srv.URL, "http") + "/ws?token=" + b.token
}

func (b *fakeBackend) serveWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != b.token {
		// The real server answers a refused token with 404 so a scanner
		// cannot fingerprint it; the client's error prose depends on that.
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	b.mu.Lock()
	b.conn = conn
	ready := b.connReady
	b.connReady = make(chan struct{})
	b.mu.Unlock()
	close(ready)

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var frame transport.ClientFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			b.t.Errorf("fake backend: undecodable client frame %s: %v", data, err)
			return
		}
		switch frame.Type {
		case "rpc":
			b.answerRPC(r.Context(), conn, frame)
		case "subscribe":
			b.mu.Lock()
			b.subscribed = append(b.subscribed, frame.Channels...)
			b.mu.Unlock()
		case "replay":
			b.mu.Lock()
			b.replayIDs = append(b.replayIDs, frame.ID)
			b.mu.Unlock()
			b.write(r.Context(), conn, transport.ServerFrame{Type: "replay", ID: frame.ID})
		}
	}
}

func (b *fakeBackend) answerRPC(ctx context.Context, conn *websocket.Conn, frame transport.ClientFrame) {
	handle := b.handle
	if handle == nil {
		handle = func(f transport.ClientFrame) (json.RawMessage, *transport.FrameError) {
			return json.RawMessage(fmt.Sprintf("%q", "ok:"+f.Method)), nil
		}
	}
	result, rpcErr := handle(frame)
	b.write(ctx, conn, transport.ServerFrame{Type: "rpc", ID: frame.ID, Result: result, Error: rpcErr})
}

func (b *fakeBackend) write(ctx context.Context, conn *websocket.Conn, frame transport.ServerFrame) {
	payload, err := json.Marshal(frame)
	if err != nil {
		b.t.Errorf("fake backend: marshal %s frame: %v", frame.Type, err)
		return
	}
	b.writeRaw(ctx, conn, payload)
}

func (b *fakeBackend) writeRaw(ctx context.Context, conn *websocket.Conn, payload []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return
	}
}

// waitConn blocks until a client has connected.
func (b *fakeBackend) waitConn(t *testing.T) *websocket.Conn {
	t.Helper()
	b.mu.Lock()
	conn := b.conn
	ready := b.connReady
	b.mu.Unlock()
	if conn != nil {
		return conn
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("no client connected to the fake backend")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn
}

// pushEvent sends one plain event frame.
func (b *fakeBackend) pushEvent(t *testing.T, channel string, data any) {
	t.Helper()
	conn := b.waitConn(t)
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal event data: %v", err)
	}
	b.mu.Lock()
	b.seq++
	seq := b.seq
	b.mu.Unlock()
	b.write(context.Background(), conn, transport.ServerFrame{
		Type: "event", Channel: channel, Seq: seq, Data: encoded,
	})
}

// pushBatch sends the coalesced envelope the real server splices for a
// multi-event flush window, inert per-entry "type":"event" included.
func (b *fakeBackend) pushBatch(t *testing.T, channels ...string) {
	t.Helper()
	conn := b.waitConn(t)
	entries := make([]string, 0, len(channels))
	for _, channel := range channels {
		b.mu.Lock()
		b.seq++
		seq := b.seq
		b.mu.Unlock()
		entries = append(entries, fmt.Sprintf(
			`{"type":"event","channel":%q,"seq":%d,"data":{"n":%d}}`, channel, seq, seq))
	}
	b.writeRaw(context.Background(), conn, []byte(
		`{"type":"batch","events":[`+strings.Join(entries, ",")+`]}`))
}

// pushPing sends the keepalive frame the real server heartbeats with.
func (b *fakeBackend) pushPing(t *testing.T) {
	t.Helper()
	conn := b.waitConn(t)
	b.write(context.Background(), conn, transport.ServerFrame{Type: "ping"})
}

func (b *fakeBackend) subscriptions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.subscribed...)
}

func (b *fakeBackend) replayRequests() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.replayIDs...)
}

// dial connects a client to this backend and closes it with the test.
func (b *fakeBackend) dial(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := DialURL(ctx, b.wsURL(), Options{})
	if err != nil {
		t.Fatalf("DialURL: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

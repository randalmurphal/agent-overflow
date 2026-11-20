package browser

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// ---------------------------------------------------------------------
// The browser-level dial
// ---------------------------------------------------------------------

// fakeCDPCall is one command a fake endpoint answered, the session it was
// sent on (empty for the browser), and the path of the connection it
// arrived on.
type fakeCDPCall struct {
	Method    string
	Params    json.RawMessage
	SessionID string
	Path      string
}

// newFakeCDPServer is a loopback websocket endpoint speaking just enough CDP
// to accept a browser-level connection: every command is recorded and
// answered, by default with an empty success. `respond` may return a richer
// result body for one method — which is how the headless engine's tests get
// a browser that can hand out a target — and nil falls back to the empty
// success.
//
// It answers on the SESSION the command arrived on: chromedp routes a
// response to a target's executor by the sessionId echoed back, and a reply
// that dropped it would be delivered to the browser instead and the caller
// would wait forever.
//
// No browser is started by any of this, on either engine's tests.
func newFakeCDPServer(t *testing.T, respond func(call fakeCDPCall) any) (wsURL string, calls func() []fakeCDPCall) {
	t.Helper()
	endpoint := startFakeCDPEndpoint(t, respond, nil)
	return endpoint.wsURL, endpoint.calls
}

// fakeCDPError, returned by respond, answers the command with a CDP error.
type fakeCDPError struct {
	Code    int
	Message string
}

// fakeCDPNoReply, returned by respond, leaves the command unanswered while
// the connection stays open, as a browser that stopped answering does.
type fakeCDPNoReply struct{}

// fakeCDPEndpoint is a started fake CDP server. announce, when set, returns
// the events to send before the reply to one command, as {method, params}
// maps, the order Chromium uses: the targets Target.setDiscoverTargets
// reports arrive before its reply. send writes one event to every open
// connection. hangUp closes every open connection, which is what a browser
// that died looks like to its client.
type fakeCDPEndpoint struct {
	wsURL  string
	calls  func() []fakeCDPCall
	send   func(event map[string]any)
	hangUp func()
}

func startFakeCDPEndpoint(t *testing.T, respond func(call fakeCDPCall) any, announce func(call fakeCDPCall) []map[string]any) *fakeCDPEndpoint {
	t.Helper()
	var mu sync.Mutex
	var seen []fakeCDPCall
	// Each open connection with the lock its writers share.
	open := make(map[net.Conn]*sync.Mutex)
	write := func(conn net.Conn, lock *sync.Mutex, frame any) error {
		body, _ := json.Marshal(frame)
		lock.Lock()
		defer lock.Unlock()
		return wsutil.WriteServerText(conn, body)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Local discovery may probe any listening port. Only CDP WebSocket
		// handshakes belong to this fixture; unrelated HTTP is not a test failure.
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.NotFound(w, r)
			return
		}

		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		lock := &sync.Mutex{}
		mu.Lock()
		open[conn] = lock
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(open, conn)
			mu.Unlock()
			// A hang-up may have closed it already. This goroutine can
			// outlive the test, so it logs rather than reports to t.
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("close the fake CDP connection: %v", err)
			}
		}()
		for {
			payload, err := wsutil.ReadClientText(conn)
			if err != nil {
				return
			}
			var msg struct {
				ID        int64           `json:"id"`
				Method    string          `json:"method"`
				Params    json.RawMessage `json:"params"`
				SessionID string          `json:"sessionId"`
			}
			if err := json.Unmarshal(payload, &msg); err != nil {
				t.Errorf("bad CDP frame %q: %v", payload, err)
				return
			}
			call := fakeCDPCall{Method: msg.Method, Params: msg.Params, SessionID: msg.SessionID, Path: r.URL.Path}
			mu.Lock()
			seen = append(seen, call)
			mu.Unlock()
			if announce != nil {
				for _, event := range announce(call) {
					if err := write(conn, lock, event); err != nil {
						return
					}
				}
			}
			var result any = map[string]any{}
			if respond != nil {
				if answer := respond(call); answer != nil {
					result = answer
				}
			}
			if _, silent := result.(fakeCDPNoReply); silent {
				continue
			}
			frame := map[string]any{"id": msg.ID, "result": result}
			if refusal, ok := result.(fakeCDPError); ok {
				frame = map[string]any{"id": msg.ID, "error": map[string]any{"code": refusal.Code, "message": refusal.Message}}
			}
			if msg.SessionID != "" {
				frame["sessionId"] = msg.SessionID
			}
			if err := write(conn, lock, frame); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return &fakeCDPEndpoint{
		wsURL: "ws" + strings.TrimPrefix(server.URL, "http"),
		calls: func() []fakeCDPCall {
			mu.Lock()
			defer mu.Unlock()
			return append([]fakeCDPCall(nil), seen...)
		},
		send: func(event map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			for conn, lock := range open {
				if err := write(conn, lock, event); err != nil {
					t.Errorf("send %v: %v", event["method"], err)
				}
			}
		},
		hangUp: func() {
			mu.Lock()
			defer mu.Unlock()
			for conn := range open {
				if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Errorf("hang up the fake CDP connection: %v", err)
				}
			}
		},
	}
}

// pausedRequest is the Fetch.requestPaused event Chromium sends on the
// session whose interception matched a document request of frame.
func pausedRequest(id, frame, url string) map[string]any {
	return map[string]any{"method": "Fetch.requestPaused", "params": map[string]any{
		"requestId": id, "frameId": frame, "resourceType": "Document",
		"request": map[string]any{"url": url, "method": "GET", "headers": map[string]any{}},
	}}
}

// browserCallParams decodes the parameters of every command of method the
// fake answered on the browser session.
func browserCallParams[T any](t *testing.T, calls []fakeCDPCall, method string) []T {
	t.Helper()
	var out []T
	for _, call := range calls {
		if call.Method != method || call.SessionID != "" {
			continue
		}
		var params T
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatalf("decode %s %s: %v", method, call.Params, err)
		}
		out = append(out, params)
	}
	return out
}

// assertOnePolicyPause holds a CDP engine to one navigation-policy
// interception: Fetch.enable once, on the browser session, whatever pages
// are attached, and one answer to a paused document request. A second
// interception (a page's own) would pause every request twice, on the
// hosted engine each time across the relay.
func assertOnePolicyPause(t *testing.T, calls func() []fakeCDPCall, send func(map[string]any)) {
	t.Helper()
	var enables []string
	for _, call := range calls() {
		if call.Method == "Fetch.enable" {
			enables = append(enables, "session "+call.SessionID)
		}
	}
	if len(enables) != 1 || enables[0] != "session " {
		t.Fatalf("Fetch.enable ran on %v, want once on the browser session", enables)
	}
	// A frame no page answers for, refused once the owner bound passes.
	send(pausedRequest("R-once", "F-any", "https://example.test/"))
	answers := func() int {
		n := 0
		for _, call := range calls() {
			if call.Method == "Fetch.continueRequest" || call.Method == "Fetch.failRequest" {
				n++
			}
		}
		return n
	}
	eventually(t, "the paused request to be answered", func() bool { return answers() > 0 })
	time.Sleep(50 * time.Millisecond)
	if n := answers(); n != 1 {
		t.Fatalf("one paused request was answered %d times", n)
	}
}

// requestAnswer is the part of Fetch.continueRequest and Fetch.failRequest
// a test reads.
type requestAnswer struct {
	RequestID   string `json:"requestId"`
	ErrorReason string `json:"errorReason"`
}

// fakeCDPBrowser is what the launcher's WebView2 debugging endpoint looks
// like to the dial: a browser that answers commands but has NO tabs of its
// own to hand out.
func fakeCDPBrowser(t *testing.T) (wsURL string, methods func() []string) {
	t.Helper()
	wsURL, calls := newFakeCDPServer(t, nil)
	return wsURL, func() []string {
		seen := calls()
		names := make([]string, 0, len(seen))
		for _, call := range seen {
			names = append(names, call.Method)
		}
		return names
	}
}

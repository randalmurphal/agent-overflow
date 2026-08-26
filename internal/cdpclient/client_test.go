package cdpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeDevtools is an httptest server speaking just enough of the
// protocol: a /json/list target listing and a WebSocket that hands every
// received frame to a scripted responder. Nothing here launches a
// browser — a test that needed one would be a test nobody could run.
type fakeDevtools struct {
	t      *testing.T
	server *httptest.Server
	// respond receives each decoded client frame and writes whatever the
	// scenario wants back, in whatever order it likes.
	respond func(w *fakeSession, frame map[string]any)
}

type fakeSession struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
}

func (s *fakeSession) send(payload any) {
	s.t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		s.t.Fatalf("encode fake reply: %v", err)
	}
	if err := s.conn.Write(s.ctx, websocket.MessageText, data); err != nil {
		s.t.Logf("fake devtools write: %v", err)
	}
}

func newFakeDevtools(t *testing.T, respond func(*fakeSession, map[string]any)) *fakeDevtools {
	t.Helper()
	fake := &fakeDevtools{t: t, respond: respond}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[
  {"id":"1","type":"page","title":"Agent Overflow","url":"http://127.0.0.1:4321/?token=t",
   "webSocketDebuggerUrl":"%s"}
]`, "ws"+strings.TrimPrefix(fake.server.URL, "http")+"/devtools/page/1")
	})
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Logf("fake devtools accept: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		session := &fakeSession{t: t, conn: conn, ctx: ctx}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Errorf("fake devtools got an undecodable frame: %v", err)
				return
			}
			fake.respond(session, frame)
		}
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeDevtools) endpoint() Endpoint { return Endpoint{HTTPBase: f.server.URL} }

func frameID(frame map[string]any) int {
	id, _ := frame["id"].(float64)
	return int(id)
}

func TestAttachAndCallCorrelatesOutOfOrderReplies(t *testing.T) {
	// Both calls are held, then answered in REVERSE arrival order: a client
	// that paired replies with calls by arrival would hand each caller the
	// other's result, and both would look like successes.
	var held []map[string]any
	fake := newFakeDevtools(t, func(s *fakeSession, frame map[string]any) {
		held = append(held, frame)
		if len(held) < 2 {
			return
		}
		for i := len(held) - 1; i >= 0; i-- {
			method, _ := held[i]["method"].(string)
			s.send(map[string]any{"id": frameID(held[i]), "result": map[string]any{"who": method}})
		}
		held = nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, target, err := Attach(ctx, fake.endpoint(), "http://127.0.0.1:4321/?token=other")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer conn.Close()
	if target.ID != "1" {
		t.Fatalf("attached to target %+v, want the listed page", target)
	}

	type answer struct {
		Who string `json:"who"`
	}
	results := make(chan error, 2)
	for _, method := range []string{"Profiler.stop", "Profiler.enable"} {
		go func() {
			var got answer
			err := conn.CallInto(ctx, &got, method, nil)
			if err == nil && got.Who != method {
				err = fmt.Errorf("%s got the reply for %q", method, got.Who)
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("call: %v", err)
		}
	}
}

func TestCallSurfacesProtocolErrors(t *testing.T) {
	fake := newFakeDevtools(t, func(s *fakeSession, frame map[string]any) {
		s.send(map[string]any{"id": frameID(frame), "error": map[string]any{
			"code": -32000, "message": "Tracing is already started", "data": "detail",
		}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := Attach(ctx, fake.endpoint(), "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer conn.Close()

	_, err = conn.Call(ctx, "Tracing.start", map[string]any{"transferMode": "ReturnAsStream"})
	var protoErr *ProtocolError
	if !errors.As(err, &protoErr) {
		t.Fatalf("want a ProtocolError, got %v", err)
	}
	if protoErr.Method != "Tracing.start" || protoErr.Code != -32000 {
		t.Fatalf("unexpected protocol error: %+v", protoErr)
	}
	if !strings.Contains(protoErr.Error(), "already started") {
		t.Fatalf("error text drops the message: %v", protoErr)
	}
}

func TestSubscriptionFiltersAndDelivers(t *testing.T) {
	fake := newFakeDevtools(t, func(s *fakeSession, frame map[string]any) {
		// One unrelated notification, then the one being waited for, then
		// the reply. A subscriber must see only its own method.
		s.send(map[string]any{"method": "Tracing.dataCollected", "params": map[string]any{"value": []any{}}})
		s.send(map[string]any{"method": "Tracing.tracingComplete", "params": map[string]any{"stream": "handle-7"}})
		s.send(map[string]any{"id": frameID(frame), "result": map[string]any{}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := Attach(ctx, fake.endpoint(), "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer conn.Close()

	sub := conn.Subscribe("Tracing.tracingComplete")
	defer sub.Close()
	if _, err := conn.Call(ctx, "Tracing.end", nil); err != nil {
		t.Fatalf("Tracing.end: %v", err)
	}
	ev, err := sub.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for tracingComplete: %v", err)
	}
	if ev.Method != "Tracing.tracingComplete" {
		t.Fatalf("got event %q", ev.Method)
	}
	var payload struct {
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(ev.Params, &payload); err != nil || payload.Stream != "handle-7" {
		t.Fatalf("event params %s (%v)", ev.Params, err)
	}
}

// A browser that goes away mid-call must fail the caller rather than
// leaving it parked on a reply that can never arrive: a wedged profile
// run looks alive to whatever is supervising it.
func TestCallFailsWhenTheBrowserGoesAway(t *testing.T) {
	fake := newFakeDevtools(t, func(s *fakeSession, frame map[string]any) {
		_ = s.conn.Close(websocket.StatusGoingAway, "browser closing")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := Attach(ctx, fake.endpoint(), "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Call(ctx, "Profiler.stop", nil); err == nil {
		t.Fatal("a closed connection must fail the parked call")
	}
	sub := conn.Subscribe("Tracing.tracingComplete")
	defer sub.Close()
	if _, err := sub.Wait(ctx); err == nil {
		t.Fatal("a wait must not outlive the connection")
	}
}

func TestListTargetsRejectsANonDevtoolsEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	if _, err := ListTargets(context.Background(), server.URL); err == nil {
		t.Fatal("a 404 from the listing endpoint must be an error")
	}
}

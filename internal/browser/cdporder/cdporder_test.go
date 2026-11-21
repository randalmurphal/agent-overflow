package cdporder

import (
	"context"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
)

// fakeTransport hands Read the messages fed to it and records Write's.
type fakeTransport struct {
	in chan *cdproto.Message
}

func (f *fakeTransport) Read(_ context.Context, msg *cdproto.Message) error {
	m, ok := <-f.in
	if !ok {
		return io.EOF
	}
	*msg = *m
	return nil
}

func (f *fakeTransport) Write(context.Context, *cdproto.Message) error { return nil }
func (f *fakeTransport) Close() error                                  { return nil }

// A session's watched events and the replies to its watched commands reach
// its handler in the order they were read off the connection, one at a
// time, however long the handler takes over each; other methods and other
// sessions' messages do not reach it, and nothing does once it stopped.
func TestWatchHandsOverTheWireOrder(t *testing.T) {
	wire := []*cdproto.Message{
		{SessionID: "S", Method: "Page.b", Params: []byte(`{"n":1}`)},
		{SessionID: "X", Method: "Page.a", Params: []byte(`{"n":2}`)},
		{SessionID: "S", Method: "Page.unwatched", Params: []byte(`{"n":3}`)},
		{SessionID: "S", Method: "Page.a", Params: []byte(`{"n":4}`)},
		{SessionID: "S", ID: 7, Result: []byte(`{"tree":5}`)},
		{SessionID: "S", ID: 8, Result: []byte(`{"other":6}`)},
		{SessionID: "S", Method: "Page.b", Params: []byte(`{"n":7}`)},
	}
	want := []string{`Page.b {"n":1}`, `Page.a {"n":4}`, `reply Page.tree {"tree":5}`, `Page.b {"n":7}`}
	for _, delays := range [][]time.Duration{
		{0, 0, 0, 0},
		{30 * time.Millisecond, 0, 0, 0},
		{0, 0, 30 * time.Millisecond, 0},
		{10 * time.Millisecond, 20 * time.Millisecond, 0, 5 * time.Millisecond},
	} {
		transport := &fakeTransport{in: make(chan *cdproto.Message, len(wire)+1)}
		tap := NewTap(transport)
		var mu sync.Mutex
		var got []string
		done := make(chan struct{})
		stop := tap.Watch("S", []string{"Page.a", "Page.b", "Page.tree"}, func(m Message) {
			mu.Lock()
			n := len(got)
			mu.Unlock()
			time.Sleep(delays[n%len(delays)])
			line := m.Method + " " + string(m.Params)
			if m.Reply {
				line = "reply " + line
			}
			mu.Lock()
			got = append(got, line)
			if len(got) == len(want) {
				close(done)
			}
			mu.Unlock()
		})
		if err := tap.Write(t.Context(), &cdproto.Message{ID: 7, SessionID: "S", Method: "Page.tree"}); err != nil {
			t.Fatal(err)
		}
		if err := tap.Write(t.Context(), &cdproto.Message{ID: 8, SessionID: "S", Method: "Page.other"}); err != nil {
			t.Fatal(err)
		}
		for _, m := range wire {
			transport.in <- m
			var read cdproto.Message
			if err := tap.Read(t.Context(), &read); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("handled %q, want %q", got, want)
		}
		mu.Lock()
		if !slices.Equal(got, want) {
			t.Errorf("with handler delays %v: handled %q, want %q", delays, got, want)
		}
		mu.Unlock()
		stop()
		transport.in <- &cdproto.Message{SessionID: "S", Method: "Page.a", Params: []byte(`{"n":8}`)}
		var read cdproto.Message
		if err := tap.Read(t.Context(), &read); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		if len(got) != len(want) {
			t.Errorf("a stopped watch handled %q", got[len(want):])
		}
		mu.Unlock()
		tap.mu.Lock()
		if len(tap.sessions) != 0 || len(tap.pending) != 0 {
			t.Errorf("a stopped watch left %d sessions and %d pending replies", len(tap.sessions), len(tap.pending))
		}
		tap.mu.Unlock()
	}
}

// Two watchers of one session and command each get its reply, as each gets
// the events they watch, and a stopped one no longer holds the reply up for
// the other.
func TestEveryWatcherGetsAWatchedReply(t *testing.T) {
	transport := &fakeTransport{in: make(chan *cdproto.Message, 2)}
	tap := NewTap(transport)
	first, second := make(chan Message, 2), make(chan Message, 2)
	stopFirst := tap.Watch("S", []string{"Page.tree"}, func(m Message) { first <- m })
	defer stopFirst()
	stopSecond := tap.Watch("S", []string{"Page.tree"}, func(m Message) { second <- m })
	defer stopSecond()
	for _, id := range []int64{1, 2} {
		if err := tap.Write(t.Context(), &cdproto.Message{ID: id, SessionID: "S", Method: "Page.tree"}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id int64) {
		t.Helper()
		transport.in <- &cdproto.Message{SessionID: "S", ID: id, Result: []byte(`{"tree":1}`)}
		var msg cdproto.Message
		if err := tap.Read(t.Context(), &msg); err != nil {
			t.Fatal(err)
		}
	}
	await := func(who string, got chan Message) {
		t.Helper()
		select {
		case m := <-got:
			if !m.Reply || string(m.Params) != `{"tree":1}` {
				t.Fatalf("the %s watcher got %+v", who, m)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the %s watcher never got the reply", who)
		}
	}
	read(1)
	await("first", first)
	await("second", second)
	stopFirst()
	read(2)
	await("second", second)
	tap.mu.Lock()
	defer tap.mu.Unlock()
	if len(tap.pending) != 0 {
		t.Fatalf("answered replies left %d pending", len(tap.pending))
	}
}

// A reply to a watched command that failed carries the protocol error.
func TestWatchCarriesAFailedReply(t *testing.T) {
	transport := &fakeTransport{in: make(chan *cdproto.Message, 1)}
	tap := NewTap(transport)
	got := make(chan Message, 1)
	stop := tap.Watch("S", []string{"Page.tree"}, func(m Message) { got <- m })
	defer stop()
	if err := tap.Write(t.Context(), &cdproto.Message{ID: 1, SessionID: "S", Method: "Page.tree"}); err != nil {
		t.Fatal(err)
	}
	transport.in <- &cdproto.Message{SessionID: "S", ID: 1, Error: &cdproto.Error{Code: -32000, Message: "gone"}}
	var read cdproto.Message
	if err := tap.Read(t.Context(), &read); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		var out struct{}
		if !m.Reply || m.Decode(&out) == nil {
			t.Fatalf("the failed reply reached the handler as %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failed reply never reached the handler")
	}
}

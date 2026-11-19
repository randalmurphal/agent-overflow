package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/cdporder"

	"github.com/chromedp/cdproto"
)

// wireTransport hands a Tap the frames of a scripted wire, one per Read.
type wireTransport struct{ in chan *cdproto.Message }

func (w *wireTransport) Read(_ context.Context, msg *cdproto.Message) error {
	m, ok := <-w.in
	if !ok {
		return io.EOF
	}
	*msg = *m
	return nil
}
func (w *wireTransport) Write(context.Context, *cdproto.Message) error { return nil }
func (w *wireTransport) Close() error                                  { return nil }

const (
	wireSession = "S-1"
	wireMain    = "T-main"
	// wireTreeID is the id of the page's Page.getFrameTree command.
	wireTreeID = 41
)

// wireEvent is a page-session event of method with params.
func wireEvent(method string, params map[string]any) *cdproto.Message {
	raw, _ := json.Marshal(params)
	return &cdproto.Message{SessionID: wireSession, Method: cdproto.MethodType(method), Params: raw}
}

func wireLoad(started bool) *cdproto.Message {
	if started {
		return wireEvent("Page.frameStartedLoading", map[string]any{"frameId": wireMain})
	}
	return wireEvent("Page.frameStoppedLoading", map[string]any{"frameId": wireMain})
}

func wireAttach(frame string) *cdproto.Message {
	return wireEvent("Page.frameAttached", map[string]any{"frameId": frame, "parentFrameId": wireMain})
}

func wireDetach(frame, reason string) *cdproto.Message {
	return wireEvent("Page.frameDetached", map[string]any{"frameId": frame, "reason": reason})
}

// wireTargetAttached is the attach of a cross-site frame's iframe target to
// the page's session, as Chromium sends it before the frame's swap.
func wireTargetAttached(frame, session string) *cdproto.Message {
	return wireEvent("Target.attachedToTarget", map[string]any{
		"sessionId": session, "waitingForDebugger": false,
		"targetInfo": map[string]any{"targetId": frame, "type": "iframe", "title": "", "url": "", "attached": true, "canAccessOpener": false, "parentFrameId": wireMain},
	})
}

func wireTargetDetached(session string) *cdproto.Message {
	return wireEvent("Target.detachedFromTarget", map[string]any{"sessionId": session})
}

// wireTree is the reply to the page's frame tree read, naming children
// under the main frame.
func wireTree(children ...string) *cdproto.Message {
	frame := func(id, parent string) map[string]any {
		return map[string]any{"id": id, "parentId": parent, "loaderId": "L", "url": "about:blank", "securityOrigin": "://", "mimeType": "text/html"}
	}
	var kids []any
	for _, child := range children {
		kids = append(kids, map[string]any{"frame": frame(child, wireMain)})
	}
	raw, _ := json.Marshal(map[string]any{"frameTree": map[string]any{"frame": frame(wireMain, ""), "childFrames": kids}})
	return &cdproto.Message{SessionID: wireSession, ID: wireTreeID, Result: raw}
}

// wireAnswers is what a page's handling of a wire leaves: its last loading
// report (unknown when none came) and which frames it owns.
type wireAnswers struct {
	loading string
	owns    string
}

// runWire feeds wire through a Tap to a page's lifecycle input, sleeping
// delay(i) before handling the i-th message, and answers what the page
// ends with once every message is handled.
func runWire(t *testing.T, wire []*cdproto.Message, frames []string, delay func(int) time.Duration) wireAnswers {
	t.Helper()
	var mu sync.Mutex
	var kept uint64
	loading := "unknown"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &cdpPage{ctx: ctx, handle: wireMain, frames: newFrameTracker(), hooks: pageHooks{Loading: func(now bool, order uint64) {
		mu.Lock()
		defer mu.Unlock()
		if order > kept {
			kept, loading = order, fmt.Sprint(now)
		}
	}}}
	transport := &wireTransport{in: make(chan *cdproto.Message, len(wire))}
	tap := cdporder.NewTap(transport)
	seeded := make(chan struct{})
	handled := make(chan struct{}, len(wire))
	var n int
	stop := tap.Watch(wireSession, lifecycleMethods, func(m cdporder.Message) {
		time.Sleep(delay(n))
		n++
		p.lifecycle(m, seeded)
		handled <- struct{}{}
	})
	defer stop()
	if err := tap.Write(ctx, &cdproto.Message{ID: wireTreeID, SessionID: wireSession, Method: cdproto.CommandPageGetFrameTree}); err != nil {
		t.Fatal(err)
	}
	for _, m := range wire {
		transport.in <- m
		var read cdproto.Message
		if err := tap.Read(ctx, &read); err != nil {
			t.Fatal(err)
		}
	}
	for range wire {
		select {
		case <-handled:
		case <-time.After(10 * time.Second):
			t.Fatal("the page never handled its wire")
		}
	}
	answers := wireAnswers{loading: loading}
	for _, frame := range frames {
		answers.owns += fmt.Sprintf("%s:%v ", frame, p.OwnsFrame(frame))
	}
	return answers
}

// A page's loading and frames are what its events and frame tree say in
// the order the browser sent them, however long its handler takes over
// each: the answers are the same with no delay, with the first message
// handled after loadReorderWindow's old second, and with uneven delays.
// Each wire is one the per-method subscriptions and their time window got
// wrong.
func TestCDPPageReadsItsWireInOrder(t *testing.T) {
	cases := []struct {
		name   string
		wire   []*cdproto.Message
		frames []string
		want   wireAnswers
	}{
		{
			"a stop at the attach, then a navigation that has not stopped",
			[]*cdproto.Message{wireTree(), wireLoad(false), wireLoad(true)},
			nil, wireAnswers{loading: "true"},
		},
		{
			"a load that already finished, handled late",
			[]*cdproto.Message{wireTree(), wireLoad(true), wireLoad(false)},
			nil, wireAnswers{loading: "false"},
		},
		{
			"attached mid-load: nothing said yet",
			[]*cdproto.Message{wireTree()},
			nil, wireAnswers{loading: "unknown"},
		},
		{
			"an iframe in both its attach and the tree, then detached",
			[]*cdproto.Message{wireAttach("F-1"), wireTree("F-1"), wireDetach("F-1", "remove")},
			[]string{"F-1"}, wireAnswers{loading: "unknown", owns: "F-1:false "},
		},
		{
			"an iframe there before the attach, swapped out and back",
			[]*cdproto.Message{wireTree("F-2"), wireDetach("F-2", "swap"), wireAttach("F-2")},
			[]string{"F-2"}, wireAnswers{loading: "unknown", owns: "F-2:true "},
		},
		{
			"an iframe detached before the tree read",
			[]*cdproto.Message{wireAttach("F-3"), wireDetach("F-3", "remove"), wireTree()},
			[]string{"F-3"}, wireAnswers{loading: "unknown", owns: "F-3:false "},
		},
		{
			// As real Chromium sends a cross-site iframe's.
			"a cross-site iframe moved to its own renderer",
			[]*cdproto.Message{wireAttach("F-x"), wireTargetAttached("F-x", "S-x"), wireDetach("F-x", "swap")},
			[]string{"F-x"}, wireAnswers{loading: "unknown", owns: "F-x:true "},
		},
		{
			"a cross-site iframe, then the tree read that no longer names it",
			[]*cdproto.Message{wireAttach("F-x"), wireTargetAttached("F-x", "S-x"), wireDetach("F-x", "swap"), wireTree()},
			[]string{"F-x"}, wireAnswers{loading: "unknown", owns: "F-x:true "},
		},
		{
			"a cross-site iframe removed",
			[]*cdproto.Message{wireAttach("F-x"), wireTargetAttached("F-x", "S-x"), wireDetach("F-x", "swap"), wireDetach("F-x", "remove"), wireTargetDetached("S-x")},
			[]string{"F-x"}, wireAnswers{loading: "unknown", owns: "F-x:false "},
		},
	}
	delays := map[string]func(int) time.Duration{
		"no delay": func(int) time.Duration { return 0 },
		"the first message late": func(i int) time.Duration {
			if i == 0 {
				return 1100 * time.Millisecond
			}
			return 0
		},
		"uneven delays": func(i int) time.Duration { return time.Duration((i*7)%3) * 15 * time.Millisecond },
	}
	for _, tc := range cases {
		for name, delay := range delays {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				if got := runWire(t, tc.wire, tc.frames, delay); got != tc.want {
					t.Errorf("answers %+v, want %+v", got, tc.want)
				}
			})
		}
	}
}

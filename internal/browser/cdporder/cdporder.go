// Package cdporder gives a CDP target session one ordered input of the
// events and command replies it watches, in the order the browser sent
// them on the connection.
//
// chromedp delivers each event method on a subscription of its own, read
// by its own goroutine, so two events of different methods can be handled
// in the opposite order to the one the browser sent them in, and a
// command's reply reaches its caller apart from both. State that two
// methods describe together (a frame's load starting and stopping, a frame
// attaching and the frame tree read at the page's attach) needs the
// browser's order. The connection's Transport sees every message before
// chromedp demultiplexes it, so this package wraps it (allocator) and
// copies each watched message, as it is read, to the queue of the session
// that watches it (Watch). A watched command's reply is queued at its
// place too, so a snapshot read at a session's start sits between the
// events it reflects and the events after it. Every watcher of a session
// gets each message it watches, a command's reply as much as an event.
//
// Reading the connection never waits: a queue grows while its handler is
// behind, as chromedp's own subscriptions do.
package cdporder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// dialTimeout bounds the websocket dial, as remote.NewAllocator's default.
const dialTimeout = 10 * time.Second

// Message is one watched event or command reply of a session, as the
// browser sent it. Params holds an event's parameters or a reply's
// result; Err is a reply's protocol error.
type Message struct {
	Method string
	Params jsonv2.Value
	Reply  bool
	Err    error
}

// Decode decodes the message's parameters or result into out.
func (m Message) Decode(out any) error {
	if m.Err != nil {
		return m.Err
	}
	return jsonv2.Unmarshal(m.Params, out, chromedp.DefaultUnmarshalOptions)
}

// errUnordered is Watch's answer for a context whose browser was not
// connected through an allocator of this package.
var errUnordered = errors.New("cdporder: the browser connection gives no ordered input")

// allocator connects to a running browser at a websocket address, as
// remote.NewAllocator with remote.NoModifyURL does, through a connection
// that feeds Watch.
type allocator struct {
	wsURL string
	wg    sync.WaitGroup
	mu    sync.Mutex
	taps  map[*chromedp.Browser]*Tap
}

var (
	_ chromedp.Allocator = (*allocator)(nil)
	_ chromedp.Attacher  = (*allocator)(nil)
)

// NewAllocator answers an allocator context for the browser at wsURL, a
// "ws://host:port/devtools/browser/..." address, for chromedp.NewContext.
func NewAllocator(parent context.Context, wsURL string) (context.Context, context.CancelFunc) {
	return chromedp.NewAllocatorContext(parent, &allocator{wsURL: wsURL, taps: make(map[*chromedp.Browser]*Tap)})
}

// Allocate dials the browser and connects chromedp through a Tap.
func (a *allocator) Allocate(ctx context.Context, opts ...chromedp.BrowserOption) (*chromedp.Browser, error) {
	if chromedp.FromContext(ctx) == nil {
		return nil, chromedp.ErrInvalidContext
	}
	// The connection outlives ctx until chromedp has closed its pages, as
	// remote.Allocator's does.
	wctx, cancel := context.WithCancel(context.Background())
	a.wg.Go(func() {
		<-ctx.Done()
		// ctx has ended, so Cancel answers that end or the lost
		// connection, never a failure of its own: the outcome is
		// the caller's that ended ctx.
		_ = chromedp.Cancel(ctx)
		cancel()
	})
	dialCtx, dialCancel := context.WithTimeout(wctx, dialTimeout)
	defer dialCancel()
	conn, err := remote.DialContext(dialCtx, a.wsURL)
	if err != nil {
		return nil, fmt.Errorf("could not dial %q: %w", a.wsURL, err)
	}
	tap := NewTap(conn)
	browser, err := chromedp.NewBrowserTransport(wctx, tap, opts...)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.taps[browser] = tap
	a.mu.Unlock()
	a.wg.Go(func() {
		<-browser.LostConnection
		a.mu.Lock()
		delete(a.taps, browser)
		a.mu.Unlock()
	})
	return browser, nil
}

// Wait blocks until the allocator's connections are released.
func (a *allocator) Wait() { a.wg.Wait() }

// Attaches says the allocator connects to a browser already running.
func (a *allocator) Attaches() bool { return true }

func (a *allocator) tap(browser *chromedp.Browser) *Tap {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.taps[browser]
}

// Watch calls handle with each event of methods the target session of ctx
// receives from now on, and the reply to each command of methods the
// session sends, one at a time on a goroutine of its own, in the order
// the browser sent them, until ctx ends.
func Watch(ctx context.Context, methods []string, handle func(Message)) error {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Target == nil {
		return chromedp.ErrInvalidContext
	}
	a, ok := c.Allocator.(*allocator)
	if !ok {
		return errUnordered
	}
	tap := a.tap(c.Browser)
	if tap == nil {
		return errUnordered
	}
	stop := tap.Watch(c.Target.SessionID, methods, handle)
	context.AfterFunc(ctx, stop)
	return nil
}

// WatchBrowser is Watch for the browser-level session of the browser ctx
// is connected to (its first subscription or call connects it), whose
// target events and download events need the browser's order as much as a
// page's frame events do.
func WatchBrowser(ctx context.Context, methods []string, handle func(Message)) error {
	c := chromedp.FromContext(ctx)
	if c == nil {
		return chromedp.ErrInvalidContext
	}
	if c.Browser == nil {
		return ErrNotConnected
	}
	a, ok := c.Allocator.(*allocator)
	if !ok {
		return errUnordered
	}
	tap := a.tap(c.Browser)
	if tap == nil {
		return errUnordered
	}
	// The browser-level session sends its messages with no session id.
	stop := tap.Watch("", methods, handle)
	context.AfterFunc(ctx, stop)
	return nil
}

// ErrNotConnected is WatchBrowser's answer for a context whose browser is
// not connected yet.
var ErrNotConnected = errors.New("cdporder: the context's browser is not connected yet")

// Tap is a chromedp.Transport that copies the watched messages it reads to
// their sessions' queues before chromedp sees them.
type Tap struct {
	chromedp.Transport
	mu       sync.Mutex
	sessions map[target.SessionID]map[*queue]struct{}
	// pending is each watched command sent and not answered yet, by
	// message id.
	pending map[int64]pendingReply
}

// pendingReply is a watched command awaiting its reply, and every queue
// watching the command on its session when it was sent: each gets the
// reply, as each gets the session's events it watches.
type pendingReply struct {
	method string
	queues []*queue
}

// NewTap wraps transport.
func NewTap(transport chromedp.Transport) *Tap {
	return &Tap{Transport: transport, sessions: make(map[target.SessionID]map[*queue]struct{}), pending: make(map[int64]pendingReply)}
}

// SetDebugf passes chromedp's protocol logger to the wrapped transport.
func (t *Tap) SetDebugf(f func(string, ...any)) {
	if d, ok := t.Transport.(interface{ SetDebugf(func(string, ...any)) }); ok {
		d.SetDebugf(f)
	}
}

// Watch is the package's Watch for a session of this connection; the
// answered stop ends it.
func (t *Tap) Watch(session target.SessionID, methods []string, handle func(Message)) (stop func()) {
	q := newQueue(methods, handle)
	t.mu.Lock()
	if t.sessions[session] == nil {
		t.sessions[session] = make(map[*queue]struct{})
	}
	t.sessions[session][q] = struct{}{}
	t.mu.Unlock()
	go q.run()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.sessions[session], q)
			if len(t.sessions[session]) == 0 {
				delete(t.sessions, session)
			}
			for id, p := range t.pending {
				p.queues = slices.DeleteFunc(p.queues, func(w *queue) bool { return w == q })
				if len(p.queues) == 0 {
					delete(t.pending, id)
				} else {
					t.pending[id] = p
				}
			}
			t.mu.Unlock()
			q.stop()
		})
	}
}

// Write notes a watched command before it is sent, so its reply, which
// can only follow, finds the queues watching it.
func (t *Tap) Write(ctx context.Context, msg *cdproto.Message) error {
	if msg.Method != "" {
		t.mu.Lock()
		var watchers []*queue
		for q := range t.sessions[msg.SessionID] {
			if q.watches(string(msg.Method)) {
				watchers = append(watchers, q)
			}
		}
		if len(watchers) > 0 {
			t.pending[msg.ID] = pendingReply{method: string(msg.Method), queues: watchers}
		}
		t.mu.Unlock()
	}
	return t.Transport.Write(ctx, msg)
}

// Read reads the next message and queues it for the sessions watching it.
func (t *Tap) Read(ctx context.Context, msg *cdproto.Message) error {
	if err := t.Transport.Read(ctx, msg); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case msg.Method != "":
		for q := range t.sessions[msg.SessionID] {
			if q.watches(string(msg.Method)) {
				q.push(Message{Method: string(msg.Method), Params: bytes.Clone(msg.Params)})
			}
		}
	case msg.ID != 0:
		p, ok := t.pending[msg.ID]
		if !ok {
			return nil
		}
		delete(t.pending, msg.ID)
		for _, q := range p.queues {
			reply := Message{Method: p.method, Params: bytes.Clone(msg.Result), Reply: true}
			if msg.Error != nil {
				reply.Err = msg.Error
			}
			q.push(reply)
		}
	}
	return nil
}

// queue is one watcher's messages, handed to its handler in order.
type queue struct {
	methods map[string]struct{}
	handle  func(Message)
	mu      sync.Mutex
	items   []Message
	stopped bool
	wake    chan struct{}
}

func newQueue(methods []string, handle func(Message)) *queue {
	q := &queue{methods: make(map[string]struct{}, len(methods)), handle: handle, wake: make(chan struct{}, 1)}
	for _, method := range methods {
		q.methods[method] = struct{}{}
	}
	return q
}

func (q *queue) watches(method string) bool {
	_, ok := q.methods[method]
	return ok
}

func (q *queue) push(m Message) {
	q.mu.Lock()
	if !q.stopped {
		q.items = append(q.items, m)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) stop() {
	q.mu.Lock()
	q.stopped, q.items = true, nil
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// run hands the queued messages to the handler until the queue stops.
func (q *queue) run() {
	for range q.wake {
		for {
			q.mu.Lock()
			if q.stopped {
				q.mu.Unlock()
				return
			}
			if len(q.items) == 0 {
				q.mu.Unlock()
				break
			}
			m := q.items[0]
			q.items[0] = Message{}
			q.items = q.items[1:]
			q.mu.Unlock()
			q.handle(m)
		}
	}
}

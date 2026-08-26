// Package cdpclient is a minimal Chrome DevTools Protocol client: find a
// page target over the debugger's HTTP endpoint, open its WebSocket, and
// call methods on it with id correlation plus a subscription channel for
// domain events.
//
// It is deliberately NOT a generated binding surface. chromedp/cdproto is
// already in this module (internal/screenshot uses it), and it carries
// every domain of the protocol as generated Go — megabytes of types to
// call six methods with. The callers here (`ao-harness profile`,
// `ao-harness bench --trace`) each speak a handful of methods whose
// parameters are three fields wide, so the typed helpers live beside the
// caller and this package stays about the wire: discovery, correlation,
// events, and the read limit.
//
// It links nothing else in this repo, for the same reason
// harnessclient does not link the transport server: an instrument must be
// able to attach to a process it knows nothing about.
package cdpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// ReadLimit is the frame cap on a CDP connection. The default in
// coder/websocket is 32 KiB, which is off by four orders of magnitude for
// this protocol: `Profiler.stop` answers with the whole .cpuprofile in one
// result (tens of MiB for a minute of sampling at 100µs), and an `IO.read`
// chunk is bounded only by the size asked for. A client that left the
// default there would fail on exactly the calls it exists to make.
const ReadLimit = 256 << 20

// Event is one protocol notification: a method name and its raw
// parameters. The payload stays raw because every consumer decodes a
// different shape out of it and most decode nothing at all.
type Event struct {
	Method string
	Params json.RawMessage
}

// ProtocolError is an error envelope the browser returned for one call.
type ProtocolError struct {
	Method  string
	Code    int
	Message string
	Data    string
}

func (e *ProtocolError) Error() string {
	msg := fmt.Sprintf("%s: %s (code %d)", e.Method, e.Message, e.Code)
	if e.Data != "" {
		msg += ": " + e.Data
	}
	return msg
}

type pendingCall struct {
	method string
	result chan json.RawMessage
	fail   chan error
}

// Conn is one WebSocket to one debugger target.
type Conn struct {
	url  string
	conn *websocket.Conn

	writeMu sync.Mutex

	mu     sync.Mutex
	nextID int
	// pending correlates a reply to the call that is parked on it. The
	// protocol answers out of order (a slow Profiler.stop does not block a
	// later Runtime.evaluate), so the id is the only thing that pairs them.
	pending map[int]*pendingCall
	subs    map[int]*Subscription
	nextSub int
	closed  bool
	closeer error

	readDone chan struct{}
}

// Dial opens a debugger WebSocket. The URL is a `webSocketDebuggerUrl`
// from target discovery, or one a caller was handed directly.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", wsURL, err)
	}
	conn.SetReadLimit(ReadLimit)
	c := &Conn{
		url:      wsURL,
		conn:     conn,
		pending:  map[int]*pendingCall{},
		subs:     map[int]*Subscription{},
		readDone: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// URL is the endpoint this connection was opened against.
func (c *Conn) URL() string { return c.url }

// Close tears the connection down; parked calls fail rather than hang.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	err := c.conn.Close(websocket.StatusNormalClosure, "client closing")
	<-c.readDone
	return err
}

// Done is closed when the read loop exits, whatever ended it.
func (c *Conn) Done() <-chan struct{} { return c.readDone }

// Call invokes one protocol method. params may be nil.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	call := &pendingCall{method: method, result: make(chan json.RawMessage, 1), fail: make(chan error, 1)}

	c.mu.Lock()
	if c.closed {
		err := c.closeer
		c.mu.Unlock()
		if err == nil {
			err = errors.New("connection closed")
		}
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = call
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	frame := map[string]any{"id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("%s: encode params: %w", method, err)
	}
	c.writeMu.Lock()
	err = c.conn.Write(ctx, websocket.MessageText, payload)
	c.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%s: write: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case err := <-call.fail:
		return nil, err
	case result := <-call.result:
		return result, nil
	}
}

// CallInto is Call with the result decoded into out.
func (c *Conn) CallInto(ctx context.Context, out any, method string, params any) error {
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

// Subscription is a channel of the events one caller asked for.
type Subscription struct {
	conn    *Conn
	key     int
	methods map[string]struct{}
	ch      chan Event

	mu      sync.Mutex
	dropped int
	closed  bool
}

// subscriptionBuffer is how many events one subscription may hold before
// arrivals are dropped. The subscribers here wait for a single terminal
// notification (`Tracing.tracingComplete`), so anything beyond a small
// buffer is retained noise — and BLOCKING instead would park the read
// loop, which would deadlock every parked call on the connection.
const subscriptionBuffer = 64

// Subscribe registers interest in the named event methods. Register
// BEFORE the call that causes the event: the browser can deliver a
// notification inside the round trip that triggered it.
func (c *Conn) Subscribe(methods ...string) *Subscription {
	set := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		set[method] = struct{}{}
	}
	sub := &Subscription{conn: c, methods: set, ch: make(chan Event, subscriptionBuffer)}
	c.mu.Lock()
	c.nextSub++
	sub.key = c.nextSub
	c.subs[sub.key] = sub
	c.mu.Unlock()
	return sub
}

// C is the subscription's event channel.
func (s *Subscription) C() <-chan Event { return s.ch }

// Dropped counts events this subscription could not hold. Non-zero means
// the reader was slower than the browser, not that nothing happened.
func (s *Subscription) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Close unregisters the subscription. Idempotent.
func (s *Subscription) Close() {
	s.conn.mu.Lock()
	delete(s.conn.subs, s.key)
	s.conn.mu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Wait blocks for the next matching event, the context, or the
// connection dying. A wait that outlives the connection is the shape
// that hangs a profile run forever, so the closed socket is an error
// here rather than silence.
func (s *Subscription) Wait(ctx context.Context) (Event, error) {
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case ev := <-s.ch:
		return ev, nil
	case <-s.conn.Done():
		// Drain first: the event may have arrived in the same instant the
		// socket closed, and dropping it would report "browser gone" about
		// a run that finished.
		select {
		case ev := <-s.ch:
			return ev, nil
		default:
		}
		return Event{}, fmt.Errorf("the browser closed the debugger connection while waiting for %s", s.methodList())
	}
}

func (s *Subscription) methodList() string {
	names := make([]string, 0, len(s.methods))
	for name := range s.methods {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "any event"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (s *Subscription) offer(ev Event) {
	if len(s.methods) > 0 {
		if _, ok := s.methods[ev.Method]; !ok {
			return
		}
	}
	select {
	case s.ch <- ev:
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

// wireMessage is the one frame shape the protocol has: a reply (id set)
// or a notification (method set).
type wireMessage struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

func (c *Conn) readLoop() {
	defer close(c.readDone)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.failAll(err)
			return
		}
		var msg wireMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.failAll(fmt.Errorf("decode devtools frame: %w", err))
			_ = c.conn.Close(websocket.StatusProtocolError, "undecodable frame")
			return
		}
		if msg.ID != 0 {
			c.complete(msg)
			continue
		}
		if msg.Method == "" {
			continue
		}
		c.mu.Lock()
		subs := make([]*Subscription, 0, len(c.subs))
		for _, sub := range c.subs {
			subs = append(subs, sub)
		}
		c.mu.Unlock()
		for _, sub := range subs {
			sub.offer(Event{Method: msg.Method, Params: msg.Params})
		}
	}
}

func (c *Conn) complete(msg wireMessage) {
	c.mu.Lock()
	call := c.pending[msg.ID]
	delete(c.pending, msg.ID)
	c.mu.Unlock()
	if call == nil {
		return
	}
	if msg.Error != nil {
		call.fail <- &ProtocolError{
			Method: call.method, Code: msg.Error.Code,
			Message: msg.Error.Message, Data: msg.Error.Data,
		}
		return
	}
	call.result <- msg.Result
}

func (c *Conn) failAll(err error) {
	c.mu.Lock()
	if c.closeer == nil {
		c.closeer = err
	}
	c.closed = true
	pending := c.pending
	c.pending = map[int]*pendingCall{}
	c.mu.Unlock()
	for _, call := range pending {
		call.fail <- fmt.Errorf("%s: %w", call.method, err)
	}
}

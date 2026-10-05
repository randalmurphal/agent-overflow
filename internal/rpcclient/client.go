// Package rpcclient is the small serialized transport client shared by local
// owner commands and computer-to-computer calls. It holds no subscriptions.
package rpcclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Client makes one call at a time on its connection. Once Hello or the
// first Call has run, a reader owns the connection: it drops event and
// heartbeat frames, answers the far side's pings, and ends Done when the
// connection does, so an idle client that is held for reuse is known dead
// before its next call.
type Client struct {
	conn *websocket.Conn
	mu   sync.Mutex
	next uint64

	reading sync.Once
	replies chan transport.ServerFrame
	// done closes when the reader stops; err is why, set before.
	done chan struct{}
	err  error
	// closing closes on Close, so a reader holding a reply nobody is
	// waiting for does not outlive the connection.
	closing   chan struct{}
	closeOnce sync.Once
}

// New wraps an open connection. The read bound is the transport's own
// inbound bound, because this client reads what that transport sends: a
// paired computer answers an agent thread call with whole answers, whole
// transcript windows and whole item ranges, each of which is larger than a
// megabyte by design, and a reply over the bound closes the connection
// instead of returning an error the caller could act on.
func New(conn *websocket.Conn) *Client {
	conn.SetReadLimit(transport.DefaultReadLimit)
	return &Client{conn: conn, replies: make(chan transport.ServerFrame), done: make(chan struct{}), closing: make(chan struct{})}
}

func (c *Client) Close() {
	c.closeOnce.Do(func() { close(c.closing) })
	_ = c.conn.CloseNow()
}

// Done is closed once the connection has ended after Hello or a Call
// started its reader.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) read() {
	defer close(c.done)
	for {
		var frame transport.ServerFrame
		if err := wsjson.Read(context.Background(), c.conn, &frame); err != nil {
			c.err = err
			return
		}
		if frame.Type != "rpc" {
			continue
		}
		select {
		case c.replies <- frame:
		case <-c.closing:
			c.err = net.ErrClosed
			return
		}
	}
}

// Hello is read before any remote mutation, so identity and feature support
// can be verified on the authenticated connection actually used for the call.
type Hello struct {
	Routes          []computerroute.Route `json:"routes"`
	Type            string                `json:"type"`
	BackendID       string                `json:"backendId"`
	ProtocolVersion int                   `json:"protocolVersion"`
	Capabilities    []string              `json:"capabilities"`
}

// Ping round-trips a protocol ping, which the reader answers on this side
// and the far side's read loop on its own. A ping that times out leaves the
// connection open for the caller to close.
func (c *Client) Ping(ctx context.Context) error { return c.conn.Ping(ctx) }

// errHelloAfterReading is a Hello asked for once the reader owns the
// connection, which has already consumed the hello frame.
var errHelloAfterReading = errors.New("rpcclient: Hello must be read before the first Call")

func (c *Client) Hello(ctx context.Context) (Hello, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var frame Hello
	started := true
	c.reading.Do(func() { started = false })
	if started {
		return frame, errHelloAfterReading
	}
	err := wsjson.Read(ctx, c.conn, &frame)
	go c.read()
	if err != nil {
		return frame, err
	}
	if frame.Type != "hello" {
		return frame, fmt.Errorf("computer did not identify itself")
	}
	return frame, nil
}

type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Message }

func (c *Client) Call(ctx context.Context, method string, result any, params ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reading.Do(func() { go c.read() })
	c.next++
	id := fmt.Sprint(c.next)
	body := make([]json.RawMessage, len(params))
	for i, param := range params {
		raw, err := json.Marshal(param)
		if err != nil {
			return err
		}
		body[i] = raw
	}
	if err := wsjson.Write(ctx, c.conn, transport.ClientFrame{Type: "rpc", ID: id, Method: method, Params: body}); err != nil {
		return err
	}
	for {
		var frame transport.ServerFrame
		select {
		case frame = <-c.replies:
		case <-c.done:
			return c.err
		case <-ctx.Done():
			// The reply may still arrive, and the next call must not read
			// it as its own; the connection ends with the abandoned call.
			c.Close()
			return ctx.Err()
		}
		if frame.ID != id {
			continue
		}
		if frame.Error != nil {
			return &Error{Code: frame.Error.Code, Message: frame.Error.Message}
		}
		if result == nil {
			return nil
		}
		destination := reflect.ValueOf(result)
		if destination.Kind() != reflect.Pointer || destination.IsNil() {
			return &json.InvalidUnmarshalError{Type: reflect.TypeOf(result)}
		}
		fresh := reflect.New(destination.Elem().Type())
		if err := json.Unmarshal(frame.Result, fresh.Interface()); err != nil {
			return err
		}
		destination.Elem().Set(fresh.Elem())
		return nil
	}
}

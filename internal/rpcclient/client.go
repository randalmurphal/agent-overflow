// Package rpcclient is the small serialized transport client shared by local
// owner commands and computer-to-computer calls. It holds no subscriptions.
package rpcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type Client struct {
	conn *websocket.Conn
	mu   sync.Mutex
	next uint64
}

func New(conn *websocket.Conn) *Client { conn.SetReadLimit(1 << 20); return &Client{conn: conn} }
func (c *Client) Close()               { _ = c.conn.CloseNow() }

// Hello is read before any remote mutation, so identity and feature support
// can be verified on the authenticated connection actually used for the call.
type Hello struct {
	Type            string   `json:"type"`
	BackendID       string   `json:"backendId"`
	ProtocolVersion int      `json:"protocolVersion"`
	Capabilities    []string `json:"capabilities"`
}

func (c *Client) Hello(ctx context.Context) (Hello, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var frame Hello
	if err := wsjson.Read(ctx, c.conn, &frame); err != nil {
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
		if err := wsjson.Read(ctx, c.conn, &frame); err != nil {
			return err
		}
		if frame.Type != "rpc" || frame.ID != id {
			continue
		}
		if frame.Error != nil {
			return &Error{Code: frame.Error.Code, Message: frame.Error.Message}
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(frame.Result, result)
	}
}

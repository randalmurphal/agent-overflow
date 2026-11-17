//go:build windows

package webview2host

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The smokes' browser-level CDP connection (smokeDialCDP).

// collect answers every message that arrives within d.
func (c *smokeCDP) collect(d time.Duration) []smokeCDPMessage {
	var out []smokeCDPMessage
	deadline := time.After(d)
	for {
		select {
		case message, ok := <-c.feed:
			if !ok {
				return out
			}
			out = append(out, message)
		case <-deadline:
			return out
		}
	}
}

// smokeCDP is a browser-level CDP connection. One goroutine reads it, since
// a read that times out closes a coder/websocket connection; replies answer
// calls, and events wait in order in events.
type smokeCDP struct {
	t      *testing.T
	conn   *websocket.Conn
	next   int64
	feed   chan smokeCDPMessage
	events []smokeCDPMessage
}

type smokeCDPMessage struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func newSmokeCDP(t *testing.T, conn *websocket.Conn) *smokeCDP {
	c := &smokeCDP{t: t, conn: conn, next: 100, feed: make(chan smokeCDPMessage, 256)}
	t.Cleanup(func() { conn.CloseNow() })
	go func() {
		defer close(c.feed)
		for {
			_, payload, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var message smokeCDPMessage
			if err := json.Unmarshal(payload, &message); err != nil {
				t.Errorf("CDP frame %q: %v", payload, err)
				return
			}
			c.feed <- message
		}
	}()
	return c
}

func (c *smokeCDP) read(what string, deadline time.Time) smokeCDPMessage {
	c.t.Helper()
	select {
	case message, ok := <-c.feed:
		if !ok {
			c.t.Fatalf("the CDP connection closed waiting for %s", what)
		}
		return message
	case <-time.After(time.Until(deadline)):
		c.t.Fatalf("no %s within the deadline", what)
	}
	return smokeCDPMessage{}
}

// call sends one command and fails the test with the browser's own message
// if it refuses.
func (c *smokeCDP) call(method string, params any, sessionID string, result any) {
	c.t.Helper()
	if refusal := c.try(method, params, sessionID, result); refusal != "" {
		c.t.Fatalf("%s was refused: %s", method, refusal)
	}
}

// try sends one command and answers the browser's refusal, empty when it
// accepted it.
func (c *smokeCDP) try(method string, params any, sessionID string, result any) string {
	c.t.Helper()
	c.next++
	id := c.next
	frame := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		frame["sessionId"] = sessionID
	}
	body, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, body); err != nil {
		c.t.Fatalf("send %s: %v", method, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		message := c.read("the reply to "+method, deadline)
		if message.Method != "" {
			c.events = append(c.events, message)
			continue
		}
		if message.ID != id {
			continue
		}
		if message.Error != nil {
			return message.Error.Message
		}
		if result != nil {
			if err := json.Unmarshal(message.Result, result); err != nil {
				c.t.Fatalf("%s result %s: %v", method, message.Result, err)
			}
		}
		return ""
	}
}

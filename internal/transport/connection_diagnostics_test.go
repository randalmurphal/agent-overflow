package transport

import (
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type connectionLogCapture struct {
	mu     sync.Mutex
	text   strings.Builder
	closed chan struct{}
	once   sync.Once
}

func (c *connectionLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.text.Write(p)
	if strings.Contains(string(p), "closed after") {
		c.once.Do(func() { close(c.closed) })
	}
	return n, err
}
func TestConnectionLogsCorrelateReconnectIdentity(t *testing.T) {
	capture := &connectionLogCapture{closed: make(chan struct{})}
	prior := log.Writer()
	log.SetOutput(capture)
	t.Cleanup(func() { log.SetOutput(prior) })
	f := newServerFixture(t)
	conn, _, err := websocket.Dial(t.Context(), "ws://"+f.srv.Addr()+"/ws?token=test-token&did=device-123&conn=page-1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Drain hello so server and client can complete a clean close handshake.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(websocket.StatusNormalClosure, "test-close"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-capture.closed:
	case <-ctx.Done():
		t.Fatal("missing close log")
	}
	capture.mu.Lock()
	text := capture.text.String()
	capture.mu.Unlock()
	if !strings.Contains(text, " connected ") || !strings.Contains(text, "closed after") {
		t.Fatalf("missing lifecycle logs: %s", text)
	}
	for _, value := range []string{`device="device-123"`, `page="page-1234"`} {
		if strings.Count(text, value) != 2 {
			t.Fatalf("identity not carried across lifecycle: %s", text)
		}
	}
	if strings.Contains(text, "test-token") {
		t.Fatal("credential logged")
	}
}

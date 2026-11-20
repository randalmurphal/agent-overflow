package linear

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// connCount counts a server's connections as they open and close.
type connCount struct {
	mu             sync.Mutex
	opened, closed int
}

func (c *connCount) track(_ net.Conn, s http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch s {
	case http.StateNew:
		c.opened++
	case http.StateClosed, http.StateHijacked:
		c.closed++
	}
}

func (c *connCount) counts() (opened, closed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opened, c.closed
}

// A bound client's workspace check and the call it admits share one
// connection, and closing the client closes it, whichever of the two
// transports closes first.
func TestBoundClientSharesItsConnections(t *testing.T) {
	for name, closeBoth := range map[string]func(c *Client){
		"main first":    func(c *Client) { c.http.Close(); c.bound.checker.Close() },
		"checker first": func(c *Client) { c.bound.checker.Close(); c.http.Close() },
		"Close":         (*Client).Close,
	} {
		t.Run(name, func(t *testing.T) {
			e := trackerfake.New(trackerfake.Options{Now: func() time.Time { return cfStart }})
			t.Cleanup(e.Close)
			if err := e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{conformanceWorkspace()}}); err != nil {
				t.Fatal(err)
			}
			h, err := e.Handler(trackerfake.Origin{Tracker: trackerfake.TrackerLinear})
			if err != nil {
				t.Fatal(err)
			}
			var conns connCount
			srv := httptest.NewUnstartedServer(h)
			srv.Config.ConnState = conns.track
			srv.Start()
			t.Cleanup(srv.Close)
			c, err := New(Options{
				BaseURL: srv.URL, Version: "test", Workspace: "example",
				Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte("lin_api_conformance1")))),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Viewer(t.Context()); err != nil {
				t.Fatal(err)
			}
			if opened, _ := conns.counts(); opened != 1 {
				t.Errorf("the check and the call opened %d connections, want 1", opened)
			}
			closeBoth(c)
			deadline := time.Now().Add(5 * time.Second)
			for {
				opened, closed := conns.counts()
				if closed == opened {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%d of %d connections still open after Close", opened-closed, opened)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

package trackerfake

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// TestServersCloseWaits checks that Close returns only after every serve
// goroutine has: with no request in flight Close has nothing else to wait
// for.
func TestServersCloseWaits(t *testing.T) {
	e := New(Options{})
	defer e.Close()
	for range 20 {
		srv, err := Serve(e, testSite)
		if err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if n := srv.live.Load(); n != 0 {
			t.Fatalf("%d serve goroutines left after Close", n)
		}
	}
}

func TestServersCloseEndsEverything(t *testing.T) {
	seen := make(chan Request, 8)
	e := New(Options{OnRequest: func(r Request) {
		select {
		case seen <- r:
		default:
		}
	}})
	defer e.Close()
	if err := e.Seed(testFixture()); err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(e, testSite)
	if err != nil {
		t.Fatal(err)
	}
	if n := srv.live.Load(); n != 3 {
		t.Fatalf("%d serve goroutines, want 3", n)
	}
	if err := e.AddFault(Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultHold}); err != nil {
		t.Fatal(err)
	}
	answered := make(chan error, 1)
	go func() {
		body := `{"body":` + testDoc + `}`
		_, err := send(context.Background(), "POST", srv.JiraURL(testSite)+"/rest/api/3/issue/WEB-1/comment", withJSON(basic(testJiraToken), body), body)
		answered <- err
	}()
	if r := <-seen; !r.Held {
		t.Fatalf("first report %+v", r)
	}
	srv.Close()
	if n := srv.live.Load(); n != 0 {
		t.Fatalf("%d serve goroutines left after Close", n)
	}
	if err := <-answered; err == nil {
		t.Fatal("a held request was answered by a closed server")
	}
	srv.Close()
	for _, url := range []string{srv.JiraURL(testSite), srv.TempoURL(), srv.LinearURL()} {
		if _, err := send(context.Background(), "GET", url+"/", nil, ""); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("%s after Close: %v", url, err)
		}
	}
	if err := srv.Err(); err != nil {
		t.Fatalf("a server stopped with %v", err)
	}
}

func TestServeListenFailureClosesWhatStarted(t *testing.T) {
	e := New(Options{})
	defer e.Close()
	var opened []net.Listener
	listen := func(network, addr string) (net.Listener, error) {
		if len(opened) == 2 {
			return nil, errors.New("no ports left")
		}
		ln, err := net.Listen(network, addr)
		if err == nil {
			opened = append(opened, ln)
		}
		return ln, err
	}
	srv, err := serve(e, listen, []string{testSite})
	if srv != nil || err == nil || !strings.Contains(err.Error(), "serve linear") || !strings.Contains(err.Error(), "no ports left") {
		t.Fatalf("serve: %v, %v", srv, err)
	}
	for _, ln := range opened {
		if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
			_ = c.Close()
			t.Fatalf("%s still accepts after a failed start", ln.Addr())
		}
	}
	if _, err := Serve(e, testSite, testSite); err == nil {
		t.Fatal("a site named twice was served")
	}
}

func TestCloseClientConnections(t *testing.T) {
	w := newWorld(t)
	if r := w.jira(t, "GET", "/rest/api/3/myself", ""); r.status != 200 {
		t.Fatalf("answered %d", r.status)
	}
	w.arm(t, Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultHold})
	answered := w.comment(context.Background())
	w.await(t, held)
	w.srv.CloseClientConnections()
	if got := <-answered; got.err == nil {
		t.Fatalf("a request on a closed connection was answered %d", got.r.status)
	}
	if r := w.jira(t, "GET", "/rest/api/3/myself", ""); r.status != 200 {
		t.Fatalf("a new connection answered %d", r.status)
	}
}

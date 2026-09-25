//go:build linux

package devscan

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingPageServer is a loopback page that counts every connection
// accepted, so a test can prove a port was never dialled rather than only
// left off the list.
func countingPageServer(t *testing.T) (*atomic.Int32, int) {
	t.Helper()
	var dials atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dials.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return &dials, loopbackPort(t, srv)
}

// A scoped scan sees the root's own listener and its descendants', and
// never dials a page outside those trees that an unscoped scan would
// offer. The two descendants share an ancestor, so whichever is walked
// second stops at the recorded ancestor and must still reach the root.
func TestScopedScanLooksOnlyInsideItsRoots(t *testing.T) {
	rootDials, rootPort := countingPageServer(t)
	childDials, childPort := countingPageServer(t)
	siblingDials, siblingPort := countingPageServer(t)
	strangerDials, strangerPort := countingPageServer(t)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, rootPort, 100)
	f.listenRow(false, hexLoopbackV4, childPort, 101)
	f.listenRow(false, hexLoopbackV4, siblingPort, 102)
	f.listenRow(false, hexLoopbackV4, strangerPort, 103)
	f.process(t, 200, 1, 200, "bash")
	f.process(t, 300, 200, 300, "node", 100)
	f.process(t, 400, 300, 400, "sh")
	f.process(t, 500, 400, 400, "vite", 101)
	f.process(t, 501, 400, 400, "vite", 102)
	f.process(t, 800, 200, 800, "postgres", 103)
	root := f.write(t)

	servers, err := newScanner(root, time.Now).scopedTo([]int{300}).Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if row := rowFor(t, servers, rootPort); row.PID != 300 || row.Source != SourceSeen {
		t.Errorf("root's own listener = %+v, want pid 300 seen", row)
	}
	if row := rowFor(t, servers, childPort); row.PID != 500 {
		t.Errorf("descendant listener = %+v, want pid 500", row)
	}
	if row := rowFor(t, servers, siblingPort); row.PID != 501 {
		t.Errorf("descendant listener = %+v, want pid 501", row)
	}
	if rootDials.Load() == 0 || childDials.Load() == 0 || siblingDials.Load() == 0 {
		t.Errorf("in-scope pages were not probed: root %d, child %d, sibling %d",
			rootDials.Load(), childDials.Load(), siblingDials.Load())
	}
	absentFrom(t, servers, strangerPort)
	if got := strangerDials.Load(); got != 0 {
		t.Errorf("the scan dialled a listener outside its scope %d times", got)
	}
}

// A hand-named port whose listener is outside the scope is not dialled
// either. The setting stays on screen, reported as nothing listening.
func TestScopedScanDoesNotDialAHandNamedPortOutsideItsRoots(t *testing.T) {
	dials, port := countingPageServer(t)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, port, 100)
	f.process(t, 300, 1, 300, "node")
	f.process(t, 800, 1, 800, "node", 100)
	root := f.write(t)

	servers, err := newScanner(root, time.Now).scopedTo([]int{300}).Scan(context.Background(), nil, []int{port})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	row := rowFor(t, servers, port)
	if !row.Allowed || row.Source != SourceAllowed || row.Listening || row.PID != 0 {
		t.Errorf("row = %+v, want the allowed setting with nothing in scope listening", row)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("the scan dialled a hand-named port outside its scope %d times", got)
	}
}

// A listener nothing readable holds has no process tree to be inside, so
// a scoped scan leaves it alone even with 0 among its roots. An unscoped
// scan still offers it.
func TestAnUnknownPIDListenerIsOnlyInAnUnscopedScan(t *testing.T) {
	dials, port := countingPageServer(t)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, port, 100)
	f.process(t, 300, 1, 300, "node")
	root := f.write(t)

	servers, err := newScanner(root, time.Now).scopedTo([]int{0, 300}).Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("scoped scan: %v", err)
	}
	absentFrom(t, servers, port)
	if got := dials.Load(); got != 0 {
		t.Fatalf("the scoped scan dialled an unknown-pid listener %d times", got)
	}

	servers, err = newScanner(root, time.Now).Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unscoped scan: %v", err)
	}
	if row := rowFor(t, servers, port); row.PID != 0 || row.Source != SourceSeen {
		t.Errorf("row = %+v, want an unowned seen candidate", row)
	}
	if dials.Load() == 0 {
		t.Error("the unscoped scan did not probe the unknown-pid listener")
	}
}

// A scoped scanner with no roots sees nothing.
func TestAScopeWithNoRootsSeesNothing(t *testing.T) {
	dials, port := countingPageServer(t)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, port, 100)
	f.process(t, 300, 1, 300, "node", 100)
	root := f.write(t)

	servers, err := newScanner(root, time.Now).scopedTo(nil).Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(servers) != 0 || dials.Load() != 0 {
		t.Errorf("servers = %+v after %d dials, want nothing", servers, dials.Load())
	}
}

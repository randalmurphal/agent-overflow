//go:build linux

package devscan

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Scan end to end: a fixture proc tree names the ports, and the ports are
// real httptest servers on loopback so the probe has something true to
// answer about. Nothing here spawns a process or leaves the machine.

func htmlServer(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	t.Cleanup(srv.Close)
	return srv, loopbackPort(t, srv)
}

func rowFor(t *testing.T, servers []DevServer, port int) DevServer {
	t.Helper()
	for _, row := range servers {
		if row.Port == port {
			return row
		}
	}
	t.Fatalf("port %d is missing from %+v", port, servers)
	return DevServer{}
}

func absentFrom(t *testing.T, servers []DevServer, port int) {
	t.Helper()
	for _, row := range servers {
		if row.Port == port {
			t.Fatalf("port %d should not be listed: %+v", port, row)
		}
	}
}

// The whole rule in one scan: a thread's own dev server is attributed and
// allowed, somebody else's page is offered as a candidate, and a loopback
// service that is not a page is not offered at all.
func TestScanSeparatesOwnedFromSeenFromNotAPage(t *testing.T) {
	_, ownedPort := htmlServer(t)
	_, strangerPort := htmlServer(t)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()
	apiPort := loopbackPort(t, api)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, ownedPort, 100)
	f.listenRow(false, hexLoopbackV4, strangerPort, 101)
	f.listenRow(false, hexLoopbackV4, apiPort, 102)
	f.process(t, 500, 400, 300, "vite", 100)
	f.process(t, 400, 300, 300, "npm")
	f.process(t, 300, 1, 300, "claude")
	f.process(t, 800, 1, 800, "python3", 101, 102)
	root := f.write(t)

	scanner := newScanner(root, time.Now)
	servers, err := scanner.Scan(context.Background(), []Owner{{ThreadID: "thread-a", PID: 300, PGID: 300}}, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	owned := rowFor(t, servers, ownedPort)
	if owned.ThreadID != "thread-a" || owned.Source != SourceAttributed || !owned.Allowed || !owned.Listening {
		t.Errorf("owned row = %+v, want thread-a/attributed/allowed/listening", owned)
	}
	if owned.Process != "vite" || owned.PID != 500 {
		t.Errorf("owned row lost its process facts: %+v", owned)
	}

	stranger := rowFor(t, servers, strangerPort)
	if stranger.ThreadID != "" || stranger.Source != SourceSeen || stranger.Allowed {
		t.Errorf("stranger row = %+v, want an unowned, unallowed candidate", stranger)
	}

	absentFrom(t, servers, apiPort)

	for i := 1; i < len(servers); i++ {
		if servers[i-1].Port > servers[i].Port {
			t.Fatalf("servers are not sorted by port: %+v", servers)
		}
	}
}

// A hand-named port is published on the owner's say-so. The probe is a
// filter on candidates nobody chose, so it must not run here at all: a
// backend API the person deliberately named would otherwise be dropped by
// a rule they already overrode.
func TestScanPublishesHandNamedPortsWithoutProbing(t *testing.T) {
	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer api.Close()
	apiPort := loopbackPort(t, api)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, apiPort, 100)
	f.process(t, 900, 1, 900, "node", 100)
	root := f.write(t)

	scanner := newScanner(root, time.Now)
	servers, err := scanner.Scan(context.Background(), nil, []int{apiPort})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	row := rowFor(t, servers, apiPort)
	if !row.Allowed || row.Source != SourceAllowed || !row.Listening {
		t.Errorf("hand-named row = %+v, want allowed/allowed/listening", row)
	}
	if hits.Load() != 0 {
		t.Errorf("the probe dialled a hand-named port %d times, want 0", hits.Load())
	}
}

// A hand-named port with nothing on it stays on screen, marked not
// listening. A row that vanished would read as the setting having been
// lost rather than as the dev server being down.
func TestScanKeepsAHandNamedPortWithNothingServing(t *testing.T) {
	f := newProcFixture(t)
	root := f.write(t)

	scanner := newScanner(root, time.Now)
	servers, err := scanner.Scan(context.Background(), nil, []int{4321})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	row := rowFor(t, servers, 4321)
	if !row.Allowed || row.Source != SourceAllowed || row.Listening {
		t.Errorf("row = %+v, want an allowed port that is not listening", row)
	}
	if ports := PreviewPorts(servers); len(ports) != 1 || ports[0] != 4321 {
		t.Errorf("PreviewPorts = %v, want [4321]", ports)
	}
}

// A dev server restarting is the common case, and the URL must survive
// it. The row stays for the grace, marked not listening, and goes when
// the grace lapses.
func TestScanHoldsAnAttributedPortThroughARestart(t *testing.T) {
	_, port := htmlServer(t)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, port, 100)
	f.process(t, 500, 300, 300, "vite", 100)
	f.process(t, 300, 1, 300, "claude")
	root := f.write(t)

	now := time.Unix(1_700_000_000, 0)
	scanner := newScanner(root, func() time.Time { return now })
	owners := []Owner{{ThreadID: "thread-a", PID: 300, PGID: 300}}

	servers, err := scanner.Scan(context.Background(), owners, nil)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if row := rowFor(t, servers, port); !row.Listening || !row.Allowed {
		t.Fatalf("first scan row = %+v, want a live allowed listener", row)
	}

	// The dev server goes away: same proc root, empty socket table.
	(&procFixture{root: root}).write(t)

	now = now.Add(attributedGrace / 2)
	servers, err = scanner.Scan(context.Background(), owners, nil)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	row := rowFor(t, servers, port)
	if row.Listening {
		t.Errorf("row = %+v, want it held but marked not listening", row)
	}
	if !row.Allowed || row.ThreadID != "thread-a" || row.Source != SourceAttributed || row.Process != "vite" {
		t.Errorf("the held row lost what it was: %+v", row)
	}

	now = now.Add(attributedGrace)
	servers, err = scanner.Scan(context.Background(), owners, nil)
	if err != nil {
		t.Fatalf("third scan: %v", err)
	}
	absentFrom(t, servers, port)
}

// A port nobody owned and that stopped listening simply goes: there is no
// preview anybody could lose, so holding it would only leave a dead row
// on screen.
func TestScanDropsASeenPortThatWentAway(t *testing.T) {
	srv, port := htmlServer(t)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, port, 100)
	f.process(t, 800, 1, 800, "node", 100)
	root := f.write(t)

	now := time.Unix(1_700_000_000, 0)
	scanner := newScanner(root, func() time.Time { return now })

	servers, err := scanner.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if row := rowFor(t, servers, port); row.Source != SourceSeen {
		t.Fatalf("first scan row = %+v, want a seen candidate", row)
	}

	srv.Close()
	(&procFixture{root: root}).write(t)
	now = now.Add(time.Second)

	servers, err = scanner.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	absentFrom(t, servers, port)
}

// A dev server bound to both loopback families is two rows in the
// kernel's table and one thing on screen.
func TestScanFoldsBothAddressFamiliesIntoOneRow(t *testing.T) {
	_, port := htmlServer(t)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, port, 100)
	f.listenRow(true, hexLoopbackV6, port, 101)
	f.process(t, 500, 300, 300, "vite", 100, 101)
	f.process(t, 300, 1, 300, "claude")
	root := f.write(t)

	scanner := newScanner(root, time.Now)
	servers, err := scanner.Scan(context.Background(), []Owner{{ThreadID: "thread-a", PID: 300, PGID: 300}}, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("servers = %+v, want exactly one row for port %d", servers, port)
	}
	if servers[0].ThreadID != "thread-a" {
		t.Errorf("row = %+v, want thread-a", servers[0])
	}
}

// A port that is BOTH hand-named and attributed reports "allowed". Source
// says where the row came from, and this one came from the persisted list;
// calling it attributed would hide the entry from the settings screen and
// leave the person no way to stop sharing it while the process runs. The
// thread's own facts stay on the row either way.
func TestAHandNamedPortStaysAllowedEvenWhenAThreadOwnsIt(t *testing.T) {
	_, port := htmlServer(t)

	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, port, 100)
	f.process(t, 500, 300, 300, "vite", 100)
	f.process(t, 300, 1, 300, "claude")
	root := f.write(t)

	scanner := newScanner(root, time.Now)
	servers, err := scanner.Scan(context.Background(),
		[]Owner{{ThreadID: "thread-a", PID: 300, PGID: 300}}, []int{port})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	row := rowFor(t, servers, port)
	if row.Source != SourceAllowed {
		t.Errorf("source = %q, want %q: the persisted entry must stay visible", row.Source, SourceAllowed)
	}
	if row.ThreadID != "thread-a" || row.Process != "vite" || row.PID != 500 {
		t.Errorf("row lost the thread it also belongs to: %+v", row)
	}
	if !row.Allowed || !row.Listening {
		t.Errorf("row = %+v, want allowed and listening", row)
	}
}

// The scan's probes run in parallel, and the reason is a real machine
// rather than a benchmark: a few loopback ports that accept and then take
// their time are enough to spend a whole scan deadline serially, and
// every candidate behind them is then reached with a context that is
// already done. Eight deliberately slow candidates take one round trip
// here, not eight.
func TestScanProbesCandidatesInParallel(t *testing.T) {
	const (
		candidates = 8
		serve      = 200 * time.Millisecond
	)

	f := newProcFixture(t)
	for i := range candidates {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(serve)
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html>"))
		}))
		defer srv.Close()
		port := loopbackPort(t, srv)
		inode := 100 + i
		pid := 900 + i
		f.listenRow(false, hexLoopbackV4, port, inode)
		f.process(t, pid, 1, pid, "node", inode)
	}
	root := f.write(t)

	scanner := newScanner(root, time.Now)
	start := time.Now()
	servers, err := scanner.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	elapsed := time.Since(start)

	if len(servers) != candidates {
		t.Fatalf("servers = %d, want %d: every slow candidate must still be judged", len(servers), candidates)
	}
	// Serial probing costs candidates*serve. Half of that is a bound only
	// a parallel pass can meet, and is loose enough not to flake on a
	// loaded machine.
	if budget := candidates * serve / 2; elapsed > budget {
		t.Fatalf("scan took %s, want under %s: the probes ran serially", elapsed, budget)
	}
}

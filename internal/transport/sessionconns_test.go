package transport

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSessionConnsClosesEveryConnectionOnOneSession(t *testing.T) {
	registry := newSessionConns()
	var closed atomic.Int64
	first := registry.attach("sess-1", func() { closed.Add(1) })
	second := registry.attach("sess-1", func() { closed.Add(1) })
	other := registry.attach("sess-2", func() { closed.Add(100) })

	if got := registry.CountForSession("sess-1"); got != 2 {
		t.Fatalf("CountForSession = %d, want 2", got)
	}
	if got := registry.CloseSession("sess-1"); got != 2 {
		t.Fatalf("CloseSession closed %d connections, want 2", got)
	}
	if got := closed.Load(); got != 2 {
		t.Fatalf("close callbacks ran %d times, want 2", got)
	}
	if got := registry.CountForSession("sess-1"); got != 0 {
		t.Fatalf("CountForSession after close = %d, want 0", got)
	}
	if got := registry.CountForSession("sess-2"); got != 1 {
		t.Fatalf("an unrelated session lost its connection: %d", got)
	}
	// Detaching afterwards is harmless — the connection's own cleanup
	// still runs, and it must not panic or resurrect an entry.
	first()
	second()
	other()
	if got := registry.Sessions(); got != 0 {
		t.Fatalf("registry holds %d sessions after every detach, want 0", got)
	}
}

// TestSessionConnsHandlesTheFourLifecycleOrders — every ordering of
// attach, detach and revoke has to be a normal answer, because a real
// client disconnects, reconnects, and is revoked without coordinating any
// of it with the server.
func TestSessionConnsHandlesTheFourLifecycleOrders(t *testing.T) {
	t.Run("connection closed before revoke", func(t *testing.T) {
		registry := newSessionConns()
		var closed atomic.Int64
		detach := registry.attach("sess", func() { closed.Add(1) })
		detach()
		if got := registry.CloseSession("sess"); got != 0 {
			t.Fatalf("CloseSession closed %d, want 0", got)
		}
		if got := closed.Load(); got != 0 {
			t.Fatal("a detached connection's close callback ran anyway")
		}
	})

	t.Run("revoke before any connection", func(t *testing.T) {
		registry := newSessionConns()
		if got := registry.CloseSession("sess"); got != 0 {
			t.Fatalf("CloseSession on an unknown session closed %d, want 0", got)
		}
		// The registry keeps no tombstone: refusing a later connection is
		// the database row's job, and a second source of that truth is one
		// that could disagree.
		var closed atomic.Int64
		registry.attach("sess", func() { closed.Add(1) })
		if got := registry.CountForSession("sess"); got != 1 {
			t.Fatalf("a later attach was rejected by the registry: %d", got)
		}
	})

	t.Run("two connections on one session", func(t *testing.T) {
		registry := newSessionConns()
		var closed atomic.Int64
		registry.attach("sess", func() { closed.Add(1) })
		registry.attach("sess", func() { closed.Add(1) })
		if got := registry.CloseSession("sess"); got != 2 {
			t.Fatalf("CloseSession closed %d, want both", got)
		}
	})

	t.Run("reconnect after revoke", func(t *testing.T) {
		registry := newSessionConns()
		registry.attach("sess", func() {})
		registry.CloseSession("sess")
		var closed atomic.Int64
		registry.attach("sess", func() { closed.Add(1) })
		if got := registry.CloseSession("sess"); got != 1 {
			t.Fatalf("a reconnected connection was not tracked: %d closed", got)
		}
	})
}

func TestSessionConnsDetachIsIdempotent(t *testing.T) {
	registry := newSessionConns()
	var closed atomic.Int64
	detachA := registry.attach("sess", func() { closed.Add(1) })
	registry.attach("sess", func() { closed.Add(1) })
	detachA()
	detachA()
	detachA()
	if got := registry.CountForSession("sess"); got != 1 {
		t.Fatalf("repeated detach removed %d entries, want 1", 2-got)
	}
	if got := registry.CloseSession("sess"); got != 1 {
		t.Fatalf("CloseSession closed %d, want the surviving connection", got)
	}
}

func TestSessionConnsIgnoresConnectionsThatNameNoSession(t *testing.T) {
	registry := newSessionConns()
	detach := registry.attach("", func() { t.Fatal("a session-less connection was closed by a revocation") })
	if got := registry.Sessions(); got != 0 {
		t.Fatalf("registry holds %d sessions for a session-less connection", got)
	}
	if got := registry.CloseSession(""); got != 0 {
		t.Fatalf("CloseSession(\"\") closed %d connections", got)
	}
	detach()
}

func TestSessionConnsNilRegistryIsUsable(t *testing.T) {
	var registry *SessionConns
	detach := registry.attach("sess", func() { t.Fatal("nil registry invoked a close callback") })
	detach()
	if got := registry.CloseSession("sess"); got != 0 {
		t.Fatalf("nil registry closed %d connections", got)
	}
	if registry.CountForSession("sess") != 0 || registry.Sessions() != 0 {
		t.Fatal("nil registry reported connections")
	}
}

// TestSessionConnsCloseRunsOutsideTheLock — a teardown that blocked while
// holding the registry lock would stall every other attach and detach in
// the process. The proof is that a close callback can itself use the
// registry without deadlocking.
func TestSessionConnsCloseRunsOutsideTheLock(t *testing.T) {
	registry := newSessionConns()
	done := make(chan int, 1)
	registry.attach("sess", func() {
		done <- registry.CountForSession("other")
	})
	registry.attach("other", func() {})

	closedCh := make(chan int, 1)
	go func() { closedCh <- registry.CloseSession("sess") }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a close callback that touched the registry deadlocked")
	}
	if got := <-closedCh; got != 1 {
		t.Fatalf("CloseSession closed %d, want 1", got)
	}
}

func TestSessionConnsUnderConcurrentAttachDetachAndClose(t *testing.T) {
	registry := newSessionConns()
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				detach := registry.attach("sess", func() {})
				if worker%2 == 0 {
					registry.CloseSession("sess")
				}
				detach()
			}
		}()
	}
	wg.Wait()
	registry.CloseSession("sess")
	if got := registry.Sessions(); got != 0 {
		t.Fatalf("registry holds %d sessions after every connection detached", got)
	}
}

// sessionFixture is an integration fixture whose upgrade path resolves a
// durable session, so revocation can be exercised against real sockets.
type sessionFixture struct {
	*integrationFixture
	// resolve decides what a request's session is and whether it may
	// connect. Guarded because a test flips it between dials.
	mu      sync.Mutex
	session string
	refuse  bool
	// dead names sessions SessionLive refuses, which is how a test makes
	// a session stop WITHOUT the registry teardown running — an expiry,
	// or a revocation performed by something that is not this process.
	dead map[string]bool
	// credential is what PageSessionCredential hands the page.
	credential string
	// mutate adjusts the Config before New, for the knobs one test needs.
	mutate func(*Config)
}

func newSessionFixture(t *testing.T) *sessionFixture {
	return newSessionFixtureWith(t, nil)
}

func newSessionFixtureWith(t *testing.T, mutate func(*Config)) *sessionFixture {
	t.Helper()
	fixture := &sessionFixture{session: "sess-1", dead: map[string]bool{}}
	d := NewDispatcher()
	stub := &integrationStub{}
	if _, err := d.Register(stub, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	bus := NewEventBus(64)
	cfg := Config{
		Dispatcher: d,
		EventBus:   bus,
		Token:      "integration-token",
		SessionForRequest: func(*http.Request) (string, bool) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.refuse {
				return "", false
			}
			return fixture.session, true
		},
		SessionLive: func(sessionID string) bool {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			return !fixture.dead[sessionID]
		},
		PageSessionCredential: func() string {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			return fixture.credential
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	fixture.integrationFixture = &integrationFixture{
		t: t, srv: srv, stub: stub, bus: bus, addr: srv.Addr(),
	}
	return fixture
}

func (f *sessionFixture) setRefuse(refuse bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = refuse
}

// setSessionDead makes SessionLive refuse one id, without the live-session
// registry closing anything: the state a session reaches when it simply
// expires.
func (f *sessionFixture) setSessionDead(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead[sessionID] = true
}

func (f *sessionFixture) setPageCredential(credential string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credential = credential
}

// TestRevokingASessionClosesItsLiveSockets is the end-to-end shape of the
// requirement: two real clients on one session, both torn down by one
// CloseSession, with an unrelated session untouched.
func TestRevokingASessionClosesItsLiveSockets(t *testing.T) {
	f := newSessionFixture(t)
	first := f.dial(t)
	second := f.dial(t)

	registry := f.srv.SessionConns()
	waitForSessionConns(t, registry, "sess-1", 2)

	if got := registry.CloseSession("sess-1"); got != 2 {
		t.Fatalf("CloseSession closed %d sockets, want 2", got)
	}
	for i, conn := range []*websocket.Conn{first, second} {
		if err := readUntilClosed(conn); err == nil {
			t.Fatalf("connection %d stayed open after its session was revoked", i)
		}
	}
	waitForSessionConns(t, registry, "sess-1", 0)
}

// TestRevokedSessionCannotReconnect — the socket teardown is only half of
// revocation. A client whose credential is dead must be refused at the
// upgrade, with the same unfingerprintable shape a bad launch credential
// gets, or it would simply dial again.
func TestRevokedSessionCannotReconnect(t *testing.T) {
	f := newSessionFixture(t)
	conn := f.dial(t)
	registry := f.srv.SessionConns()
	waitForSessionConns(t, registry, "sess-1", 1)

	f.setRefuse(true)
	registry.CloseSession("sess-1")
	if err := readUntilClosed(conn); err == nil {
		t.Fatal("the live connection survived revocation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws://"+f.addr+"/ws?token=integration-token", nil)
	if err == nil {
		t.Fatal("a revoked session reconnected")
	}
	if resp == nil {
		t.Fatalf("dial failed without an HTTP response: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("refused upgrade answered %d, want 404 so the route stays unfingerprintable",
			resp.StatusCode)
	}

	// Restoring the session lets a client back in, so the refusal is
	// about the credential and not about the server having latched.
	f.setRefuse(false)
	f.dial(t)
	waitForSessionConns(t, registry, "sess-1", 1)
}

// TestConnectionCloseDeregistersThroughTheOrdinaryCleanupPass — the
// registry must not need its own teardown path. A client that simply goes
// away has to leave the registry empty.
func TestConnectionCloseDeregistersThroughTheOrdinaryCleanupPass(t *testing.T) {
	f := newSessionFixture(t)
	conn := f.dial(t)
	registry := f.srv.SessionConns()
	waitForSessionConns(t, registry, "sess-1", 1)

	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitForSessionConns(t, registry, "sess-1", 0)
	if got := registry.Sessions(); got != 0 {
		t.Fatalf("registry holds %d sessions after the only client left", got)
	}
	// A revocation after the fact is a no-op rather than an error.
	if got := registry.CloseSession("sess-1"); got != 0 {
		t.Fatalf("CloseSession on a departed client closed %d", got)
	}
}

// TestConnectionsThatNameNoSessionAreNotTracked pins today's behavior:
// with no session hook wired, the registry stays empty and nothing about
// the existing launch-credential path changes.
func TestConnectionsThatNameNoSessionAreNotTracked(t *testing.T) {
	f := newIntegrationFixture(t)
	f.dial(t)
	if got := f.srv.SessionConns().Sessions(); got != 0 {
		t.Fatalf("registry tracked %d sessions with no session hook wired", got)
	}
}

func waitForSessionConns(t *testing.T, registry *SessionConns, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if registry.CountForSession(sessionID) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s holds %d connections, want %d",
		sessionID, registry.CountForSession(sessionID), want)
}

// readUntilClosed drains a connection until it errors, which is what a
// force-close looks like from the client side. Returns the terminal error.
func readUntilClosed(conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errors.New("connection stayed open")
		}
	}
}

// TestRevokedConnectionsAreNamedInTheCloseLog — revocation tears a socket
// down by cancelling its context, which at the error alone is
// indistinguishable from a server shutdown. Reporting both the same way
// would make a revocation invisible in the log exactly when somebody is
// checking whether one took effect.
func TestRevokedConnectionsAreNamedInTheCloseLog(t *testing.T) {
	h := &connHandler{}
	if got := h.closeReason(context.Canceled); got != "server shutdown" {
		t.Fatalf("ordinary cancel reported as %q", got)
	}
	h.closeCause.Store(closeCauseRevoked)
	if got := h.closeReason(context.Canceled); got != "session revoked" {
		t.Fatalf("revoked connection reported as %q", got)
	}
	// The cause wins over any terminal error, because every teardown path a
	// revocation triggers ends in one error or another.
	if got := h.closeReason(errors.New("use of closed network connection")); got != "session revoked" {
		t.Fatalf("revoked connection reported as %q", got)
	}
}

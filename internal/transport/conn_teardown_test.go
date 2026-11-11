package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// teardownApp's methods report what a call sees while its connection ends.
type teardownApp struct {
	bus     *EventBus
	entered chan string
	release chan struct{}

	mu  sync.Mutex
	log []string
	// loopsAtCleanup is how many connection goroutines were still running
	// when the connection's cleanup ran.
	loopsAtCleanup int
	done           chan struct{}
}

func newTeardownApp() *teardownApp {
	return &teardownApp{entered: make(chan string, 64), release: make(chan struct{}), done: make(chan struct{}, 64)}
}

func (a *teardownApp) record(line string) {
	a.mu.Lock()
	a.log = append(a.log, line)
	a.mu.Unlock()
}

func (a *teardownApp) lines() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.log...)
}

// registerCleanup records the cleanup and how many connection goroutines
// it found still running.
func (a *teardownApp) registerCleanup(ctx context.Context) error {
	state := ConnStateFromContext(ctx)
	if state == nil {
		return fmt.Errorf("call has no connection state")
	}
	if !state.RegisterCleanup(func() {
		a.mu.Lock()
		a.loopsAtCleanup = connGoroutines()
		a.log = append(a.log, "cleanup")
		a.mu.Unlock()
		a.done <- struct{}{}
	}) {
		return fmt.Errorf("connection already tearing down")
	}
	return nil
}

// Block waits for its caller to go away.
func (a *teardownApp) Block(ctx context.Context) error {
	if err := a.registerCleanup(ctx); err != nil {
		return err
	}
	a.entered <- "block"
	<-ctx.Done()
	a.record(fmt.Sprintf("block returned (subscribers=%d)", a.bus.SubscriberCount()))
	return ctx.Err()
}

// Detached keeps working after its caller is gone.
func (a *teardownApp) Detached(ctx context.Context) error {
	if err := a.registerCleanup(ctx); err != nil {
		return err
	}
	work := context.WithoutCancel(ctx)
	a.entered <- "detached"
	<-a.release
	a.record(fmt.Sprintf("detached finished (err=%v)", work.Err()))
	return nil
}

// connGoroutines counts goroutines running a connHandler method.
func connGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "transport.(*connHandler).") {
			count++
		}
	}
	return count
}

func newTeardownFixture(t *testing.T) (*serverFixture, *teardownApp) {
	t.Helper()
	app := newTeardownApp()
	f := newServerFixtureWith(t, func(cfg *Config) {
		d := NewDispatcher()
		if _, err := d.Register(app, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
			t.Fatalf("register teardown receiver: %v", err)
		}
		cfg.Dispatcher = d
	})
	app.bus = f.bus
	// Runs before the fixture's shutdown so a failed assertion cannot leave
	// a detached call holding the connection forever.
	t.Cleanup(func() {
		select {
		case <-app.release:
		default:
			close(app.release)
		}
	})
	return f, app
}

func callWithoutReply(t *testing.T, conn *websocket.Conn, id, method string) {
	t.Helper()
	buf, err := json.Marshal(ClientFrame{Type: frameTypeRPC, ID: id, Method: method})
	if err != nil {
		t.Fatalf("marshal rpc frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("write rpc frame: %v", err)
	}
}

func waitEntered(t *testing.T, app *teardownApp, want string) {
	t.Helper()
	select {
	case got := <-app.entered:
		if got != want {
			t.Fatalf("entered %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s never started", want)
	}
}

func waitCleanup(t *testing.T, app *teardownApp) {
	t.Helper()
	select {
	case <-app.done:
	case <-time.After(3 * time.Second):
		t.Fatalf("connection cleanup never ran; log %q", app.lines())
	}
}

var disconnects = map[string]func(*websocket.Conn){
	"graceful close": func(c *websocket.Conn) { _ = c.Close(websocket.StatusNormalClosure, "") },
	"abrupt drop":    func(c *websocket.Conn) { _ = c.CloseNow() },
}

// A call blocked on its caller returns as soon as the caller's connection
// ends. Event delivery has stopped by then, and the connection's cleanups
// run only after the call returned and the connection's loops exited.
func TestDisconnectCancelsCallsBeforeCleanups(t *testing.T) {
	for name, disconnect := range disconnects {
		t.Run(name, func(t *testing.T) {
			f, app := newTeardownFixture(t)
			conn := f.dial(t)
			callWithoutReply(t, conn, "block-1", "Block")
			waitEntered(t, app, "block")

			disconnect(conn)
			waitCleanup(t, app)
			want := []string{"block returned (subscribers=0)", "cleanup"}
			if got := app.lines(); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("teardown order = %q, want %q", got, want)
			}
			if app.loopsAtCleanup != 0 {
				t.Fatalf("%d connection goroutines still running when cleanups ran", app.loopsAtCleanup)
			}
		})
	}
}

// A call that detached from its caller finishes after the disconnect. Its
// reply has no socket to go to; writing it neither blocks nor panics, and
// the connection's cleanups wait for it.
func TestDetachedCallCompletesAfterDisconnect(t *testing.T) {
	for name, disconnect := range disconnects {
		t.Run(name, func(t *testing.T) {
			f, app := newTeardownFixture(t)
			conn := f.dial(t)
			callWithoutReply(t, conn, "detached-1", "Detached")
			waitEntered(t, app, "detached")

			disconnect(conn)
			deadline := time.Now().Add(3 * time.Second)
			for f.bus.SubscriberCount() != 0 {
				if time.Now().After(deadline) {
					t.Fatal("event subscriber still attached while the detached call runs")
				}
				time.Sleep(2 * time.Millisecond)
			}
			if got := app.lines(); len(got) != 0 {
				t.Fatalf("log before release = %q, want nothing", got)
			}
			close(app.release)
			waitCleanup(t, app)
			want := []string{"detached finished (err=<nil>)", "cleanup"}
			if got := app.lines(); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("teardown order = %q, want %q", got, want)
			}
		})
	}
}

// Repeated connections that each end mid-call leave no connection
// goroutine behind.
func TestRepeatedDisconnectsLeakNoConnectionGoroutines(t *testing.T) {
	f, app := newTeardownFixture(t)
	before := connGoroutines()
	for i := range 20 {
		conn := f.dial(t)
		callWithoutReply(t, conn, fmt.Sprintf("block-%d", i), "Block")
		waitEntered(t, app, "block")
		if i%2 == 0 {
			disconnects["graceful close"](conn)
		} else {
			disconnects["abrupt drop"](conn)
		}
		waitCleanup(t, app)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		after := connGoroutines()
		if after <= before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection goroutines = %d after 20 disconnects, want at most %d", after, before)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := f.bus.SubscriberCount(); got != 0 {
		t.Fatalf("subscribers after 20 disconnects = %d, want 0", got)
	}
}

// The session watchdog can be inside a session check, which reads the
// store, when its connection ends. The connection's cleanups, and so the
// server's shutdown, wait for that check instead of leaving it running
// against a store its owner may already have closed.
func TestConnectionCleanupsWaitForTheSessionWatchdog(t *testing.T) {
	var (
		mu      sync.Mutex
		checks  int
		gate    chan struct{}
		blocked = make(chan struct{})
	)
	var scopes []string
	for _, scope := range Scopes {
		scopes = append(scopes, string(scope))
	}
	f := newSessionFixtureWith(t, func(cfg *Config) {
		cfg.SessionRecheckInterval = 5 * time.Millisecond
		sessionAuthorityForTest(cfg).check = func(string) SessionStatus {
			mu.Lock()
			checks++
			held := gate
			gate = nil
			mu.Unlock()
			if held != nil {
				close(blocked)
				<-held
			}
			return SessionStatus{Scopes: scopes}
		}
	})
	conn, _, err := websocket.Dial(context.Background(), "ws://"+f.addr+"/ws?token=integration-token", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	registry := f.srv.SessionConns()
	waitForSessionConns(t, registry, "sess-1", 1)
	// Past the upgrade's own checks, only the watchdog asks.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := checks
		mu.Unlock()
		if n >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watchdog never rechecked the session")
		}
		time.Sleep(2 * time.Millisecond)
	}
	release := make(chan struct{})
	mu.Lock()
	gate = release
	mu.Unlock()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("the watchdog never reached the held check")
	}

	_ = conn.CloseNow()
	time.Sleep(100 * time.Millisecond)
	if got := registry.CountForSession("sess-1"); got != 1 {
		close(release)
		t.Fatalf("connection cleanups ran (registry count %d) while its watchdog was still checking", got)
	}
	close(release)
	waitForSessionConns(t, registry, "sess-1", 0)
}

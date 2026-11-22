package transport

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// parkingApp's Park ignores every context and returns only when released,
// the shape of a bound method whose wait outlives the server's lifetime.
type parkingApp struct {
	parked  chan struct{}
	release chan struct{}
}

func (p *parkingApp) Park() {
	close(p.parked)
	<-p.release
}

type shutdownLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *shutdownLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *shutdownLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

const drainOverrun = "connection handlers are still running past the drain deadline"

func captureShutdownLog(t *testing.T) *shutdownLog {
	t.Helper()
	capture := &shutdownLog{}
	prior := log.Writer()
	log.SetOutput(capture)
	t.Cleanup(func() { log.SetOutput(prior) })
	return capture
}

func parkingServer(t *testing.T) (*serverFixture, *parkingApp) {
	t.Helper()
	parker := &parkingApp{parked: make(chan struct{}), release: make(chan struct{})}
	f := newServerFixtureWith(t, func(cfg *Config) {
		d := NewDispatcher()
		if _, err := d.Register(parker, RegisterOptions{Package: "main", TypeName: "Parker"}); err != nil {
			t.Fatalf("register: %v", err)
		}
		cfg.Dispatcher = d
	})
	// Registered after the fixture, so it runs before the fixture's
	// shutdown, which would otherwise wait on the parked call.
	t.Cleanup(func() {
		select {
		case <-parker.release:
		default:
			close(parker.release)
		}
	})
	return f, parker
}

// A shutdown whose handler join outlasts its context says so once, with
// the goroutine profile naming the parked handler, and still waits for it.
func TestShutdownPastItsDrainDeadlineLogsOnceAndKeepsWaiting(t *testing.T) {
	capture := captureShutdownLog(t)
	f, parker := parkingServer(t)
	conn := f.dial(t)
	frame, err := json.Marshal(ClientFrame{Type: frameTypeRPC, ID: "park", Method: "Park"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parker.parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached its handler")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.srv.Shutdown(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(capture.String(), "goroutine profile:") {
		if time.Now().After(deadline) {
			t.Fatalf("no overrun report after the drain deadline; log:\n%s", capture.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Long enough past the deadline for a second report, had there been one.
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned %v with a handler still running", err)
	case <-time.After(300 * time.Millisecond):
	}
	text := capture.String()
	if got := strings.Count(text, drainOverrun); got != 1 {
		t.Fatalf("overrun reported %d times, want once; log:\n%s", got, text)
	}
	if got := strings.Count(text, "goroutine profile:"); got != 1 {
		t.Fatalf("goroutine profile written %d times, want once; log:\n%s", got, text)
	}
	if !strings.Contains(text, "parkingApp).Park") {
		t.Fatalf("the goroutine profile does not name the parked handler; log:\n%s", text)
	}
	close(parker.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return once the handler finished")
	}
}

// A shutdown whose handlers finish inside its deadline reports nothing.
func TestShutdownWithinItsDrainDeadlineLogsNothing(t *testing.T) {
	capture := captureShutdownLog(t)
	f, parker := parkingServer(t)
	conn := f.dial(t)
	frame, err := json.Marshal(ClientFrame{Type: frameTypeRPC, ID: "park", Method: "Park"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parker.parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached its handler")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.srv.Shutdown(ctx) }()
	time.Sleep(50 * time.Millisecond)
	close(parker.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return once the handler finished")
	}
	if text := capture.String(); strings.Contains(text, drainOverrun) || strings.Contains(text, "goroutine profile:") {
		t.Fatalf("a drain inside its deadline reported an overrun; log:\n%s", text)
	}
}

package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

func startTestServer(t *testing.T, onReport func(MockInfo, Report)) *Server {
	t.Helper()
	srv, err := NewServer(ServerConfig{
		Resolve: func(reg Registration) (Assignment, error) {
			if reg.Protocol == "refuse-me" {
				return Assignment{}, errors.New("no scenario for you")
			}
			return Assignment{
				ScenarioName: "test-scenario",
				ScenarioJSON: json.RawMessage(`{"version":1}`),
				FixtureRoot:  "/fixtures",
			}, nil
		},
		OnReport: onReport,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

func clientFor(t *testing.T, srv *Server) *Client {
	t.Helper()
	t.Setenv(EnvAddr, srv.Addr())
	t.Setenv(EnvToken, srv.Token())
	c, ok := FromEnv()
	if !ok {
		t.Fatal("FromEnv: not configured")
	}
	return c
}

func TestRegisterCommandReportRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var reports []Report
	srv := startTestServer(t, func(_ MockInfo, rep Report) {
		mu.Lock()
		reports = append(reports, rep)
		mu.Unlock()
	})
	c := clientFor(t, srv)

	resp, err := c.Register(Registration{Protocol: "claude", Cwd: "/ws", PID: 42})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.MockID != "mock-1" || resp.FixtureRoot != "/fixtures" {
		t.Fatalf("unexpected registration response: %+v", resp)
	}

	// Registration itself reports.
	mu.Lock()
	if len(reports) != 1 || reports[0].Kind != ReportRegistered {
		t.Fatalf("reports after register = %+v", reports)
	}
	mu.Unlock()

	// Command round-trip through the long-poll.
	if err := srv.Command("mock-1", Command{Type: CommandAdvance, Name: "gate"}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Command, 1)
	go c.Poll(ctx, func(cmd Command) {
		got <- cmd
		cancel()
	})
	select {
	case cmd := <-got:
		if cmd.Type != CommandAdvance || cmd.Name != "gate" {
			t.Fatalf("command = %+v", cmd)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not arrive via long-poll")
	}

	// Progress report lands in OnReport.
	c.Report(Report{Kind: ReportTurnStarted, Turn: 1})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(reports)
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("report did not arrive")
		}
		time.Sleep(10 * time.Millisecond)
	}

	mocks := srv.Mocks()
	if len(mocks) != 1 || mocks[0].Scenario != "test-scenario" || mocks[0].Registration.Cwd != "/ws" {
		t.Fatalf("Mocks() = %+v", mocks)
	}
}

func TestRegisterRefusal(t *testing.T) {
	srv := startTestServer(t, nil)
	c := clientFor(t, srv)
	if _, err := c.Register(Registration{Protocol: "refuse-me"}); err == nil {
		t.Fatal("Register succeeded despite resolver refusal")
	}
}

func TestAuthRequired(t *testing.T) {
	srv := startTestServer(t, nil)
	resp, err := http.Post("http://"+srv.Addr()+"/register", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unauthenticated register = %d, want 404", resp.StatusCode)
	}
}

func TestCommandUnknownMock(t *testing.T) {
	srv := startTestServer(t, nil)
	if err := srv.Command("mock-99", Command{Type: CommandExit}); err == nil {
		t.Fatal("Command accepted unknown mock")
	}
}

// TestShutdownReleasesLongPoll: http.Server.Shutdown waits for active
// handlers, so a connected /commands poll must be released explicitly —
// otherwise every harness shutdown eats the poll window.
func TestShutdownReleasesLongPoll(t *testing.T) {
	srv := startTestServer(t, nil)
	c := clientFor(t, srv)
	if _, err := c.Register(Registration{Protocol: "claude", Cwd: "/ws", PID: 1}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	pollCtx, cancelPoll := context.WithCancel(context.Background())
	defer cancelPoll()
	polling := make(chan struct{})
	go func() {
		close(polling)
		c.Poll(pollCtx, func(Command) {})
	}()
	<-polling
	time.Sleep(50 * time.Millisecond) // let the poll request connect

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown did not release the long-poll: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Shutdown took %s; the long-poll was not released promptly", elapsed)
	}
}

// TestCommandRefusedAfterExit: once a mock reports exiting, queued
// commands would never be consumed — Command must fail loudly instead
// of letting a test wait forever on an accepted-but-dead advance.
func TestCommandRefusedAfterExit(t *testing.T) {
	srv := startTestServer(t, nil)
	c := clientFor(t, srv)
	resp, err := c.Register(Registration{Protocol: "claude", Cwd: "/ws", PID: 1})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := srv.Command(resp.MockID, Command{Type: CommandAdvance, Name: "g"}); err != nil {
		t.Fatalf("Command before exit: %v", err)
	}

	c.Report(Report{Kind: ReportExiting, Detail: "0"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mocks := srv.Mocks()
		if len(mocks) == 1 && mocks[0].Exited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mock never marked exited: %+v", mocks)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := srv.Command(resp.MockID, Command{Type: CommandAdvance, Name: "g"}); err == nil {
		t.Fatal("Command accepted for an exited mock")
	}
}

// TestClearMocksDropsRegistrationsKeepsIDSequence: harness reset clears
// the registry so stale mocks don't leak into the next test, but the id
// sequence keeps counting — a remembered pre-reset id must miss, never
// alias a new mock.
func TestClearMocksDropsRegistrationsKeepsIDSequence(t *testing.T) {
	srv := startTestServer(t, nil)
	c := clientFor(t, srv)
	if _, err := c.Register(Registration{Protocol: "claude", Cwd: "/a", PID: 1}); err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	if _, err := c.Register(Registration{Protocol: "codex", Cwd: "/b", PID: 2}); err != nil {
		t.Fatalf("Register 2: %v", err)
	}
	if got := srv.Mocks(); len(got) != 2 {
		t.Fatalf("Mocks before clear = %+v, want 2", got)
	}

	srv.ClearMocks()
	if got := srv.Mocks(); len(got) != 0 {
		t.Fatalf("Mocks after ClearMocks = %+v, want none", got)
	}
	if err := srv.Command("mock-1", Command{Type: CommandAdvance, Name: "g"}); err == nil {
		t.Fatal("Command addressed a cleared mock")
	}

	resp, err := c.Register(Registration{Protocol: "claude", Cwd: "/c", PID: 3})
	if err != nil {
		t.Fatalf("Register after clear: %v", err)
	}
	if resp.MockID != "mock-3" {
		t.Fatalf("post-clear mock id = %q, want mock-3 (sequence must not restart)", resp.MockID)
	}
}

func TestFromEnvAbsent(t *testing.T) {
	t.Setenv(EnvAddr, "")
	t.Setenv(EnvToken, "")
	os.Unsetenv(EnvAddr)
	os.Unsetenv(EnvToken)
	if _, ok := FromEnv(); ok {
		t.Fatal("FromEnv reported configured without env vars")
	}
}

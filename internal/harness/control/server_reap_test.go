package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// bareServer builds an unstarted server with a hand-placed registration,
// which is all the reaping and delivery paths need — they never touch the
// listener.
func bareServer(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer(ServerConfig{
		Resolve: func(Registration) (Assignment, error) { return Assignment{}, nil },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func (s *Server) addMock(id string, seq, pid int) *mockConn {
	conn := &mockConn{
		seq: seq,
		info: MockInfo{
			MockID:          id,
			Registration:    Registration{Protocol: "claude", Cwd: "/ws", PID: pid},
			RegisteredAt:    time.Now(),
			PendingAdvances: []string{},
		},
		commands: make(chan Command, commandQueueCap),
	}
	s.mu.Lock()
	s.mocks[id] = conn
	if seq > s.seq {
		s.seq = seq
	}
	s.mu.Unlock()
	return conn
}

// TestKilledMockReadsDeadAndRefusesCommands is the SIGKILL case: the mock
// reports nothing on its way out, so a registry that believed only
// reports answered "live" for a corpse — while Command() enqueued into a
// channel with no reader and returned success to the RPC caller.
func TestKilledMockReadsDeadAndRefusesCommands(t *testing.T) {
	srv := bareServer(t)
	srv.addMock("mock-1", 1, 4242)
	srv.alive = func(int) bool { return true }

	if err := srv.Command("mock-1", Command{Type: CommandAdvance}); err != nil {
		t.Fatalf("Command to a live mock: %v", err)
	}
	if got := srv.Mocks(); len(got) != 1 || got[0].Exited {
		t.Fatalf("live mock listed as %+v, want one row with Exited=false", got)
	}

	srv.alive = func(int) bool { return false }
	got := srv.Mocks()
	if len(got) != 1 || !got[0].Exited {
		t.Fatalf("killed mock listed as %+v, want one row with Exited=true", got)
	}
	if err := srv.Command("mock-1", Command{Type: CommandAdvance}); err == nil {
		t.Fatal("Command accepted for a mock whose process is gone")
	}
}

// TestDeadMocksAreReapedAfterGrace: every session stop outside
// HarnessReset (thread stop/delete, session restart, provider crash) used
// to leak a registration and its 64-slot command channel forever. The
// grace is what keeps the final reports readable first.
func TestDeadMocksAreReapedAfterGrace(t *testing.T) {
	t.Run("exited", func(t *testing.T) {
		srv := bareServer(t)
		srv.reapGrace = 50 * time.Millisecond
		conn := srv.addMock("mock-1", 1, 0) // pid 0: never probed
		srv.mu.Lock()
		srv.applyReportLocked(conn, Report{Kind: ReportExiting, Detail: "0"})
		srv.mu.Unlock()

		if got := srv.Mocks(); len(got) != 1 || !got[0].Exited {
			t.Fatalf("inside the grace, Mocks() = %+v, want one exited row", got)
		}
		time.Sleep(80 * time.Millisecond)
		if got := srv.Mocks(); len(got) != 0 {
			t.Fatalf("past the grace, Mocks() = %+v, want the row reaped", got)
		}
	})

	t.Run("killed", func(t *testing.T) {
		srv := bareServer(t)
		srv.reapGrace = 50 * time.Millisecond
		srv.addMock("mock-1", 1, 4242)
		srv.alive = func(int) bool { return false }

		if got := srv.Mocks(); len(got) != 1 || !got[0].Exited {
			t.Fatalf("first sight of a dead pid = %+v, want one row marked exited", got)
		}
		time.Sleep(80 * time.Millisecond)
		if got := srv.Mocks(); len(got) != 0 {
			t.Fatalf("past the grace, Mocks() = %+v, want the row reaped", got)
		}
	})
}

// TestReapedIDsAreNeverReused: a stale mock id held by a test that ran
// before the reap must MISS, never alias a mock that registered after it.
func TestReapedIDsAreNeverReused(t *testing.T) {
	srv := startTestServer(t, nil)
	srv.reapGrace = 0
	c := clientFor(t, srv)

	first, err := c.Register(Registration{Protocol: "claude", Cwd: "/a", PID: 4242})
	if err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	srv.mu.Lock()
	srv.alive = func(pid int) bool { return pid != 4242 }
	srv.mu.Unlock()
	if got := srv.Mocks(); len(got) != 0 {
		t.Fatalf("Mocks() after the reap = %+v, want none", got)
	}

	second, err := c.Register(Registration{Protocol: "claude", Cwd: "/b", PID: 4343})
	if err != nil {
		t.Fatalf("Register 2: %v", err)
	}
	if second.MockID == first.MockID {
		t.Fatalf("reaped id %q was reused", first.MockID)
	}
	if err := srv.Command(first.MockID, Command{Type: CommandAdvance}); !errors.Is(err, ErrUnknownMock) {
		t.Fatalf("Command to the reaped id = %v, want ErrUnknownMock", err)
	}
}

// failingWriter is an http.ResponseWriter whose body write always fails,
// which is what a client that vanished mid-response produces.
type failingWriter struct{ header http.Header }

func (f *failingWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }
func (f *failingWriter) WriteHeader(int)           {}

// TestFailedDeliveryRequeuesTheBatch: the long-poll DEQUEUES commands and
// then writes them. A failed write meant the batch was gone while
// HarnessMockCommand had already told its caller the command was
// accepted, and the driving test waited forever on a gate nothing would
// release. The response never left, so the mock did not receive it and a
// requeue cannot duplicate.
func TestFailedDeliveryRequeuesTheBatch(t *testing.T) {
	srv := bareServer(t)
	conn := srv.addMock("mock-1", 1, 0)

	for _, name := range []string{"first", "second"} {
		if err := srv.Command("mock-1", Command{Type: CommandAdvance, Name: name}); err != nil {
			t.Fatalf("Command %s: %v", name, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/commands?mock=mock-1", nil)
	srv.handleCommands(&failingWriter{}, req)

	srv.mu.Lock()
	front := append([]Command(nil), conn.front...)
	srv.mu.Unlock()
	if len(front) != 2 || front[0].Name != "first" || front[1].Name != "second" {
		t.Fatalf("requeued batch = %+v, want [first second] in order", front)
	}

	// A third command arriving after the failure must go BEHIND the
	// requeued pair — the mock has to see them in issue order.
	if err := srv.Command("mock-1", Command{Type: CommandAdvance, Name: "third"}); err != nil {
		t.Fatalf("Command third: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.handleCommands(rec, req)
	var delivered []Command
	if err := json.Unmarshal(rec.Body.Bytes(), &delivered); err != nil {
		t.Fatalf("decode delivered batch: %v (body %q)", err, rec.Body.String())
	}
	if len(delivered) != 3 || delivered[0].Name != "first" || delivered[1].Name != "second" || delivered[2].Name != "third" {
		t.Fatalf("delivered batch = %+v, want [first second third]", delivered)
	}
	srv.mu.Lock()
	remaining := len(conn.front)
	srv.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("front buffer after a successful delivery = %d, want empty", remaining)
	}
}

// TestFailedDeliveryReports: a lost-then-requeued batch has to be visible
// somewhere, or a flaky control channel looks like a flaky app.
func TestFailedDeliveryReports(t *testing.T) {
	var seen []Report
	srv, err := NewServer(ServerConfig{
		Resolve:  func(Registration) (Assignment, error) { return Assignment{}, nil },
		OnReport: func(_ MockInfo, rep Report) { seen = append(seen, rep) },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.addMock("mock-1", 1, 0)
	if err := srv.Command("mock-1", Command{Type: CommandAdvance}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	srv.handleCommands(&failingWriter{}, httptest.NewRequest(http.MethodGet, "/commands?mock=mock-1", nil))

	if len(seen) != 1 || seen[0].Kind != ReportFixtureError {
		t.Fatalf("reports after a failed delivery = %+v, want one %s", seen, ReportFixtureError)
	}
}

// TestPollStopsWhenTheMockIsUnknown: a mock that outlives a harness reset
// gets 404 on every poll forever after, and registration happens once at
// boot so there is no recovery. Retrying it as transient spun the process
// at 1Hz — logging a failure a second — for the rest of its life.
func TestPollStopsWhenTheMockIsUnknown(t *testing.T) {
	srv := startTestServer(t, nil)
	c := clientFor(t, srv)
	if _, err := c.Register(Registration{Protocol: "claude", Cwd: "/ws", PID: 0}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv.ClearMocks()

	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		c.Poll(ctx, func(Command) { t.Error("unknown mock delivered a command") })
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Poll kept retrying after the backend stopped recognising this mock")
	}
}

// TestMockInfoProjectsGateState: openGate and pendingAdvances are the
// only way a driving test can tell "the mock is parked on my gate" from
// "my advance did nothing" — the mock is a separate process, and it is by
// definition parked when the answer matters.
func TestMockInfoProjectsGateState(t *testing.T) {
	srv := bareServer(t)
	conn := srv.addMock("mock-1", 1, 0)

	apply := func(rep Report) {
		srv.mu.Lock()
		srv.applyReportLocked(conn, rep)
		srv.mu.Unlock()
	}
	read := func() MockInfo {
		got := srv.Mocks()
		if len(got) != 1 {
			t.Fatalf("Mocks() = %+v, want one row", got)
		}
		return got[0]
	}

	apply(Report{Kind: ReportTurnStarted, Turn: 1})
	if info := read(); info.OpenGate != "" || len(info.PendingAdvances) != 0 {
		t.Fatalf("fresh turn = %+v, want no gate and no pending advances", info)
	}

	apply(Report{Kind: ReportAdvanceBuffered, Turn: 1, Gate: "early"})
	if info := read(); info.OpenGate != "" || len(info.PendingAdvances) != 1 || info.PendingAdvances[0] != "early" {
		t.Fatalf("after a buffered advance = %+v", info)
	}

	apply(Report{Kind: ReportWaitingSignal, Turn: 1, Detail: "g1"})
	if info := read(); info.OpenGate != "g1" {
		t.Fatalf("while parked = %+v, want openGate g1", info)
	}

	apply(Report{Kind: ReportAdvanceBuffered, Turn: 1, Gate: "mismatch", OpenGate: "g1"})
	if info := read(); info.OpenGate != "g1" || len(info.PendingAdvances) != 2 {
		t.Fatalf("after a mismatched advance = %+v", info)
	}

	// A DROPPED advance is reported on the same kind but must not join the
	// backlog — nothing is holding it.
	apply(Report{Kind: ReportAdvanceBuffered, Turn: 1, Gate: "overflow", Detail: AdvanceDroppedDetail})
	if info := read(); len(info.PendingAdvances) != 2 {
		t.Fatalf("a dropped advance joined the backlog: %+v", info)
	}

	// A release by a LIVE advance clears the gate and consumes nothing:
	// neither backlog entry could have opened g1, which is why they were
	// buffered in the first place.
	apply(Report{Kind: ReportAdvanceReleased, Turn: 1, Gate: "g1"})
	if info := read(); info.OpenGate != "" || len(info.PendingAdvances) != 2 {
		t.Fatalf("after the release = %+v, want no gate and both buffered advances still parked", info)
	}

	// A release whose gate MATCHES a buffered advance is the consumption
	// path — the gate opened and took the parked command with it.
	apply(Report{Kind: ReportWaitingSignal, Turn: 1, Detail: "early"})
	apply(Report{Kind: ReportAdvanceReleased, Turn: 1, Gate: "early"})
	if info := read(); info.OpenGate != "" || len(info.PendingAdvances) != 1 || info.PendingAdvances[0] != "mismatch" {
		t.Fatalf("after the consuming release = %+v, want only the mismatched advance left", info)
	}

	// The engine drops its buffer at every turn boundary, so the
	// projection resyncs there rather than carrying a stale entry forward.
	apply(Report{Kind: ReportTurnStarted, Turn: 2})
	if info := read(); info.OpenGate != "" || len(info.PendingAdvances) != 0 {
		t.Fatalf("after the turn boundary = %+v, want the backlog cleared", info)
	}
}

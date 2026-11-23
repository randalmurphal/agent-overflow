package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// fixtureWait bounds every wait the connection fixture makes. Nothing in it
// sleeps: a wait ends as soon as the line it waits for exists.
const fixtureWait = 5 * time.Second

// connFixture is a real Conn whose app-server is an in-memory line pipe. The
// Conn's own read loop runs against the pipe, so every line a test feeds is
// classified and routed by the code a live connection runs, and every line the
// session writes is visible to the test.
type connFixture struct {
	t    *testing.T
	conn *Conn
	pipe *linePipe

	answersMu sync.Mutex
	answers   map[string]any
	open      map[int64]string
	// unclaimed lists, in write order, the open requests claimRequest has not
	// handed to the test yet.
	unclaimed []int64
	// taken counts the written lines nextWrite and nextRequest have consumed.
	taken int
}

// newConnFixture starts a Conn on an in-memory pipe. Cleanup closes the Conn,
// which ends its read loop and tears down every session on it, and fails the
// test for a request the session wrote that nobody answered.
func newConnFixture(t *testing.T) *connFixture {
	t.Helper()
	fx := &connFixture{
		t:       t,
		answers: make(map[string]any),
		open:    make(map[int64]string),
	}
	fx.pipe = newLinePipe(fx.observeWrite)
	fx.conn = newConnWithProcess(fx.pipe, nil, "codex")
	fixtures.Store(fx.conn, fx)
	go fx.conn.readLoop()
	t.Cleanup(func() {
		defer fixtures.Delete(fx.conn)
		if err := fx.conn.Close(); err != nil {
			t.Errorf("close fixture connection: %v", err)
		}
		if err := fx.pipe.err(); err != nil {
			t.Error(err)
		}
		fx.answersMu.Lock()
		defer fx.answersMu.Unlock()
		for id, method := range fx.open {
			t.Errorf("the session wrote request %d (%s) and nothing answered it", id, method)
		}
	})
	return fx
}

// fixtures maps a fixture's Conn back to the fixture, so a test that holds
// only the session can feed it and read what it wrote.
var fixtures sync.Map // *Conn -> *connFixture

// fixtureSession attaches a session to a new fixture connection; see
// connFixture.session.
func fixtureSession(t *testing.T, threadID, rootThreadID string, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	return newConnFixture(t).session(threadID, rootThreadID, onEvent)
}

// fixtureFor returns the fixture whose connection s is on.
func fixtureFor(t *testing.T, s *Session) *connFixture {
	t.Helper()
	fx, ok := fixtures.Load(s.conn)
	if !ok {
		t.Fatal("the session is not on a fixture connection")
	}
	return fx.(*connFixture)
}

// feed hands line to the read loop of the connection s is on and returns once
// it is dispatched.
func feed(t *testing.T, s *Session, line []byte) {
	t.Helper()
	fx := fixtureFor(t, s)
	if err := fx.pipe.deliver(line); err != nil {
		t.Fatal(err)
	}
}

// session attaches a Session to the fixture's connection the way Conn.open
// does: built by Conn.newSession, routed by registerSession and
// bindSessionRoot. A non-empty rootThreadID attaches it as a resume of that
// thread; an empty one as a fresh start whose thread id is not yet known. The
// session owns the connection, as one from NewSession does.
func (fx *connFixture) session(threadID, rootThreadID string, onEvent func(provider.ProviderEvent)) *Session {
	if onEvent == nil {
		onEvent = func(provider.ProviderEvent) {}
	}
	cfg := Config{ResumeThreadID: rootThreadID}
	s := fx.conn.newSession(context.Background(), threadID, cfg, onEvent, true, nil, nil)
	fresh := fx.conn.registerSession(s, rootThreadID)
	fx.conn.bindSessionRoot(s, fresh)
	return s
}

// feed hands one app-server line to the connection's read loop and returns
// once the loop has dispatched it.
func (fx *connFixture) feed(line []byte) {
	fx.t.Helper()
	if err := fx.pipe.deliver(line); err != nil {
		fx.t.Fatal(err)
	}
}

// answer scripts the result every later request with this method gets. A
// func(params json.RawMessage) any result is called per request; a string
// result is raw JSON.
func (fx *connFixture) answer(method string, result any) {
	fx.answersMu.Lock()
	fx.answers[method] = result
	fx.answersMu.Unlock()
}

// respond answers request id with result, as the app-server would. A string
// result is sent as raw JSON.
func (fx *connFixture) respond(id int64, result any) {
	fx.t.Helper()
	fx.answersMu.Lock()
	delete(fx.open, id)
	fx.answersMu.Unlock()
	if raw, ok := result.(string); ok {
		result = json.RawMessage(raw)
	}
	fx.feed(mustJSONLine(fx.t, map[string]any{"jsonrpc": "2.0", "id": id, "result": result}))
}

// claimRequest waits for the next request the connection wrote that no script
// answers and returns its id; the test owes it a response line.
func (fx *connFixture) claimRequest() int64 {
	fx.t.Helper()
	deadline := time.After(fixtureWait)
	for {
		fx.answersMu.Lock()
		if len(fx.unclaimed) > 0 {
			id := fx.unclaimed[0]
			fx.unclaimed = fx.unclaimed[1:]
			delete(fx.open, id)
			fx.answersMu.Unlock()
			return id
		}
		fx.answersMu.Unlock()
		select {
		case <-fx.pipe.wrote:
		case <-deadline:
			fx.t.Fatal("the connection wrote no request to answer")
			return 0
		}
	}
}

// answerNext answers the next unanswered request with result (a string is
// raw JSON).
func (fx *connFixture) answerNext(result any) {
	fx.t.Helper()
	fx.respond(fx.claimRequest(), result)
}

// respondError answers request id with a JSON-RPC error.
func (fx *connFixture) respondError(id int64, code int, message string) {
	fx.t.Helper()
	fx.answersMu.Lock()
	delete(fx.open, id)
	fx.answersMu.Unlock()
	fx.feed(mustJSONLine(fx.t, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}))
}

// written waits until the connection has written at least n lines and returns
// every line written so far, in write order.
func (fx *connFixture) written(n int) [][]byte {
	fx.t.Helper()
	lines, ok := fx.pipe.waitWritten(n)
	if !ok {
		fx.t.Fatalf("the connection wrote %d lines, want at least %d", len(lines), n)
	}
	return lines
}

// nextWrite returns the next line the connection wrote that the test has not
// taken, decoded.
func (fx *connFixture) nextWrite() map[string]any {
	fx.t.Helper()
	line := fx.written(fx.taken + 1)[fx.taken]
	fx.taken++
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		fx.t.Fatalf("connection wrote a line that is not JSON: %q", line)
	}
	return msg
}

// nextRequest returns the next request the connection wrote with this method,
// taking every line before it.
func (fx *connFixture) nextRequest(method string) map[string]any {
	fx.t.Helper()
	for {
		lines, ok := fx.pipe.waitWritten(fx.taken + 1)
		if !ok {
			fx.t.Fatalf("the connection wrote no %s request", method)
		}
		for fx.taken < len(lines) {
			var msg map[string]any
			if err := json.Unmarshal(lines[fx.taken], &msg); err != nil {
				fx.t.Fatalf("connection wrote a line that is not JSON: %q", lines[fx.taken])
			}
			fx.taken++
			if msg["method"] == method {
				return msg
			}
		}
	}
}

// exitAppServer ends the app-server as a crash does, and waits for the
// connection's read loop to finish tearing down its sessions.
func (fx *connFixture) exitAppServer() {
	fx.t.Helper()
	fx.pipe.exit()
	select {
	case <-fx.conn.Done():
	case <-time.After(fixtureWait):
		fx.t.Fatal("the connection's read loop did not end")
	}
}

func (fx *connFixture) observeWrite(line []byte) {
	var msg struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &msg) != nil || msg.ID == nil || msg.Method == "" {
		return
	}
	fx.answersMu.Lock()
	result, scripted := fx.answers[msg.Method]
	if !scripted {
		fx.open[*msg.ID] = msg.Method
		fx.unclaimed = append(fx.unclaimed, *msg.ID)
	}
	fx.answersMu.Unlock()
	if scripted {
		if answerFn, ok := result.(func(json.RawMessage) any); ok {
			result = answerFn(msg.Params)
		}
		if raw, ok := result.(string); ok {
			result = json.RawMessage(raw)
		}
		fx.pipe.enqueue(mustJSONLine(fx.t, map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result}))
	}
}

func mustJSONLine(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// linePipe is the connection fixture's app-server: what the Conn writes goes
// to writes, and what the test feeds is what the Conn's ReadLine returns. A
// line counts as dispatched when the read loop asks for the next one.
type linePipe struct {
	wrote   chan struct{}
	reads   chan pipeLine
	exited  chan struct{}
	onWrite func([]byte)
	// exitErr is what Err reports once the app-server exited; a test sets it
	// before exitAppServer.
	exitErr error

	mu         sync.Mutex
	writes     [][]byte
	inFlight   chan struct{}
	exitOnce   sync.Once
	overflowed bool
}

type pipeLine struct {
	data       []byte
	dispatched chan struct{}
}

const linePipeCapacity = 4096

func newLinePipe(onWrite func([]byte)) *linePipe {
	return &linePipe{
		wrote:   make(chan struct{}, 1),
		reads:   make(chan pipeLine, linePipeCapacity),
		exited:  make(chan struct{}),
		onWrite: onWrite,
	}
}

var errPipeClosed = errors.New("fixture pipe closed")

func (p *linePipe) WriteLine(data []byte) error {
	select {
	case <-p.exited:
		return errPipeClosed
	default:
	}
	line := append([]byte(nil), data...)
	p.mu.Lock()
	p.writes = append(p.writes, line)
	p.mu.Unlock()
	p.onWrite(line)
	select {
	case p.wrote <- struct{}{}:
	default:
	}
	return nil
}

// waitWritten waits until at least n lines were written and returns them all;
// ok is false when fixtureWait passed first.
func (p *linePipe) waitWritten(n int) (lines [][]byte, ok bool) {
	deadline := time.After(fixtureWait)
	for {
		p.mu.Lock()
		lines = append([][]byte(nil), p.writes...)
		p.mu.Unlock()
		if len(lines) >= n {
			return lines, true
		}
		select {
		case <-p.wrote:
		case <-deadline:
			return lines, false
		}
	}
}

func (p *linePipe) ReadLine() ([]byte, error) {
	p.mu.Lock()
	if p.inFlight != nil {
		close(p.inFlight)
		p.inFlight = nil
	}
	p.mu.Unlock()
	select {
	case line := <-p.reads:
		p.mu.Lock()
		p.inFlight = line.dispatched
		p.mu.Unlock()
		return line.data, nil
	case <-p.exited:
		return nil, io.EOF
	}
}

// deliver queues a line and waits until the read loop has dispatched it.
func (p *linePipe) deliver(data []byte) error {
	dispatched := make(chan struct{})
	select {
	case p.reads <- pipeLine{data: data, dispatched: dispatched}:
	case <-p.exited:
		return errors.New("fixture: fed a line after the app-server exited")
	}
	select {
	case <-dispatched:
		return nil
	case <-p.exited:
		return errors.New("fixture: the app-server exited before the line was dispatched")
	case <-time.After(fixtureWait):
		return errors.New("fixture: the read loop did not finish dispatching a line")
	}
}

// enqueue queues a line without waiting for its dispatch.
func (p *linePipe) enqueue(data []byte) {
	select {
	case p.reads <- pipeLine{data: data, dispatched: make(chan struct{})}:
	default:
		p.mu.Lock()
		p.overflowed = true
		p.mu.Unlock()
	}
}

// exit ends the app-server: its output reaches EOF and writes fail.
func (p *linePipe) exit() { p.exitOnce.Do(func() { close(p.exited) }) }

func (p *linePipe) Close() error {
	p.exit()
	return nil
}

func (p *linePipe) PID() int              { return 0 }
func (p *linePipe) StderrTail() string    { return "" }
func (p *linePipe) Done() <-chan struct{} { return p.exited }
func (p *linePipe) Err() error            { return p.exitErr }

func (p *linePipe) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.overflowed {
		return errors.New("fixture: more than 4096 lines queued for the read loop")
	}
	return nil
}

package aocli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/transport"
)

// The execution surface's shared skeleton: the session contract, usage and exit
// codes, and the refusal paths every command inherits. Per-command contracts
// live in exec_run_test.go and exec_automation_test.go against this same fake
// backend.

// fakeBackend answers the scoped RPC route the way the app does, recording what
// each command actually sent so the tests assert on the wire rather than on the
// CLI's own view of it.
type fakeBackend struct {
	server *httptest.Server

	mu      sync.Mutex
	calls   []transport.ClientFrame
	results map[string]json.RawMessage
	errors  map[string]*transport.FrameError
	// queues answer successive calls to one method differently, which is what a
	// long poll needs: `run watch` calls the same method until the run rests, and
	// every interesting behavior is a SEQUENCE rather than a single answer.
	queues map[string][]fakeReply
}

// fakeReply is one answer in a queue. Exactly one of its fields decides the
// answer; the zero value is "the method returned no result", which no caller
// wants and every caller notices.
type fakeReply struct {
	// result is encoded into the frame's result.
	result any
	// frameErr is a refusal the app expressed inside the envelope.
	frameErr *transport.FrameError
	// status answers with a bare HTTP status instead of a frame — 401 is the
	// route's own answer to a credential whose session ended.
	status int
	// drop hijacks the connection and closes it without answering, which is what
	// an app that went away mid-call looks like to the client.
	drop bool
	// hold sleeps for the call's own `waitMillis` before answering, so a test can
	// exercise the CLI against a server that really blocks.
	hold bool
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	backend := &fakeBackend{
		results: map[string]json.RawMessage{},
		errors:  map[string]*transport.FrameError{},
		queues:  map[string][]fakeReply{},
	}
	backend.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != transport.ScopedRPCPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var frame transport.ClientFrame
		if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		backend.mu.Lock()
		backend.calls = append(backend.calls, frame)
		queued, hasQueued := backend.nextQueuedLocked(frame.Method)
		result, hasResult := backend.results[frame.Method]
		frameErr := backend.errors[frame.Method]
		backend.mu.Unlock()

		response := transport.ServerFrame{Type: "rpc", ID: frame.ID}
		switch {
		case hasQueued:
			if queued.hold {
				time.Sleep(holdFor(frame))
			}
			switch {
			case queued.drop:
				hijackAndClose(w)
				return
			case queued.status != 0:
				http.Error(w, http.StatusText(queued.status), queued.status)
				return
			case queued.frameErr != nil:
				response.Error = queued.frameErr
			default:
				encoded, err := json.Marshal(queued.result)
				if err != nil {
					panic(err)
				}
				response.Result = encoded
			}
		case frameErr != nil:
			response.Error = frameErr
		case hasResult:
			response.Result = result
		default:
			response.Error = &transport.FrameError{
				Code: transport.ErrCodeMethodNotFound, Message: "method not registered",
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(backend.server.Close)
	return backend
}

// queue sets the answers to the next len(replies) calls of one method. Later
// calls fall through to reply/refuse, so a queue describes only the part of the
// conversation a test is about.
func (b *fakeBackend) queue(method string, replies ...fakeReply) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queues[method] = append(b.queues[method], replies...)
}

func (b *fakeBackend) nextQueuedLocked(method string) (fakeReply, bool) {
	pending := b.queues[method]
	if len(pending) == 0 {
		return fakeReply{}, false
	}
	b.queues[method] = pending[1:]
	return pending[0], true
}

// holdFor reads the call's own requested wait, bounded so a bug in the CLI's
// budget arithmetic fails the test instead of hanging it.
func holdFor(frame transport.ClientFrame) time.Duration {
	if len(frame.Params) == 0 {
		return 0
	}
	var input struct {
		WaitMillis int64 `json:"waitMillis"`
	}
	if err := json.Unmarshal(frame.Params[0], &input); err != nil || input.WaitMillis <= 0 {
		return 0
	}
	if hold := time.Duration(input.WaitMillis) * time.Millisecond; hold < 2*time.Second {
		return hold
	}
	return 2 * time.Second
}

// hijackAndClose kills the connection without a response, which is the one
// failure `run watch` has to tell apart from a refusal.
func hijackAndClose(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("test server does not support hijacking")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

func (b *fakeBackend) reply(method string, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.results[method] = encoded
}

func (b *fakeBackend) refuse(method, code, message string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.errors[method] = &transport.FrameError{Code: code, Message: message}
}

// reset drops the recorded calls, so a table may drive one method several times
// and still assert "called once, with these params" per row.
func (b *fakeBackend) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = nil
}

func (b *fakeBackend) recorded(method string) []transport.ClientFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	var matched []transport.ClientFrame
	for _, call := range b.calls {
		if call.Method == method {
			matched = append(matched, call)
		}
	}
	return matched
}

func (b *fakeBackend) env() func(string) (string, bool) {
	values := map[string]string{
		EnvEndpoint: b.server.URL,
		EnvToken:    "test-token",
		EnvThreadID: "thread-1",
	}
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// runCLI executes one command and returns its exit code plus both streams.
func runCLI(args []string, lookupEnv func(string) (string, bool)) (int, string, string) {
	var stdout, stderr strings.Builder
	code := RunWithEnv(args, lookupEnv, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func noEnv(string) (string, bool) { return "", false }

func TestExecutionCommandsRefuseToRunOutsideASession(t *testing.T) {
	for _, args := range [][]string{
		{"run", "start", "flow"},
		{"run", "list"},
		{"run", "status", "item"},
		{"notes", "get", "automation"},
		{"schedule", "flow", "--cron", "0 3 * * *"},
	} {
		code, stdout, stderr := runCLI(args, noEnv)
		if code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
		if stdout != "" {
			t.Fatalf("%v wrote to stdout on failure: %q", args, stdout)
		}
		if !strings.Contains(stderr, "not inside an Agent Overflow session") {
			t.Fatalf("%v stderr = %q", args, stderr)
		}
	}
}

func TestSessionFromEnvRejectsAHalfInjectedEnvironment(t *testing.T) {
	only := func(name, value string) func(string) (string, bool) {
		return func(lookup string) (string, bool) {
			if lookup == name {
				return value, true
			}
			return "", false
		}
	}
	if _, err := SessionFromEnv(only(EnvEndpoint, "http://127.0.0.1:1")); err == nil ||
		!strings.Contains(err.Error(), EnvToken) {
		t.Fatalf("endpoint without token: %v", err)
	}
	if _, err := SessionFromEnv(only(EnvToken, "tok")); err == nil ||
		!strings.Contains(err.Error(), EnvEndpoint) {
		t.Fatalf("token without endpoint: %v", err)
	}
	session, err := SessionFromEnv(func(name string) (string, bool) {
		return map[string]string{
			EnvEndpoint: " http://127.0.0.1:1 ", EnvToken: "tok",
			EnvRunID: "run", EnvPhaseID: "build",
		}[name], true
	})
	if err != nil {
		t.Fatalf("SessionFromEnv: %v", err)
	}
	if session.Endpoint != "http://127.0.0.1:1" {
		t.Fatalf("endpoint was not trimmed: %q", session.Endpoint)
	}
	if !session.InsidePhase() {
		t.Fatalf("a session with %s and %s is a phase session: %#v", EnvRunID, EnvPhaseID, session)
	}
}

func TestOfflineCommandsWorkWithNoSessionEnvironment(t *testing.T) {
	code, stdout, stderr := runCLI([]string{"workflow", "schema"}, noEnv)
	if code != exitOK {
		t.Fatalf("workflow schema exit = %d (%s)", code, stderr)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(stdout), &schema); err != nil {
		t.Fatalf("workflow schema did not print JSON: %v", err)
	}
	if schema["$schema"] == nil {
		t.Fatalf("workflow schema output is not a JSON Schema: %v", schema)
	}
	if code, _, _ := runCLI([]string{"workflow", "schema", "extra"}, noEnv); code != exitError {
		t.Fatalf("workflow schema with arguments exit = %d", code)
	}

	root := t.TempDir()
	code, _, stderr = runCLI([]string{"--config-root", root, "workflow", "list"}, noEnv)
	if code != exitOK {
		t.Fatalf("workflow list exit = %d (%s)", code, stderr)
	}
}

func TestUnknownCommandsAndMissingArgumentsExitTwo(t *testing.T) {
	backend := newFakeBackend(t)
	for _, args := range [][]string{
		{"nonsense"},
		{"run"},
		{"run", "nonsense"},
		{"run", "start"},
		{"run", "start", "a", "b"},
		{"run", "status"},
		{"run", "retry-unit", "item"},
		{"run", "resolve"},
		{"run", "answer", "item"},
		{"run", "list", "extra"},
		{"notes"},
		{"notes", "nonsense"},
		{"notes", "get"},
		{"schedule", "flow"},
	} {
		if code, _, _ := runCLI(args, backend.env()); code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
	}
	if calls := backend.recorded("WorkflowAgentStartRun"); len(calls) != 0 {
		t.Fatalf("a usage error still reached the backend: %#v", calls)
	}
}

func TestHelpExitsZeroAndPrintsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"help"}, {"workflow", "help"}, {"run", "help"}, {"notes", "help"},
		{"run", "start", "--help"}, {"schedule", "--help"},
		{"run", "resolve", "--help"}, {"run", "answer", "--help"},
		{"run", "watch", "--help"}, {"run", "amend", "--help"}, {"run", "guide", "--help"},
	} {
		code, stdout, stderr := runCLI(args, noEnv)
		if code != exitOK {
			t.Fatalf("%v exit = %d (%s)", args, code, stderr)
		}
		if !strings.Contains(stdout, "Usage:") {
			t.Fatalf("%v printed no usage: %q", args, stdout)
		}
	}
}
func TestGrantRefusalIsReportedWithItsMessage(t *testing.T) {
	backend := newFakeBackend(t)
	backend.refuse("WorkflowAgentSchedule", transport.ErrCodeGrantRequired,
		`this phase was not granted "schedule"; add "schedule" to the phase's grants: to allow it`)
	code, stdout, stderr := runCLI([]string{"schedule", "flow", "--cron", "0 3 * * *"}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if stdout != "" {
		t.Fatalf("a refusal printed to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, `not granted "schedule"`) {
		t.Fatalf("stderr did not carry the refusal: %q", stderr)
	}
}

func TestRevokedTokenIsReportedAsAnEndedSession(t *testing.T) {
	backend := newFakeBackend(t)
	env := func(name string) (string, bool) {
		return map[string]string{EnvEndpoint: backend.server.URL, EnvToken: "revoked"}[name], true
	}
	code, _, stderr := runCLI([]string{"run", "list"}, env)
	if code != exitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr, "no longer valid") {
		t.Fatalf("stderr = %q", stderr)
	}
}

package aocli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	backend := &fakeBackend{
		results: map[string]json.RawMessage{},
		errors:  map[string]*transport.FrameError{},
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
		result, hasResult := backend.results[frame.Method]
		frameErr := backend.errors[frame.Method]
		backend.mu.Unlock()

		response := transport.ServerFrame{Type: "rpc", ID: frame.ID}
		switch {
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

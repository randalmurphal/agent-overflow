//go:build !noremote

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

// The session surface's shared skeleton: the session contract and the refusal
// paths every session command inherits, driven through `remote` against a fake
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

func TestSessionCommandsRefuseToRunOutsideASession(t *testing.T) {
	code, stdout, stderr := runCLI([]string{"remote", "list"}, noEnv)
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if stdout != "" {
		t.Fatalf("wrote to stdout on failure: %q", stdout)
	}
	if !strings.Contains(stderr, "not inside an Agent Overflow session") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// A session command's own unknown subcommand and help, through `remote`, the
// one session command; a build without remote access refuses it whole.
func TestRemoteUnknownCommandAndHelp(t *testing.T) {
	backend := newFakeBackend(t)
	code, _, stderr := runCLI([]string{"remote", "nonsense"}, backend.env())
	if code != exitError || !strings.Contains(stderr, "unknown") {
		t.Fatalf("remote nonsense: exit = %d, stderr = %q, want an unknown-command refusal", code, stderr)
	}
	code, stdout, stderr := runCLI([]string{"remote", "help"}, noEnv)
	if code != exitOK || !strings.Contains(stdout, "Usage:") {
		t.Fatalf("remote help: exit = %d, stdout = %q (%s)", code, stdout, stderr)
	}
}

func TestGrantRefusalIsReportedWithItsMessage(t *testing.T) {
	backend := newFakeBackend(t)
	backend.refuse("AgentRemoteComputers", transport.ErrCodeGrantRequired,
		`this phase was not granted "remote-commands"; add "remote-commands" to the phase's grants: to allow it`)
	code, stdout, stderr := runCLI([]string{"remote", "list"}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if stdout != "" {
		t.Fatalf("a refusal printed to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, `not granted "remote-commands"`) {
		t.Fatalf("stderr did not carry the refusal: %q", stderr)
	}
}

func TestRevokedTokenIsReportedAsAnEndedSession(t *testing.T) {
	backend := newFakeBackend(t)
	env := func(name string) (string, bool) {
		return map[string]string{EnvEndpoint: backend.server.URL, EnvToken: "revoked"}[name], true
	}
	code, _, stderr := runCLI([]string{"remote", "list"}, env)
	if code != exitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr, "no longer valid") {
		t.Fatalf("stderr = %q", stderr)
	}
}

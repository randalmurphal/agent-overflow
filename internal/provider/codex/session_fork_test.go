package codex

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestParseThreadForkResponseRejectsMissingThreadID(t *testing.T) {
	if _, err := parseThreadForkResponse(json.RawMessage(`{"thread":{"turns":[]}}`)); err == nil {
		t.Fatal("expected missing thread.id to fail")
	}
}

func TestParseThreadForkResponseReadsThreadIDWithoutTurns(t *testing.T) {
	result, err := parseThreadForkResponse(json.RawMessage(
		`{"thread":{"id":"fork-1","turns":[{"id":"turn-a"},{"id":"turn-b"}]}}`,
	))
	if err != nil {
		t.Fatalf("parse fork response: %v", err)
	}
	if result.ThreadID != "fork-1" {
		t.Fatalf("result = %+v, want fork-1", result)
	}
}

func TestParseThreadForkResponseAcceptsExcludedTurns(t *testing.T) {
	result, err := parseThreadForkResponse(json.RawMessage(`{"thread":{"id":"fork-1","turns":[]}}`))
	if err != nil {
		t.Fatalf("parse fork response: %v", err)
	}
	if result.ThreadID != "fork-1" {
		t.Fatalf("result = %+v, want fork-1", result)
	}
}

// newForkMockSession spins up a Session against a shell mock whose
// thread/fork handler echoes the request line to stderr (so tests can
// assert the params AO sent) and answers with the provided result JSON.
func newForkMockSession(t *testing.T, forkResult, turnsListResult string) *Session {
	t.Helper()
	script := `#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/fork"'; then
        echo "$line" > "$FORK_REQUEST_FILE"
        printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" '` + forkResult + `'
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/turns/list"'; then
        echo "$line" > "$FORK_TURNS_REQUEST_FILE"
        printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" '` + turnsListResult + `'
    fi
done
`
	dir := t.TempDir()
	scriptPath := dir + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	t.Setenv("FORK_REQUEST_FILE", dir+"/fork-request.json")
	t.Setenv("FORK_TURNS_REQUEST_FILE", dir+"/fork-turns-request.json")

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  scriptPath,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func readForkRequest(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("FORK_REQUEST_FILE"))
	if err != nil {
		t.Fatalf("read captured fork request: %v", err)
	}
	return string(data)
}

func readForkTurnsRequest(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("FORK_TURNS_REQUEST_FILE"))
	if err != nil {
		t.Fatalf("read captured fork turns request: %v", err)
	}
	return string(data)
}

func TestSessionForkAtSendsLastTurnIdAndValidatesTail(t *testing.T) {
	s := newForkMockSession(
		t,
		`{"thread":{"id":"mock-fork-1","turns":[]}}`,
		`{"data":[{"id":"turn-b"}],"nextCursor":null}`,
	)
	forkedID, err := s.ForkAt(context.Background(), "turn-b")
	if err != nil {
		t.Fatalf("ForkAt: %v", err)
	}
	if forkedID != "mock-fork-1" {
		t.Fatalf("ForkAt = %q, want mock-fork-1", forkedID)
	}
	request := readForkRequest(t)
	if !strings.Contains(request, `"lastTurnId":"turn-b"`) {
		t.Fatalf("fork request missing lastTurnId param: %s", request)
	}
	if !strings.Contains(request, `"excludeTurns":true`) {
		t.Fatalf("fork request must exclude transcript turns: %s", request)
	}
	turnsRequest := readForkTurnsRequest(t)
	for _, want := range []string{
		`"threadId":"mock-fork-1"`,
		`"limit":1`,
		`"sortDirection":"desc"`,
		`"itemsView":"notLoaded"`,
	} {
		if !strings.Contains(turnsRequest, want) {
			t.Errorf("fork tail request missing %s: %s", want, turnsRequest)
		}
	}
}

func TestSessionForkAtRejectsMismatchedSurvivingTail(t *testing.T) {
	s := newForkMockSession(
		t,
		`{"thread":{"id":"mock-fork-1","turns":[]}}`,
		`{"data":[{"id":"turn-c"}],"nextCursor":null}`,
	)
	_, err := s.ForkAt(context.Background(), "turn-b")
	if err == nil || !strings.Contains(err.Error(), "expected anchor") {
		t.Fatalf("ForkAt error = %v, want surviving-tail mismatch", err)
	}
}

func TestSessionFullForkOmitsLastTurnIdAndSkipsTailValidation(t *testing.T) {
	s := newForkMockSession(t, `{"thread":{"id":"mock-fork-2","turns":[]}}`, `{}`)
	forkedID, err := s.Fork(context.Background())
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if forkedID != "mock-fork-2" {
		t.Fatalf("Fork = %q, want mock-fork-2", forkedID)
	}
	if request := readForkRequest(t); strings.Contains(request, "lastTurnId") {
		t.Fatalf("full fork must not send lastTurnId: %s", request)
	}
	if request := readForkRequest(t); !strings.Contains(request, `"excludeTurns":true`) {
		t.Fatalf("full fork must exclude transcript turns: %s", request)
	}
	if _, err := os.Stat(os.Getenv("FORK_TURNS_REQUEST_FILE")); !os.IsNotExist(err) {
		t.Fatalf("full fork must not list turns for tail validation; stat err = %v", err)
	}
}

func TestSessionForkAtRejectsEmptyTailPageWithContinuation(t *testing.T) {
	s := newForkMockSession(
		t,
		`{"thread":{"id":"mock-fork-1","turns":[]}}`,
		`{"data":[],"nextCursor":"unexpected-more"}`,
	)
	_, err := s.ForkAt(context.Background(), "turn-b")
	if err == nil || !strings.Contains(err.Error(), "empty first page with a continuation cursor") {
		t.Fatalf("ForkAt error = %v, want malformed metadata-page failure", err)
	}
}

func TestSessionForkAtRejectsTurnShellWithoutID(t *testing.T) {
	s := newForkMockSession(
		t,
		`{"thread":{"id":"mock-fork-1","turns":[]}}`,
		`{"data":[{"id":"  "}],"nextCursor":null}`,
	)
	_, err := s.ForkAt(context.Background(), "turn-b")
	if err == nil || !strings.Contains(err.Error(), "response data[0] is missing id") {
		t.Fatalf("ForkAt error = %v, want malformed turn-shell failure", err)
	}
}

func TestSessionForkWithMock(t *testing.T) {
	script := `#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/fork"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-fork-456\"}}}"
    fi
done
`
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  scriptPath,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	forkedThreadID, err := s.Fork(context.Background())
	if err != nil {
		t.Fatalf("Fork() error = %v", err)
	}
	if forkedThreadID != "mock-thread-fork-456" {
		t.Fatalf("Fork() = %q, want %q", forkedThreadID, "mock-thread-fork-456")
	}
}

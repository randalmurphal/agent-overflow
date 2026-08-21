package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// conflictWireMessage is upstream's own text, verbatim from
// codex-rs/thread-store/src/local/writer_lock.rs and pinned by
// codex-rs/app-server/tests/suite/v2/thread_resume.rs.
const conflictWireMessage = "thread 019a1c1f-0000-7000-8000-000000000000 already has an active writer"

// TestClassifyThreadWriterConflict is the classification rule on its own: the
// -32600 code is shared by every invalid request, so the message match is
// what separates "another process owns this thread" from the rest.
func TestClassifyThreadWriterConflict(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		conflict bool
	}{
		{"nil", nil, false},
		{
			"cross-process file lock",
			&RPCError{Method: "thread/resume", Code: -32600, Message: conflictWireMessage},
			true,
		},
		{
			"in-process live recorder",
			&RPCError{
				Method:  "thread/resume",
				Code:    -32600,
				Message: "thread 019a1c1f-0000-7000-8000-000000000000 already has a live local writer",
			},
			true,
		},
		{
			"wrapped",
			fmt.Errorf("codex: thread/fork: %w",
				&RPCError{Method: "thread/fork", Code: -32600, Message: conflictWireMessage}),
			true,
		},
		{
			"same message, different code",
			&RPCError{Method: "thread/resume", Code: -32602, Message: conflictWireMessage},
			false,
		},
		{
			"same code, unrelated message",
			&RPCError{Method: "thread/resume", Code: -32600, Message: "thread not found"},
			false,
		},
		{"not an RPC error", errors.New(conflictWireMessage), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			classified := classifyThreadWriterConflict(tc.err)
			if got := IsThreadWriterConflict(classified); got != tc.conflict {
				t.Fatalf("IsThreadWriterConflict = %v, want %v (err %v)", got, tc.conflict, classified)
			}
			if tc.err == nil {
				if classified != nil {
					t.Fatalf("nil in, %v out", classified)
				}
				return
			}
			if !tc.conflict {
				// Non-conflicts must pass through untouched, or a real
				// error would be replaced by a misleading message.
				if classified.Error() != tc.err.Error() {
					t.Fatalf("non-conflict rewritten: got %q, want %q", classified, tc.err)
				}
				return
			}
			if !strings.Contains(classified.Error(), "another Codex process") {
				t.Errorf("user-facing text %q does not name the cause", classified)
			}
			if strings.Contains(classified.Error(), "019a1c1f") {
				t.Errorf("user-facing text %q leaks the thread uuid", classified)
			}
			// The wire text stays reachable for the event log.
			var conflict *ThreadWriterConflictError
			if !errors.As(classified, &conflict) {
				t.Fatal("errors.As failed on a classified conflict")
			}
			if !strings.Contains(conflict.Wire, "writer") {
				t.Errorf("Wire = %q, want the app-server's own message", conflict.Wire)
			}
			// The original RPCError survives unwrapping.
			var rpcErr *RPCError
			if !errors.As(classified, &rpcErr) || rpcErr.Code != -32600 {
				t.Errorf("original RPCError not reachable through Unwrap: %v", classified)
			}
		})
	}
}

// newWriterConflictSession spins a Session against a mock app-server that
// answers thread/read, thread/resume and thread/fork with the real 0.147
// writer-ownership refusal, so the three call sites are exercised end to end
// rather than by inspection.
func newWriterConflictSession(t *testing.T) *Session {
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
    printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"%s"}}\n' "$id" "` + conflictWireMessage + `"
done
`
	dir := t.TempDir()
	scriptPath := dir + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
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
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestWriterConflictSurfacesOnEveryLockTakingCall — the lock is a property of
// the thread, not of one method, so probe, resume and fork must all report it
// as user-facing state instead of a raw wire string.
func TestWriterConflictSurfacesOnEveryLockTakingCall(t *testing.T) {
	s := newWriterConflictSession(t)
	ctx := context.Background()

	calls := map[string]func() error{
		"thread/read (probe)": func() error {
			_, err := s.Probe(ctx)
			return err
		},
		"thread/resume": func() error { return s.Resume(ctx) },
		"thread/fork": func() error {
			_, err := s.Fork(ctx)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("expected the refusal to surface as an error")
			}
			if !IsThreadWriterConflict(err) {
				t.Fatalf("not classified as a writer conflict: %v", err)
			}
			if !strings.Contains(err.Error(), ThreadWriterConflictMessage) {
				t.Fatalf("error %q does not carry the user-facing message", err)
			}
		})
	}
}

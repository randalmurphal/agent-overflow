package codex

import (
	"context"
	"os"
	"testing"

	"agent-overflow/internal/provider"
)

func TestSessionRollbackRejectsZeroOrNegativeTurns(t *testing.T) {
	s := &Session{}
	for _, n := range []int{0, -1, -5} {
		if err := s.Rollback(context.Background(), n); err == nil {
			t.Errorf("Rollback(%d) expected error, got nil", n)
		}
	}
}

func TestSessionRollbackWithMock(t *testing.T) {
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
    if echo "$line" | grep -q '"method":"thread/rollback"'; then
        # Echo the request back into the response envelope so the test can
        # inspect the params the session sent by reading stderr-captured input.
        # The response shape mirrors ThreadRollbackResponse = {thread: Thread}.
        echo "$line" >&2
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\",\"turns\":[]}}}"
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

	if err := s.Rollback(context.Background(), 2); err != nil {
		t.Fatalf("Rollback(2) error = %v", err)
	}
}

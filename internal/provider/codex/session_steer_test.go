package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// TestSessionSteerSendsTurnSteerOnActiveTurn pins the wire shape for
// turn/steer: we expect threadId, expectedTurnId, and an input array
// matching the same UserInput shape Send produces. Per the upstream
// Codex protocol (codex-rs/app-server-protocol/src/protocol/v2.rs
// TurnSteerParams), turn/steer DOES NOT take effort / approvalPolicy
// / sandboxPolicy / collaborationMode — those are turn-creation
// params for turn/start, not steer.
func TestSessionSteerSendsTurnSteerOnActiveTurn(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-active\"}}}"
    elif echo "$line" | grep -q '"method":"turn/steer"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turnId\":\"turn-active\"}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-steer\"}}}"
    fi
done
`, capturePath)
	scriptPath := filepath.Join(t.TempDir(), "codex")
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

	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Steer(context.Background(), "second take", provider.SendOptions{}); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	_ = s.Close()

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}

	var turnSteer map[string]any
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, `"method":"turn/steer"`) {
			continue
		}
		if err := json.Unmarshal([]byte(line), &turnSteer); err != nil {
			t.Fatalf("unmarshal turn/steer: %v", err)
		}
		break
	}
	if turnSteer == nil {
		t.Fatalf("captured no turn/steer request: %s", string(captured))
	}
	params := turnSteer["params"].(map[string]any)
	if params["threadId"] != "mock-thread-steer" {
		t.Fatalf("threadId = %v, want mock-thread-steer", params["threadId"])
	}
	if params["expectedTurnId"] != "turn-active" {
		t.Fatalf("expectedTurnId = %v, want turn-active", params["expectedTurnId"])
	}
	if _, ok := params["effort"]; ok {
		t.Fatal("turn/steer params unexpectedly carried effort — reserved for turn/start")
	}
	if _, ok := params["approvalPolicy"]; ok {
		t.Fatal("turn/steer params unexpectedly carried approvalPolicy — reserved for turn/start")
	}
	if _, ok := params["sandboxPolicy"]; ok {
		t.Fatal("turn/steer params unexpectedly carried sandboxPolicy — reserved for turn/start")
	}
	if _, ok := params["collaborationMode"]; ok {
		t.Fatal("turn/steer params unexpectedly carried collaborationMode — reserved for turn/start")
	}
	input := params["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1", len(input))
	}
	textInput := input[0].(map[string]any)
	if textInput["type"] != "text" || textInput["text"] != "second take" {
		t.Fatalf("input[0] = %v, want text=\"second take\"", textInput)
	}
}

// TestSessionSteerWithoutActiveTurnReturnsErrNoActiveTurn pins the
// "no active turn yet" branch so the app layer can errors.Is-check
// against the sentinel and fall back to Send.
func TestSessionSteerWithoutActiveTurnReturnsErrNoActiveTurn(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-noturn\"}}}"
done
`
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

	err = s.Steer(context.Background(), "anything", provider.SendOptions{})
	if !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("Steer without active turn err = %v, want ErrNoActiveTurn", err)
	}
}

// TestSessionSteerEmptyContentRejected pins the input-required guard
// shared with Send: a zero-length steer with no attachments fails fast
// rather than hitting the wire with an empty input vec.
func TestSessionSteerEmptyContentRejected(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-empty\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-empty\"}}}"
    fi
done
`, capturePath)
	scriptPath := filepath.Join(t.TempDir(), "codex")
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

	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Steer(context.Background(), "   ", provider.SendOptions{}); err == nil {
		t.Fatal("Steer with whitespace-only content err = nil, want input-required failure")
	}
}

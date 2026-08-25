package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

const testThread = "thread-test"

// -- Session lifecycle tests using cat subprocess --

// newTestCodexSession creates a Session backed by `cat`, which echoes
// stdin to stdout. readLoop is started, enabling end-to-end testing of
// dispatch, notifications, responses, and server requests.
func newTestCodexSession(t *testing.T) (*Session, <-chan provider.ProviderEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})
	return s, eventCh
}

func codexWaitEvent(t *testing.T, ch <-chan provider.ProviderEvent) provider.ProviderEvent {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for event")
		return provider.ProviderEvent{}
	}
}

func rawInterAgentSubagentNotificationLine(t *testing.T, notification map[string]any) []byte {
	t.Helper()
	return rawInterAgentSubagentNotificationLineForThread(t, "parent-thread", notification)
}

func rawInterAgentSubagentNotificationLineForThread(t *testing.T, threadID string, notification map[string]any) []byte {
	t.Helper()
	return rawInterAgentSubagentNotificationLineForThreadAndPhase(t, threadID, "commentary", notification)
}

func rawInterAgentSubagentNotificationLineForThreadAndPhase(t *testing.T, threadID string, phase string, notification map[string]any) []byte {
	t.Helper()
	return rawInterAgentMessageLineForThreadAndPhase(t, threadID, phase, subagentNotificationTag(t, notification))
}

func subagentNotificationTag(t *testing.T, notification map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("marshal subagent notification: %v", err)
	}
	return "<subagent_notification>" + string(encoded) + "</subagent_notification>"
}

func rawUserSubagentNotificationLineForThread(t *testing.T, threadID string, notification map[string]any) []byte {
	t.Helper()
	return rawUserMessageLineForThread(t, threadID, subagentNotificationTag(t, notification))
}

func rolloutUserSubagentNotificationLine(t *testing.T, agentPath string, status any) []byte {
	t.Helper()
	item := map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{
				"type": "input_text",
				"text": subagentNotificationTag(t, map[string]any{
					"agent_path": agentPath,
					"status":     status,
				}),
			},
		},
	}
	line, err := json.Marshal(map[string]any{
		"timestamp": "2026-06-16T05:55:55.622Z",
		"type":      "response_item",
		"payload":   item,
	})
	if err != nil {
		t.Fatalf("marshal rollout response item: %v", err)
	}
	return line
}

func rawUserMessageLineForThread(t *testing.T, threadID string, text string) []byte {
	t.Helper()
	return rawUserMessageLineForThreadAndBlockType(t, threadID, "input_text", text)
}

func rawUserMessageLineForThreadAndBlockType(t *testing.T, threadID string, blockType string, text string) []byte {
	t.Helper()
	item := map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{
				"type": blockType,
				"text": text,
			},
		},
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "rawResponseItem/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   "turn-2",
			"item":     item,
		},
	})
	if err != nil {
		t.Fatalf("marshal raw user message line: %v", err)
	}
	return line
}

func rawInterAgentMessageLine(t *testing.T, content string) []byte {
	t.Helper()
	return rawInterAgentMessageLineForThread(t, "parent-thread", content)
}

func rawInterAgentMessageLineForThread(t *testing.T, threadID string, content string) []byte {
	t.Helper()
	return rawInterAgentMessageLineForThreadAndPhase(t, threadID, "commentary", content)
}

func rawInterAgentMessageLineForThreadAndPhase(t *testing.T, threadID string, phase string, content string) []byte {
	t.Helper()
	return rawInterAgentMessageLineForThreadAndPhaseAndBlockType(t, threadID, phase, content, "output_text")
}

func rawInterAgentMessageLineForThreadAndPhaseAndBlockType(t *testing.T, threadID string, phase string, content string, blockType string) []byte {
	t.Helper()
	communication := map[string]any{
		"author":           "/root/researcher",
		"recipient":        "/root",
		"other_recipients": []string{},
		"content":          content,
		"trigger_turn":     false,
	}
	encoded, err := json.Marshal(communication)
	if err != nil {
		t.Fatalf("marshal inter-agent communication: %v", err)
	}
	return rawMessageLineForThreadAndPhaseAndBlockType(t, threadID, phase, blockType, string(encoded))
}

func rawMessageLine(t *testing.T, text string) []byte {
	t.Helper()
	return rawMessageLineForThread(t, "parent-thread", text)
}

func rawMessageLineForThread(t *testing.T, threadID string, text string) []byte {
	t.Helper()
	return rawMessageLineForThreadAndPhase(t, threadID, "commentary", text)
}

func rawMessageLineForThreadAndPhase(t *testing.T, threadID string, phase string, text string) []byte {
	t.Helper()
	return rawMessageLineForThreadAndPhaseAndBlockType(t, threadID, phase, "output_text", text)
}

func rawMessageLineForThreadAndPhaseAndBlockType(t *testing.T, threadID string, phase string, blockType string, text string) []byte {
	t.Helper()
	item := map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]string{
			{
				"type": blockType,
				"text": text,
			},
		},
	}
	if phase != "" {
		item["phase"] = phase
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "rawResponseItem/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   "turn-2",
			"item":     item,
		},
	})
	if err != nil {
		t.Fatalf("marshal raw message line: %v", err)
	}
	return line
}

// codexReviewerEchoScript is a fake app-server that answers thread/start and
// thread/resume with a caller-chosen `approvalsReviewer` (or none at all, for
// the pre-0.115 silent-drop simulation) and logs every inbound line.
func codexReviewerEchoScript(t *testing.T, capturePath, threadResult string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-reviewer\"}}}"
    elif echo "$line" | grep -qE '"method":"thread/(start|resume)"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
    fi
done
`, capturePath, threadResult)
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return scriptPath
}

func codexCapturedRequest(t *testing.T, capturePath, method string) map[string]any {
	t.Helper()
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	needle := `"method":"` + method + `"`
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("unmarshal %s: %v", method, err)
		}
		params, ok := req["params"].(map[string]any)
		if !ok {
			t.Fatalf("%s carried no params object: %s", method, line)
		}
		return params
	}
	t.Fatalf("captured no %s request: %s", method, string(captured))
	return nil
}

package codex

import (
	"bytes"
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

func TestSendOutputSchemaIsPerTurn(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	script := fmt.Sprintf(`#!/bin/bash
turn=0
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        turn=$((turn + 1))
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-$turn\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"provider-thread\"}}}"
    fi
done
`, capturePath)
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	session, err := NewSession(context.Background(), testThread, Config{
		Binary:  scriptPath,
		Model:   "test-model",
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	schemas := []json.RawMessage{
		json.RawMessage(`{"type":"object","required":["first"]}`),
		json.RawMessage(`{"type":"object","required":["second"]}`),
		nil,
	}
	for i, schema := range schemas {
		if err := session.Send(context.Background(), fmt.Sprintf("turn %d", i+1), provider.SendOptions{OutputSchema: schema}); err != nil {
			t.Fatalf("Send turn %d: %v", i+1, err)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var turnStarts []map[string]json.RawMessage
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, `"method":"turn/start"`) {
			continue
		}
		var request struct {
			Params map[string]json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatalf("unmarshal turn/start: %v", err)
		}
		turnStarts = append(turnStarts, request.Params)
	}
	if len(turnStarts) != 3 {
		t.Fatalf("turn/start requests = %d, want 3", len(turnStarts))
	}
	for i := 0; i < 2; i++ {
		if !bytes.Equal(turnStarts[i]["outputSchema"], schemas[i]) {
			t.Fatalf("turn %d outputSchema = %s, want %s", i+1, turnStarts[i]["outputSchema"], schemas[i])
		}
	}
	if _, present := turnStarts[2]["outputSchema"]; present {
		t.Fatalf("free-form turn unexpectedly included outputSchema: %s", turnStarts[2]["outputSchema"])
	}
}

func TestSendBindsOutputSchemaAcrossTurnStartOrdering(t *testing.T) {
	for _, notificationFirst := range []bool{false, true} {
		name := "response before notification"
		if notificationFirst {
			name = "notification before response"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			proc, err := provider.Spawn(ctx, provider.SpawnConfig{
				Binary: "sh",
				Args:   []string{"-c", "cat > /dev/null; sleep 60"},
			})
			if err != nil {
				t.Fatalf("spawn: %v", err)
			}
			t.Cleanup(func() { _ = proc.Close() })

			eventCh := make(chan provider.ProviderEvent, 16)
			session := &Session{
				proc:     proc,
				threadID: testThread,
				pending:  make(map[int64]chan json.RawMessage),
				onEvent: func(event provider.ProviderEvent) {
					eventCh <- event
				},
				cancel: cancel,
			}
			session.setRootThreadID("provider-thread")
			go session.readLoop()

			sendDone := make(chan error, 1)
			go func() {
				sendDone <- session.Send(context.Background(), "produce output", provider.SendOptions{
					OutputSchema: json.RawMessage(`{"type":"object"}`),
				})
			}()

			var responseCh chan json.RawMessage
			var requestID int64
			deadline := time.Now().Add(3 * time.Second)
			for responseCh == nil && time.Now().Before(deadline) {
				session.mu.Lock()
				for id, ch := range session.pending {
					requestID = id
					responseCh = ch
					break
				}
				session.mu.Unlock()
				if responseCh == nil {
					time.Sleep(2 * time.Millisecond)
				}
			}
			if responseCh == nil {
				t.Fatal("Send never registered a pending turn/start request")
			}

			turnStarted := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"provider-thread","turn":{"id":"turn-1"}}}`)
			if notificationFirst {
				session.dispatchLine(turnStarted)
			}
			responseCh <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"turn":{"id":"turn-1"}}}`, requestID))
			select {
			case err := <-sendDone:
				if err != nil {
					t.Fatalf("Send: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Send did not return")
			}
			if !notificationFirst {
				session.dispatchLine(turnStarted)
			}

			session.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"provider-thread","turnId":"turn-1","item":{"id":"message-1","type":"agentMessage","text":"{\"status\":\"done\"}"}}}`))
			session.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"provider-thread","turn":{"id":"turn-1","status":"completed"}}}`))

			deadlineCh := time.After(3 * time.Second)
			for {
				select {
				case event := <-eventCh:
					if event.Kind != provider.EventTurnComplete {
						continue
					}
					want := json.RawMessage(`{"status":"done"}`)
					if !bytes.Equal(event.StructuredOutput, want) {
						t.Fatalf("StructuredOutput = %s, want %s", event.StructuredOutput, want)
					}
					return
				case <-deadlineCh:
					t.Fatal("turn completion event was not emitted")
				}
			}
		})
	}
}

func TestStructuredOutputFromFinalAgentMessage(t *testing.T) {
	tests := []struct {
		name        string
		schemaed    bool
		messages    []string
		wantPayload json.RawMessage
	}{
		{
			name:        "schemaed final payload",
			schemaed:    true,
			messages:    []string{`{"draft":true}`, `{"status":"done","outputs":{"answer":42}}`},
			wantPayload: json.RawMessage(`{"status":"done","outputs":{"answer":42}}`),
		},
		{
			name:     "schemaed absent payload",
			schemaed: true,
		},
		{
			name:     "schemaed unparseable final payload",
			schemaed: true,
			messages: []string{`{"draft":true}`, `not JSON`},
		},
		{
			name:     "unschemaed JSON message",
			messages: []string{`{"status":"done"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var complete provider.ProviderEvent
			session := &Session{
				threadID: testThread,
				onEvent: func(event provider.ProviderEvent) {
					if event.Kind == provider.EventTurnComplete {
						complete = event
					}
				},
			}
			session.setPendingTurnSchema(tt.schemaed)
			session.dispatchRoutableNotification("turn/started", json.RawMessage(`{"threadId":"provider-thread","turn":{"id":"turn-1"}}`), "provider-thread")
			for i, message := range tt.messages {
				params, err := json.Marshal(map[string]any{
					"threadId": "provider-thread",
					"turnId":   "turn-1",
					"item": map[string]any{
						"id":   fmt.Sprintf("message-%d", i+1),
						"type": "agentMessage",
						"text": message,
					},
				})
				if err != nil {
					t.Fatalf("marshal item/completed fixture: %v", err)
				}
				session.dispatchRoutableNotification("item/completed", params, "provider-thread")
			}
			session.dispatchRoutableNotification("turn/completed", json.RawMessage(`{"threadId":"provider-thread","turn":{"id":"turn-1","status":"completed"}}`), "provider-thread")

			if complete.Kind != provider.EventTurnComplete {
				t.Fatal("turn completion event was not emitted")
			}
			if !bytes.Equal(complete.StructuredOutput, tt.wantPayload) {
				t.Fatalf("StructuredOutput = %s, want %s", complete.StructuredOutput, tt.wantPayload)
			}
		})
	}
}

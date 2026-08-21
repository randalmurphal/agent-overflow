package codex

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// TestAgentMessageDeliveryRidesOnTheBlockMeta — item 6. `delivery: "async"`
// is the only signal separating a mid-turn interjection from the turn's final
// answer (`phase` says finalAnswer for both), so it has to reach the frontend
// as a typed field rather than being dropped in the item decode.
func TestAgentMessageDeliveryRidesOnTheBlockMeta(t *testing.T) {
	cases := []struct {
		name         string
		item         string
		wantDelivery string
		wantPresent  bool
	}{
		{
			"async interjection",
			`{"id":"msg-1","type":"agentMessage","status":"completed","phase":"finalAnswer","delivery":"async","text":"still working on it"}`,
			AgentMessageDeliveryAsync, true,
		},
		{
			"ordinary final answer",
			`{"id":"msg-1","type":"agentMessage","status":"completed","phase":"finalAnswer","text":"done"}`,
			"", false,
		},
		{
			// A pre-0.149 app-server omits the key entirely; absence must
			// stay absence, never a default that a reader could invert into
			// "this one IS the final answer".
			"pre-0.149 message",
			`{"id":"msg-1","type":"agentMessage","status":"completed","text":"done"}`,
			"", false,
		},
		{
			// An unknown future variant is carried verbatim rather than
			// coerced — the enum is upstream's to extend.
			"unknown future variant",
			`{"id":"msg-1","type":"agentMessage","status":"completed","delivery":"someFutureMode","text":"hi"}`,
			"someFutureMode", true,
		},
		{
			"assistantMessage alias",
			`{"id":"msg-1","type":"assistantMessage","status":"completed","delivery":"async","text":"hi"}`,
			AgentMessageDeliveryAsync, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, events := externalTurnTestSession(t, "codex-thread-1")
			line := `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"codex-thread-1","turnId":"turn-1","item":` + tc.item + `}}`
			s.dispatchLine([]byte(line))

			var block *provider.ProviderEvent
			for i := range *events {
				if (*events)[i].Kind == provider.EventContentBlockStop {
					block = &(*events)[i]
				}
			}
			if block == nil {
				t.Fatalf("no content-block-stop event in %+v", *events)
			}
			got, present := metaValue(t, block.Meta, "delivery")
			if present != tc.wantPresent {
				t.Fatalf("delivery present = %v, want %v (meta %s)", present, tc.wantPresent, block.Meta)
			}
			if present && got != tc.wantDelivery {
				t.Errorf("delivery = %v, want %q", got, tc.wantDelivery)
			}
			// The pre-existing key must survive in both branches.
			if blockType, ok := metaValue(t, block.Meta, "blockType"); !ok || blockType != "text" {
				t.Errorf("blockType = %v (present=%v), want text", blockType, ok)
			}
		})
	}
}

// TestParseUserInputIsBlocking — item 7's second half. Upstream's custom
// Deserialize defaults the field to TRUE, so an absent or malformed value must
// read as blocking; only an explicit false is non-blocking.
func TestParseUserInputIsBlocking(t *testing.T) {
	cases := []struct {
		params string
		want   bool
	}{
		{`{"isBlocking":false}`, false},
		{`{"isBlocking":true}`, true},
		{`{}`, true},                    // pre-0.147 app-server
		{`{"isBlocking":null}`, true},   // explicit null takes the default
		{`{"isBlocking":"nope"}`, true}, // wrong type: fail toward blocking
		{`not json`, true},              // unparseable: fail toward blocking
	}
	for _, tc := range cases {
		if got := parseUserInputIsBlocking(json.RawMessage(tc.params)); got != tc.want {
			t.Errorf("parseUserInputIsBlocking(%s) = %v, want %v", tc.params, got, tc.want)
		}
	}
}

// TestNonBlockingUserInputIsLoggedNotAdapted pins the deliberate no-op: AO
// still renders the prompt as blocking, and says so once in the log.
func TestNonBlockingUserInputIsLoggedNotAdapted(t *testing.T) {
	s, events := externalTurnTestSession(t, "codex-thread-1")
	// No writer is attached, so the JSON-RPC response write fails harmlessly;
	// the events and the log line are what this asserts.
	line := []byte(`{"jsonrpc":"2.0","id":7,"method":"item/tool/requestUserInput","params":{"threadId":"codex-thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":false,"questions":[{"id":"q1","prompt":"Which branch?"}]}}`)

	logged := captureLog(t, func() { s.dispatchLine(line) })

	if !strings.Contains(logged, "isBlocking=false") {
		t.Errorf("expected the non-blocking case to be logged, got %q", logged)
	}
	var sawRequest bool
	for _, evt := range *events {
		if evt.Kind == provider.EventUserInputRequest {
			sawRequest = true
		}
	}
	if !sawRequest {
		t.Errorf("expected the prompt to be surfaced unchanged, got %+v", *events)
	}
}

// TestThreadProjectIDIsIgnoredWithoutError — item 7's first half. Codex 0.149
// added `Thread.projectId`; AO decodes the same responses with narrow structs
// and must simply not see it. A decode that tightened into deny-unknown-fields
// would break every start and every probe at once, so both responses are
// exercised with the field present.
func TestThreadProjectIDIsIgnoredWithoutError(t *testing.T) {
	t.Run("thread/read (probe)", func(t *testing.T) {
		resp := json.RawMessage(`{"thread":{"id":"th-1","projectId":"proj-1","status":{"type":"idle"}}}`)
		result, err := decodeProbeResponse(resp)
		if err != nil {
			t.Fatalf("decodeProbeResponse with projectId: %v", err)
		}
		if result.Status != "idle" {
			t.Errorf("status = %q, want idle", result.Status)
		}
	})

	t.Run("thread/fork", func(t *testing.T) {
		result, err := parseThreadForkResponse(json.RawMessage(
			`{"thread":{"id":"fork-1","projectId":"proj-1","turns":[{"id":"turn-a"}]}}`,
		))
		if err != nil {
			t.Fatalf("parseThreadForkResponse with projectId: %v", err)
		}
		if result.ThreadID != "fork-1" || result.LastTurnID != "turn-a" {
			t.Errorf("result = %+v, want fork-1 ending at turn-a", result)
		}
	})

	t.Run("thread/start handshake", func(t *testing.T) {
		script := `#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"userAgent\":\"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) codex_cli_rs\"}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\",\"projectId\":\"proj-1\",\"turns\":[]}}}"
        continue
    fi
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
			t.Fatalf("NewSession with projectId on the thread: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if got := s.rootThreadID(); got != "mock-thread-123" {
			t.Errorf("rootThreadID = %q, want mock-thread-123", got)
		}
		// Same handshake proves the version read that drives item 3.
		if got := s.AppServerVersion(); got != "0.149.0" {
			t.Errorf("AppServerVersion = %q, want 0.149.0", got)
		}
	})
}

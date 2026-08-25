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

// turnIdentityScript is a fake app-server that records every frame it is sent
// and answers `turn/start` / `turn/steer` with a turn id, so a test can read
// back exactly what went on the wire.
//
// `steerError`, when non-empty, is a raw JSON object used as the `turn/steer`
// JSON-RPC error member instead of a result — which is how the three refusal
// shapes are exercised without a real codex.
func turnIdentityScript(t *testing.T, capturePath, steerError string) string {
	t.Helper()
	steerBranch := fmt.Sprintf(
		`        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turnId\":\"turn-active\"}}"`)
	if steerError != "" {
		steerBranch = fmt.Sprintf(
			`        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"error\":%s}"`, bashJSON(steerError))
	}
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
%s
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"codex-thread-ident\"}}}"
    fi
done
`, capturePath, steerBranch)
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return path
}

// capturedFrameParams returns the params object of the first frame for method.
func capturedFrameParams(t *testing.T, capturePath, method string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `"method":"`+method+`"`) {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("decode %s frame %q: %v", method, line, err)
		}
		params, ok := frame["params"].(map[string]any)
		if !ok {
			t.Fatalf("%s frame has no params object: %+v", method, frame)
		}
		return params
	}
	t.Fatalf("no %s frame in:\n%s", method, raw)
	return nil
}

// TestTurnVerbsStampTheClientUserMessageID is the correlation contract: both
// outbound verbs carry the caller's own id, so the `userMessage` echo's
// `clientId` names the row that produced it without anything having to rely on
// ordering. Upstream types the field `Option<String>` on TurnStartParams AND
// TurnSteerParams alike (app-server-protocol/src/protocol/v2/turn.rs), and it
// has existed since 0.136 — below AO's 0.143 provider floor — so it is sent
// unconditionally with no version gate.
func TestTurnVerbsStampTheClientUserMessageID(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "codex-stdin.log")
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  turnIdentityScript(t, capture, ""),
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Send(context.Background(), "hello", provider.SendOptions{
		ClientUserMessageID: "user:4",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Steer(context.Background(), "second take", provider.SendOptions{
		ClientUserMessageID: "user:4:flush:1",
	}); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	start := capturedFrameParams(t, capture, "turn/start")
	if start["clientUserMessageId"] != "user:4" {
		t.Errorf("turn/start clientUserMessageId = %v, want user:4", start["clientUserMessageId"])
	}
	steer := capturedFrameParams(t, capture, "turn/steer")
	if steer["clientUserMessageId"] != "user:4:flush:1" {
		t.Errorf("turn/steer clientUserMessageId = %v, want user:4:flush:1", steer["clientUserMessageId"])
	}
	// The steer's precondition rides with it. Upstream refuses an empty
	// `expectedTurnId` outright, so this is the only shape that can succeed.
	if steer["expectedTurnId"] != "turn-active" {
		t.Errorf("turn/steer expectedTurnId = %v, want the tracked active turn", steer["expectedTurnId"])
	}
}

// TestTurnVerbsOmitAnAbsentClientUserMessageID — upstream mints its own uuid
// for a producer that supplies none, so an explicit empty string would be a
// value no echo could ever match rather than an absence.
func TestTurnVerbsOmitAnAbsentClientUserMessageID(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "codex-stdin.log")
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  turnIdentityScript(t, capture, ""),
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
	if err := s.Steer(context.Background(), "more", provider.SendOptions{
		ClientUserMessageID: "   ",
	}); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	if _, ok := capturedFrameParams(t, capture, "turn/start")["clientUserMessageId"]; ok {
		t.Error("turn/start sent an empty clientUserMessageId instead of omitting the key")
	}
	if _, ok := capturedFrameParams(t, capture, "turn/steer")["clientUserMessageId"]; ok {
		t.Error("turn/steer sent a blank clientUserMessageId instead of omitting the key")
	}
}

// TestSteerRejectionsAreClassified covers all three refusals upstream can
// answer `turn/steer` with. They arrive as the SAME JSON-RPC code (-32600
// invalid_request), so the code says nothing and the discrimination has to come
// from the payload — and getting it wrong picks the wrong recovery:
//
//   - the two precondition refusals mean the addressed turn is not the one
//     running, so the message should open a turn of its own;
//   - a non-steerable turn means one IS running and starting a second would
//     interleave the user's message with a review or a compaction.
func TestSteerRejectionsAreClassified(t *testing.T) {
	notSteerableData := `{"message":"cannot steer a review turn",` +
		`"codexErrorInfo":{"activeTurnNotSteerable":{"turnKind":"review"}},` +
		`"additionalDetails":null}`

	for _, tc := range []struct {
		name            string
		wire            *RPCError
		wantNoActive    bool
		wantNotSteerabl bool
	}{
		{
			name:         "no active turn",
			wire:         &RPCError{Method: "turn/steer", Code: -32600, Message: "no active turn to steer"},
			wantNoActive: true,
		},
		{
			name: "expected turn mismatch",
			wire: &RPCError{Method: "turn/steer", Code: -32600,
				Message: "expected active turn id `turn-a` but found `turn-b`"},
			wantNoActive: true,
		},
		{
			name: "review turn is not steerable",
			wire: &RPCError{Method: "turn/steer", Code: -32600,
				Message: "cannot steer a review turn", Data: json.RawMessage(notSteerableData)},
			wantNotSteerabl: true,
		},
		{
			name: "compact turn is not steerable",
			wire: &RPCError{Method: "turn/steer", Code: -32600,
				Message: "cannot steer a compact turn",
				Data: json.RawMessage(`{"message":"cannot steer a compact turn",` +
					`"codexErrorInfo":{"activeTurnNotSteerable":{"turnKind":"compact"}}}`)},
			wantNotSteerabl: true,
		},
		{
			// Every other -32600 keeps its own shape. Reading an unrelated
			// refusal as a race would re-dispatch the message as a fresh turn
			// on a thread that just refused it for a different reason.
			name: "an unrelated invalid request is left alone",
			wire: &RPCError{Method: "turn/steer", Code: -32600, Message: "input must not be empty"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySteerRejection(tc.wire)
			if errors.Is(got, ErrNoActiveTurn) != tc.wantNoActive {
				t.Errorf("errors.Is(%v, ErrNoActiveTurn) = %v, want %v",
					got, errors.Is(got, ErrNoActiveTurn), tc.wantNoActive)
			}
			if IsTurnNotSteerable(got) != tc.wantNotSteerabl {
				t.Errorf("IsTurnNotSteerable(%v) = %v, want %v",
					got, IsTurnNotSteerable(got), tc.wantNotSteerabl)
			}
			// The two are mutually exclusive by construction: the app layer
			// branches on IsNoActiveTurnRace first, so a non-steerable turn
			// answering true there would open the second turn this refusal
			// exists to prevent.
			if tc.wantNotSteerabl && IsNoActiveTurnRace(got) {
				t.Error("a non-steerable turn read as a no-active-turn race; the fallback would start a second turn")
			}
			if tc.wantNoActive && !IsNoActiveTurnRace(got) {
				t.Error("a precondition refusal did not read as a no-active-turn race")
			}
			// Upstream's own sentence survives the classification, so a log
			// line still says which of the two produced it.
			if !strings.Contains(got.Error(), tc.wire.Message) {
				t.Errorf("classified error %q dropped the wire message %q", got, tc.wire.Message)
			}
		})
	}
}

// TestSteerNotSteerableReachesTheCallerThroughTheWire is the same rule end to
// end, and specifically pins that the refusal's `error.data` survives
// sendRequest: the typed `codexErrorInfo` is the only thing separating this
// state from the two precondition races, which arrive with the same code and
// no data at all.
func TestSteerNotSteerableReachesTheCallerThroughTheWire(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "codex-stdin.log")
	binary := turnIdentityScript(t, capture,
		`{"code":-32600,"message":"cannot steer a review turn",`+
			`"data":{"message":"cannot steer a review turn",`+
			`"codexErrorInfo":{"activeTurnNotSteerable":{"turnKind":"review"}}}}`)
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  binary,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Send(context.Background(), "review this", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	steerErr := s.Steer(context.Background(), "actually, wait", provider.SendOptions{})
	if !IsTurnNotSteerable(steerErr) {
		t.Fatalf("Steer err = %v, want ErrTurnNotSteerable", steerErr)
	}
	if IsNoActiveTurnRace(steerErr) {
		t.Error("the non-steerable refusal also read as a no-active-turn race")
	}
}

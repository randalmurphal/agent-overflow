package app

import (
	"agent-overflow/internal/provider/providertest"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/triage"
)

// writeCodexTurnStartCaptureBinary is a fake codex app-server whose active
// turn has just ended: turn/steer answers NoActiveTurn and turn/start opens a
// turn. It appends every line AO writes to capturePath.
func writeCodexTurnStartCaptureBinary(t *testing.T, threadID, capturePath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    printf '%%s\n' "$line" >> '%s'
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    case "$line" in *'"method":"initialize"'*) echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":`+providertest.CodexInitializeResultQuoted+`}"; continue;; esac
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/steer"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32000,"message":"NoActiveTurn"}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"turn":{"id":"start-turn"}}}\n' "$id"
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, capturePath, threadID)
	path := filepath.Join(t.TempDir(), "codex-turn-start-capture.sh")
	mockexec.Write(t, path, script)
	return path
}

// turnStartsCarrying counts the turn/start requests AO wrote that carry text.
func turnStartsCarrying(t *testing.T, capturePath, text string) int {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read capture: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, `"method":"turn/start"`) && strings.Contains(line, text) {
			count++
		}
	}
	return count
}

// A queued Codex message whose steer lost the race with the turn's end goes
// out as a fresh turn/start. Codex holds a turn/start's input as that turn's
// own; only steered input waiting in a running turn is dropped at
// turn/interrupt. A Stop that lands before the message's echo stops the turn
// the message opened and must not send the message again.
func TestCodexStopBeforeTheEchoOfAMessageSentAsAFreshTurnDoesNotResendIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.configureTriageQueueCallbacks()

	thread := testThread("codex-stop-turn-start")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "turn-2", ThreadID: thread.ID, TurnIndex: 2, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	capture := filepath.Join(t.TempDir(), "codex-stdin.jsonl")
	sess := installCodexTestSessionWithBinary(t, app, thread, writeCodexTurnStartCaptureBinary(t, thread.ID+"-codex", capture))
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Token: "stop-token", Codex: sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	dispatchThroughWorker(app, thread.ID, []triage.QueuedFlushItem{{ID: "queue:fresh-turn", Message: "sent as a fresh turn"}})
	flushed := app.triage.LiveStateSnapshotForThread(thread.ID).FlushedItems
	if len(flushed) != 1 {
		t.Fatalf("pending flushed items = %+v, want the fallback send", flushed)
	}
	userItemID := flushed[0].UserItemID
	if got := turnStartsCarrying(t, capture, "sent as a fresh turn"); got != 1 {
		t.Fatalf("turn/start requests carrying the message before the Stop = %d, want 1", got)
	}

	if err := app.InterruptTurn(thread.ID, nil); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	// The interrupt's request is answered on the wire; any resend has been
	// written by the time InterruptTurn returns.
	if got := turnStartsCarrying(t, capture, "sent as a fresh turn"); got != 1 {
		t.Fatalf("turn/start requests carrying the message after the Stop = %d, want 1: the Stop sent it again", got)
	}

	// The echo of the turn's input still confirms the message, once.
	echoMeta, err := json.Marshal(map[string]any{"provider_item_id": "wire-fresh", "client_id": userItemID})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: thread.ID, TurnIndex: 3,
		ItemID: userItemID, Content: "sent as a fresh turn", Meta: echoMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText: %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var rows []store.Item
	for _, item := range items {
		if item.Kind == "user_text" && item.Summary == "sent as a fresh turn" {
			rows = append(rows, item)
		}
	}
	if len(rows) != 1 || rows[0].ID != userItemID || !strings.Contains(rows[0].Meta, "wire-fresh") {
		t.Fatalf("user rows = %+v, want %s once, confirmed by its echo", rows, userItemID)
	}
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("the message is still pending after its echo")
	}
}

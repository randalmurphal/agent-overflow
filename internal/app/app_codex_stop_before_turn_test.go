package app

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/triage"
)

const codexStopBeforeTurnThread = "codex-stop-before-turn"

// How the Codex app-server resolves an interrupt that reaches it before the
// turn a turn/start opened is reported started.
const (
	// The interrupt aborts the turn: it starts and ends interrupted.
	codexResolveInterrupted = "interrupted"
	// The interrupt lost the race: the turn runs to a normal end.
	codexResolveCompleted = "completed"
)

// writeCodexStopBeforeTurnBinary is a fake codex app-server with no turn
// running: a turn/steer answers NoActiveTurn, a turn/start is answered with
// its turn id and nothing more is reported until the stops-th
// turn/interrupt, which it answers and then resolves the turn as resolution:
// turn/started, the echo of the turn's input, and turn/completed. It appends
// every line AO writes to capturePath.
func writeCodexStopBeforeTurnBinary(t *testing.T, capturePath, resolution string, stops int) string {
	t.Helper()
	var resolve string
	switch resolution {
	case codexResolveInterrupted:
		resolve = `printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%[1]s","turn":{"id":"turn-1","status":"interrupted"}}}\n'`
	case codexResolveCompleted:
		resolve = `printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%[1]s","turnId":"turn-1","item":{"id":"msg-1","type":"agentMessage","text":"it ran anyway"}}}\n'
        printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%[1]s","turn":{"id":"turn-1","status":"completed"}}}\n'`
	default:
		t.Fatalf("unknown resolution %q", resolution)
	}
	script := fmt.Sprintf(`#!/bin/bash
n=0
client=''
text=''
while IFS= read -r line; do
    printf '%%s\n' "$line" >> '%[2]s'
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%[1]s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/steer"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32000,"message":"NoActiveTurn"}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
        client=$(printf '%%s' "$line" | sed -n 's/.*"clientUserMessageId":"\([^"]*\)".*/\1/p')
        text=$(printf '%%s' "$line" | sed -n 's/.*"text":"\([^"]*\)".*/\1/p')
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"turn":{"id":"turn-1"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/interrupt"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        n=$((n+1))
        if [ "$n" -eq %[3]d ]; then
        printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"%[1]s","turn":{"id":"turn-1"}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%[1]s","turnId":"turn-1","item":{"id":"wire-user-1","type":"userMessage","clientId":"%%s","content":[{"type":"text","text":"%%s"}]}}}\n' "$client" "$text"
        %[4]s
        fi
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, codexStopBeforeTurnThread, capturePath, stops, fmt.Sprintf(resolve, codexStopBeforeTurnThread))
	path := filepath.Join(t.TempDir(), "codex-stop-before-turn.sh")
	mockexec.Write(t, path, script)
	return path
}

// A Stop pressed after a fresh-turn message reached Codex and before its
// turn was reported started ends in exactly one outcome, whichever way
// Codex resolves it, however many times the person presses it. The message
// is a direct send's turn/start, or a queued message whose steer found no
// turn running and went out as a turn/start. Codex keeps a turn/start's
// input across turn/interrupt, so the message is never sent again, and it
// lands once, confirmed by its echo.
func TestStopBeforeACodexTurnOpensEndsInOneOutcome(t *testing.T) {
	t.Parallel()
	type testCase struct {
		queued     bool
		resolution string
		stops      int
	}
	var cases []testCase
	for _, queued := range []bool{false, true} {
		for _, resolution := range []string{codexResolveInterrupted, codexResolveCompleted} {
			for _, stops := range []int{1, 3} {
				cases = append(cases, testCase{queued: queued, resolution: resolution, stops: stops})
			}
		}
	}
	for _, tc := range cases {
		shape := "direct"
		if tc.queued {
			shape = "queued"
		}
		t.Run(fmt.Sprintf("%s/%s/%d-stops", shape, tc.resolution, tc.stops), func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			app.configureTriageQueueCallbacks()
			thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5.4", "chat")
			if err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			capture := filepath.Join(t.TempDir(), "codex-stdin.jsonl")
			if _, err := app.settings.Update(map[string]any{"codexBinaryPath": writeCodexStopBeforeTurnBinary(t, capture, tc.resolution, tc.stops)}); err != nil {
				t.Fatalf("set binary: %v", err)
			}
			if err := app.StartSession(thread.ID); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			t.Cleanup(func() { _ = app.StopSession(thread.ID) })

			const message = "stop me before I start"
			if tc.queued {
				app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{ID: "queue:stop-before", Message: message}})
			} else if err := app.SendMessage(context.Background(), thread.ID, message, nil); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			if got := turnStartsCarrying(t, capture, message); got != 1 {
				t.Fatalf("turn/start requests carrying the message before the Stop = %d, want 1", got)
			}
			sample := app.triage.SampleInterrupt(thread.ID)
			if sample.Turn != -1 || sample.CutTurn() < 0 {
				t.Fatalf("sample = open %d cut %d, want the message waiting for its turn", sample.Turn, sample.CutTurn())
			}
			turnIndex := sample.CutTurn()

			for range tc.stops {
				if err := app.InterruptTurn(thread.ID, nil); err != nil {
					t.Fatalf("InterruptTurn: %v", err)
				}
			}

			var turn store.Turn
			waitUntil(t, 10*time.Second, func() bool {
				got, found, err := app.store.GetTurnByThreadIndex(thread.ID, turnIndex)
				if err != nil {
					t.Fatalf("GetTurnByThreadIndex: %v", err)
				}
				turn = got
				return found && got.CompletedAt != nil
			})
			if got := turnStartsCarrying(t, capture, message); got != 1 {
				t.Fatalf("turn/start requests carrying the message after the Stop = %d, want 1: the Stop sent it again", got)
			}
			waitUntil(t, 10*time.Second, func() bool { return !app.triage.HasPendingSendForThread(thread.ID) })
			items, err := app.store.ListItems(thread.ID)
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			var userRows []store.Item
			for _, item := range items {
				if item.Kind == "user_text" && item.Summary == message {
					userRows = append(userRows, item)
				}
			}
			if len(userRows) != 1 || userRows[0].TurnIndex != turnIndex {
				t.Fatalf("user rows = %+v, want the message once on turn %d", userRows, turnIndex)
			}
			if draft, _, err := app.store.GetThreadDraft(thread.ID); err != nil || draft.Content != "" {
				t.Fatalf("draft = %q (%v), want it empty: the message was delivered", draft.Content, err)
			}
			rows := stopRowsOf(t, app.store, thread.ID)
			if tc.resolution == codexResolveCompleted {
				if turn.StopReason == "interrupted" || len(rows) != 0 {
					t.Fatalf("turn stop reason %q, stop rows %+v; want a normal end and no stop row", turn.StopReason, rows)
				}
				return
			}
			if turn.StopReason != "interrupted" || len(rows) != 1 || rows[0].TurnIndex != turnIndex {
				t.Fatalf("turn stop reason %q, stop rows %+v; want the turn interrupted with one stop row on it", turn.StopReason, rows)
			}
		})
	}
}

package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/triage"
)

// How the Claude CLI resolves an interrupt that reaches it before the turn of
// the message it holds opens.
const (
	// The interrupt aborts the turn the message opens.
	resolveAborted = "aborted"
	// The interrupt reached an idle CLI and the message runs to a normal end.
	resolveRunsAnyway = "runs-anyway"
	// The CLI removes the message from its queue without running it.
	resolveUnrun = "unrun"
)

// writeClaudeStopBeforeTurnBinary is a fake Claude CLI that holds the user
// message it is sent and opens no turn for it. It acknowledges every
// interrupt, and after the stops-th one resolves the message as resolution.
func writeClaudeStopBeforeTurnBinary(t *testing.T, resolution string, stops int) string {
	t.Helper()
	static := func(line string) string { return "      printf '%s\\n' " + shellQuote(line) + "\n" }
	lifecycle := func(state string) string {
		return `      printf '{"type":"command_lifecycle","command_uuid":"%s","state":"` + state + `"}\n' "$uuid"` + "\n"
	}
	initLine := static(`{"type":"system","subtype":"init","session_id":"sess-stop-before","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`)
	var resolve string
	switch resolution {
	case resolveAborted:
		resolve = initLine + lifecycle("started") +
			static(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["[Request interrupted by user]"]}`) +
			lifecycle("cancelled")
	case resolveRunsAnyway:
		resolve = initLine + lifecycle("started")
		for _, line := range testutil.MockClaudeStreamedText("msg-ran", "it ran anyway") {
			resolve += static(line)
		}
		resolve += static(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn"}`) + lifecycle("completed")
	case resolveUnrun:
		resolve = lifecycle("cancelled")
	default:
		t.Fatalf("unknown resolution %q", resolution)
	}
	script := fmt.Sprintf(`#!/bin/bash
uuid=''
n=0
while IFS= read -r line; do
  case "$line" in
    *'"subtype":"interrupt"'*)
      reqid=$(printf '%%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$reqid"
      n=$((n+1))
      if [ "$n" -eq %d ]; then
%s      fi
      ;;
    *'"type":"user"'*)
      uuid=$(printf '%%s' "$line" | sed -n 's/.*"uuid":"\([^"]*\)".*/\1/p')
      printf '{"type":"command_lifecycle","command_uuid":"%%s","state":"queued"}\n' "$uuid"
      ;;
  esac
done
`, stops, resolve)
	path := filepath.Join(t.TempDir(), "claude-stop-before-turn.sh")
	mockexec.Write(t, path, script)
	return path
}

// stopRowsOf returns the thread's "Stopped by user" rows.
func stopRowsOf(t *testing.T, st *store.Store, threadID string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var rows []store.Item
	for _, item := range items {
		if item.Kind == triage.ItemKindError && item.Summary == triage.StoppedByUser {
			rows = append(rows, item)
		}
	}
	return rows
}

// A Stop pressed after a message reached the CLI and before its turn opened
// ends in exactly one outcome, whichever way the CLI resolves it, however
// many times the person presses it.
func TestStopBeforeAClaudeTurnOpensEndsInOneOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		resolution string
		stops      int
	}{
		{resolveAborted, 1},
		{resolveRunsAnyway, 1},
		{resolveUnrun, 1},
		{resolveAborted, 3},
		{resolveRunsAnyway, 3},
	} {
		t.Run(fmt.Sprintf("%s/%d-stops", tc.resolution, tc.stops), func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			// The production wiring: a message the CLI retires unrun goes
			// back to the composer.
			app.configureTriageQueueCallbacks()
			thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
			if err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeClaudeStopBeforeTurnBinary(t, tc.resolution, tc.stops)}); err != nil {
				t.Fatalf("set binary: %v", err)
			}
			if err := app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			t.Cleanup(func() { _ = app.StopSession(thread.ID) })

			const message = "stop me before I start"
			if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, message, SendMessageOptions{}); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			turnIndex := -1
			items, err := app.store.ListItems(thread.ID)
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			for _, item := range items {
				if item.Kind == "user_text" && item.Summary == message {
					turnIndex = item.TurnIndex
				}
			}
			if turnIndex < 0 {
				t.Fatalf("items = %+v, want the sent message's row", items)
			}
			if sample := app.triage.SampleInterrupt(thread.ID); sample.Turn != -1 || sample.CutTurn() != turnIndex {
				t.Fatalf("sample = open %d cut %d, want the message waiting for turn %d", sample.Turn, sample.CutTurn(), turnIndex)
			}

			for range tc.stops {
				if err := app.InterruptTurn(thread.ID, nil); err != nil {
					t.Fatalf("InterruptTurn: %v", err)
				}
			}

			switch tc.resolution {
			case resolveUnrun:
				waitUntil(t, 10*time.Second, func() bool {
					draft, _, err := app.store.GetThreadDraft(thread.ID)
					return err == nil && strings.Contains(draft.Content, message)
				})
				if rows := stopRowsOf(t, app.store, thread.ID); len(rows) != 0 {
					t.Fatalf("stop rows = %+v, want none for a turn that never ran", rows)
				}
			default:
				var turn store.Turn
				waitUntil(t, 10*time.Second, func() bool {
					got, found, err := app.store.GetTurnByThreadIndex(thread.ID, turnIndex)
					if err != nil {
						t.Fatalf("GetTurnByThreadIndex: %v", err)
					}
					turn = got
					return found && got.CompletedAt != nil
				})
				rows := stopRowsOf(t, app.store, thread.ID)
				if tc.resolution == resolveRunsAnyway {
					if turn.StopReason == "interrupted" || len(rows) != 0 {
						t.Fatalf("turn stop reason %q, stop rows %+v; want a normal end and no stop row", turn.StopReason, rows)
					}
					return
				}
				if turn.StopReason != "interrupted" || len(rows) != 1 || rows[0].TurnIndex != turnIndex {
					t.Fatalf("turn stop reason %q, stop rows %+v; want the turn interrupted with one stop row on it", turn.StopReason, rows)
				}
			}
		})
	}
}

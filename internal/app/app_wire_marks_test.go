package app

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
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// loopWakeMark is the sender line a loop "inbox"'s wake reaches the model
// under.
const loopWakeMark = `[Agent Overflow message from your loop "inbox". The user did not write it.]`

func loopWakeOrigin() *usermessage.OriginWake {
	return &usermessage.OriginWake{Kind: store.AgentWakeLoop, Label: "inbox"}
}

// A loop's wake and the person's message Claude joins at a turn boundary go
// out as one message in which only the wake is marked, and the row keeps
// both as written.
func TestAJoinedDispatchMarksOnlyTheAppsMember(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	workspace := initGitRepo(t)
	thread := testThread("marks-claude-join")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = workspace
	thread.SessionRef = "marks-claude-session"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// A running turn, so the dispatch persists its row before the echo.
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: now}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if _, err := app.store.AppendItem(store.Item{
		ID: "toolu_sleep", ThreadID: thread.ID, TurnIndex: 3, Kind: "tool_call", Role: "assistant",
		Status: "running", Summary: "sleep 10", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed tool_call: %v", err)
	}
	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	sess, err := claude.NewSession(context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: workspace},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude), Token: "tok", Claude: sess, Liveness: newSessionLiveness(time.Now()),
	})

	const wake, person = "Loop fired.", "my own words"
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:wake", Message: wake, EnqueuedAt: now, Payload: flushQueuePayloadJSON(t, flushQueuePayload{
			SendID: agentWakeSendID("w-1", now), Origin: usermessage.OriginAgentWake, OriginWake: loopWakeOrigin(),
		})},
		{ID: "queue:person", Message: person, EnqueuedAt: now + 1, Payload: flushQueuePayloadJSON(t, flushQueuePayload{
			SendID: "send-person", ExpandComposerCommands: true,
		})},
	})

	envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1)
	if len(envelopes) != 1 {
		t.Fatalf("stdin envelopes = %d, want the one joined message", len(envelopes))
	}
	got, _ := envelopeText(t, envelopes[0])
	want := loopWakeMark + "\n" + wake + "\n" + usermessage.EndLine + usermessage.JoinSeparator + person
	if got != want {
		t.Errorf("wire text:\n got %q\nwant %q", got, want)
	}
	rows, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for _, row := range rows {
		if row.Kind == "user_text" {
			stored = append(stored, row.Summary)
		}
	}
	if len(stored) != 1 || stored[0] != wake+usermessage.JoinSeparator+person {
		t.Errorf("stored rows = %q, want one holding the members as written", stored)
	}
}

// A joined row a session end put back on the queue whole is marked member
// by member again from the parts it recorded.
func TestARequeuedJoinedRowIsMarkedByItsParts(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	workspace := initGitRepo(t)
	thread := testThread("marks-claude-requeue")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = workspace
	thread.SessionRef = "marks-requeue-session"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	sess, err := claude.NewSession(context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: workspace},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude), Token: "tok", Claude: sess, Liveness: newSessionLiveness(time.Now()),
	})

	now := time.Now().UnixMilli()
	const person, wake = "first, mine", "Loop fired."
	wakeID := agentWakeSendID("w-1", now)
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
		ID: "queue:joined", Message: person + usermessage.JoinSeparator + wake, EnqueuedAt: now,
		Payload: flushQueuePayloadJSON(t, flushQueuePayload{
			SendID: "send-person", JoinedSendIDs: []string{"send-person", wakeID}, ExpandComposerCommands: true,
			JoinedParts: []usermessage.JoinedPart{
				{SendID: "send-person", Bytes: len(person)},
				{SendID: wakeID, Bytes: len(wake), Origin: usermessage.OriginAgentWake, OriginWake: loopWakeOrigin()},
			},
		}),
	}})

	envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1)
	if len(envelopes) != 1 {
		t.Fatalf("stdin envelopes = %d, want one", len(envelopes))
	}
	got, _ := envelopeText(t, envelopes[0])
	want := person + usermessage.JoinSeparator + loopWakeMark + "\n" + wake + "\n" + usermessage.EndLine
	if got != want {
		t.Errorf("wire text:\n got %q\nwant %q", got, want)
	}
}

// writeCodexRecordingBinary is a fake Codex app-server that appends every
// line it is written to logPath and accepts turn/start.
func writeCodexRecordingBinary(t *testing.T, threadID, logPath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    printf '%%s\n' "$line" >> '%s'
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"turnId":"start-turn"}}\n' "$id"
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, logPath, threadID)
	path := filepath.Join(t.TempDir(), "codex-recording.sh")
	mockexec.Write(t, path, script)
	return path
}

// codexTurnStartTexts is the input text of every turn/start in logPath.
func codexTurnStartTexts(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var texts []string
	for _, line := range strings.Split(string(data), "\n") {
		var request struct {
			Method string `json:"method"`
			Params struct {
				Input []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &request) != nil || request.Method != "turn/start" {
			continue
		}
		var text strings.Builder
		for _, input := range request.Params.Input {
			if input.Type == "text" {
				text.WriteString(input.Text)
			}
		}
		texts = append(texts, text.String())
	}
	return texts
}

// A Codex interrupt drops the input it was steered, so the resend puts each
// row back on the wire as its dispatch did: a loop's wake marked with its
// sender, and the person's `/workflow` message with its block expanded.
func TestCodexResendAfterInterruptRebuildsTheWireText(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("marks-codex-resend")
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	log := filepath.Join(t.TempDir(), "codex-stdin.jsonl")
	sess, err := codex.NewSession(context.Background(), thread.ID,
		codex.Config{Binary: writeCodexRecordingBinary(t, thread.ID+"-codex", log), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	appSess := session{Provider: string(provider.Codex), Token: "tok", Codex: sess, Liveness: newSessionLiveness(time.Now())}
	app.sessionManager().put(thread.ID, appSess)

	wakeMeta, err := usermessage.Marshal(usermessage.Input{
		SendID: agentWakeSendID("w-1", 1), Origin: usermessage.OriginAgentWake, OriginWake: loopWakeOrigin(),
	})
	if err != nil {
		t.Fatal(err)
	}
	personMeta, err := usermessage.Marshal(usermessage.Input{SendID: "send-person", ExpandComposerCommands: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range []store.Item{
		{ID: "user:1:flush:1", Summary: "Loop fired.", Meta: wakeMeta},
		{ID: "user:1:flush:2", Summary: "/workflow go", Meta: personMeta},
	} {
		row.ThreadID, row.TurnIndex, row.Kind, row.Role, row.Status = thread.ID, 1, "user_text", "user", "completed"
		app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, fmt.Sprintf("queue:q%d", i), row, int64(10+i), triage.PendingSendExpectation{})
	}
	tok := app.triage.MarkFlushSendsInterrupted(thread.ID, -1)
	app.eagerPersistFlushSendsOnInterrupt(thread.ID, appSess, -1, tok)

	var texts []string
	waitUntil(t, 5*time.Second, func() bool {
		texts = codexTurnStartTexts(t, log)
		return len(texts) > 0
	})
	want := loopWakeMark + "\nLoop fired.\n" + usermessage.EndLine + "\n\n/workflow go\n\nAgent Overflow workflows"
	if len(texts) != 1 || !strings.HasPrefix(texts[0], want) {
		t.Fatalf("resent text = %q, want one turn/start opening %q", texts, want)
	}
	if strings.Count(texts[0], "Agent Overflow workflows are available") != 1 {
		t.Errorf("resent text = %q, want the person's command block once", texts[0])
	}
}

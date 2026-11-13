package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// A person stopping a requested turn reaches the caller as a stop, never as a
// restart: the wake carries the cause the settlement recorded, because the
// interrupted state alone cannot tell the two apart.
func TestAPersonsStopReachesTheCallerAsAStopNotARestart(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	// The turn holds until the gate opens, then ends the way the CLI ends a
	// turn a person stopped.
	gate := filepath.Join(t.TempDir(), "stop")
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-stop","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		`{"type":"assistant","message":{"id":"msg-stop","role":"assistant","content":[{"type":"text","text":"working on it"}]}}`,
		testutil.MockWaitForFile(gate),
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["[Request interrupted by user]"]}`,
	}})
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	// The caller is mid-turn, so the wake queues and stays readable.
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "caller-open", ThreadID: f.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	ack := f.runningRequest(t, "filter the previews")

	// The interrupt waits for the CLI's ack, which the mock sends only once
	// the gate lets it read past the held turn.
	stopped := make(chan error, 1)
	go func() { stopped <- f.app.InterruptTurn(ack.ThreadID, nil) }()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestInterrupted)

	var message string
	waitUntil(t, 10*time.Second, func() bool {
		for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
			if queued.SendID == threadWakeSendID(ack.Token) {
				message = queued.Message
				return true
			}
		}
		return false
	})
	if !strings.Contains(message, "was interrupted: Its turn was stopped before it answered.") {
		t.Errorf("wake = %q, want the stop as its cause", message)
	}
	if strings.Contains(message, "restart") {
		t.Errorf("wake = %q blames a restart for a person's stop", message)
	}
}

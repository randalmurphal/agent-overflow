package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// queueScenario parks turn 1 on a waitSignal so a queue can build up behind
// it, then answers each dispatched turn by echoing the queued text back — the
// `userMessage` item an app's pending-send correlation needs, and proof the
// mock bound the dispatched message's text and client id for the turn's steps.
func queueScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-queue",
		Provider: scenario.ProviderCodex,
		Turns: []scenario.Turn{
			{Label: "held", Steps: []scenario.Step{
				{Emit: &scenario.EmitStep{Lines: []string{
					`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
				}}},
				{WaitSignal: &scenario.WaitSignalStep{Name: "hold"}},
				{Emit: &scenario.EmitStep{Lines: []string{
					`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
				}}},
			}},
			{Label: "dispatched", Steps: []scenario.Step{
				{Emit: &scenario.EmitStep{Lines: []string{
					`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
					`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"umsg-${TURN}","type":"userMessage","clientId":"${QUEUE_CLIENT_ID}","content":[{"type":"text","text":"${USER_INPUT}"}]}}}`,
					`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
				}}},
			}},
		},
		AfterTurns: "repeatLast",
		Codex:      &scenario.CodexOptions{ThreadID: "th-q"},
	}
}

// startControlledMock boots the real binary with a live control channel, so a
// test can release a waitSignal gate — the only way to hold a turn open for an
// unbounded, non-flaky amount of wall time.
func startControlledMock(t *testing.T, sc *scenario.Scenario, args []string) (*mockProc, func(name string)) {
	t.Helper()
	scenarioJSON, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}
	mockIDCh := make(chan string, 1)
	srv, err := control.NewServer(control.ServerConfig{
		Resolve: func(control.Registration) (control.Assignment, error) {
			return control.Assignment{ScenarioName: sc.Name, ScenarioJSON: scenarioJSON}, nil
		},
		OnReport: func(info control.MockInfo, _ control.Report) {
			select {
			case mockIDCh <- info.MockID:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	p := startMock(t, args, []string{
		control.EnvAddr + "=" + srv.Addr(),
		control.EnvToken + "=" + srv.Token(),
	}, t.TempDir())

	var mockID string
	select {
	case mockID = <-mockIDCh:
	case <-time.After(testTimeout):
		t.Fatal("mock id never observed")
	}
	return p, func(name string) {
		t.Helper()
		if err := srv.Command(mockID, control.Command{Type: control.CommandAdvance, Name: name}); err != nil {
			t.Fatalf("advance %q: %v", name, err)
		}
	}
}

// TestCodexInitializeReportsAVersion pins the handshake field every codex
// method gate reads. Without a parseable `userAgent` the app fails every
// version gate closed, which would make a harness session behave like a build
// older than anything AO supports.
func TestCodexInitializeReportsAVersion(t *testing.T) {
	env := writeScenarioFile(t, queueScenario(), "")
	p := startMock(t, []string{"app-server"}, env, t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"agent_overflow"}}}`)
	resp := p.expectLine(testTimeout)
	var body struct {
		Result struct {
			UserAgent string `json:"userAgent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(resp), &body); err != nil {
		t.Fatalf("decode initialize response %q: %v", resp, err)
	}
	if !strings.HasPrefix(body.Result.UserAgent, "codex_cli_rs/"+mockVersionNumber) {
		t.Fatalf("initialize userAgent = %q, want codex_cli_rs/%s...", body.Result.UserAgent, mockVersionNumber)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
}

// TestCodexThreadQueueDispatchesAutomatically is the mock's half of the
// provider-queue contract: submissions land in FIFO order, edits and deletes
// address them by upstream's own param names, and the queue head starts a turn
// on its OWN when the thread goes idle — with no `thread/queue/start` from the
// client, which the mock refuses outright.
func TestCodexThreadQueueDispatchesAutomatically(t *testing.T) {
	p, advance := startControlledMock(t, queueScenario(), []string{"app-server"})

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLine(testTimeout)
	p.send(`{"jsonrpc":"2.0","method":"initialized"}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`)
	p.expectLine(testTimeout)

	// Turn 1 starts and parks.
	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"th-q","input":[{"type":"text","text":"first"}]}}`)
	p.expectLine(testTimeout)
	p.expectLineContaining(`"method":"turn/started"`, testTimeout)

	// Three submissions while it is held. Every mutation raises the change
	// notification — including the adding client's own, which is why a client
	// cannot treat the notification as "someone else did something".
	for i, text := range []string{"second", "third", "doomed"} {
		p.send(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"thread/queue/add","params":{"threadId":"th-q","input":[{"type":"text","text":%q}],"clientUserMessageId":"user:%d"}}`,
			10+i, text, i))
		p.expectLineContaining(`"queuedSubmission"`, testTimeout)
		p.expectLineContaining(`"method":"thread/queue/changed"`, testTimeout)
	}

	// Nothing has run: the queue holds them until the thread is idle.
	p.send(`{"jsonrpc":"2.0","id":20,"method":"thread/queue/list","params":{"threadId":"th-q"}}`)
	listed := p.expectLineContaining(`"nextCursor"`, testTimeout)
	for _, want := range []string{`"queue-sub-1"`, `"queue-sub-2"`, `"queue-sub-3"`, `"second"`, `"third"`, `"doomed"`} {
		if !strings.Contains(listed, want) {
			t.Fatalf("queue list missing %s: %s", want, listed)
		}
	}
	if strings.Index(listed, `"second"`) > strings.Index(listed, `"third"`) {
		t.Fatalf("queue list is not in FIFO order: %s", listed)
	}

	// `update` and `reorder` exist upstream and have no AO caller, so the mock
	// refuses them for the same reason it refuses `start`: a harness that grew
	// a caller must fail here rather than pass against a mock more permissive
	// than the app.
	p.send(`{"jsonrpc":"2.0","id":21,"method":"thread/queue/update","params":{"threadId":"th-q","queuedSubmissionId":"queue-sub-1","input":[{"type":"text","text":"edited"}]}}`)
	if got := p.expectLineContaining(`"error"`, testTimeout); !strings.Contains(got, "does not edit or re-order") {
		t.Fatalf("thread/queue/update response = %s, want a refusal", got)
	}
	p.send(`{"jsonrpc":"2.0","id":26,"method":"thread/queue/reorder","params":{"threadId":"th-q","queuedSubmissionIds":["queue-sub-2","queue-sub-1"]}}`)
	if got := p.expectLineContaining(`"error"`, testTimeout); !strings.Contains(got, "does not edit or re-order") {
		t.Fatalf("thread/queue/reorder response = %s, want a refusal", got)
	}

	// Delete the tail. A second delete of the same id reports deleted:false —
	// a state, not an error.
	p.send(`{"jsonrpc":"2.0","id":22,"method":"thread/queue/delete","params":{"threadId":"th-q","queuedSubmissionId":"queue-sub-3"}}`)
	if got := p.expectLineContaining(`"deleted"`, testTimeout); !strings.Contains(got, `"deleted":true`) {
		t.Fatalf("delete response = %s", got)
	}
	p.expectLineContaining(`"method":"thread/queue/changed"`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":23,"method":"thread/queue/delete","params":{"threadId":"th-q","queuedSubmissionId":"queue-sub-3"}}`)
	if got := p.expectLineContaining(`"deleted"`, testTimeout); !strings.Contains(got, `"deleted":false`) {
		t.Fatalf("second delete response = %s, want deleted:false", got)
	}

	// A client must never drive dispatch: the mock refuses `start` loudly.
	p.send(`{"jsonrpc":"2.0","id":24,"method":"thread/queue/start","params":{"threadId":"th-q"}}`)
	if got := p.expectLineContaining(`"error"`, testTimeout); !strings.Contains(got, "dispatch automatically") {
		t.Fatalf("thread/queue/start response = %s, want a refusal", got)
	}

	// Release turn 1. Each remaining submission now runs as its own turn, in
	// order, echoing the text and client id the queue held for it — and the
	// deletion of queue-sub-3 removes a turn rather than merely a row.
	advance("hold")
	p.expectLineContaining(`"status":"completed"`, testTimeout)

	first := p.expectLineContaining(`"type":"userMessage"`, testTimeout)
	if !strings.Contains(first, `"second"`) || !strings.Contains(first, `"clientId":"user:0"`) {
		t.Fatalf("first dispatched turn echoed %s", first)
	}
	second := p.expectLineContaining(`"type":"userMessage"`, testTimeout)
	if !strings.Contains(second, `"third"`) || !strings.Contains(second, `"clientId":"user:1"`) {
		t.Fatalf("second dispatched turn echoed %s", second)
	}

	p.send(`{"jsonrpc":"2.0","id":25,"method":"thread/queue/list","params":{"threadId":"th-q"}}`)
	if got := p.expectLineContaining(`"nextCursor"`, testTimeout); !strings.Contains(got, `"data":[]`) {
		t.Fatalf("queue should be empty after the drain: %s", got)
	}

	p.closeStdinAndExpectExit(0, testTimeout)
}

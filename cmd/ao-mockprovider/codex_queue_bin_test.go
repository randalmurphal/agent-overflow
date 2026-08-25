package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// queueScenario parks turn 1 on a waitSignal, which is what lets a test hold a
// turn open while it exercises the queue tripwires and the steer echo. Later
// turns echo their own `userMessage` with the client id the caller stamped —
// the item an app's pending-send correlation needs.
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
					`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"umsg-${TURN}","type":"userMessage","clientId":"${CLIENT_ID}","content":[{"type":"text","text":"${USER_INPUT}"}]}}}`,
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

// TestCodexThreadQueueRefusesAddAndStart is the mock's tripwire, and the whole
// reason the family is still mocked at all.
//
// Agent Overflow dispatches every mid-turn message with `turn/steer`. Handing
// one to the app-server's own queue instead would put TWO dispatchers on one
// message — the provider drains its queue from `on_thread_idle`, on its own
// clock — so `add` and `start` must both be unreachable from the app. The mock
// answers them with an error rather than accepting them, which is what turns a
// regrown caller into a failing harness run instead of a duplicated turn.
//
// `list` and `delete` still answer, because AO still calls them: the rollback
// purge and the legacy-row sunset both read and empty this queue.
func TestCodexThreadQueueRefusesAddAndStart(t *testing.T) {
	env := writeScenarioFile(t, queueScenario(), "")
	p := startMock(t, []string{"app-server"}, env, t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLine(testTimeout)
	p.send(`{"jsonrpc":"2.0","method":"initialized"}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`)
	p.expectLine(testTimeout)

	p.send(`{"jsonrpc":"2.0","id":3,"method":"thread/queue/add","params":{"threadId":"th-q","input":[{"type":"text","text":"queued"}],"clientUserMessageId":"user:0"}}`)
	if got := p.expectLineContaining(`"error"`, testTimeout); !strings.Contains(got, "turn/steer") {
		t.Fatalf("thread/queue/add response = %s, want a refusal naming turn/steer", got)
	}
	p.send(`{"jsonrpc":"2.0","id":4,"method":"thread/queue/start","params":{"threadId":"th-q"}}`)
	if got := p.expectLineContaining(`"error"`, testTimeout); !strings.Contains(got, "dispatch automatically") {
		t.Fatalf("thread/queue/start response = %s, want a refusal", got)
	}

	// `update` / `reorder` keep their own refusal: they exist upstream and
	// have no AO caller either.
	p.send(`{"jsonrpc":"2.0","id":5,"method":"thread/queue/update","params":{"threadId":"th-q","queuedSubmissionId":"queue-sub-1","input":[{"type":"text","text":"edited"}]}}`)
	if got := p.expectLineContaining(`"error"`, testTimeout); !strings.Contains(got, "does not edit or re-order") {
		t.Fatalf("thread/queue/update response = %s, want a refusal", got)
	}
	p.send(`{"jsonrpc":"2.0","id":6,"method":"thread/queue/reorder","params":{"threadId":"th-q","queuedSubmissionIds":["queue-sub-2","queue-sub-1"]}}`)
	if got := p.expectLineContaining(`"error"`, testTimeout); !strings.Contains(got, "does not edit or re-order") {
		t.Fatalf("thread/queue/reorder response = %s, want a refusal", got)
	}

	// The two survivors answer over an empty queue — the shape every harness
	// thread is in, and the one the sunset and the rollback purge read.
	p.send(`{"jsonrpc":"2.0","id":7,"method":"thread/queue/list","params":{"threadId":"th-q"}}`)
	if got := p.expectLineContaining(`"nextCursor"`, testTimeout); !strings.Contains(got, `"data":[]`) {
		t.Fatalf("queue list = %s, want an empty queue (nothing can add to it)", got)
	}
	p.send(`{"jsonrpc":"2.0","id":8,"method":"thread/queue/delete","params":{"threadId":"th-q","queuedSubmissionId":"queue-sub-1"}}`)
	if got := p.expectLineContaining(`"deleted"`, testTimeout); !strings.Contains(got, `"deleted":false`) {
		t.Fatalf("delete response = %s, want deleted:false", got)
	}

	p.closeStdinAndExpectExit(0, testTimeout)
}

// TestCodexSteerEchoesTheClientMessageID pins the other half of the same
// contract. AO registers a steer's pending send BY the `clientUserMessageId`
// it stamped, so an echo that omits the id is provably not that entry's and
// the message would persist as injected provider context instead of the
// user's own. The echo is adapter-owned because its text and id exist only at
// steer time.
func TestCodexSteerEchoesTheClientMessageID(t *testing.T) {
	p, advance := startControlledMock(t, queueScenario(), []string{"app-server"})
	defer advance("hold")

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLine(testTimeout)
	p.send(`{"jsonrpc":"2.0","method":"initialized"}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`)
	p.expectLine(testTimeout)

	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"th-q","input":[{"type":"text","text":"first"}],"clientUserMessageId":"user:0"}}`)
	p.expectLine(testTimeout)
	p.expectLineContaining(`"method":"turn/started"`, testTimeout)

	p.send(`{"jsonrpc":"2.0","id":4,"method":"turn/steer","params":{"threadId":"th-q","expectedTurnId":"turn-1","input":[{"type":"text","text":"steered"}],"clientUserMessageId":"user:0:flush:1"}}`)
	echo := p.expectLineContaining(`"type":"userMessage"`, testTimeout)
	if !strings.Contains(echo, `"steered"`) || !strings.Contains(echo, `"clientId":"user:0:flush:1"`) {
		t.Fatalf("steer echo = %s, want the steered text and its clientId", echo)
	}
	// A second steer in the SAME turn must not reuse the first echo's item id.
	p.send(`{"jsonrpc":"2.0","id":5,"method":"turn/steer","params":{"threadId":"th-q","expectedTurnId":"turn-1","input":[{"type":"text","text":"again"}],"clientUserMessageId":"user:0:flush:2"}}`)
	second := p.expectLineContaining(`"type":"userMessage"`, testTimeout)
	if firstID, secondID := userMessageItemID(t, echo), userMessageItemID(t, second); firstID == secondID {
		t.Fatalf("two steers in one turn shared the item id %q", firstID)
	}
}

// userMessageItemID pulls `params.item.id` out of an `item/completed` line.
func userMessageItemID(t *testing.T, line string) string {
	t.Helper()
	var body struct {
		Params struct {
			Item struct {
				ID string `json:"id"`
			} `json:"item"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &body); err != nil {
		t.Fatalf("decode item/completed %q: %v", line, err)
	}
	return body.Params.Item.ID
}

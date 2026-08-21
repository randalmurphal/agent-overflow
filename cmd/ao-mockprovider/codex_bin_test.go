package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// --- codex mode ---

func codexScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-happy",
		Provider: scenario.ProviderCodex,
		Turns: []scenario.Turn{{Label: "reply", Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
			`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"msg-${TURN}","type":"agentMessage","text":"codex turn ${TURN}"}}}`,
			`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
		}}}}}},
		AfterTurns: "repeatLast",
		Codex: &scenario.CodexOptions{
			ThreadID: "th-42",
			Responses: map[string]string{
				"initialize": `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"userAgent":"ao-mock"}}`,
			},
		},
	}
}

func TestCodexHandshakeAndScenarioTurn(t *testing.T) {
	env := writeScenarioFile(t, codexScenario(), "")
	p := startMock(t, []string{"app-server"}, env, t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"agent_overflow"}}}`)
	initResp := p.expectLine(testTimeout)
	if initResp != `{"jsonrpc":"2.0","id":1,"result":{"userAgent":"ao-mock"}}` {
		t.Fatalf("initialize response = %q (scenario override not applied?)", initResp)
	}

	p.send(`{"jsonrpc":"2.0","method":"initialized"}`)
	// No historyMode in the params: upstream's default is legacy, and the
	// echo is what tells the app the thread it just got is NOT revertible.
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"th-42","historyMode":"legacy"}}}` {
		t.Fatalf("thread/start response = %q", got)
	}

	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"th-42","input":[]}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}` {
		t.Fatalf("turn/start response = %q", got)
	}
	p.expectLineContaining(`"method":"turn/started"`, testTimeout)
	p.expectLineContaining(`"text":"codex turn 1"`, testTimeout)
	done := p.expectLineContaining(`"method":"turn/completed"`, testTimeout)
	if !strings.Contains(done, `"threadId":"th-42"`) || !strings.Contains(done, `"id":"turn-1"`) {
		t.Fatalf("turn/completed = %q", done)
	}

	// Unknown request must still get an answer so the app never hangs.
	p.send(`{"jsonrpc":"2.0","id":4,"method":"thread/read","params":{}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":4,"result":{}}` {
		t.Fatalf("unknown-method response = %q", got)
	}

	p.closeStdinAndExpectExit(0, testTimeout)
}

func TestCodexInterruptAbortsWaitSignalTurn(t *testing.T) {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-interrupt",
		Provider: scenario.ProviderCodex,
		Turns: []scenario.Turn{{Steps: []scenario.Step{
			{Emit: &scenario.EmitStep{Lines: []string{
				`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
			}}},
			{WaitSignal: &scenario.WaitSignalStep{Name: "hold"}},
			{Emit: &scenario.EmitStep{Lines: []string{`{"jsonrpc":"2.0","method":"must/not/run"}`}}},
		}}},
	}
	p := startMock(t, []string{"app-server"}, writeScenarioFile(t, sc, ""), t.TempDir())
	p.send(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{}}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"turn/start","params":{"threadId":"mock-codex-thread","input":[]}}`)
	p.expectLineContaining(`"id":2`, testTimeout)
	p.expectLineContaining(`"method":"turn/started"`, testTimeout)

	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/interrupt","params":{"threadId":"mock-codex-thread","turnId":"turn-1"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":3,"result":{}}` {
		t.Fatalf("turn/interrupt response = %q", got)
	}
	completed := p.expectLine(testTimeout)
	if !strings.Contains(completed, `"method":"turn/completed"`) ||
		!strings.Contains(completed, `"status":"interrupted"`) ||
		!strings.Contains(completed, `"id":"turn-1"`) {
		t.Fatalf("interrupted turn/completed = %q", completed)
	}
	var notification struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(completed), &notification); err != nil {
		t.Fatalf("decode interrupted notification: %v", err)
	}
	events := codex.ClassifyNotification("thread-interrupt", notification.Method, notification.Params)
	if len(events) != 1 {
		t.Fatalf("interrupted completion events = %+v", events)
	}
	meta, ok := events[0].TurnComplete.(*provider.WireTurnCompleteMeta)
	if events[0].Kind != provider.EventTurnComplete || !ok ||
		!meta.Aborted || meta.StopReason != "interrupted" || meta.ErrorMessage != "" {
		t.Fatalf("interrupted completion events = %+v", events)
	}

	// The next request response must not be preceded by the skipped marker.
	p.send(`{"jsonrpc":"2.0","id":4,"method":"thread/read","params":{}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":4,"result":{}}` {
		t.Fatalf("line after interrupted completion = %q; remaining scenario step ran", got)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
}

func TestCodexThreadResumeEchoesRequestedID(t *testing.T) {
	env := writeScenarioFile(t, codexScenario(), "")
	p := startMock(t, []string{"app-server"}, env, t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/resume","params":{"threadId":"resumed-7"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"resumed-7","historyMode":"legacy"}}}` {
		t.Fatalf("thread/resume response = %q", got)
	}

	// ${THREAD_ID} now substitutes to the resumed id in turn frames.
	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"resumed-7","input":[]}}`)
	p.expectLineContaining(`"id":3`, testTimeout)
	started := p.expectLineContaining(`"method":"turn/started"`, testTimeout)
	if !strings.Contains(started, `"threadId":"resumed-7"`) {
		t.Fatalf("turn/started after resume = %q", started)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
}

// TestCodexThreadForkCutsAtTheAnchor drives `thread/fork` over the real
// binary: the cut an anchor produces, the full copy an absent anchor
// produces (the shape AO's mid-turn tail fork sends), and the refusal
// codex answers a `lastTurnId` naming the in-progress turn with — the
// refusal AO's tail normalisation exists to stay clear of.
//
// Turn 2's `turn/started` is the synchronisation point rather than turn
// 1's `turn/completed`: the engine finishes a turn AFTER its terminal
// frame, so only a frame from the next turn proves turn 1 left flight.
func TestCodexThreadForkCutsAtTheAnchor(t *testing.T) {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-fork",
		Provider: scenario.ProviderCodex,
		Turns: []scenario.Turn{
			{Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
				`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
				`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
			}}}}},
			{Steps: []scenario.Step{
				{Emit: &scenario.EmitStep{Lines: []string{
					`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
				}}},
				{WaitSignal: &scenario.WaitSignalStep{Name: "hold"}},
			}},
		},
	}
	p := startMock(t, []string{"app-server"}, writeScenarioFile(t, sc, ""), t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{}}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"turn/start","params":{"threadId":"mock-codex-thread","input":[]}}`)
	p.expectLineContaining(`"id":2`, testTimeout)
	p.expectLineContaining(`"turn":{"id":"turn-1"}`, testTimeout)
	p.expectLineContaining(`"method":"turn/completed"`, testTimeout)

	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"mock-codex-thread","input":[]}}`)
	p.expectLineContaining(`"id":3`, testTimeout)
	p.expectLineContaining(`"turn":{"id":"turn-2"}`, testTimeout)

	fork := func(t *testing.T, id int, params string) (threadID string, turnIDs []string, rpcErr string) {
		t.Helper()
		p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"thread/fork","params":%s}`, id, params))
		line := p.expectLineContaining(fmt.Sprintf(`"id":%d`, id), testTimeout)
		var resp struct {
			Result struct {
				Thread struct {
					ID    string `json:"id"`
					Turns []struct {
						ID string `json:"id"`
					} `json:"turns"`
				} `json:"thread"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("decode thread/fork response %q: %v", line, err)
		}
		if resp.Error != nil {
			return "", nil, resp.Error.Message
		}
		for _, turn := range resp.Result.Thread.Turns {
			turnIDs = append(turnIDs, turn.ID)
		}
		return resp.Result.Thread.ID, turnIDs, ""
	}

	anchored, turns, rpcErr := fork(t, 4, `{"threadId":"mock-codex-thread","lastTurnId":"turn-1"}`)
	if rpcErr != "" || len(turns) != 1 || turns[0] != "turn-1" {
		t.Fatalf("anchored fork = %q / %v / err %q, want the turn-1 cut", anchored, turns, rpcErr)
	}

	full, turns, rpcErr := fork(t, 5, `{"threadId":"mock-codex-thread"}`)
	if rpcErr != "" || len(turns) != 2 || turns[1] != "turn-2" {
		t.Fatalf("anchorless fork = %q / %v / err %q, want every begun turn", full, turns, rpcErr)
	}
	if full == anchored {
		t.Errorf("two forks answered with the same thread id %q", full)
	}

	if _, _, rpcErr = fork(t, 6, `{"threadId":"mock-codex-thread","lastTurnId":"turn-2"}`); rpcErr == "" {
		t.Error("fork anchored at the in-progress turn succeeded; codex refuses it")
	}
	if _, _, rpcErr = fork(t, 7, `{"threadId":"mock-codex-thread","lastTurnId":"turn-99"}`); rpcErr == "" {
		t.Error("fork anchored at an unknown turn succeeded")
	}

	p.closeStdinAndExpectExit(0, testTimeout)
}

func TestCodexApprovalRoundTrip(t *testing.T) {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-approval",
		Provider: scenario.ProviderCodex,
		Turns: []scenario.Turn{{Steps: []scenario.Step{{Approval: &scenario.ApprovalStep{
			ToolName: "command",
			Input:    json.RawMessage(`{"command":"rm -rf build"}`),
			OnAllow: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
				`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"cmd-1","type":"commandExecution","status":"completed"}}}`,
			}}}},
			OnDeny: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
				`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"cmd-1","type":"commandExecution","status":"failed"}}}`,
			}}}},
		}}}}},
	}

	run := func(t *testing.T, decision, wantStatus string) {
		env := writeScenarioFile(t, sc, "")
		p := startMock(t, []string{"app-server"}, env, t.TempDir())

		p.send(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{}}`)
		p.expectLineContaining(`"id":1`, testTimeout)
		p.send(`{"jsonrpc":"2.0","id":2,"method":"turn/start","params":{"threadId":"mock-codex-thread","input":[]}}`)
		p.expectLineContaining(`"id":2`, testTimeout)

		reqLine := p.expectLineContaining(`"method":"item/commandExecution/requestApproval"`, testTimeout)
		var req struct {
			ID     int64 `json:"id"`
			Params struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Command  string `json:"command"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(reqLine), &req); err != nil {
			t.Fatalf("parse approval request %q: %v", reqLine, err)
		}
		if req.ID == 0 || req.Params.Command != "rm -rf build" || req.Params.TurnID != "turn-1" {
			t.Fatalf("approval request = %+v", req)
		}

		// Answer in the exact shape the app writes (codex/approval.go).
		p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"decision":%q}}`, req.ID, decision))
		branch := p.expectLineContaining(`"id":"cmd-1"`, testTimeout)
		if !strings.Contains(branch, `"status":"`+wantStatus+`"`) {
			t.Fatalf("branch line = %q, want status %q", branch, wantStatus)
		}
		p.closeStdinAndExpectExit(0, testTimeout)
	}

	t.Run("accept", func(t *testing.T) { run(t, "accept", "completed") })
	t.Run("decline", func(t *testing.T) { run(t, "decline", "failed") })
}

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/harness/scenario"
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
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"th-42"}}}` {
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

func TestCodexThreadResumeEchoesRequestedID(t *testing.T) {
	env := writeScenarioFile(t, codexScenario(), "")
	p := startMock(t, []string{"app-server"}, env, t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/resume","params":{"threadId":"resumed-7"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"resumed-7"}}}` {
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

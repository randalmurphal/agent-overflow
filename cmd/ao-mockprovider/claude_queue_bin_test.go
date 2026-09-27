package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/scenario"
)

// Queued-message consumption (claude-wire.md §Queued-message consumption).
// A user envelope that arrives mid-turn is acked `queued` on arrival either
// way; scenario.ClaudeOptions.QueuedInputAtBoundary decides whether its
// init, replay echo and `started` follow at once (the default) or after the
// running turn's last frame (the CLI's turn-pickup flavor).

const claudeResultLine = `{"type":"result","subtype":"success","is_error":false}`

// claudeTurnTextLines is one streamed text block keyed by the turn number,
// so the two turns of a scenario never share a message id.
var claudeTurnTextLines = []string{
	`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-${TURN}","role":"assistant"}}}`,
	`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
	`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello turn ${TURN}"}}}`,
	`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
	`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`,
	`{"type":"assistant","message":{"id":"msg-${TURN}","role":"assistant","content":[{"type":"text","text":"hello turn ${TURN}"}]}}`,
}

func queuedUserLine(uuid, text string) string {
	return `{"type":"user","uuid":"` + uuid + `","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}]}}`
}

// claudeHeldTurnScenario holds turn 1 open at "hold" after its text block;
// turn 2 answers with a text block of its own.
func claudeHeldTurnScenario(atBoundary bool) *scenario.Scenario {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "queued-input",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{
			{Label: "held", Steps: []scenario.Step{
				{Emit: &scenario.EmitStep{Lines: claudeTurnTextLines}},
				{WaitSignal: &scenario.WaitSignalStep{Name: "hold"}},
				{Emit: &scenario.EmitStep{Lines: []string{claudeResultLine}}},
			}},
			{Label: "reply", Steps: []scenario.Step{
				{Emit: &scenario.EmitStep{Lines: append(append([]string(nil), claudeTurnTextLines...), claudeResultLine)}},
			}},
		},
		AfterTurns: "silent",
	}
	if atBoundary {
		sc.Claude = &scenario.ClaudeOptions{QueuedInputAtBoundary: true}
	}
	return sc
}

func expectLifecycle(t *testing.T, line, commandUUID, state string) {
	t.Helper()
	var f struct {
		Type        string `json:"type"`
		CommandUUID string `json:"command_uuid"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal([]byte(line), &f); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if f.Type != "command_lifecycle" || f.CommandUUID != commandUUID || f.State != state {
		t.Fatalf("want command_lifecycle %s/%s, got %s", commandUUID, state, line)
	}
}

// expectNoLine fails on any stdout line within d: the negative half of a
// hold, which expectLine cannot state.
func (p *mockProc) expectNoLine(d time.Duration) {
	p.t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("stdout closed while expecting silence; stderr:\n%s", p.stderr.String())
		}
		p.all = append(p.all, line)
		p.t.Fatalf("unexpected stdout line during a hold: %s", line)
	case <-time.After(d):
	}
}

// startHeldTurn opens turn 1 from a user envelope and drains its frames up
// to the "hold" gate, so the next stdin line arrives mid-turn.
func startHeldTurn(t *testing.T, p *mockProc) {
	t.Helper()
	p.send(queuedUserLine("u1", "first"))
	expectLifecycle(t, p.expectLine(testTimeout), "u1", "queued")
	p.expectLineContaining(`"subtype":"init"`, testTimeout)
	p.expectLineContaining(`"isReplay":true`, testTimeout)
	expectLifecycle(t, p.expectLine(testTimeout), "u1", "started")
	p.expectLineContaining(`"text_delta","text":"hello turn 1"`, testTimeout)
	p.expectLineContaining(`"type":"assistant"`, testTimeout)
}

func expectTurnTwoPickup(t *testing.T, p *mockProc) {
	t.Helper()
	p.expectLineContaining(`"subtype":"init"`, testTimeout)
	echo := p.expectLine(testTimeout)
	if !strings.Contains(echo, `"isReplay":true`) || !strings.Contains(echo, `"uuid":"u2"`) {
		t.Fatalf("turn-2 echo = %q", echo)
	}
	expectLifecycle(t, p.expectLine(testTimeout), "u2", "started")
}

func TestClaudeQueuedInputAtBoundaryWaitsForTheRunningTurn(t *testing.T) {
	args := append(append([]string(nil), claudeSessionArgs...), "--resume", "sess-q")
	p, advance := startControlledMock(t, claudeHeldTurnScenario(true), args)
	// No turn running: the option changes nothing about the first pickup.
	startHeldTurn(t, p)

	// Mid-turn: the ack on arrival, then nothing until the turn ends.
	p.send(queuedUserLine("u2", "second"))
	expectLifecycle(t, p.expectLine(testTimeout), "u2", "queued")
	p.expectNoLine(300 * time.Millisecond)

	// The running turn's last frame precedes the held pickup.
	advance("hold")
	if got := p.expectLine(testTimeout); got != claudeResultLine {
		t.Fatalf("line after the hold = %q, want turn-1 result", got)
	}
	expectTurnTwoPickup(t, p)
	p.expectLineContaining(`"text_delta","text":"hello turn 2"`, testTimeout)
	p.expectLineContaining(`"type":"result"`, testTimeout)

	p.closeStdinAndExpectExit(0, testTimeout)
	validateClaudeFrames(t, p.all)
}

func TestClaudeQueuedInputIsPickedUpMidTurnByDefault(t *testing.T) {
	args := append(append([]string(nil), claudeSessionArgs...), "--resume", "sess-q")
	p, advance := startControlledMock(t, claudeHeldTurnScenario(false), args)
	startHeldTurn(t, p)

	// The pickup frames are written while turn 1 still holds; turn 2's
	// steps queue behind it.
	p.send(queuedUserLine("u2", "second"))
	expectLifecycle(t, p.expectLine(testTimeout), "u2", "queued")
	expectTurnTwoPickup(t, p)
	p.expectNoLine(300 * time.Millisecond)

	advance("hold")
	if got := p.expectLine(testTimeout); got != claudeResultLine {
		t.Fatalf("line after the hold = %q, want turn-1 result", got)
	}
	p.expectLineContaining(`"text_delta","text":"hello turn 2"`, testTimeout)
	p.expectLineContaining(`"type":"result"`, testTimeout)

	p.closeStdinAndExpectExit(0, testTimeout)
	validateClaudeFrames(t, p.all)
}

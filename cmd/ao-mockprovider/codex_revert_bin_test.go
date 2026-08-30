package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// revertScenario is codexScenario without the initialize override, so the
// mock reports its real (99.0.0) userAgent and a session built on it opens
// every version gate.
func revertScenario(threadID string) *scenario.Scenario {
	sc := codexScenario()
	sc.Name = "codex-revert"
	sc.Codex = &scenario.CodexOptions{ThreadID: threadID}
	return sc
}

// TestCodexThreadRevertCutsInPlace drives the whole in-place cut over the
// wire: the paginated opt-in on thread/start, the ThreadRevertResponse
// shape AO decodes, and the `thread/reverted` echo it waits on. The
// response is fed through the app's own parser rather than string-matched,
// because "the mock is truthful" means "the client accepts it".
func TestCodexThreadRevertCutsInPlace(t *testing.T) {
	env := writeScenarioFile(t, revertScenario("th-paginated"), "")
	p := startMock(t, []string{"app-server"}, env, t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"userAgent":"codex_cli_rs/`) {
		t.Fatalf("initialize response = %q, want a parseable app-server version", got)
	}
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"historyMode":"paginated"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"th-paginated","historyMode":"paginated"}}}` {
		t.Fatalf("thread/start response = %q", got)
	}

	// One completed turn, so the anchor below names real history.
	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"th-paginated","input":[]}}`)
	p.expectLineContaining(`"id":3`, testTimeout)
	p.expectLineContaining(`"method":"turn/completed"`, testTimeout)

	p.send(`{"jsonrpc":"2.0","id":4,"method":"thread/revert","params":{"threadId":"th-paginated","beforeTurnId":"turn-1"}}`)
	response := p.expectLineContaining(`"id":4`, testTimeout)
	var decoded struct {
		Result struct {
			Thread struct {
				ID          string `json:"id"`
				HistoryMode string `json:"historyMode"`
				Turns       []any  `json:"turns"`
			} `json:"thread"`
			TurnsBackwardsCursor *string `json:"turnsBackwardsCursor"`
			ItemsBackwardsCursor *string `json:"itemsBackwardsCursor"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response), &decoded); err != nil {
		t.Fatalf("decode thread/revert response %q: %v", response, err)
	}
	// Upstream's ThreadRevertResponse: the thread's identity echo (the one
	// field Revert validates, because an answer naming a different thread
	// would mean AO's SessionRef no longer names the thread that was cut),
	// an EMPTY turns array (re-hydration is a separate list call), and both
	// backwards cursors present as explicit nulls.
	if decoded.Result.Thread.ID != "th-paginated" ||
		decoded.Result.Thread.HistoryMode != "paginated" ||
		len(decoded.Result.Thread.Turns) != 0 {
		t.Fatalf("thread/revert response = %q, want upstream's shape", response)
	}
	if decoded.Result.TurnsBackwardsCursor != nil || decoded.Result.ItemsBackwardsCursor != nil ||
		!strings.Contains(response, `"turnsBackwardsCursor":null`) ||
		!strings.Contains(response, `"itemsBackwardsCursor":null`) {
		t.Fatalf("thread/revert cursors = %q, want both present and null", response)
	}

	// The echo follows the response on the same connection; Revert's wait
	// is armed before the request precisely because of that ordering.
	echo := p.expectLineContaining(`"method":"thread/reverted"`, testTimeout)
	if !strings.Contains(echo, `"threadId":"th-paginated"`) {
		t.Fatalf("thread/reverted = %q", echo)
	}

	p.closeStdinAndExpectExit(0, testTimeout)
}

// TestCodexThreadRevertOwnsActiveTurnShutdown pins the live Esc-un-send wire
// order. The active turn's terminal notification precedes the revert response,
// and the skipped scenario tail never runs.
func TestCodexThreadRevertOwnsActiveTurnShutdown(t *testing.T) {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-live-revert",
		Provider: scenario.ProviderCodex,
		Codex:    &scenario.CodexOptions{ThreadID: "th-live-revert"},
		Turns: []scenario.Turn{{Steps: []scenario.Step{
			{Emit: &scenario.EmitStep{Lines: []string{
				`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
			}}},
			{WaitSignal: &scenario.WaitSignalStep{Name: "hold"}},
			{Emit: &scenario.EmitStep{Lines: []string{`{"jsonrpc":"2.0","method":"must/not/run"}`}}},
		}}},
	}
	p := startMock(t, []string{"app-server"}, writeScenarioFile(t, sc, ""), t.TempDir())
	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"historyMode":"paginated"}}`)
	p.expectLineContaining(`"id":2`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"th-live-revert","input":[]}}`)
	p.expectLineContaining(`"id":3`, testTimeout)
	p.expectLineContaining(`"method":"turn/started"`, testTimeout)

	p.send(`{"jsonrpc":"2.0","id":4,"method":"thread/revert","params":{"threadId":"th-live-revert","beforeTurnId":"turn-1"}}`)
	completed := p.expectLine(testTimeout)
	if !strings.Contains(completed, `"method":"turn/completed"`) ||
		!strings.Contains(completed, `"status":"interrupted"`) {
		t.Fatalf("first live-revert frame = %q, want interrupted turn completion", completed)
	}
	if response := p.expectLine(testTimeout); !strings.Contains(response, `"id":4`) ||
		!strings.Contains(response, `"result"`) {
		t.Fatalf("thread/revert response = %q", response)
	}
	p.expectLineContaining(`"method":"thread/reverted"`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":5,"method":"thread/read","params":{}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"id":5`) || strings.Contains(got, "must/not/run") {
		t.Fatalf("line after live revert = %q; interrupted scenario tail ran", got)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
}

// TestCodexThreadRevertRefusesALegacyThread pins upstream's pre-mutation
// refusal verbatim. AO falls back to `thread/fork` on it, and that
// fallback is only safe because the refusal happens before the handler
// touches the thread.
func TestCodexThreadRevertRefusesALegacyThread(t *testing.T) {
	env := writeScenarioFile(t, revertScenario("th-legacy"), "")
	p := startMock(t, []string{"app-server"}, env, t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	// No historyMode: upstream's default, and the reason AO's own threads
	// were never revertible until it started asking.
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"historyMode":"legacy"`) {
		t.Fatalf("thread/start response = %q, want the legacy default", got)
	}
	p.send(`{"jsonrpc":"2.0","id":3,"method":"thread/revert","params":{"threadId":"th-legacy","beforeTurnId":"turn-1"}}`)
	refusal := p.expectLineContaining(`"id":3`, testTimeout)
	if !strings.Contains(refusal, `"code":-32600`) ||
		!strings.Contains(refusal, "thread/revert only supports paginated threads") {
		t.Fatalf("legacy refusal = %q", refusal)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
}

// TestCodexThreadHistoryModeSurvivesTheProcess is the property cold and
// fork-fallback rollbacks depend on: the throwaway resume is a second mock
// process that never saw thread/start. A history mode held only in memory
// would read as legacy there and send a paginated thread down the fork
// fallback silently, since both cuts "work".
func TestCodexThreadHistoryModeSurvivesTheProcess(t *testing.T) {
	home := t.TempDir()
	env := append(writeScenarioFile(t, revertScenario("th-durable"), ""),
		control.EnvTranscriptHome+"="+home)

	starter := startMock(t, []string{"app-server"}, env, t.TempDir())
	starter.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	starter.expectLineContaining(`"id":1`, testTimeout)
	starter.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"historyMode":"paginated"}}`)
	starter.expectLineContaining(`"historyMode":"paginated"`, testTimeout)
	starter.closeStdinAndExpectExit(0, testTimeout)

	resumer := startMock(t, []string{"app-server"}, env, t.TempDir())
	resumer.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	resumer.expectLineContaining(`"id":1`, testTimeout)
	resumer.send(`{"jsonrpc":"2.0","id":2,"method":"thread/resume","params":{"threadId":"th-durable"}}`)
	if got := resumer.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"th-durable","historyMode":"paginated"}}}` {
		t.Fatalf("thread/resume response = %q, want the started mode", got)
	}

	// And the resumed process believes an anchor from before it attached:
	// its own turn ledger is empty, which is ignorance, not evidence.
	resumer.send(`{"jsonrpc":"2.0","id":3,"method":"thread/revert","params":{"threadId":"th-durable","beforeTurnId":"turn-7"}}`)
	if got := resumer.expectLineContaining(`"id":3`, testTimeout); !strings.Contains(got, `"result"`) {
		t.Fatalf("revert of a pre-resume anchor = %q, want the cut", got)
	}
	resumer.expectLineContaining(`"method":"thread/reverted"`, testTimeout)

	// A thread this process STARTED is different: it ran the whole
	// history, so an anchor it does not recognise is nonsense.
	fresh := startMock(t, []string{"app-server"}, env, t.TempDir())
	fresh.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	fresh.expectLineContaining(`"id":1`, testTimeout)
	fresh.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"historyMode":"paginated"}}`)
	fresh.expectLineContaining(`"id":2`, testTimeout)
	fresh.send(`{"jsonrpc":"2.0","id":3,"method":"thread/revert","params":{"threadId":"th-durable","beforeTurnId":"turn-7"}}`)
	if got := fresh.expectLineContaining(`"id":3`, testTimeout); !strings.Contains(got, `"error"`) {
		t.Fatalf("revert of an unknown anchor on a started thread = %q, want a refusal", got)
	}
	fresh.closeStdinAndExpectExit(0, testTimeout)
	resumer.closeStdinAndExpectExit(0, testTimeout)
}

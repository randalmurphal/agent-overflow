package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// terminalStarted is the item/started a unified-exec PTY announces itself
// with, in the captured shape (codex-wire.md §Background terminals).
func terminalStarted(itemID, processID, command string) string {
	return `{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"` + itemID +
		`","type":"commandExecution","source":"unifiedExecStartup","processId":"` + processID +
		`","status":"inProgress","command":"` + command + `","cwd":"/home/user/project","exitCode":null,"durationMs":null}}}`
}

// codexTerminalsMock starts a mock whose first turn starts two background
// terminals and a foreground command, then holds until interrupted.
func codexTerminalsMock(t *testing.T) *mockProc {
	t.Helper()
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-terminals",
		Provider: scenario.ProviderCodex,
		Turns: []scenario.Turn{{Steps: []scenario.Step{
			{Emit: &scenario.EmitStep{Lines: []string{
				`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
				terminalStarted("shell-b", "78", "tail -f b.log"),
				terminalStarted("shell-a", "9", "tail -f a.log"),
				`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"fg-1","type":"commandExecution","source":"agent","processId":"12","status":"inProgress","command":"ls"}}}`,
				`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"done-1","type":"commandExecution","source":"unifiedExecStartup","processId":"5","status":"inProgress","command":"true"}}}`,
				`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"done-1","type":"commandExecution","source":"unifiedExecStartup","processId":"5","status":"completed","command":"true","exitCode":0}}}`,
			}}},
			{WaitSignal: &scenario.WaitSignalStep{Name: "hold"}},
		}}},
		Codex: &scenario.CodexOptions{ThreadID: "th-terminals"},
	}
	p := startMock(t, []string{"app-server"}, writeScenarioFile(t, sc, ""), t.TempDir())
	p.send(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{}}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"turn/start","params":{"threadId":"th-terminals","input":[]}}`)
	p.expectLineContaining(`"id":2`, testTimeout)
	p.expectLineContaining(`"id":"done-1","type":"commandExecution","source":"unifiedExecStartup","processId":"5","status":"completed"`, testTimeout)
	return p
}

type killedFrame struct {
	Method string `json:"method"`
	Params struct {
		ThreadID      string         `json:"threadId"`
		TurnID        string         `json:"turnId"`
		CompletedAtMs int64          `json:"completedAtMs"`
		Item          map[string]any `json:"item"`
	} `json:"params"`
}

// expectTerminalKilled reads the item/completed the app-server writes for a
// terminal it killed and checks it against the capture and the app's own
// classifier.
func expectTerminalKilled(t *testing.T, p *mockProc, itemID string) {
	t.Helper()
	line := p.expectLine(testTimeout)
	var frame killedFrame
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	item := frame.Params.Item
	if frame.Method != "item/completed" || item["id"] != itemID || item["type"] != "commandExecution" ||
		item["source"] != "unifiedExecStartup" || item["status"] != "failed" || item["exitCode"] != float64(-1) ||
		frame.Params.ThreadID != "th-terminals" || frame.Params.TurnID != "turn-1" || frame.Params.CompletedAtMs == 0 {
		t.Fatalf("kill frame = %s, want %s's item/completed failed with exit -1 under turn-1", line, itemID)
	}
	if _, ok := item["durationMs"].(float64); !ok {
		t.Fatalf("kill frame = %s, want a durationMs", line)
	}
	if !strings.Contains(line, `"cwd":"/home/user/project"`) {
		t.Fatalf("kill frame = %s, want the started item's fields repeated", line)
	}
	raw, err := json.Marshal(frame.Params)
	if err != nil {
		t.Fatal(err)
	}
	events := codex.ClassifyNotification("thread-terminals", frame.Method, raw)
	if len(events) != 1 || events[0].Kind != provider.EventToolComplete || events[0].ItemID != itemID {
		t.Fatalf("kill frame events = %+v, want one tool completion for %s", events, itemID)
	}
}

func expectTerminalList(t *testing.T, p *mockProc, id int, want ...string) {
	t.Helper()
	line := p.expectLine(testTimeout)
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			Data []struct {
				ItemID    string `json:"itemId"`
				ProcessID string `json:"processId"`
				Command   string `json:"command"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil || resp.ID != id || resp.Result.NextCursor != nil {
		t.Fatalf("list response = %q (%v)", line, err)
	}
	var got []string
	for _, row := range resp.Result.Data {
		got = append(got, row.ProcessID+":"+row.ItemID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("listed terminals = %v, want %v", got, want)
	}
}

// The captured codex-cli 0.159.1 shapes (fixtures/codex/*_20261015.ndjson):
// terminate answers, then writes the killed command's item/completed under
// the turn it started in; a process id no running terminal has answers
// false and writes nothing; list follows the running set, sorted by process
// id; an interrupt kills no terminal.
func TestCodexBackgroundTerminalTerminate(t *testing.T) {
	p := codexTerminalsMock(t)

	p.send(`{"jsonrpc":"2.0","id":3,"method":"thread/backgroundTerminals/list","params":{"threadId":"th-terminals"}}`)
	expectTerminalList(t, p, 3, "9:shell-a", "78:shell-b")

	p.send(`{"jsonrpc":"2.0","id":4,"method":"thread/backgroundTerminals/terminate","params":{"threadId":"th-terminals"}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"code":-32600`) || !strings.Contains(got, "missing field `processId`") {
		t.Fatalf("terminate without a process id = %q", got)
	}
	p.send(`{"jsonrpc":"2.0","id":5,"method":"thread/backgroundTerminals/terminate","params":{"threadId":"th-terminals","processId":"abc"}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"code":-32600`) || !strings.Contains(got, "invalid background terminal process id") {
		t.Fatalf("terminate of a non-numeric process id = %q", got)
	}
	for id, params := range map[int]string{
		6: `{"threadId":"th-terminals","processId":"999"}`,
		7: `{"threadId":"other-thread","processId":"78"}`,
		8: `{"threadId":"th-terminals","processId":"5"}`,
	} {
		p.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"thread/backgroundTerminals/terminate","params":` + params + `}`)
		if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"result":{"terminated":false}}` {
			t.Fatalf("terminate %s = %q, want terminated false", params, got)
		}
	}

	// The turn is still running: the kill lands under it and the turn goes on.
	p.send(`{"jsonrpc":"2.0","id":9,"method":"thread/backgroundTerminals/terminate","params":{"threadId":"th-terminals","processId":"78"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":9,"result":{"terminated":true}}` {
		t.Fatalf("terminate response = %q", got)
	}
	expectTerminalKilled(t, p, "shell-b")
	p.send(`{"jsonrpc":"2.0","id":10,"method":"thread/backgroundTerminals/terminate","params":{"threadId":"th-terminals","processId":"78"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":10,"result":{"terminated":false}}` {
		t.Fatalf("second terminate = %q, want terminated false", got)
	}

	// An interrupt ends the turn and leaves the other terminal running; its
	// later kill still names the turn it started in.
	p.send(`{"jsonrpc":"2.0","id":11,"method":"turn/interrupt","params":{"threadId":"th-terminals","turnId":"turn-1"}}`)
	p.expectLineContaining(`"id":11`, testTimeout)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"status":"interrupted"`) {
		t.Fatalf("line after the interrupt answer = %q, want the interrupted turn/completed alone", got)
	}
	p.send(`{"jsonrpc":"2.0","id":12,"method":"thread/backgroundTerminals/list","params":{"threadId":"th-terminals"}}`)
	expectTerminalList(t, p, 12, "9:shell-a")
	p.send(`{"jsonrpc":"2.0","id":13,"method":"thread/backgroundTerminals/terminate","params":{"threadId":"th-terminals","processId":"9"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":13,"result":{"terminated":true}}` {
		t.Fatalf("terminate after the interrupt = %q", got)
	}
	expectTerminalKilled(t, p, "shell-a")
	p.send(`{"jsonrpc":"2.0","id":14,"method":"thread/backgroundTerminals/list","params":{"threadId":"th-terminals"}}`)
	expectTerminalList(t, p, 14)
	p.closeStdinAndExpectExit(0, testTimeout)
}

// clean answers `{}`, then writes the item/completed of every running
// terminal; a second clean finds none.
func TestCodexBackgroundTerminalsClean(t *testing.T) {
	p := codexTerminalsMock(t)
	p.send(`{"jsonrpc":"2.0","id":3,"method":"thread/backgroundTerminals/clean","params":{"threadId":"th-terminals"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":3,"result":{}}` {
		t.Fatalf("clean response = %q", got)
	}
	expectTerminalKilled(t, p, "shell-b")
	expectTerminalKilled(t, p, "shell-a")
	p.send(`{"jsonrpc":"2.0","id":4,"method":"thread/backgroundTerminals/clean","params":{"threadId":"th-terminals"}}`)
	if got := p.expectLine(testTimeout); got != `{"jsonrpc":"2.0","id":4,"result":{}}` {
		t.Fatalf("second clean response = %q", got)
	}
	p.send(`{"jsonrpc":"2.0","id":5,"method":"thread/backgroundTerminals/list","params":{"threadId":"th-terminals"}}`)
	expectTerminalList(t, p, 5)
	p.closeStdinAndExpectExit(0, testTimeout)
}

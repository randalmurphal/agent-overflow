package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// codexTerminalLedger follows the unified-exec PTYs this process announced,
// off every frame it wrote (scenario emits included), so the background
// terminal RPCs answer with what the real app-server writes for them.
//
// Modeled on the codex-cli 0.159.1 captures behind codex-wire.md
// §Background terminals (docs/references/fixtures/codex/*_20261015.ndjson):
//
//   - a running terminal is a `commandExecution` item/started with
//     `source:"unifiedExecStartup"` and a `processId`, until an
//     item/completed for the same item id;
//   - `thread/backgroundTerminals/terminate` answers `{"terminated":true}`
//     and then writes the killed command's item/completed: the started item
//     with `status:"failed"`, `exitCode:-1` and `durationMs` since its start,
//     under the turn id it started in, whether that turn still runs or ended
//     long ago. Nothing else follows, and a running turn goes on;
//   - a process id that matches no running terminal of the thread answers
//     `{"terminated":false}` and writes nothing; a missing one, or one that
//     is not a number, is refused with -32600 and upstream's message;
//   - `thread/backgroundTerminals/clean` answers `{}` and then writes that
//     item/completed for every running terminal (upstream drains a map, so
//     the order is unspecified; this writes them in start order);
//   - `thread/backgroundTerminals/list` lists the running terminals sorted by
//     process id, each with the model's command rather than the item's
//     shell-wrapped one;
//   - `turn/interrupt` kills none of them.
//
// Guarded by the writer's lock: observe runs as the writer's observer, and
// every read happens inside lineWriter.locked.
type codexTerminalLedger struct {
	running []*codexTerminal
}

type codexTerminal struct {
	threadID  string
	turnID    string
	itemID    string
	processID string
	// item is the item object its item/started carried, which the kill's
	// item/completed repeats with the terminal fields.
	item      map[string]any
	startedAt time.Time
}

// observe reads one written frame. Called under the writer lock, in wire
// order.
func (l *codexTerminalLedger) observe(line string) {
	if !strings.Contains(line, `"commandExecution"`) {
		return
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string         `json:"threadId"`
			TurnID   string         `json:"turnId"`
			Item     map[string]any `json:"item"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &frame) != nil || frame.Params.Item == nil {
		return
	}
	item := frame.Params.Item
	itemID, _ := item["id"].(string)
	if item["type"] != "commandExecution" || itemID == "" {
		return
	}
	switch frame.Method {
	case "item/started":
		processID, _ := item["processId"].(string)
		if item["source"] != "unifiedExecStartup" || processID == "" {
			return
		}
		l.remove(itemID)
		l.running = append(l.running, &codexTerminal{
			threadID: frame.Params.ThreadID, turnID: frame.Params.TurnID,
			itemID: itemID, processID: processID, item: item, startedAt: time.Now(),
		})
	case "item/completed":
		l.remove(itemID)
	}
}

func (l *codexTerminalLedger) remove(itemID string) {
	for i, terminal := range l.running {
		if terminal.itemID == itemID {
			l.running = append(l.running[:i], l.running[i+1:]...)
			return
		}
	}
}

// ofThread returns the running terminals of threadID, in start order.
func (l *codexTerminalLedger) ofThread(threadID string) []*codexTerminal {
	var out []*codexTerminal
	for _, terminal := range l.running {
		if terminal.threadID == threadID {
			out = append(out, terminal)
		}
	}
	return out
}

// killedLine is the item/completed the app-server writes for a terminal it
// killed.
func (t *codexTerminal) killedLine(now time.Time) string {
	item := make(map[string]any, len(t.item)+3)
	for k, v := range t.item {
		item[k] = v
	}
	item["status"] = "failed"
	item["exitCode"] = -1
	item["durationMs"] = now.Sub(t.startedAt).Milliseconds()
	return mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  "item/completed",
		"params": map[string]any{
			"item": item, "threadId": t.threadID, "turnId": t.turnID,
			"completedAtMs": now.UnixMilli(),
		},
	})
}

// handleTerminalRequest answers the background terminal RPCs from the
// ledger. Returns false when method is not one of them.
func (a *codexAdapter) handleTerminalRequest(id json.RawMessage, method string, params json.RawMessage) bool {
	switch method {
	case "thread/backgroundTerminals/list", "thread/backgroundTerminals/terminate", "thread/backgroundTerminals/clean":
	default:
		return false
	}
	threadID := readParamString(params, "threadId")
	processID := readParamString(params, "processId")
	if method == "thread/backgroundTerminals/terminate" {
		if processID == "" {
			a.writeRPCError(id, -32600, "Invalid request: missing field `processId`")
			return true
		}
		if _, err := strconv.ParseInt(processID, 10, 32); err != nil {
			a.writeRPCError(id, -32600, "invalid background terminal process id: invalid digit found in string")
			return true
		}
	}
	a.w.locked(func(write func(string)) {
		running := a.terminals.ofThread(threadID)
		now := time.Now()
		switch method {
		case "thread/backgroundTerminals/list":
			write(mustJSON(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"data": terminalListRows(running), "nextCursor": nil},
			}))
		case "thread/backgroundTerminals/terminate":
			var killed *codexTerminal
			for _, terminal := range running {
				if terminal.processID == processID {
					killed = terminal
					break
				}
			}
			write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"terminated":%t}}`, id, killed != nil))
			if killed != nil {
				write(killed.killedLine(now))
			}
		case "thread/backgroundTerminals/clean":
			write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, id))
			for _, terminal := range running {
				write(terminal.killedLine(now))
			}
		}
	})
	return true
}

// modelCommand is the command as the model wrote it, which a list row
// carries (upstream's `hook_command`). The item's `command` is the
// shell-wrapped form (`/usr/bin/zsh -lc "..."`); the model's own string is
// on the item's `commandActions`. An item with no actions has only its
// `command` to give.
func (t *codexTerminal) modelCommand() string {
	if actions, ok := t.item["commandActions"].([]any); ok && len(actions) > 0 {
		if action, ok := actions[0].(map[string]any); ok {
			if command, ok := action["command"].(string); ok && command != "" {
				return command
			}
		}
	}
	command, _ := t.item["command"].(string)
	return command
}

// terminalListRows is the list answer's data: one row per running terminal,
// sorted by process id as upstream sorts them. The usage fields are null, as
// they are for a process the server has no sample of yet.
func terminalListRows(running []*codexTerminal) []map[string]any {
	sorted := slices.Clone(running)
	slices.SortStableFunc(sorted, func(x, y *codexTerminal) int {
		xi, _ := strconv.Atoi(x.processID)
		yi, _ := strconv.Atoi(y.processID)
		return cmp.Compare(xi, yi)
	})
	rows := make([]map[string]any, 0, len(sorted))
	for _, terminal := range sorted {
		cwd, _ := terminal.item["cwd"].(string)
		rows = append(rows, map[string]any{
			"itemId": terminal.itemID, "processId": terminal.processID, "command": terminal.modelCommand(), "cwd": cwd,
			"osPid": nil, "cpuPercent": nil, "rssKb": nil,
		})
	}
	return rows
}

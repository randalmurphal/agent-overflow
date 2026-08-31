package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// backgroundScriptCapture is a tiny helper that captures every line of
// stdin the fake CLI receives into a file so the test can assert what
// the session wrote (the `stop_task` control_request lines). The path
// is t.TempDir()-scoped so concurrent tests don't collide.
type backgroundScriptCapture struct {
	capturePath string
}

func newBackgroundScriptCapture(t *testing.T) *backgroundScriptCapture {
	t.Helper()
	return &backgroundScriptCapture{
		capturePath: filepath.Join(t.TempDir(), "stdin-capture.log"),
	}
}

// Lines returns every captured stdin line as a slice, stripping the
// trailing newline on each. Non-existent file returns an empty slice
// (the fake hasn't written anything yet).
func (b *backgroundScriptCapture) Lines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(b.capturePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read capture: %v", err)
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// writeClaudeBackgroundStopScript produces a shell script that behaves
// like Claude CLI for the backgrounded-Bash + stop_task contract:
//
//  1. First stdin line (the user message) produces a backgrounded
//     tool_use + placeholder tool_result + init + task_started. No
//     `result` yet — the turn "continues" while the backgrounded Bash
//     runs.
//  2. Each subsequent stdin line is captured to `capturePath`. If the
//     line is a stop_task control_request, the script replies with a
//     control_response{subtype:success} AND a follow-up
//     system/task_updated{status:killed} so the triage pipeline lands a
//     killed sibling.
//
// The task_id / tool_use_id pairs come from taskIDs (one tuple per
// backgrounded Bash). Each tuple drives one task_started + one killed
// task_updated once the stop lands.
func writeClaudeBackgroundStopScript(t *testing.T, dir, capturePath string, pairs []struct{ ToolUseID, TaskID string }) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude-bg-stop.sh")

	// Build the first-turn reply: one tool_use per backgrounded Bash
	// plus its placeholder tool_result + task_started. The printf calls
	// are one-per-line to keep quoting simple.
	var firstTurnLines []string
	firstTurnLines = append(firstTurnLines,
		`{"type":"system","subtype":"init","session_id":"sess-bg","model":"claude-opus-4-7","cwd":"/tmp","tools":["Bash"],"claude_code_version":"1.0"}`,
	)
	for i, p := range pairs {
		msgID := fmt.Sprintf("msg-%d", i+1)
		firstTurnLines = append(firstTurnLines,
			fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"sleep 3600","run_in_background":true}}]}}`, msgID, p.ToolUseID),
			fmt.Sprintf(`{"type":"user","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"backgroundTaskId":%q},"message":{"role":"user","content":[{"tool_use_id":%q,"type":"tool_result","content":"Command running in background with ID: %s. Output: /tmp/%s.out","is_error":false}]}}`, p.TaskID, p.ToolUseID, p.TaskID, p.TaskID),
			fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":%q,"tool_use_id":%q}`, p.TaskID, p.ToolUseID),
		)
	}
	firstTurnLines = append(firstTurnLines, `{"type":"result","subtype":"success","is_error":false}`)

	var firstPrintfs strings.Builder
	for _, line := range firstTurnLines {
		firstPrintfs.WriteString("  printf '%s\\n' ")
		firstPrintfs.WriteString(shellSingleQuoteForBackground(line))
		firstPrintfs.WriteString("\n")
	}

	// Per-task_id bash case arms for the stop_task matcher. We use the
	// CAPTURED task_id (not the task_id of ANY stop request) so a
	// stop_task for task-A emits a killed task_updated for task-A only.
	// We also match the request_id echoed back into the control_response.
	//
	// One outer `case "$line" in ... esac` holds every pair's match arm
	// — each arm is a single pattern + body + `;;`. Prior shape
	// generated one case-per-pair, which is malformed bash (nested
	// `case` blocks without intervening `;;` before the next `case`).
	var stopCaseBuilder strings.Builder
	stopCaseBuilder.WriteString("        case \"$line\" in\n")
	for _, p := range pairs {
		stopCaseBuilder.WriteString(fmt.Sprintf("          *'\"task_id\":%q'*)\n", p.TaskID))
		// Echo a success control_response carrying the request_id.
		stopCaseBuilder.WriteString(
			`            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')` + "\n",
		)
		stopCaseBuilder.WriteString(
			fmt.Sprintf(`            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$reqid"` + "\n"),
		)
		// And the follow-up system/task_updated killed so triage flips
		// the sibling row to status=killed.
		stopCaseBuilder.WriteString(
			fmt.Sprintf(`            printf '{"type":"system","subtype":"task_updated","task_id":%q,"tool_use_id":%q,"patch":{"status":"killed"}}\n'`+"\n", p.TaskID, p.ToolUseID),
		)
		stopCaseBuilder.WriteString("            ;;\n")
	}
	stopCaseBuilder.WriteString("        esac\n")

	script := `#!/bin/bash
set -u
capture=` + shellSingleQuoteForBackground(capturePath) + `
idx=0
while IFS= read -r line; do
  # Always log to the capture file, including the first user line, so
  # tests can reconstruct the full stdin trace.
  printf '%s\n' "$line" >> "$capture"
  case $idx in
    0)
` + firstPrintfs.String() + `      ;;
    *)
      case "$line" in
        *'"stop_task"'*)
` + stopCaseBuilder.String() + `          ;;
      esac
      ;;
  esac
  idx=$((idx+1))
done
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude bg stop script: %v", err)
	}
	return path
}

// shellSingleQuoteForBackground is the file-local shell escaper used by
// writeClaudeBackgroundStopScript. Kept distinct from the e2e_lifecycle
// file's helper so the two can evolve independently — the helpers in
// this file are scoped to the background scenarios.
func shellSingleQuoteForBackground(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitUntilE2E polls until predicate returns true or deadline passes.
// Shared with app_e2e_lifecycle_test.go's waitUntil in spirit but
// keeps this file self-contained — no dependency on the
// sibling test file's helpers.
func waitUntilE2E(t *testing.T, d time.Duration, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitUntilE2E: %s still false after %v", description, d)
}

// seedBackgroundLaunchRow seeds a `tool_call` row that looks like a
// backgrounded launch Codex or Claude would have persisted — the shape
// the ghost-flip reconcile and the interrupt exemption care about.
// Kept local to this file so Phase 7 tests can stage arbitrary launch
// states without reaching into Phase 4 helpers from app_codex_reconcile_test.go.
func seedBackgroundLaunchRowE2E(
	t *testing.T, st *store.Store,
	threadID, itemID, toolName, summary string,
) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:           itemID,
		ThreadID:     threadID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		Summary:      summary,
		ToolName:     toolName,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed background launch %s: %v", itemID, err)
	}
}

func findItemsByKindE2E(t *testing.T, st *store.Store, threadID, kind string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items for %s: %v", threadID, err)
	}
	var matches []store.Item
	for _, item := range items {
		if item.Kind == kind {
			matches = append(matches, item)
		}
	}
	return matches
}

// findStopTaskLine returns the single stop_task control_request line
// from the captured stdin trace. Fails the test if zero or multiple
// matches exist.
func findStopTaskLine(t *testing.T, lines []string) string {
	t.Helper()
	var match string
	count := 0
	for _, line := range lines {
		if strings.Contains(line, `"stop_task"`) {
			match = line
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 stop_task line, got %d: %v", count, lines)
	}
	return match
}

// assertStopTaskWireShape pins the control_request envelope to the
// spike-verified shape. Parsing (rather than substring) makes the
// assertion robust to JSON key ordering.
func assertStopTaskWireShape(t *testing.T, line, wantTaskID string) {
	t.Helper()
	var raw struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
			TaskID  string `json:"task_id"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatalf("unmarshal stop_task line: %v (raw: %s)", err, line)
	}
	if raw.Type != "control_request" {
		t.Errorf("type = %q, want control_request", raw.Type)
	}
	if raw.RequestID == "" {
		t.Error("request_id must be populated so CLI can correlate response")
	}
	if raw.Request.Subtype != "stop_task" {
		t.Errorf("request.subtype = %q, want stop_task", raw.Request.Subtype)
	}
	if raw.Request.TaskID != wantTaskID {
		t.Errorf("request.task_id = %q, want %q", raw.Request.TaskID, wantTaskID)
	}
}

// writeSilentClaudeBinary emits a shell script that behaves like Claude
// CLI for the interrupt tests: it consumes stdin silently and exits
// when stdin closes. Interrupt control_requests are auto-acked with a
// synthetic success control_response so the session takes the clean
// round-trip path instead of falling through to the kill fallback (a
// 10s timeout per test case).
func writeSilentClaudeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "silent-claude.sh")
	script := `#!/bin/bash
# Emit a minimal init so the session's spawn handshake is happy,
# then drain stdin until it closes. Auto-ack interrupt control_requests
# so Interrupt() doesn't hit its kill-fallback timeout.
printf '%s\n' '{"type":"system","subtype":"init","session_id":"silent","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
while IFS= read -r line; do
  # Match either field order: json.Marshal on map[string]any sorts
  # keys alphabetically, so "subtype" can land before "type".
  case "$line" in
    *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
      reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
      ;;
  esac
done
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write silent claude: %v", err)
	}
	return path
}

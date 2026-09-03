package sessionimport

import (
	"os"
	"path/filepath"
	"strings"
)

// Decoding for the `tool_result` half of a tool call: content flattening,
// exit codes, background-launch acks, and the detection of output Claude
// has since garbage-collected.

// toolResultText flattens a tool_result block's content: a plain string,
// or the text bodies of a block list. Unrecognised structured shapes fall
// back to their JSON so nothing is silently lost.
func toolResultText(content any) string {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var b strings.Builder
		for _, entry := range typed {
			block, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if rawString(block, "type") == "text" {
				b.WriteString(rawString(block, "text"))
			}
		}
		return b.String()
	default:
		if encoded := rawJSON(content); encoded != nil {
			return string(encoded)
		}
		return ""
	}
}

// toolResultExitCode reads a shell tool's exit code from either the
// structured `toolUseResult` sibling or the block content.
func toolResultExitCode(block map[string]any, toolUseResult any) (int, bool) {
	if m := rawMapValue(toolUseResult); m != nil {
		if code, ok := intAtAnyKey(m, "exit_code", "exitCode"); ok {
			return code, true
		}
	}
	if m := rawMapValue(block["content"]); m != nil {
		if code, ok := intAtAnyKey(m, "exit_code", "exitCode"); ok {
			return code, true
		}
	}
	return 0, false
}

// backgroundToolResult reports whether a tool_result is a placeholder for
// work that kept running: a backgrounded Bash (`backgroundTaskId`), an
// async agent launch ack (`isAsync` / `status:"async_launched"`), or a
// Monitor watch-task launch ack (`taskId` paired with `persistent` or
// `timeoutMs`). Mirrors the live parser's discriminators, including the
// rule that a bare `agentId` or a bare `taskId` proves nothing.
func backgroundToolResult(toolUseResult any) bool {
	m := rawMapValue(toolUseResult)
	if m == nil {
		return false
	}
	if strings.TrimSpace(rawString(m, "backgroundTaskId")) != "" {
		return true
	}
	if rawBool(m, "isAsync") || rawString(m, "status") == "async_launched" {
		return true
	}
	if strings.TrimSpace(rawString(m, "taskId")) != "" {
		_, persistent := m["persistent"]
		_, timeout := m["timeoutMs"]
		if persistent || timeout {
			return true
		}
	}
	return false
}

func skillForkResult(toolUseResult any) (agentID, commandName string, ok bool) {
	m := rawMapValue(toolUseResult)
	if m == nil || rawString(m, "status") != "forked" {
		return "", "", false
	}
	agentID = strings.TrimSpace(rawString(m, "agentId"))
	if agentID == "" {
		return "", "", false
	}
	return agentID, strings.TrimSpace(rawString(m, "commandName")), true
}

// Markers Claude leaves behind when a tool result was too large to inline
// (toolResultStorage.ts). The preview stays in the transcript; the full
// body lives in `<sessionDir>/tool-results/` until housekeeping removes it.
const (
	toolResultClearedMessage  = "[Old tool result content cleared]"
	persistedOutputTag        = "<persisted-output>"
	persistedOutputPathMarker = "Full output saved to: "
	toolResultsSubdir         = "tool-results"
)

// toolOutputUnavailable reports whether a tool result's real output is
// gone. The cleared marker says so outright; an externalised result is
// only unavailable once its file is actually missing, which is checked
// both at the recorded absolute path (the same machine) and under the
// session's own sidecar directory (a transcript that moved).
func (c *converter) toolOutputUnavailable(content string) bool {
	if strings.Contains(content, toolResultClearedMessage) {
		return true
	}
	if !strings.Contains(content, persistedOutputTag) {
		return false
	}
	path := externalisedOutputPath(content)
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err == nil {
		return false
	}
	if c.opts.SessionDir == "" {
		// Without the session directory there is no second place to look,
		// and guessing "gone" from an absolute path written on another
		// machine would mark healthy imports unavailable.
		return false
	}
	local := filepath.Join(c.opts.SessionDir, toolResultsSubdir, filepath.Base(path))
	_, err := os.Stat(local)
	return err != nil
}

func externalisedOutputPath(content string) string {
	idx := strings.Index(content, persistedOutputPathMarker)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(persistedOutputPathMarker):]
	if end := strings.IndexByte(rest, '\n'); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// backgroundAckPrefix and backgroundAckTaskIDMarker pin the Bash
// backgrounding ack's text. The CLI (BashTool.tsx, 2.1.88 source; the
// "running in background" literal is still present in the 2.1.257
// binary) has three variants, one per trigger, and all three open with
// "Command " and name the task as ` with ID: <id>` on their first line:
//
//	Command running in background with ID: b20hid1oz. Output is being written to: …
//	Command exceeded the assistant-mode blocking budget (…) and was moved to the background with ID: …
//	Command was manually backgrounded by user with ID: …
//
// The prefix is matched on the flattened body (a prefix, never a
// contains — quoted output does not classify), and the id is what the
// later `system/task_updated` / `task_notification` terminal carries as
// `task_id`.
const backgroundAckPrefix = "Command "
const backgroundAckTaskIDMarker = " with ID: "

// BackgroundAckTaskID recognises the Bash backgrounding ack from the
// tool_result TEXT alone and recovers the task id it names
// (claude-wire.md §E2b). It exists for a SIDECHAIN Bash launch, where
// Claude omits the `toolUseResult` envelope and therefore the
// `backgroundTaskId` marker, and it is the one rule the live parser and
// this reader share: both consult it only when no structured sibling is
// present AND the launch asked for backgrounding.
//
// Returning ("", false) means "not a backgrounding ack": the launch
// settles in place with the result it actually got. That is the correct
// reading of a refused command (hook deny, permission denial) and the
// tolerable reading of a reworded ack — an instantly-done row is
// recoverable, a permanently-running one blocks the reaper and the
// flush queue.
func BackgroundAckTaskID(text string) (taskID string, ok bool) {
	if !strings.HasPrefix(text, backgroundAckPrefix) {
		return "", false
	}
	first := text
	if idx := strings.IndexByte(first, '\n'); idx >= 0 {
		first = first[:idx]
	}
	_, rest, found := strings.Cut(first, backgroundAckTaskIDMarker)
	if !found {
		return "", false
	}
	if id := leadingTaskID(rest); id != "" {
		return id, true
	}
	return "", false
}

// leadingTaskID returns the longest prefix of s made of the characters
// a Claude task id is spelled with (lowercase alphanumerics, `-`, `_`).
// Observed ids are nine lowercase alphanumerics (`b20hid1oz`), but the
// wire promises no length, and the terminator is whatever punctuation
// the ack puts after the id (`.` today).
func leadingTaskID(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || c == '-' || c == '_' {
			continue
		}
		return s[:i]
	}
	return s
}

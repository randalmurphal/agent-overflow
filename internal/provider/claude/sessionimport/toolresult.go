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

package sessionimport

import (
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
)

func TestConvertSubagentRowsNestUnderTheirTask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		userRow("u1", "", "delegate it", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			toolUseBlock("toolu_task", "Task", map[string]any{"description": "do work"}),
		}, "2026-01-01T00:00:01.000Z"),
		toolResultRow("r1", "a1", "toolu_task", "agent finished", "2026-01-01T00:00:05.000Z",
			with("toolUseResult", map[string]any{"agentId": "agent7", "status": "completed"})),
	)
	writeJSONL(t, filepath.Join(dir, sessionA, subagentsSubdir, "agent-agent7.jsonl"),
		userRow("s1", "", "the task prompt", "2026-01-01T00:00:02.000Z", with("isSidechain", true)),
		assistantRow("s2", "s1", "msg_sub", []any{textBlock("subagent thinking out loud")},
			"2026-01-01T00:00:03.000Z", with("isSidechain", true)),
		assistantRow("s3", "s2", "msg_sub2", []any{
			toolUseBlock("toolu_sub", "Read", map[string]any{"file_path": "/repo/a.go"}),
		}, "2026-01-01T00:00:04.000Z", with("isSidechain", true)),
	)

	got := renderEvents(loadBranch(t, path, 0, 1).Events)
	want := strings.Join([]string{
		`turn_start turn=1 item= parent= src=u1 content=""`,
		`user_text turn=1 item=u1 parent= src=u1 content="delegate it"`,
		`tool_start turn=1 item=toolu_task parent= src=a1 content=""`,
		`text_delta turn=1 item=msg_sub#0 parent=toolu_task src=s2 content="subagent thinking out loud"`,
		`tool_start turn=1 item=toolu_sub parent=toolu_task src=s3 content=""`,
		`tool_complete turn=1 item=toolu_task parent= src=r1 content="agent finished"`,
		`turn_complete turn=1 item= parent= src=r1 content=""`,
	}, "\n")
	if got != want {
		t.Errorf("events mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestConvertWarnsWhenSubagentTranscriptIsGone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		userRow("u1", "", "delegate it", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			toolUseBlock("toolu_task", "Task", map[string]any{"description": "do work"}),
		}, "2026-01-01T00:00:01.000Z"),
		toolResultRow("r1", "a1", "toolu_task", "agent finished", "2026-01-01T00:00:05.000Z",
			with("toolUseResult", map[string]any{"agentId": "vanished", "status": "completed"})),
	)
	branch := loadBranch(t, path, 0, 1)
	if !hasWarning(branch.Warnings, WarnMissingSubagent) {
		t.Errorf("warnings = %+v, want %s", branch.Warnings, WarnMissingSubagent)
	}
	if len(branch.Events) == 0 {
		t.Error("a missing subagent transcript must not drop the parent rows")
	}
}

func TestSubagentTranscriptPathRefusesTraversal(t *testing.T) {
	for _, agentID := range []string{"../escape", "sub/dir", `back\slash`, ".hidden", ""} {
		if _, ok := subagentTranscriptPath("/session", agentID); ok {
			t.Errorf("agentID %q was accepted as a path component", agentID)
		}
	}
	got, ok := subagentTranscriptPath("/session", "abc123")
	if !ok || got != filepath.Join("/session", subagentsSubdir, "agent-abc123.jsonl") {
		t.Errorf("subagentTranscriptPath = %q, %v", got, ok)
	}
}

func TestSubagentBindsToItsFirstLaunchOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	// An async agent is acked at launch and named AGAIN by a later resume
	// ack. Its transcript belongs under the launch, once.
	writeJSONL(t, path,
		userRow("u1", "", "delegate it", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			toolUseBlock("toolu_launch", "Agent", map[string]any{"description": "work"}),
		}, "2026-01-01T00:00:01.000Z"),
		toolResultRow("r1", "a1", "toolu_launch", "Async agent launched successfully.", "2026-01-01T00:00:02.000Z",
			with("toolUseResult", map[string]any{"isAsync": true, "status": "async_launched", "agentId": "agent7"})),
		assistantRow("a2", "r1", "msg_2", []any{
			toolUseBlock("toolu_resume", "SendMessage", map[string]any{"agentId": "agent7"}),
		}, "2026-01-01T00:00:03.000Z"),
		toolResultRow("r2", "a2", "toolu_resume", "resumed", "2026-01-01T00:00:04.000Z",
			with("toolUseResult", map[string]any{"agentId": "agent7", "status": "completed"})),
	)
	writeJSONL(t, filepath.Join(dir, sessionA, subagentsSubdir, "agent-agent7.jsonl"),
		userRow("s1", "", "the task prompt", "2026-01-01T00:00:02.500Z", with("isSidechain", true)),
		assistantRow("s2", "s1", "msg_sub", []any{textBlock("agent output")},
			"2026-01-01T00:00:03.500Z", with("isSidechain", true)),
	)

	branch := loadBranch(t, path, 0, 1)
	var nested []importir.Event
	for _, e := range branch.Events {
		if e.ParentToolUseID != "" {
			nested = append(nested, e)
		}
	}
	if len(nested) != 1 {
		t.Fatalf("subagent rows emitted %d times, want 1: %s", len(nested), renderEvents(branch.Events))
	}
	if nested[0].ParentToolUseID != "toolu_launch" {
		t.Errorf("nested under %q, want the launch tool call", nested[0].ParentToolUseID)
	}
}

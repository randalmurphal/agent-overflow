package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func worktreeToolUseLine(id, name string, parentToolUseID string) []byte {
	parent := ""
	if parentToolUseID != "" {
		parent = `,"parent_tool_use_id":"` + parentToolUseID + `"`
	}
	return []byte(`{"type":"assistant"` + parent + `,"message":{"id":"msg-wt","role":"assistant","content":[` +
		`{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":{"name":"feature-x"}}` +
		`]}}`)
}

func worktreeToolResultLine(id, content, toolUseResult string, isError bool, parentToolUseID string) []byte {
	parent := ""
	if parentToolUseID != "" {
		parent = `,"parent_tool_use_id":"` + parentToolUseID + `"`
	}
	errField := ""
	if isError {
		errField = `,"is_error":true`
	}
	sibling := ""
	if toolUseResult != "" {
		sibling = `,"tool_use_result":` + toolUseResult
	}
	return []byte(`{"type":"user"` + parent + `,"message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"` + id + `","content":` + content + errField + `}` +
		`]}` + sibling + `}`)
}

func parseWorktreeRoundtrip(t *testing.T, name, content, toolUseResult string, isError bool, parentToolUseID string) []provider.ProviderEvent {
	t.Helper()
	p := NewParser()
	if _, err := p.ParseLine(testThread, worktreeToolUseLine("tool-wt-1", name, parentToolUseID)); err != nil {
		t.Fatalf("parse use: %v", err)
	}
	events, err := p.ParseLine(testThread, worktreeToolResultLine("tool-wt-1", content, toolUseResult, isError, parentToolUseID))
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	return events
}

func decodeWorkspaceMeta(t *testing.T, evt provider.ProviderEvent) provider.WorkspaceChangeMeta {
	t.Helper()
	var meta provider.WorkspaceChangeMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta %s: %v", evt.Meta, err)
	}
	return meta
}

func hasKind(events []provider.ProviderEvent, kind provider.EventKind) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// TestParseEnterWorktreeEmitsWorkspaceChange: the structured
// tool_use_result names the worktree the process moved into; the event
// carries it beside the ordinary tool completion.
func TestParseEnterWorktreeEmitsWorkspaceChange(t *testing.T) {
	events := parseWorktreeRoundtrip(t, "EnterWorktree",
		`"Created worktree at /repo/.claude/worktrees/feature-x on branch worktree-feature-x"`,
		`{"worktreePath":"/repo/.claude/worktrees/feature-x","worktreeBranch":"worktree-feature-x","message":"Created worktree"}`,
		false, "")

	if !hasKind(events, provider.EventToolComplete) {
		t.Fatalf("tool completion row missing: %+v", events)
	}
	evt := pickEvent(t, events, provider.EventWorkspaceChanged)
	if evt.ItemID != "tool-wt-1" {
		t.Errorf("ItemID = %q, want tool-wt-1", evt.ItemID)
	}
	meta := decodeWorkspaceMeta(t, evt)
	want := provider.WorkspaceChangeMeta{
		Tool:         "EnterWorktree",
		Cwd:          "/repo/.claude/worktrees/feature-x",
		WorktreePath: "/repo/.claude/worktrees/feature-x",
		Branch:       "worktree-feature-x",
	}
	if meta != want {
		t.Errorf("meta = %+v, want %+v", meta, want)
	}
}

func TestParseExitWorktreeKeepEmitsReturnToOriginalCwd(t *testing.T) {
	events := parseWorktreeRoundtrip(t, "ExitWorktree",
		`"Exited worktree; kept /repo/.claude/worktrees/feature-x"`,
		`{"action":"keep","originalCwd":"/repo","worktreePath":"/repo/.claude/worktrees/feature-x","worktreeBranch":"worktree-feature-x","message":"kept"}`,
		false, "")

	meta := decodeWorkspaceMeta(t, pickEvent(t, events, provider.EventWorkspaceChanged))
	want := provider.WorkspaceChangeMeta{
		Tool:         "ExitWorktree",
		Cwd:          "/repo",
		WorktreePath: "/repo/.claude/worktrees/feature-x",
		Branch:       "worktree-feature-x",
	}
	if meta != want {
		t.Errorf("meta = %+v, want %+v", meta, want)
	}
}

func TestParseExitWorktreeRemoveFlagsRemoval(t *testing.T) {
	events := parseWorktreeRoundtrip(t, "ExitWorktree",
		`"Exited and removed worktree"`,
		`{"action":"remove","originalCwd":"/repo","worktreePath":"/repo/.claude/worktrees/feature-x","worktreeBranch":"worktree-feature-x","discardedFiles":[],"message":"removed"}`,
		false, "")

	meta := decodeWorkspaceMeta(t, pickEvent(t, events, provider.EventWorkspaceChanged))
	if !meta.RemovedWorktree || meta.Cwd != "/repo" || meta.WorktreePath != "/repo/.claude/worktrees/feature-x" {
		t.Errorf("meta = %+v, want removal back to /repo", meta)
	}
}

// A refused call moved nothing, so no workspace change and no error.
func TestParseEnterWorktreeErrorResultMovesNothing(t *testing.T) {
	events := parseWorktreeRoundtrip(t, "EnterWorktree",
		`"Error: Already in a worktree session"`, "", true, "")

	if hasKind(events, provider.EventWorkspaceChanged) {
		t.Fatalf("refused EnterWorktree produced a workspace change: %+v", events)
	}
	if hasKind(events, provider.EventError) {
		t.Fatalf("refused EnterWorktree produced an error event: %+v", events)
	}
	if !hasKind(events, provider.EventToolComplete) {
		t.Fatalf("tool completion row missing: %+v", events)
	}
}

// A subagent's move is scoped to that agent; the session's directory is
// unchanged, so the thread must not follow it.
func TestParseSubagentEnterWorktreeIsAgentScoped(t *testing.T) {
	events := parseWorktreeRoundtrip(t, "EnterWorktree",
		`"Created worktree"`,
		`{"worktreePath":"/repo/.claude/worktrees/sub","worktreeBranch":"worktree-sub","message":"Created"}`,
		false, "tool-agent-launch")

	if hasKind(events, provider.EventWorkspaceChanged) {
		t.Fatalf("subagent EnterWorktree produced a session workspace change: %+v", events)
	}
	if hasKind(events, provider.EventError) {
		t.Fatalf("subagent EnterWorktree produced an error: %+v", events)
	}
}

// Without the structured sibling the parser cannot know where the process
// went. It says so on the thread instead of guessing from the message text.
func TestParseEnterWorktreeWithoutStructuredResultReportsError(t *testing.T) {
	events := parseWorktreeRoundtrip(t, "EnterWorktree",
		`"Created worktree at /repo/.claude/worktrees/feature-x"`, "", false, "")

	if hasKind(events, provider.EventWorkspaceChanged) {
		t.Fatalf("workspace change emitted without a structured result: %+v", events)
	}
	evt := pickEvent(t, events, provider.EventError)
	if evt.ItemID != "tool-wt-1" || !strings.Contains(evt.Content, "EnterWorktree") || !strings.Contains(evt.Content, "previous workspace") {
		t.Errorf("error event = %+v, want one naming the tool and the stale workspace", evt)
	}
}

func TestParseExitWorktreeWithoutOriginalCwdReportsError(t *testing.T) {
	events := parseWorktreeRoundtrip(t, "ExitWorktree",
		`"Exited"`, `{"action":"keep","worktreePath":"/repo/.claude/worktrees/feature-x","message":"kept"}`, false, "")

	if hasKind(events, provider.EventWorkspaceChanged) {
		t.Fatalf("workspace change emitted without originalCwd: %+v", events)
	}
	evt := pickEvent(t, events, provider.EventError)
	if !strings.Contains(evt.Content, "originalCwd") {
		t.Errorf("error content = %q, want the missing field named", evt.Content)
	}
}

// A result is one-shot: the second result for the same id (which the wire
// never produces, but a replay could) does not re-emit the move.
func TestParseWorktreeResultIsOneShot(t *testing.T) {
	p := NewParser()
	if _, err := p.ParseLine(testThread, worktreeToolUseLine("tool-wt-1", "EnterWorktree", "")); err != nil {
		t.Fatalf("parse use: %v", err)
	}
	result := worktreeToolResultLine("tool-wt-1", `"Created"`, `{"worktreePath":"/repo/.claude/worktrees/x","message":"Created"}`, false, "")
	first, err := p.ParseLine(testThread, result)
	if err != nil {
		t.Fatalf("parse first result: %v", err)
	}
	if !hasKind(first, provider.EventWorkspaceChanged) {
		t.Fatalf("first result produced no workspace change: %+v", first)
	}
	second, err := p.ParseLine(testThread, result)
	if err != nil {
		t.Fatalf("parse second result: %v", err)
	}
	if hasKind(second, provider.EventWorkspaceChanged) {
		t.Fatalf("second result re-emitted the workspace change: %+v", second)
	}
}

// Other tools' results never touch the worktree path, even with a sibling
// that happens to carry a worktreePath field.
func TestParseNonWorktreeToolResultNeverEmitsWorkspaceChange(t *testing.T) {
	events := parseWorktreeRoundtrip(t, "Bash",
		`"ok"`, `{"worktreePath":"/elsewhere","stdout":"ok"}`, false, "")
	if hasKind(events, provider.EventWorkspaceChanged) {
		t.Fatalf("Bash result produced a workspace change: %+v", events)
	}
}

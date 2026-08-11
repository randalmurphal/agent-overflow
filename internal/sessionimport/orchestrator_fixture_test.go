package sessionimport

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // fixture Codex thread index

	"agent-overflow/internal/store"
)

// orchestrator_fixture_test.go — hand-written provider homes under
// t.TempDir().
//
// Nothing here reads ~/.claude or ~/.codex, and nothing spawns a binary.
// Both are hard rules (root AGENTS.md §Permanent invariants): a test that
// touched the developer's real homes would be reading live session state
// and a live login.

const (
	claudeSessionA = "11111111-1111-4111-8111-111111111111"
	claudeSessionB = "22222222-2222-4222-8222-222222222222"
	codexThreadA   = "33333333-3333-4333-8333-333333333333"
	codexThreadB   = "44444444-4444-4444-8444-444444444444"
)

// providerHomes is one fixture pair of provider homes plus the workspace
// the fixture sessions claim to have run in.
type providerHomes struct {
	claudeProjects string
	codexHome      string
	workspace      string
}

func newProviderHomes(t *testing.T) providerHomes {
	t.Helper()
	root := t.TempDir()
	homes := providerHomes{
		claudeProjects: filepath.Join(root, "claude", "projects"),
		codexHome:      filepath.Join(root, "codex"),
		workspace:      filepath.Join(root, "repo"),
	}
	for _, dir := range []string{homes.claudeProjects, homes.codexHome, homes.workspace} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return homes
}

func (h providerHomes) deps(st *store.Store) Deps {
	return Deps{
		Store:             st,
		ClaudeProjectsDir: h.claudeProjects,
		CodexHome:         h.codexHome,
	}
}

// isoAt renders the fixture clock as the ISO timestamp both providers
// write. Fixed and in the past, so a row restamped with now() is off by
// years rather than milliseconds.
func isoAt(offset int64) string {
	return time.UnixMilli(baseMillis + offset).UTC().Format(time.RFC3339Nano)
}

// ---------------------------------------------------------------- Claude

// writeClaudeSession writes one transcript under the fixture projects dir
// and returns its path.
func (h providerHomes) writeClaudeSession(t *testing.T, sessionID string, rows ...map[string]any) string {
	t.Helper()
	dir := filepath.Join(h.claudeProjects, "-fixture-repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir claude project dir: %v", err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	var body []byte
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal transcript row: %v", err)
		}
		body = append(body, encoded...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

// claudeUserRow is a top-level user prompt: the only turn boundary a
// transcript records.
func (h providerHomes) claudeUserRow(uuid, parent, text string, offset int64) map[string]any {
	return map[string]any{
		"type":        "user",
		"uuid":        uuid,
		"parentUuid":  orNil(parent),
		"isSidechain": false,
		"timestamp":   isoAt(offset),
		"cwd":         h.workspace,
		"gitBranch":   "main",
		"message":     map[string]any{"role": "user", "content": text},
	}
}

func (h providerHomes) claudeAssistantRow(
	uuid, parent, messageID string, blocks []any, offset int64, usage map[string]any,
) map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"id":      messageID,
		"model":   "claude-sonnet-4-5",
		"content": blocks,
	}
	if usage != nil {
		message["usage"] = usage
	}
	return map[string]any{
		"type":        "assistant",
		"uuid":        uuid,
		"parentUuid":  orNil(parent),
		"isSidechain": false,
		"timestamp":   isoAt(offset),
		"cwd":         h.workspace,
		"message":     message,
	}
}

// claudeToolResultRow is the `user` row that closes a tool call.
func (h providerHomes) claudeToolResultRow(
	uuid, parent, toolUseID, output string, offset int64, toolUseResult any,
) map[string]any {
	row := map[string]any{
		"type":        "user",
		"uuid":        uuid,
		"parentUuid":  orNil(parent),
		"isSidechain": false,
		"timestamp":   isoAt(offset),
		"cwd":         h.workspace,
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": output},
		}},
	}
	if toolUseResult != nil {
		row["toolUseResult"] = toolUseResult
	}
	return row
}

func claudeLastPromptRow(leafUUID, prompt string) map[string]any {
	return map[string]any{"type": "last-prompt", "leafUuid": leafUUID, "lastPrompt": prompt}
}

func claudeTextBlock(text string) any {
	return map[string]any{"type": "text", "text": text}
}

func claudeToolUseBlock(id, name string, input map[string]any) any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// withField sets one extra field on a fixture row — the transcript carries
// far more per row than these builders model, and a test that needs one more
// key should not need a new builder.
func withField(row map[string]any, key string, value any) map[string]any {
	row[key] = value
	return row
}

// claudeLinearSession is the common fixture: one prompt, one tool call
// that completes, one closing assistant message carrying usage.
func (h providerHomes) claudeLinearSession(t *testing.T, sessionID string) string {
	t.Helper()
	return h.writeClaudeSession(t, sessionID,
		h.claudeUserRow("u1", "", "add a test", 0),
		h.claudeAssistantRow("a1", "u1", "msg-1", []any{
			claudeTextBlock("Reading the file."),
			claudeToolUseBlock("toolu_1", "Read", map[string]any{"file_path": "/fixture/repo/main.go"}),
		}, 1_000, nil),
		h.claudeToolResultRow("r1", "a1", "toolu_1", "package main", 2_000, nil),
		h.claudeAssistantRow("a2", "r1", "msg-2", []any{
			claudeTextBlock("Done."),
		}, 3_000, map[string]any{"input_tokens": 120, "output_tokens": 45}),
		claudeLastPromptRow("a2", "add a test"),
	)
}

// claudeShortSession is the smallest listable transcript, with each row
// carrying the extra fields in `fields`. It backs the tests that vary one
// listing input (the recorded cwd, the entrypoint marker) rather than the
// conversation.
func (h providerHomes) claudeShortSession(
	t *testing.T, sessionID string, fields map[string]any,
) string {
	t.Helper()
	apply := func(row map[string]any) map[string]any {
		for key, value := range fields {
			withField(row, key, value)
		}
		return row
	}
	return h.writeClaudeSession(t, sessionID,
		apply(h.claudeUserRow("u1", "", "add a test", 0)),
		apply(h.claudeAssistantRow("a1", "u1", "msg-1", []any{claudeTextBlock("Done.")}, 1_000, nil)),
		claudeLastPromptRow("a1", "add a test"),
	)
}

// writeClaudeSubagent writes one `subagents/agent-<agentID>.jsonl`
// sidecar for a session. Its rows join to the parent Task tool call
// through the `toolUseResult.agentId` on the result row that closes it.
func (h providerHomes) writeClaudeSubagent(t *testing.T, sessionID, agentID string, rows ...map[string]any) {
	t.Helper()
	dir := filepath.Join(h.claudeProjects, "-fixture-repo", sessionID, "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	var body []byte
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal subagent row: %v", err)
		}
		body = append(body, encoded...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-"+agentID+".jsonl"), body, 0o644); err != nil {
		t.Fatalf("write subagent transcript: %v", err)
	}
}

// ----------------------------------------------------------------- Codex

// codexFixtureSchema is the subset of Codex's `threads` table the lister
// reads, with upstream's own column names and types.
const codexFixtureSchema = `
CREATE TABLE threads (
    id TEXT PRIMARY KEY,
    rollout_path TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    source TEXT NOT NULL,
    cwd TEXT NOT NULL,
    title TEXT NOT NULL,
    tokens_used INTEGER NOT NULL DEFAULT 0,
    archived INTEGER NOT NULL DEFAULT 0,
    git_branch TEXT,
    first_user_message TEXT NOT NULL DEFAULT '',
    model TEXT,
    reasoning_effort TEXT,
    created_at_ms INTEGER,
    updated_at_ms INTEGER,
    thread_source TEXT,
    preview TEXT NOT NULL DEFAULT '',
    recency_at_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE thread_spawn_edges (
    parent_thread_id TEXT NOT NULL,
    child_thread_id TEXT NOT NULL PRIMARY KEY,
    status TEXT NOT NULL
);`

// writeCodexIndex creates the fixture thread index. Call once per home.
func (h providerHomes) writeCodexIndex(t *testing.T, threadIDs ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.codexHome, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open fixture codex index: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(codexFixtureSchema); err != nil {
		t.Fatalf("create fixture codex schema: %v", err)
	}
	for i, threadID := range threadIDs {
		if _, err := db.Exec(`
INSERT INTO threads (id, rollout_path, created_at, updated_at, source, cwd, title,
                     first_user_message, archived, thread_source, preview, recency_at_ms,
                     created_at_ms, updated_at_ms, git_branch, model, reasoning_effort, tokens_used)
VALUES (?, ?, ?, ?, '{}', ?, ?, ?, 0, NULL, 'preview', ?, ?, ?, 'main', 'gpt-5.6-sol', 'high', 42)`,
			threadID, h.codexRolloutPath(threadID),
			(baseMillis)/1000, (baseMillis+9_000)/1000,
			h.workspace, "Codex "+threadID[:4], "prompt "+threadID[:4],
			baseMillis+9_000-int64(i), baseMillis, baseMillis+9_000,
		); err != nil {
			t.Fatalf("insert fixture codex thread: %v", err)
		}
	}
}

func (h providerHomes) codexRolloutPath(threadID string) string {
	return filepath.Join(h.codexHome, "sessions", "rollout-2026-08-07T15-07-44-"+threadID+".jsonl")
}

// writeCodexRollout writes one rollout JSONL at the path the index names.
func (h providerHomes) writeCodexRollout(t *testing.T, threadID string, lines ...string) string {
	t.Helper()
	path := h.codexRolloutPath(threadID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir codex sessions: %v", err)
	}
	var body []byte
	for _, line := range lines {
		body = append(body, line...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

// appendRolloutLines appends lines to a rollout the way a live Codex does,
// so a refresh sees the same file grown rather than a rewritten one.
func appendRolloutLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open rollout for append: %v", err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("append rollout line: %v", err)
		}
	}
}

// codexLinearSession is the common fixture: a session header, one turn
// with a prompt, an assistant message, a tool call, and its usage.
func (h providerHomes) codexLinearSession(t *testing.T, threadID string, extra ...string) string {
	t.Helper()
	lines := []string{
		h.codexMetaLine(threadID, ""),
		codexLine(0, "turn_context", map[string]any{
			"turn_id": "turn-1", "cwd": h.workspace,
			"model": "gpt-5.6-sol", "effort": "high",
		}),
		codexLine(100, "event_msg", map[string]any{
			"type": "task_started", "turn_id": "turn-1", "model_context_window": 258400,
		}),
		codexLine(200, "event_msg", map[string]any{
			"type": "user_message", "message": "add a test",
		}),
		codexLine(300, "response_item", map[string]any{
			"type": "function_call", "name": "shell", "call_id": "call-1",
			"arguments": `{"command":["bash","-lc","go test ./..."]}`,
		}),
		codexLine(400, "event_msg", map[string]any{
			"type": "exec_command_end", "call_id": "call-1",
			"exit_code": 0, "aggregated_output": "ok\n", "duration": map[string]any{"secs": 1},
		}),
		codexLine(500, "response_item", map[string]any{
			"type": "function_call_output", "call_id": "call-1",
			"output": map[string]any{"content": "ok"},
		}),
		codexLine(600, "event_msg", map[string]any{
			"type": "agent_message", "message": "All tests pass.", "phase": "final_answer",
		}),
		codexLine(700, "event_msg", map[string]any{
			"type": "token_count", "turn_id": "turn-1",
			"info": map[string]any{
				"total_token_usage": map[string]any{
					"input_tokens": 900, "cached_input_tokens": 100, "output_tokens": 300,
				},
				"model_context_window": 258400,
			},
		}),
		codexLine(800, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": "turn-1", "last_agent_message": "All tests pass.",
		}),
	}
	return h.writeCodexRollout(t, threadID, append(lines, extra...)...)
}

func (h providerHomes) codexMetaLine(threadID, forkedFrom string) string {
	return h.codexMetaLineFrom(threadID, forkedFrom, "codex_cli")
}

// codexMetaLineFrom is codexMetaLine with an explicit `originator` — the
// marker naming which client started the thread.
func (h providerHomes) codexMetaLineFrom(threadID, forkedFrom, originator string) string {
	payload := map[string]any{
		"id":          threadID,
		"cwd":         h.workspace,
		"originator":  originator,
		"cli_version": "0.146.0",
		"git":         map[string]any{"branch": "main"},
	}
	if forkedFrom != "" {
		payload["forked_from_id"] = forkedFrom
	}
	return codexLine(0, "session_meta", payload)
}

func codexLine(offset int64, kind string, payload map[string]any) string {
	encoded, err := json.Marshal(map[string]any{
		"timestamp": isoAt(offset),
		"type":      kind,
		"payload":   payload,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal rollout line: %v", err))
	}
	return string(encoded)
}

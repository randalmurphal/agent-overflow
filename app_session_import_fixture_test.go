//go:build !providersmoke

package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // fixture Codex thread index
)

// app_session_import_fixture_test.go — hand-written provider homes for the
// App-level session-import tests.
//
// Every home is a t.TempDir() the App is pointed at through
// credentialHomeOverride, which is the same seam the harness uses. Nothing
// here reads ~/.claude or ~/.codex and nothing spawns a binary; both are hard
// rules (root AGENTS.md §Permanent invariants). The rows are the minimum the
// readers need, not copies of real sessions.

const (
	importFixtureClaudeSession = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	importFixtureClaudeBranchy = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	importFixtureCodexThread   = "cccccccc-3333-4333-8333-cccccccccccc"
	// importFixtureMillis is a fixed wall time in the past, so a row that was
	// restamped with now() is off by years rather than milliseconds.
	importFixtureMillis int64 = 1_700_000_000_000
)

// importHome is one fixture provider home plus the workspace its sessions
// claim to have run in.
type importHome struct {
	root      string
	workspace string
}

func newImportHome(t *testing.T) importHome {
	t.Helper()
	root := t.TempDir()
	home := importHome{root: root, workspace: filepath.Join(root, "repo")}
	for _, dir := range []string{home.claudeProjectDir(), home.codexHome(), home.workspace} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return home
}

func (h importHome) claudeProjectDir() string {
	return filepath.Join(h.root, ".claude", "projects", "-fixture-repo")
}

func (h importHome) codexHome() string { return filepath.Join(h.root, ".codex") }

func (h importHome) claudeSessionPath(sessionID string) string {
	return filepath.Join(h.claudeProjectDir(), sessionID+".jsonl")
}

// attach points an App at this home. credentialHomeOverride is the ONE seam
// session import resolves provider homes through (app_session_import.go), so
// pointing it here is what keeps the tests off the developer's real homes.
func (h importHome) attach(app *App) { app.credentialHomeOverride = h.root }

func importFixtureISO(offset int64) string {
	return time.UnixMilli(importFixtureMillis + offset).UTC().Format(time.RFC3339Nano)
}

// --------------------------------------------------------------- Claude

func (h importHome) writeClaudeSession(t *testing.T, sessionID string, rows ...map[string]any) string {
	t.Helper()
	path := h.claudeSessionPath(sessionID)
	if err := os.WriteFile(path, encodeJSONL(t, rows), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func (h importHome) appendClaudeRows(t *testing.T, sessionID string, rows ...map[string]any) {
	t.Helper()
	file, err := os.OpenFile(h.claudeSessionPath(sessionID), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open transcript for append: %v", err)
	}
	defer file.Close()
	if _, err := file.Write(encodeJSONL(t, rows)); err != nil {
		t.Fatalf("append transcript rows: %v", err)
	}
}

func (h importHome) claudeUserRow(uuid, parent, text string, offset int64) map[string]any {
	return map[string]any{
		"type": "user", "uuid": uuid, "parentUuid": orNilString(parent),
		"isSidechain": false, "timestamp": importFixtureISO(offset),
		"cwd": h.workspace, "gitBranch": "main",
		"message": map[string]any{"role": "user", "content": text},
	}
}

func (h importHome) claudeAssistantRow(uuid, parent, messageID, text string, offset int64) map[string]any {
	return map[string]any{
		"type": "assistant", "uuid": uuid, "parentUuid": orNilString(parent),
		"isSidechain": false, "timestamp": importFixtureISO(offset), "cwd": h.workspace,
		"message": map[string]any{
			"role": "assistant", "id": messageID, "model": "claude-sonnet-4-5",
			"content": []any{map[string]any{"type": "text", "text": text}},
			"usage":   map[string]any{"input_tokens": 100, "output_tokens": 20},
		},
	}
}

func claudeLastPrompt(leafUUID, prompt string) map[string]any {
	return map[string]any{"type": "last-prompt", "leafUuid": leafUUID, "lastPrompt": prompt}
}

// claudeLinearSession is one prompt and one answer — the smallest transcript
// the lister will offer and the writer will convert.
func (h importHome) claudeLinearSession(t *testing.T, sessionID string) string {
	t.Helper()
	return h.writeClaudeSession(t, sessionID,
		h.claudeUserRow("u1", "", "add a test", 0),
		h.claudeAssistantRow("a1", "u1", "msg-1", "Added it.", 1_000),
		claudeLastPrompt("a1", "add a test"),
	)
}

// claudeBranchedSession forks after the first answer: two leaves, so the
// import produces two threads and only the file-order-last one carries the
// session ref.
func (h importHome) claudeBranchedSession(t *testing.T, sessionID string) string {
	t.Helper()
	return h.writeClaudeSession(t, sessionID,
		h.claudeUserRow("u1", "", "add a test", 0),
		h.claudeAssistantRow("a1", "u1", "msg-1", "Added it.", 1_000),
		h.claudeUserRow("u2a", "a1", "now document it", 2_000),
		h.claudeAssistantRow("a2a", "u2a", "msg-2a", "Documented.", 3_000),
		h.claudeUserRow("u2b", "a1", "now benchmark it", 4_000),
		h.claudeAssistantRow("a2b", "u2b", "msg-2b", "Benchmarked.", 5_000),
		claudeLastPrompt("a2a", "now document it"),
		claudeLastPrompt("a2b", "now benchmark it"),
	)
}

// ---------------------------------------------------------------- Codex

// codexFixtureSchema is the subset of Codex's `threads` table the lister
// reads, with upstream's own column names.
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

func (h importHome) codexRolloutPath(threadID string) string {
	return filepath.Join(h.codexHome(), "sessions", "rollout-2026-08-07T15-07-44-"+threadID+".jsonl")
}

func (h importHome) writeCodexIndex(t *testing.T, threadIDs ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.codexHome(), "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open fixture codex index: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(codexFixtureSchema); err != nil {
		t.Fatalf("create fixture codex schema: %v", err)
	}
	for _, threadID := range threadIDs {
		if _, err := db.Exec(`
INSERT INTO threads (id, rollout_path, created_at, updated_at, source, cwd, title,
                     first_user_message, archived, thread_source, preview, recency_at_ms,
                     created_at_ms, updated_at_ms, git_branch, model, reasoning_effort, tokens_used)
VALUES (?, ?, ?, ?, '{}', ?, ?, ?, 0, NULL, 'preview', ?, ?, ?, 'main', 'gpt-5.6-sol', 'high', 42)`,
			threadID, h.codexRolloutPath(threadID),
			importFixtureMillis/1000, (importFixtureMillis+9_000)/1000,
			h.workspace, "Codex fixture", "add a test",
			importFixtureMillis+9_000, importFixtureMillis, importFixtureMillis+9_000,
		); err != nil {
			t.Fatalf("insert fixture codex thread: %v", err)
		}
	}
}

func (h importHome) writeCodexRollout(t *testing.T, threadID string, lines ...string) string {
	t.Helper()
	path := h.codexRolloutPath(threadID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir codex sessions: %v", err)
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

func (h importHome) appendCodexRollout(t *testing.T, threadID string, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(h.codexRolloutPath(threadID), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open rollout for append: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(joinLines(lines)); err != nil {
		t.Fatalf("append rollout lines: %v", err)
	}
}

// codexLinearSession is one turn: a header, a prompt, and an answer.
func (h importHome) codexLinearSession(t *testing.T, threadID string) string {
	t.Helper()
	lines := []string{codexFixtureLine(t, 0, "session_meta", map[string]any{
		"id": threadID, "cwd": h.workspace, "originator": "codex_cli",
		"cli_version": "0.146.0", "git": map[string]any{"branch": "main"},
	})}
	lines = append(lines, codexFixtureTurn(t, "turn-1", "add a test", "Added it.", 100)...)
	return h.writeCodexRollout(t, threadID, lines...)
}

// codexFixtureTurn renders one complete turn: context, start, prompt, answer,
// completion.
func codexFixtureTurn(t *testing.T, turnID, prompt, answer string, offset int64) []string {
	t.Helper()
	return []string{
		codexFixtureLine(t, offset, "turn_context", map[string]any{
			"turn_id": turnID, "model": "gpt-5.6-sol", "effort": "high",
		}),
		codexFixtureLine(t, offset+10, "event_msg", map[string]any{
			"type": "task_started", "turn_id": turnID, "model_context_window": 258400,
		}),
		codexFixtureLine(t, offset+20, "event_msg", map[string]any{
			"type": "user_message", "message": prompt,
		}),
		codexFixtureLine(t, offset+30, "event_msg", map[string]any{
			"type": "agent_message", "message": answer, "phase": "final_answer",
		}),
		codexFixtureLine(t, offset+40, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": turnID, "last_agent_message": answer,
		}),
	}
}

func codexFixtureLine(t *testing.T, offset int64, kind string, payload map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"timestamp": importFixtureISO(offset), "type": kind, "payload": payload,
	})
	if err != nil {
		t.Fatalf("marshal rollout line: %v", err)
	}
	return string(encoded)
}

// ---------------------------------------------------------------- shared

func encodeJSONL(t *testing.T, rows []map[string]any) []byte {
	t.Helper()
	var body []byte
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal fixture row: %v", err)
		}
		body = append(body, encoded...)
		body = append(body, '\n')
	}
	return body
}

func joinLines(lines []string) string {
	out := ""
	for _, line := range lines {
		out += line + "\n"
	}
	return out
}

func orNilString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

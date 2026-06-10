package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestV8TrimsToolResultEchoMeta seeds pre-v8 items whose meta carries
// the full Claude completion echo, runs the v8 fixup, and asserts the
// persisted rows land in the same shape the triage write path now
// produces: echo dropped on success, tail-capped excerpts on failure,
// user-input echo untouched, unrelated rows byte-identical.
func TestV8TrimsToolResultEchoMeta(t *testing.T) {
	db := migrateThrough(t, 7)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v8', '/v8', 'v8', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v8', 'p-v8', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)

	heavy := strings.Repeat("transcript line\n", 50_000) // ~800 KB
	successMeta := mustMarshalJSON(t, map[string]any{
		"toolName": "Task",
		"is_error": false,
		"input":    map[string]any{"description": "explore"},
		"tool_result": map[string]any{
			"tool_use_id": "toolu_1",
			"content":     []map[string]any{{"type": "text", "text": heavy}},
			"is_error":    false,
		},
		"tool_use_result": map[string]any{"content": heavy, "totalTokens": 9000},
	})
	failureMeta := mustMarshalJSON(t, map[string]any{
		"toolName":  "Bash",
		"is_error":  true,
		"exit_code": 1,
		"tool_use_result": map[string]any{
			"stderr": strings.Repeat("noise\n", 10_000) + "FAIL: TestThing",
		},
	})
	askMeta := mustMarshalJSON(t, map[string]any{
		"toolName": "AskUserQuestion",
		"is_error": false,
		"tool_result": map[string]any{
			"content": `{"answers":{"Approach":"B"}}`,
		},
	})
	plainMeta := `{"toolName":"Read","input":{"file_path":"/x"}}`

	seedItem := func(id, toolName, meta string, itemIndex int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
			VALUES (?, 't-v8', 0, ?, 'tool_call', 'assistant', 'completed',
			'', '', 0, '', ?, '', ?, 1, 1)`, id, itemIndex, toolName, meta)
	}
	seedItem("i-success", "Task", successMeta, 0)
	seedItem("i-failure", "Bash", failureMeta, 1)
	seedItem("i-ask", "AskUserQuestion", askMeta, 2)
	seedItem("i-plain", "Read", plainMeta, 3)

	if err := applyMigration(db, migrationByVersion(t, 8)); err != nil {
		t.Fatalf("apply v8: %v", err)
	}

	readMeta := func(id string) string {
		t.Helper()
		var meta string
		if err := db.QueryRow(`SELECT meta FROM items WHERE thread_id = 't-v8' AND id = ?`, id).Scan(&meta); err != nil {
			t.Fatalf("read meta %s: %v", id, err)
		}
		return meta
	}

	success := readMeta("i-success")
	if len(success) > 1024 {
		t.Errorf("success meta = %d bytes after v8; expected well under 1 KB", len(success))
	}
	var successTop map[string]json.RawMessage
	if err := json.Unmarshal([]byte(success), &successTop); err != nil {
		t.Fatalf("decode success meta: %v", err)
	}
	if _, ok := successTop["tool_result"]; ok {
		t.Errorf("v8 left tool_result on a success row")
	}
	if _, ok := successTop["tool_use_result"]; ok {
		t.Errorf("v8 left tool_use_result on a success row")
	}
	if _, ok := successTop["input"]; !ok {
		t.Errorf("v8 dropped unrelated meta keys")
	}

	failure := readMeta("i-failure")
	var failureDecoded struct {
		ToolUseResult struct {
			Stderr string `json:"stderr"`
		} `json:"tool_use_result"`
	}
	if err := json.Unmarshal([]byte(failure), &failureDecoded); err != nil {
		t.Fatalf("decode failure meta: %v", err)
	}
	if !strings.HasSuffix(failureDecoded.ToolUseResult.Stderr, "FAIL: TestThing") {
		t.Errorf("failure excerpt lost the error tail: %q", failureDecoded.ToolUseResult.Stderr)
	}
	if len(failureDecoded.ToolUseResult.Stderr) > 2048 {
		t.Errorf("failure excerpt = %d bytes; want ≤ 2048", len(failureDecoded.ToolUseResult.Stderr))
	}

	if got := readMeta("i-ask"); got != askMeta {
		t.Errorf("v8 must not touch the AskUserQuestion answers echo:\n got %s\nwant %s", got, askMeta)
	}
	if got := readMeta("i-plain"); got != plainMeta {
		t.Errorf("v8 must not touch rows without the echo:\n got %s\nwant %s", got, plainMeta)
	}
}

// TestV9TrimsCollabAgentStateMessagesMeta seeds pre-v9 items whose meta
// carries Codex collab agentsStates with full per-agent final messages,
// runs the v9 fixup, and asserts the persisted rows land in the same
// shape the triage write path now produces: messages dropped, statuses
// and every other key kept, bare-string states and unrelated rows
// byte-identical.
func TestV9TrimsCollabAgentStateMessagesMeta(t *testing.T) {
	db := migrateThrough(t, 8)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v9', '/v9', 'v9', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v9', 'p-v9', 'T', 'codex', '/tmp', '', 1, 1, 0, 'chat')`)

	heavy := strings.Repeat("review finding\n", 50_000) // ~750 KB
	waitMeta := mustMarshalJSON(t, map[string]any{
		"toolName": "wait_agent",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": []string{"child-1", "child-2"},
			"agentsStates": map[string]any{
				"child-1": map[string]any{"status": "completed", "message": heavy},
				"child-2": map[string]any{"status": "errored", "message": "boom"},
			},
		},
	})
	bareMeta := mustMarshalJSON(t, map[string]any{
		"toolName": "wait_agent",
		"input": map[string]any{
			"tool":         "wait_agent",
			"agentsStates": map[string]any{"child-1": "running"},
		},
	})
	plainMeta := `{"toolName":"Read","input":{"file_path":"/x"}}`

	seedItem := func(id, toolName, meta string, itemIndex int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
			VALUES (?, 't-v9', 0, ?, 'tool_call', 'assistant', 'completed',
			'', '', 0, '', ?, '', ?, 1, 1)`, id, itemIndex, toolName, meta)
	}
	seedItem("i-wait", "wait_agent", waitMeta, 0)
	seedItem("i-bare", "wait_agent", bareMeta, 1)
	seedItem("i-plain", "Read", plainMeta, 2)

	if err := applyMigration(db, migrationByVersion(t, 9)); err != nil {
		t.Fatalf("apply v9: %v", err)
	}

	readMeta := func(id string) string {
		t.Helper()
		var meta string
		if err := db.QueryRow(`SELECT meta FROM items WHERE thread_id = 't-v9' AND id = ?`, id).Scan(&meta); err != nil {
			t.Fatalf("read meta %s: %v", id, err)
		}
		return meta
	}

	wait := readMeta("i-wait")
	if len(wait) > 1024 {
		t.Errorf("wait meta = %d bytes after v9; expected well under 1 KB", len(wait))
	}
	var waitDecoded struct {
		Input struct {
			ReceiverThreadIDs []string                              `json:"receiverThreadIds"`
			AgentsStates      map[string]map[string]json.RawMessage `json:"agentsStates"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(wait), &waitDecoded); err != nil {
		t.Fatalf("decode wait meta: %v", err)
	}
	if len(waitDecoded.Input.ReceiverThreadIDs) != 2 {
		t.Errorf("v9 dropped unrelated input keys: %s", wait)
	}
	for id, wantStatus := range map[string]string{"child-1": `"completed"`, "child-2": `"errored"`} {
		entry, ok := waitDecoded.Input.AgentsStates[id]
		if !ok {
			t.Fatalf("v9 dropped agentsStates entry %s", id)
		}
		// Key presence, not value: the trim must DELETE message, not null it.
		if _, hasMessage := entry["message"]; hasMessage {
			t.Errorf("v9 left the message key on %s", id)
		}
		if got := string(entry["status"]); got != wantStatus {
			t.Errorf("status for %s = %s, want %s", id, got, wantStatus)
		}
	}

	if got := readMeta("i-bare"); got != bareMeta {
		t.Errorf("v9 must not touch bare-string agent states:\n got %s\nwant %s", got, bareMeta)
	}
	if got := readMeta("i-plain"); got != plainMeta {
		t.Errorf("v9 must not touch rows without agentsStates:\n got %s\nwant %s", got, plainMeta)
	}
}

func mustMarshalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

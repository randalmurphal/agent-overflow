package store

import (
	"encoding/json"
	"fmt"
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

func TestV21TrimsCodexV2EncryptedCollabPrompts(t *testing.T) {
	db := migrateThrough(t, 20)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v21', '/v21', 'v21', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v21', 'p-v21', 'T', 'codex', '/tmp', '', 1, 1, 0, 'chat')`)

	seed := func(id, toolName, summary, meta string, itemIndex int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
			VALUES (?, 't-v21', 0, ?, 'tool_call', 'assistant', 'completed', ?, '', 0, '', ?, '', ?, 1, 1)`,
			id, itemIndex, summary, toolName, meta)
	}
	seed("spawn-v2", "collab_agent", "collab_agent", `{"input":{"tool":"spawn_agent","activityKind":"started","prompt":"gAAAA-spawn","agentPath":"/root/reviewer"}}`, 0)
	seed("send-v2", "send_input", "send_input: gAAAA-send…", `{"input":{"tool":"send_input","activityKind":"interacted","prompt":"gAAAA-send","agentPath":"/root/reviewer"}}`, 1)
	legacy := `{"input":{"tool":"spawn_agent","prompt":"Review the parser"}}`
	seed("spawn-v1", "collab_agent", "collab_agent: Review the parser", legacy, 2)
	seed("custom-v2", "send_input", "Sent a follow-up successfully", `{"input":{"tool":"send_input","activityKind":"interacted","prompt":"gAAAA-custom","agentPath":"/root/reviewer"}}`, 3)
	seed("failed-v2", "send_input", "send_input: gAAAA-failed… (failed)", `{"input":{"tool":"send_input","activityKind":"interacted","prompt":"gAAAA-failed-payload","agentPath":"/root/reviewer"}}`, 4)
	for index := 0; index < 130; index++ {
		id := fmt.Sprintf("batched-v2-%03d", index)
		seed(id, "send_input", "send_input: gAAAA-batched…", `{"input":{"tool":"send_input","activityKind":"interacted","prompt":"gAAAA-batched-payload"}}`, index+5)
	}

	if err := applyMigration(db, migrationByVersion(t, 21)); err != nil {
		t.Fatalf("apply v21: %v", err)
	}

	for _, tc := range []struct {
		id          string
		wantSummary string
		wantPrompt  bool
	}{
		{id: "spawn-v2", wantSummary: "collab_agent"},
		{id: "send-v2", wantSummary: "send_input"},
		{id: "spawn-v1", wantSummary: "collab_agent: Review the parser", wantPrompt: true},
		{id: "custom-v2", wantSummary: "Sent a follow-up successfully"},
		{id: "failed-v2", wantSummary: "send_input (failed)"},
		{id: "batched-v2-129", wantSummary: "send_input"},
	} {
		var summary, meta string
		if err := db.QueryRow(`SELECT summary, meta FROM items WHERE thread_id = 't-v21' AND id = ?`, tc.id).Scan(&summary, &meta); err != nil {
			t.Fatalf("read %s: %v", tc.id, err)
		}
		if summary != tc.wantSummary {
			t.Errorf("%s summary = %q, want %q", tc.id, summary, tc.wantSummary)
		}
		var decoded struct {
			Input map[string]json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
			t.Fatalf("decode %s meta: %v", tc.id, err)
		}
		_, hasPrompt := decoded.Input["prompt"]
		if hasPrompt != tc.wantPrompt {
			t.Errorf("%s prompt presence = %v, want %v", tc.id, hasPrompt, tc.wantPrompt)
		}
	}
}

func TestV70TrimsUnverifiedCodexV2Profiles(t *testing.T) {
	db := migrateThrough(t, 69)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v70', '/v70', 'v70', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v70', 'p-v70', 'T', 'codex', '/tmp', '', 1, 1, 0, 'chat')`)

	seed := func(id, meta string, itemIndex int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
			VALUES (?, 't-v70', 0, ?, 'tool_call', 'assistant', 'completed',
			'collab_agent', '', 0, '', 'collab_agent', '', ?, 1, 1)`, id, itemIndex, meta)
	}
	seed("v2", `{"toolName":"collab_agent","input":{"tool":"spawn_agent","activityKind":"started","model":"gpt-parent","reasoningEffort":"high","agentPath":"/root/reviewer","receiverThreadIds":["child-1"]}}`, 0)
	seed("v1", `{"toolName":"collab_agent","input":{"tool":"spawn_agent","model":"gpt-child","reasoningEffort":"low","receiverThreadIds":["child-2"]}}`, 1)
	seed("v2-clean", `{"toolName":"collab_agent","input":{"tool":"spawn_agent","activityKind":"started","agentPath":"/root/worker","receiverThreadIds":["child-3"]}}`, 2)

	if err := applyMigration(db, migrationByVersion(t, 70)); err != nil {
		t.Fatalf("apply v70: %v", err)
	}

	readInput := func(id string) map[string]json.RawMessage {
		t.Helper()
		var meta string
		if err := db.QueryRow(`SELECT meta FROM items WHERE thread_id = 't-v70' AND id = ?`, id).Scan(&meta); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		var decoded struct {
			Input map[string]json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
			t.Fatalf("decode %s: %v", id, err)
		}
		return decoded.Input
	}

	v2 := readInput("v2")
	if _, ok := v2["model"]; ok {
		t.Fatalf("v70 kept the inferred V2 model: %#v", v2)
	}
	if _, ok := v2["reasoningEffort"]; ok {
		t.Fatalf("v70 kept the inferred V2 effort: %#v", v2)
	}
	if string(v2["agentPath"]) != `"/root/reviewer"` || string(v2["receiverThreadIds"]) != `["child-1"]` {
		t.Fatalf("v70 dropped V2 ownership metadata: %#v", v2)
	}

	v1 := readInput("v1")
	if string(v1["model"]) != `"gpt-child"` || string(v1["reasoningEffort"]) != `"low"` {
		t.Fatalf("v70 changed the typed V1 profile: %#v", v1)
	}
	clean := readInput("v2-clean")
	if string(clean["agentPath"]) != `"/root/worker"` {
		t.Fatalf("v70 changed an already-clean V2 row: %#v", clean)
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

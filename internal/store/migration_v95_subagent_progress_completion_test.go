package store

import (
	"encoding/json"
	"testing"
)

// The v95 backfill moves a detached Claude launch's persisted counters onto
// its completion sibling and strips them from the launch. A sibling that
// already carries numbers, a launch with no sibling, and a Codex spawn are
// all left alone.
func TestMigrationV95MovesSubagentProgressOntoCompletionSiblings(t *testing.T) {
	db := migrateThrough(t, 94)
	mustExec(t, db, `INSERT INTO threads(id, title, provider, workspace_path, created_at, updated_at) VALUES('t', 'T', 'claude', '/tmp', 1, 1)`)
	insert := func(id, kind, toolName, completionOf string, background int, meta string) {
		mustExec(t, db, `INSERT INTO items(id, thread_id, turn_index, item_index, kind, role, status, summary, tool_name, completion_of, is_background, meta, created_at, updated_at)
			VALUES(?, 't', 0, (SELECT COALESCE(MAX(item_index), -1) + 1 FROM items WHERE thread_id = 't'), ?, 'assistant', 'running', ?, ?, ?, ?, ?, 1, 1)`,
			id, kind, id, toolName, completionOf, background, meta)
	}
	progress := `{"toolUses":9,"totalTokens":61000,"durationMs":365000}`
	// Settled detached launch: numbers move.
	insert("agent", "tool_call", "Agent", "", 1, `{"input":{"description":"review"},"subagentProgress":`+progress+`,"live_background_active":false}`)
	insert("complete:agent", "tool_completion", "Agent", "agent", 1, `{"status_source":"task_updated"}`)
	// Sibling already carrying numbers keeps its own.
	insert("agent2", "tool_call", "Agent", "", 1, `{"subagentProgress":{"toolUses":1}}`)
	insert("complete:agent2", "tool_completion", "Agent", "agent2", 1, `{"subagentProgress":{"toolUses":5}}`)
	// Never settled: no sibling to move to, numbers stay.
	insert("agent3", "tool_call", "Agent", "", 1, `{"subagentProgress":{"toolUses":2}}`)
	// Awaited launch settles in place and keeps its numbers.
	insert("agent4", "tool_call", "Agent", "", 0, `{"subagentProgress":{"toolUses":3}}`)
	// Codex spawn and its completion are untouched.
	insert("spawn", "tool_call", "collab_agent", "", 1, `{"tool":"spawn_agent"}`)
	insert("complete:spawn", "tool_completion", "collab_agent", "spawn", 1, `{"subagentProgress":{"totalTokens":100}}`)

	if err := applyMigration(db, migrationByVersion(t, 95)); err != nil {
		t.Fatal(err)
	}

	meta := func(id string) map[string]any {
		var raw string
		if err := db.QueryRow(`SELECT meta FROM items WHERE thread_id = 't' AND id = ?`, id).Scan(&raw); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		decoded := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("%s meta %q: %v", id, raw, err)
		}
		return decoded
	}
	toolUses := func(id string) any {
		p, _ := meta(id)["subagentProgress"].(map[string]any)
		if p == nil {
			return nil
		}
		return p["toolUses"]
	}

	if got := toolUses("complete:agent"); got != float64(9) {
		t.Fatalf("sibling toolUses = %v, want 9", got)
	}
	if got := meta("complete:agent")["status_source"]; got != "task_updated" {
		t.Fatalf("sibling lost its own meta: %v", meta("complete:agent"))
	}
	if _, still := meta("agent")["subagentProgress"]; still {
		t.Fatalf("launch kept its counters: %v", meta("agent"))
	}
	if got := meta("agent")["input"].(map[string]any)["description"]; got != "review" {
		t.Fatalf("launch lost unrelated meta: %v", meta("agent"))
	}
	if got := toolUses("complete:agent2"); got != float64(5) {
		t.Fatalf("sibling with its own numbers was overwritten: %v", got)
	}
	if _, still := meta("agent2")["subagentProgress"]; still {
		t.Fatalf("settled launch kept its counters: %v", meta("agent2"))
	}
	if got := toolUses("agent3"); got != float64(2) {
		t.Fatalf("unsettled launch lost its counters: %v", got)
	}
	if got := toolUses("agent4"); got != float64(3) {
		t.Fatalf("awaited launch lost its counters: %v", got)
	}
	if p, _ := meta("complete:spawn")["subagentProgress"].(map[string]any); p["totalTokens"] != float64(100) || toolUses("spawn") != nil {
		t.Fatalf("codex rows changed: spawn=%v completion=%v", meta("spawn"), meta("complete:spawn"))
	}
}

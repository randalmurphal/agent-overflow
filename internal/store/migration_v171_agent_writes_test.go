package store

import (
	"strings"
	"testing"
)

// v171 adds tracker_agent_writes: its checks refuse a row no writer may
// leave, and a ticket's marks and a call's repeat each read their own
// index.
func TestAgentWritesMigration(t *testing.T) {
	db := migrateThrough(t, agentWritesMigrationVersion)
	insert := `INSERT INTO tracker_agent_writes(kind, site, issue_id, thread_id, call_id, step, state, at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := db.Exec(insert, "jira", "https://example.atlassian.net", "10001", "t1", "c1", "comment", "applied", 1); err != nil {
		t.Fatalf("a comment row: %v", err)
	}
	if _, err := db.Exec(insert, "linear", "example", "", "t1", "c2", "create", "sent", 1); err != nil {
		t.Fatalf("a create sent before its issue exists: %v", err)
	}
	for _, bad := range []struct {
		name string
		args []any
	}{
		{"an unknown kind", []any{"github", "s", "1", "t1", "c", "comment", "applied", 1}},
		{"no site", []any{"jira", "", "1", "t1", "c", "comment", "applied", 1}},
		{"no thread", []any{"jira", "s", "1", "", "c", "comment", "applied", 1}},
		{"no call", []any{"jira", "s", "1", "t1", "", "comment", "applied", 1}},
		{"an unknown step", []any{"jira", "s", "1", "t1", "c", "delete", "applied", 1}},
		{"an unknown state", []any{"jira", "s", "1", "t1", "c", "comment", "refused", 1}},
		{"no time", []any{"jira", "s", "1", "t1", "c", "comment", "applied", 0}},
		{"an edit without its issue", []any{"jira", "s", "", "t1", "c", "edit", "applied", 1}},
	} {
		if _, err := db.Exec(insert, bad.args...); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s: %v, want a CHECK failure", bad.name, err)
		}
	}
	planOf := func(query string, args ...any) string {
		t.Helper()
		var plan strings.Builder
		rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return plan.String()
	}
	if plan := planOf(agentWritesOfIssueSQL, "jira", "s", "1"); !strings.Contains(plan, "idx_tracker_agent_writes_issue") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("a ticket's marks:\n%s\nwant the issue index in its order", plan)
	}
	if plan := planOf(agentWriteRepeatSQL, "t1", "d"); !strings.Contains(plan, "idx_tracker_agent_writes_repeat") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("a repeat:\n%s\nwant the repeat index in its order", plan)
	}
}

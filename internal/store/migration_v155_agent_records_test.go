package store

import (
	"database/sql"
	"slices"
	"testing"
)

// v155 over a populated database: every standing_agents row becomes an
// assistant whose id is its thread's, with its home and tick setting kept,
// serving its thread's project, and its thread moves into the home;
// standing_agents is gone; every request reads as made by its caller; every
// remote watch reads as launched by the thread that owns it; and the new
// tables cascade as the spec says.
func TestMigrationV155AgentRecords(t *testing.T) {
	path := fileDBThrough(t, agentRecordsMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_groups (id, project_id, name, created_at, updated_at) VALUES ('g', 'p', 'Release', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, worktree_path, branch, group_id, created_branch, created_head_commit, model, created_at, updated_at, archived) VALUES
		('lead', 'p', 'Lead', 'claude', '/p/.worktrees/lead', '/p/.worktrees/lead', 'lead', 'g', 'main', 'abc', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('quiet', 'p', 'Quiet', 'codex', '/p', '', 1, 1, 1),
		('worker', 'p', 'Worker', 'claude', '/p', '', 1, 1, 0),
		('next', 'p', 'Next', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO standing_agents (thread_id, home, notify_on_tick, promoted_at) VALUES
		('lead', '/config/agents/lead', 1, 10), ('quiet', '/config/agents/quiet', 0, 20)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, created_at, updated_at) VALUES
		('spawn', 'lead', 'spawn', 'worker', 'accepted', 5, 5)`)
	mustExec(t, db, `INSERT INTO remote_watches (computer_id, request_id, thread_id, fingerprint, label, notification, created_at) VALUES
		('c', 'r1', 'lead', 'f', 'l', 'pending', 1), ('c', 'r2', 'worker', 'f', 'l', 'queued', 1)`)
	const requestBefore = `token || '|' || caller_thread_id || '|' || kind || '|' || target_thread_id || '|' || state`
	requestsWant, err := queryIDs(db, `SELECT `+requestBefore+` FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, agentRecordsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", agentRecordsMigrationVersion, err)
	}

	if got, err := queryIDs(db, `SELECT id || '|' || thread_id || '|' || home || '|' || creator_agent_id || '|' || created_at
		FROM agents ORDER BY id`); err != nil {
		t.Fatal(err)
	} else if want := []string{"lead|lead|/config/agents/lead||10", "quiet|quiet|/config/agents/quiet||20"}; !slices.Equal(got, want) {
		t.Errorf("agents = %q, want %q", got, want)
	}
	if got, err := queryIDs(db, `SELECT agent_id || '|' || project_id FROM agent_projects ORDER BY agent_id`); err != nil || !slices.Equal(got, []string{"lead|p", "quiet|p"}) {
		t.Errorf("agent_projects = %q %v", got, err)
	}
	const placed = `id || '|' || COALESCE(project_id, '') || '|' || workspace_path || '|' || COALESCE(worktree_path, '') || '|' ||
		COALESCE(branch, '') || '|' || COALESCE(group_id, '') || '|' || created_branch || '|' || created_head_commit`
	if got, err := queryIDs(db, `SELECT `+placed+` FROM threads ORDER BY id`); err != nil {
		t.Fatal(err)
	} else if want := []string{
		"lead||/config/agents/lead|||||", "next|p|/p|||||", "quiet||/config/agents/quiet|||||", "worker|p|/p|||||",
	}; !slices.Equal(got, want) {
		t.Errorf("threads = %q, want %q", got, want)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'standing_agents'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("standing_agents still exists: %d %v", left, err)
	}
	if got, err := queryIDs(db, `SELECT `+requestBefore+` FROM thread_requests ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, requestsWant) {
		t.Errorf("thread_requests changed:\n got %q\nwant %q", got, requestsWant)
	}
	if got, err := queryIDs(db, `SELECT token || '|' || made_in_thread_id FROM thread_requests`); err != nil || !slices.Equal(got, []string{"spawn|"}) {
		t.Errorf("made_in_thread_id = %q %v, want empty", got, err)
	}
	if got, err := queryIDs(db, `SELECT request_id || '|' || thread_id || '|' || launched_in_thread_id FROM remote_watches ORDER BY request_id`); err != nil || !slices.Equal(got, []string{"r1|lead|lead", "r2|worker|worker"}) {
		t.Errorf("remote_watches launched_in_thread_id = %q %v, want each watch's own thread", got, err)
	}
	if got, err := queryIDs(db, `SELECT name FROM pragma_index_info('idx_agent_past_threads_agent') ORDER BY seqno`); err != nil || !slices.Equal(got, []string{"agent_id", "superseded_at"}) {
		t.Errorf("idx_agent_past_threads_agent = %q %v", got, err)
	}

	for _, bad := range []string{
		`INSERT INTO agents (id, thread_id, home, created_at) VALUES ('dup', 'lead', '/h', 1)`,
		`INSERT INTO agents (id, thread_id, home, all_projects, created_at) VALUES ('x', 'worker', '/h', 2, 1)`,
		`INSERT INTO agents (id, thread_id, home, created_at) VALUES ('x', 'missing', '/h', 1)`,
		`INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES ('worker', 'missing', 1)`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}

	// A past thread's delete leaves the assistant; the current thread's
	// delete takes the assistant and leaves its other past threads as
	// ordinary threads.
	mustExec(t, db, `UPDATE agents SET thread_id = 'next' WHERE id = 'lead'`)
	mustExec(t, db, `INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES ('lead', 'lead', 30), ('worker', 'lead', 31)`)
	mustExec(t, db, `DELETE FROM threads WHERE id = 'worker'`)
	if got, err := queryIDs(db, `SELECT thread_id FROM agent_past_threads ORDER BY thread_id`); err != nil || !slices.Equal(got, []string{"lead"}) {
		t.Errorf("past threads after deleting one = %q %v", got, err)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 'next'`)
	if got, err := queryIDs(db, `SELECT id FROM agents ORDER BY id`); err != nil || !slices.Equal(got, []string{"quiet"}) {
		t.Errorf("agents after deleting the current thread = %q %v", got, err)
	}
	if got, err := queryIDs(db, `SELECT thread_id FROM agent_past_threads`); err != nil || len(got) != 0 {
		t.Errorf("past threads of a deleted assistant = %q %v", got, err)
	}
	if got, err := queryIDs(db, `SELECT id FROM threads WHERE id = 'lead'`); err != nil || len(got) != 1 {
		t.Errorf("a deleted assistant's past thread went with it: %q %v", got, err)
	}
}

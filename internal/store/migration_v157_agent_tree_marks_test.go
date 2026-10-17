package store

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// agentTreeMarksFromScratch is what every local spawn's marks must hold,
// found the long way: for each spawn that reaches its target, a walk down
// the spawns below that target, counting the open work each thread made at
// each depth (agentOpenWorkSQL). Spawns without open work below are left
// out.
func agentTreeMarksFromScratch(t *testing.T, db sqlQueryer) map[string][agentOpenDepth]int64 {
	t.Helper()
	type link struct{ token, caller, target string }
	var links []link
	rows, err := db.Query(`SELECT token, caller_thread_id, target_thread_id FROM thread_requests r WHERE ` + agentSpawnLinkSQL("r"))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.token, &l.caller, &l.target); err != nil {
			t.Fatal(err)
		}
		links = append(links, l)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	work := map[string]int64{}
	rows, err = db.Query(`SELECT caller_thread_id, COUNT(*) FROM thread_requests r WHERE ` + agentOpenWorkSQL("r") + ` GROUP BY caller_thread_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var thread string
		var n int64
		if err := rows.Scan(&thread, &n); err != nil {
			t.Fatal(err)
		}
		work[thread] = n
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	children := map[string][]string{}
	for _, l := range links {
		children[l.caller] = append(children[l.caller], l.target)
	}
	want := map[string][agentOpenDepth]int64{}
	for _, l := range links {
		var mark [agentOpenDepth]int64
		level := []string{l.target}
		for d := range agentOpenDepth {
			var next []string
			for _, thread := range level {
				mark[d] += work[thread]
				next = append(next, children[thread]...)
			}
			level = next
		}
		if mark != ([agentOpenDepth]int64{}) {
			want[l.token] = mark
		}
	}
	return want
}

// agentTreeMarks is every request's stored marks, those holding any.
func agentTreeMarks(t *testing.T, db sqlQueryer) map[string][agentOpenDepth]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT token, open_below_0, open_below_1, open_below_2, open_below_3 FROM thread_requests
		WHERE open_below_0 <> 0 OR open_below_1 <> 0 OR open_below_2 <> 0 OR open_below_3 <> 0`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][agentOpenDepth]int64{}
	for rows.Next() {
		var token string
		var mark [agentOpenDepth]int64
		if err := rows.Scan(&token, &mark[0], &mark[1], &mark[2], &mark[3]); err != nil {
			t.Fatal(err)
		}
		got[token] = mark
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return got
}

// assertAgentTreeMarks fails when a stored mark differs from the walk's.
func assertAgentTreeMarks(t *testing.T, db sqlQueryer, what string) {
	t.Helper()
	if diff := agentTreeMarksDiff(t, db); diff != "" {
		t.Fatalf("%s: marks differ from the walk:\n%s", what, diff)
	}
}

// agentTreeMarksDiff is each mark that differs from the walk's, or empty.
func agentTreeMarksDiff(t *testing.T, db sqlQueryer) string {
	t.Helper()
	want, got := agentTreeMarksFromScratch(t, db), agentTreeMarks(t, db)
	if maps.Equal(want, got) {
		return ""
	}
	var diff []string
	for _, token := range slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
		for token := range want {
			yield(token, true)
		}
		for token := range got {
			yield(token, true)
		}
	}))) {
		if want[token] != got[token] {
			diff = append(diff, fmt.Sprintf("%s: stored %v, walk %v", token, got[token], want[token]))
		}
	}
	return strings.Join(diff, "\n")
}

// v157 over a populated database: the older of two local spawns of one
// thread becomes a lineage row and the index refuses a second; every
// spawn's marks are filled from the open work below it, four threads down
// and no further; and the triggers keep them from there.
func TestMigrationV157AgentTreeMarks(t *testing.T) {
	path := fileDBThrough(t, agentTreeMarksMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	chain := []string{"t0", "t1", "t2", "t3", "t4", "t5"}
	for _, id := range append(chain, "side") {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
			VALUES (?, 'p', ?, 'claude', '/p', '', 1, 1, 0)`, id, id)
	}
	insert := func(token, caller, kind, target string, notify int, settled any, created int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, notify, state, settled_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, CASE WHEN ? IS NULL THEN 'running' ELSE 'finished' END, ?, ?, ?)`,
			token, caller, kind, target, notify, settled, settled, created, created)
	}
	for i := 0; i+1 < len(chain); i++ {
		insert("spawn-"+chain[i+1], chain[i], ThreadRequestSpawn, chain[i+1], 0, 5, 10+i)
	}
	insert("old-side", "t0", ThreadRequestSpawn, "t3", 0, 5, 1)
	insert("open-t5", "t5", ThreadRequestSend, "side", 1, nil, 30)
	insert("open-t3", "t3", ThreadRequestSend, "side", 1, nil, 31)
	insert("quiet-t3", "t3", ThreadRequestSend, "side", 0, nil, 32)
	insert("done-t2", "t2", ThreadRequestSend, "side", 1, 5, 33)

	if err := applyMigration(db, migrationByVersion(t, agentTreeMarksMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", agentTreeMarksMigrationVersion, err)
	}

	var lineage int
	if err := db.QueryRow(`SELECT lineage_only FROM thread_requests WHERE token = 'old-side'`).Scan(&lineage); err != nil || lineage != 1 {
		t.Fatalf("the older spawn of t3 reads lineage_only %d, %v; want 1", lineage, err)
	}
	if _, err := db.Exec(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, created_at, updated_at)
		VALUES ('again', 'side', 'spawn', 't4', 'running', 40, 40)`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("a second local spawn of t4 = %v; want the unique index to refuse it", err)
	}
	want := map[string][agentOpenDepth]int64{
		"spawn-t5": {1, 0, 0, 0}, "spawn-t4": {0, 1, 0, 0}, "spawn-t3": {1, 0, 1, 0}, "spawn-t2": {0, 1, 0, 1}, "spawn-t1": {0, 0, 1, 0},
	}
	if got := agentTreeMarks(t, db); !maps.Equal(got, want) {
		t.Fatalf("filled marks %v; want %v", got, want)
	}
	assertAgentTreeMarks(t, db, "after the fill")
	for _, trigger := range []string{"open_below_insert", "open_below_delete", "open_below_update", "spawn_cycle_insert", "spawn_cycle_update"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`,
			"trg_thread_requests_"+trigger).Scan(&n); err != nil || n != 1 {
			t.Fatalf("trigger %s: %d, %v", trigger, n, err)
		}
	}
	var kept int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_thread_requests_spawn_caller'`).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("idx_thread_requests_spawn_caller, which nothing reads: %d kept, %v", kept, err)
	}
	mustExec(t, db, `UPDATE thread_requests SET settled_at = 50, state = 'finished' WHERE token = 'open-t5'`)
	assertAgentTreeMarks(t, db, "after a settle")
}

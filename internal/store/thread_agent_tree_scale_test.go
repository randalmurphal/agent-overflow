package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// agentTreeShape is a standing agent's ledger and the computer around it:
// a chief that spawned three managers, which spawned ten workers, then
// history settled answers and spawns local spawns settled 20 days ago (a
// loop spawning a worker every 15 minutes leaves 2,880 in the 30 days a
// settled spawn is kept), 300 answers settled inside the window, open
// sends and openSpawns open spawns of the chief, deep open sends of the
// workers, two spawns down, and busy other threads with an open send each,
// none of them in the chief's tree. Every answer is 4 KiB.
type agentTreeShape struct {
	history, open, spawns, openSpawns, deep, busy int
}

func (shape agentTreeShape) String() string {
	var parts []string
	for _, part := range []struct {
		n    int
		what string
	}{
		{shape.history, "settled"}, {shape.open, "open"}, {shape.spawns, "settled spawns"},
		{shape.openSpawns, "open spawns"}, {shape.deep, "open at depth 2"}, {shape.busy, "busy threads"},
	} {
		if part.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.n, part.what))
		}
	}
	return strings.Join(parts, ", ")
}

func agentTreeSpawnHistory(t *testing.T, s *Store, shape agentTreeShape) (chief string, since int64) {
	t.Helper()
	now := time.Now().UnixMilli()
	chief = "chief"
	mustCreateThread(t, s, chief)
	managers, workers := make([]string, 3), make([]string, 10)
	created := int64(1)
	for i := range managers {
		managers[i] = fmt.Sprintf("manager-%d", i)
		mustCreateThread(t, s, managers[i])
		seedAgentRequest(t, s, "spawn-"+managers[i], chief, ThreadRequestSpawn, managers[i], false, created)
		created++
	}
	for i := range workers {
		workers[i] = fmt.Sprintf("worker-%d", i)
		mustCreateThread(t, s, workers[i])
		seedAgentRequest(t, s, "spawn-"+workers[i], managers[i%len(managers)], ThreadRequestSpawn, workers[i], false, created)
		created++
	}
	if shape.busy > 0 {
		list := agentTreeThreadColumns(t, s)
		mustExec(t, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
			INSERT INTO threads (id, `+list+`) SELECT printf('busy-%06d', i), `+list+` FROM seq, threads WHERE threads.id = ?2`,
			shape.busy, chief)
	}
	since = now - 30_000
	old := now - 20*24*time.Hour.Milliseconds()
	for _, batch := range []struct {
		prefix, caller, kind, target string
		n                            int
		settled                      any
	}{
		{"old", "'chief'", "send", "printf('worker-%d', i % 10)", shape.history, old},
		{"spawned", "'chief'", "spawn", "printf('spawned-thread-%06d', i)", shape.spawns, old},
		{"recent", "'chief'", "send", "printf('worker-%d', i % 10)", 300, now - 1_000},
		{"open", "'chief'", "send", "printf('worker-%d', i % 10)", shape.open, nil},
		{"open-spawn", "'chief'", "spawn", "printf('open-spawn-thread-%06d', i)", shape.openSpawns, nil},
		{"deep", "printf('worker-%d', i % 10)", "send", "'chief'", shape.deep, nil},
		{"busy", "printf('busy-%06d', i)", "send", "'chief'", shape.busy, nil},
	} {
		mustExec(t, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
			INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, notify, state, answer, answer_kind,
				settled_at, created_at, updated_at)
			SELECT printf('%s-%06d', ?2, i), `+batch.caller+`, ?3, `+batch.target+`, ?3 <> 'spawn',
			       CASE WHEN ?4 IS NULL THEN 'running' ELSE 'finished' END,
			       CASE WHEN ?4 IS NULL THEN NULL ELSE zeroblob(4096) END,
			       CASE WHEN ?4 IS NULL THEN '' ELSE 'final' END, ?4, ?5 + i, ?5 + i FROM seq WHERE ?1 > 0`,
			batch.n, batch.prefix, batch.kind, batch.settled, created)
		created += int64(batch.n)
	}
	return chief, since
}

// The listing and a read of one named row cost what the listed rows and the
// tree's depth cost, not the chief's history, its open spawns, the open
// work below it or the open work of threads outside its tree, before and
// after ANALYZE: a read of one named row stays within 1 ms, at the top of
// the tree or two spawns down, and the listing within 10 ms.
func TestListThreadAgentTreeAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds up to 57,000 requests")
	}
	for _, shape := range []agentTreeShape{
		{history: 3_000, open: 200}, {history: 3_000, open: 2_200},
		{history: 50_000, open: 200}, {history: 50_000, open: 2_200},
		{history: 50_000, open: 200, spawns: 1_500}, {history: 50_000, open: 200, spawns: 5_000},
		{history: 50_000, open: 200, busy: 2_000}, {history: 50_000, open: 200, busy: 20_000},
		{history: 50_000, open: 200, openSpawns: 2_000}, {history: 50_000, open: 200, deep: 500},
		{history: 50_000, open: 200, spawns: 5_000, openSpawns: 2_000, deep: 500},
	} {
		s := newTestStore(t)
		chief, since := agentTreeSpawnHistory(t, s, shape)
		read := ThreadAgentTreeRead{Root: chief, SettledSince: since, Depth: 4, Limit: 128, AnswerHead: 160}
		// The fewest of more runs than the read pool has connections, so
		// no connection's first run, which compiles the statement, is the
		// one measured.
		fewest := func(call func()) time.Duration {
			best := time.Duration(1 << 62)
			for range 20 {
				start := time.Now()
				call()
				best = min(best, time.Since(start))
			}
			return best
		}
		named := []struct {
			token string
			depth int
		}{{"recent-000050", 0}}
		if shape.deep > 0 {
			named = append(named, struct {
				token string
				depth int
			}{"deep-000100", 2})
		}
		for _, analyzed := range []bool{false, true} {
			if analyzed {
				mustExec(t, s.db, `ANALYZE`)
			}
			name := fmt.Sprintf("%v, analyzed %v", shape, analyzed)
			listing := fewest(func() {
				rows, more, err := s.ListThreadAgentTree(read)
				if err != nil || len(rows) != read.Limit || !more {
					t.Fatalf("%s: listing %d rows, more %v, %v", name, len(rows), more, err)
				}
			})
			if listing > 10*time.Millisecond {
				t.Errorf("%s: the listing took %v; at most 10ms", name, listing)
			}
			report := fmt.Sprintf("listing %v", listing)
			for _, row := range named {
				one := read
				one.Tokens = []string{row.token}
				took := fewest(func() {
					rows, _, err := s.ListThreadAgentTree(one)
					if err != nil || len(rows) != 1 || rows[0].Token != row.token || rows[0].Depth != row.depth {
						t.Fatalf("%s: named read of %s: %+v, %v", name, row.token, rows, err)
					}
				})
				if took > time.Millisecond {
					t.Errorf("%s: the named read of %s took %v; at most 1ms", name, row.token, took)
				}
				report += fmt.Sprintf(", %s %v", row.token, took)
			}
			t.Logf("%s: %s", name, report)
		}
	}
}

// Deleting a thread that holds 5,000 open local spawns, four spawns below
// a chief, settles each one's marks up the chain in the delete's cascade
// within 1 s, and leaves the marks the walk finds.
func TestAgentTreeMarksCascadeAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 5,000 spawned threads")
	}
	t.Parallel()
	const spawns = 5_000
	s := newTestStore(t)
	chain := []string{"chief", "manager", "lead", "worker", "busy"}
	for i, id := range chain {
		mustCreateThread(t, s, id)
		if i > 0 {
			seedAgentRequest(t, s, "spawn-"+id, chain[i-1], ThreadRequestSpawn, id, false, int64(i))
		}
	}
	mustExec(t, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
		INSERT INTO threads (id, `+agentTreeThreadColumns(t, s)+`) SELECT printf('child-%06d', i), `+agentTreeThreadColumns(t, s)+`
		  FROM seq, threads WHERE threads.id = 'busy'`, spawns)
	mustExec(t, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
		INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, created_at, updated_at)
		SELECT printf('open-spawn-%06d', i), 'busy', 'spawn', printf('child-%06d', i), 'running', 10 + i, 10 + i FROM seq`, spawns)
	var marks [agentOpenDepth]int64
	if err := s.db.QueryRow(`SELECT open_below_0, open_below_1, open_below_2, open_below_3 FROM thread_requests WHERE token = 'spawn-manager'`).
		Scan(&marks[0], &marks[1], &marks[2], &marks[3]); err != nil || marks != [agentOpenDepth]int64{1, 1, 1, spawns} {
		t.Fatalf("the manager's spawn carries %v, %v; want the busy thread's %d spawns at depth 3", marks, err, spawns)
	}
	start := time.Now()
	if err := s.DeleteThread("busy"); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("deleting a thread with %d open local spawns took %v", spawns, took)
	if took > time.Second {
		t.Errorf("the delete took %v; at most 1s", took)
	}
	assertAgentTreeMarks(t, s.db, "after the delete")
}

// agentTreeThreadColumns is every column of threads but id, for copying a
// thread's row.
func agentTreeThreadColumns(t *testing.T, s *Store) string {
	t.Helper()
	columns, err := s.db.Query(`SELECT name FROM pragma_table_info('threads') WHERE name <> 'id'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for columns.Next() {
		var name string
		if err := columns.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := columns.Close(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(names, ", ")
}

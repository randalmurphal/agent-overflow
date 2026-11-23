package store

import (
	"fmt"
	"path/filepath"
	"slices"
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

func agentTreeSpawnHistory(t testing.TB, s *Store, shape agentTreeShape) (chief string, since int64) {
	t.Helper()
	agentTreeSpawnBase(t, s, shape.history)
	return agentTreeSpawnRest(t, s, shape)
}

// agentTreeSpawnBase seeds the chief, its three managers and ten workers,
// and the settled history: the part of a shape most of its rows and bytes
// are in.
func agentTreeSpawnBase(t testing.TB, s *Store, history int) {
	t.Helper()
	chief := "chief"
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
	old := time.Now().UnixMilli() - 20*24*time.Hour.Milliseconds()
	seedAgentTreeBatch(t, s, "old", "'chief'", "send", "printf('worker-%d', i % 10)", history, old, created)
}

// agentTreeSpawnRest seeds the rest of shape on a store agentTreeSpawnBase
// seeded, created after the base's rows.
func agentTreeSpawnRest(t testing.TB, s *Store, shape agentTreeShape) (chief string, since int64) {
	t.Helper()
	// The base's 13 spawns and its history, created from 1.
	created := int64(14 + shape.history)
	now := time.Now().UnixMilli()
	chief = "chief"
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
		{"spawned", "'chief'", "spawn", "printf('spawned-thread-%06d', i)", shape.spawns, old},
		{"recent", "'chief'", "send", "printf('worker-%d', i % 10)", 300, now - 1_000},
		{"open", "'chief'", "send", "printf('worker-%d', i % 10)", shape.open, nil},
		{"open-spawn", "'chief'", "spawn", "printf('open-spawn-thread-%06d', i)", shape.openSpawns, nil},
		{"deep", "printf('worker-%d', i % 10)", "send", "'chief'", shape.deep, nil},
		{"busy", "printf('busy-%06d', i)", "send", "'chief'", shape.busy, nil},
	} {
		seedAgentTreeBatch(t, s, batch.prefix, batch.caller, batch.kind, batch.target, batch.n, batch.settled, created)
		created += int64(batch.n)
	}
	return chief, since
}

// seedAgentTreeBatch inserts n requests of one kind from caller to target,
// SQL expressions of the row number i, settled at settled or open when it
// is nil, created from created on. A settled answer is 4 KiB.
func seedAgentTreeBatch(t testing.TB, s *Store, prefix, caller, kind, target string, n int, settled any, created int64) {
	t.Helper()
	mustExec(t, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
		INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, notify, state, answer, answer_kind,
			settled_at, delivered_at, delivered_how, confirmed_at, created_at, updated_at)
		SELECT printf('%s-%06d', ?2, i), `+caller+`, ?3, `+target+`, ?3 <> 'spawn',
		       CASE WHEN ?4 IS NULL THEN 'running' ELSE 'finished' END,
		       CASE WHEN ?4 IS NULL THEN NULL ELSE zeroblob(4096) END,
		       CASE WHEN ?4 IS NULL THEN '' ELSE 'final' END, ?4,
		       ?4, CASE WHEN ?4 IS NULL THEN '' ELSE 'queued' END, ?4, ?5 + i, ?5 + i FROM seq WHERE ?1 > 0`,
		n, prefix, kind, settled, created)
}

// agentTreeEveryKind is a shape with rows of every kind, each past the
// listing's limit of 128 and deep enough for a named read two spawns down:
// what the listing and the named read are checked over. The shapes in
// agentTreeScaleShapes add volume, which changes their times and not their
// answers.
var agentTreeEveryKind = agentTreeShape{history: 300, open: 200, spawns: 200, openSpawns: 200, deep: 200, busy: 200}

// agentTreeScaleShapes are the ledgers BenchmarkListThreadAgentTree times.
var agentTreeScaleShapes = []agentTreeShape{
	{history: 3_000, open: 200}, {history: 3_000, open: 2_200},
	{history: 50_000, open: 200}, {history: 50_000, open: 2_200},
	{history: 50_000, open: 200, spawns: 1_500}, {history: 50_000, open: 200, spawns: 5_000},
	{history: 50_000, open: 200, busy: 2_000}, {history: 50_000, open: 200, busy: 20_000},
	{history: 50_000, open: 200, openSpawns: 2_000}, {history: 50_000, open: 200, deep: 500},
	{history: 50_000, open: 200, spawns: 5_000, openSpawns: 2_000, deep: 500},
}

// agentTreeNamedReads are the rows a named read is checked and timed on:
// one at the top of the tree and, when the shape has deep work, one two
// spawns down.
func agentTreeNamedReads(shape agentTreeShape) []agentTreeNamedRead {
	named := []agentTreeNamedRead{{"recent-000050", 0}}
	if shape.deep > 0 {
		named = append(named, agentTreeNamedRead{"deep-000100", 2})
	}
	return named
}

type agentTreeNamedRead struct {
	token string
	depth int
}

// The listing and a read of one named row, at the top of the tree or two
// spawns down, answer the same rows over the chief's history, its open
// spawns, the open work below it and the open work of threads outside its
// tree, before and after ANALYZE.
func TestListThreadAgentTreeOverEveryKindOfRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	chief, since := agentTreeSpawnHistory(t, s, agentTreeEveryKind)
	read := ThreadAgentTreeRead{Root: chief, SettledSince: since, Depth: 4, Limit: 128, AnswerHead: 160}
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			mustExec(t, s.db, `ANALYZE`)
		}
		rows, more, err := s.ListThreadAgentTree(read)
		if err != nil || len(rows) != read.Limit || !more {
			t.Fatalf("analyzed %v: listing %d rows, more %v, %v", analyzed, len(rows), more, err)
		}
		if got, want := agentTreeRowNames(rows), agentTreeEveryKindListing(); !slices.Equal(got, want) {
			t.Fatalf("analyzed %v: listing\n%v\nwant\n%v", analyzed, got, want)
		}
		for _, row := range agentTreeNamedReads(agentTreeEveryKind) {
			one := read
			one.Tokens = []string{row.token}
			rows, _, err := s.ListThreadAgentTree(one)
			if err != nil || len(rows) != 1 || rows[0].Token != row.token || rows[0].Depth != row.depth {
				t.Fatalf("analyzed %v: named read of %s: %+v, %v", analyzed, row.token, rows, err)
			}
		}
	}
}

// agentTreeEveryKindListing is the listing of agentTreeEveryKind, each row
// as its token at its depth, in tree order: a spawn, then what its thread
// made, oldest first. The chief spawned the managers first, so manager 0's
// spawns of workers 0, 3, 6 and 9, each with its 20 open sends two spawns
// down, come first (85 rows), and manager 1 with workers 1 and 4 fills the
// limit.
func agentTreeEveryKindListing() []string {
	var want []string
	for _, manager := range []struct {
		id      int
		workers []int
	}{{0, []int{0, 3, 6, 9}}, {1, []int{1, 4}}} {
		want = append(want, fmt.Sprintf("spawn-manager-%d@0", manager.id))
		for _, worker := range manager.workers {
			want = append(want, fmt.Sprintf("spawn-worker-%d@1", worker))
			for i := worker; i < agentTreeEveryKind.deep; i += 10 {
				want = append(want, fmt.Sprintf("deep-%06d@2", i))
			}
		}
	}
	return want
}

// agentTreeRowNames names each row as agentTreeEveryKindListing does.
func agentTreeRowNames(rows []ThreadAgentRequest) []string {
	names := make([]string, len(rows))
	for i, row := range rows {
		names[i] = fmt.Sprintf("%s@%d", row.Token, row.Depth)
	}
	return names
}

// BenchmarkListThreadAgentTree times the listing and each named read over
// each shape, before and after ANALYZE. The shapes of one history size
// share its base, seeded once and copied.
func BenchmarkListThreadAgentTree(b *testing.B) {
	dir := b.TempDir()
	bases := map[int]string{}
	for _, shape := range agentTreeScaleShapes {
		b.Run(shape.String(), func(b *testing.B) {
			base, ok := bases[shape.history]
			if !ok {
				base = filepath.Join(dir, fmt.Sprintf("history-%d.sqlite", shape.history))
				if err := copyTestStoreTemplate(b, base); err != nil {
					b.Fatal(err)
				}
				seeded, err := New(base)
				if err != nil {
					b.Fatal(err)
				}
				agentTreeSpawnBase(b, seeded, shape.history)
				if err := seeded.Close(); err != nil {
					b.Fatal(err)
				}
				bases[shape.history] = base
			}
			// Each shape's database, a few hundred MB, is removed before
			// the next is seeded.
			path := filepath.Join(b.TempDir(), "store.sqlite")
			if err := copyStoreFile(base, path); err != nil {
				b.Fatal(err)
			}
			s := openTestStore(b, path)
			chief, since := agentTreeSpawnRest(b, s, shape)
			read := ThreadAgentTreeRead{Root: chief, SettledSince: since, Depth: 4, Limit: 128, AnswerHead: 160}
			for _, analyzed := range []bool{false, true} {
				if analyzed {
					mustExec(b, s.db, `ANALYZE`)
				}
				b.Run(fmt.Sprintf("analyzed %v/listing", analyzed), func(b *testing.B) {
					for b.Loop() {
						if _, _, err := s.ListThreadAgentTree(read); err != nil {
							b.Fatal(err)
						}
					}
				})
				for _, row := range agentTreeNamedReads(shape) {
					one := read
					one.Tokens = []string{row.token}
					b.Run(fmt.Sprintf("analyzed %v/%s", analyzed, row.token), func(b *testing.B) {
						for b.Loop() {
							if _, _, err := s.ListThreadAgentTree(one); err != nil {
								b.Fatal(err)
							}
						}
					})
				}
			}
		})
	}
}

// Deleting a thread that holds 5,000 open local spawns, four spawns below
// a chief, settles each one's marks up the chain in the delete's cascade and
// leaves the marks the walk finds. Its time is logged, not gated.
func TestAgentTreeMarksCascadeAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 5,000 spawned threads")
	}
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
	t.Logf("deleting a thread with %d open local spawns took %v", spawns, time.Since(start))
	assertAgentTreeMarks(t, s.db, "after the delete")
}

// agentTreeThreadColumns is every column of threads but id, for copying a
// thread's row.
func agentTreeThreadColumns(t testing.TB, s *Store) string {
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

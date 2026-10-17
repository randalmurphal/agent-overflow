package store

import (
	"fmt"
	"testing"
	"time"
)

// agentTreeSpawnHistory seeds a standing agent's ledger: a chief that
// spawned three managers, which spawned ten workers, owed open sends,
// holding answers settled inside the window, a history of answers settled
// 20 days ago and spawns more local spawns settled then, as a loop
// spawning a worker every 15 minutes leaves 2,880 in the 30 days a settled
// spawn is kept. Every answer is 4 KiB.
func agentTreeSpawnHistory(t *testing.T, s *Store, history, open, spawns int) (chief string, since int64) {
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
	since = now - 30_000
	for _, batch := range []struct {
		prefix  string
		n       int
		settled any
	}{
		{"old", history, now - 20*24*time.Hour.Milliseconds()},
		{"spawned", spawns, now - 20*24*time.Hour.Milliseconds()},
		{"recent", 300, now - 1_000},
		{"open", open, nil},
	} {
		mustExec(t, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
			INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, notify, state, answer, answer_kind,
				settled_at, created_at, updated_at)
			SELECT printf('%s-%06d', ?2, i), ?3, CASE WHEN ?2 = 'spawned' THEN 'spawn' ELSE 'send' END,
			       CASE WHEN ?2 = 'spawned' THEN printf('spawned-thread-%06d', i) ELSE printf('worker-%d', i % 10) END,
			       CASE WHEN ?2 = 'spawned' THEN 0 ELSE 1 END,
			       CASE WHEN ?4 IS NULL THEN 'running' ELSE 'finished' END,
			       CASE WHEN ?4 IS NULL THEN NULL ELSE zeroblob(4096) END,
			       CASE WHEN ?4 IS NULL THEN '' ELSE 'final' END, ?4, ?5 + i, ?5 + i FROM seq`,
			batch.n, batch.prefix, chief, batch.settled, created)
		created += int64(batch.n)
	}
	return chief, since
}

// The listing and a read of one named row cost what the listed rows and the
// tree's spawns cost, not the chief's history, before and after ANALYZE: a
// read of one named row stays within 1 ms; the listing of 200 open rows
// within 10 ms; and at 50,000 settled answers, with 2,000 more open or up to
// 5,000 settled spawns more, the listing costs at most twice what it costs
// at 3,000 settled answers with the same open rows.
func TestListThreadAgentTreeAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 55,000 requests")
	}
	type key struct {
		open     int
		analyzed bool
	}
	small := map[key]time.Duration{}
	for _, shape := range []struct{ history, open, spawns int }{
		{3_000, 200, 0}, {3_000, 2_200, 0}, {50_000, 200, 0}, {50_000, 2_200, 0}, {50_000, 200, 1_500}, {50_000, 200, 5_000},
	} {
		s := newTestStore(t)
		chief, since := agentTreeSpawnHistory(t, s, shape.history, shape.open, shape.spawns)
		read := ThreadAgentTreeRead{Root: chief, SettledSince: since, Depth: 4, OpenDepth: 4, Limit: 128, AnswerHead: 160}
		// The fewest of more runs than the read pool has connections: after
		// ANALYZE each connection compiles the statement again once.
		fewest := func(call func()) time.Duration {
			best := time.Duration(1 << 62)
			for range 20 {
				start := time.Now()
				call()
				best = min(best, time.Since(start))
			}
			return best
		}
		for _, analyzed := range []bool{false, true} {
			if analyzed {
				mustExec(t, s.db, `ANALYZE`)
			}
			listing := fewest(func() {
				rows, more, err := s.ListThreadAgentTree(read)
				if err != nil || len(rows) != read.Limit || !more {
					t.Fatalf("listing: %d rows, more %v, %v", len(rows), more, err)
				}
			})
			named := read
			named.Tokens = []string{"recent-000050"}
			row := fewest(func() {
				rows, _, err := s.ListThreadAgentTree(named)
				if err != nil || len(rows) != 1 || rows[0].Token != "recent-000050" || rows[0].Depth != 0 {
					t.Fatalf("named read: %+v, %v", rows, err)
				}
			})
			name := fmt.Sprintf("%d settled, %d open, %d settled spawns, analyzed %v", shape.history, shape.open, shape.spawns, analyzed)
			t.Logf("%s: listing %v, one named row %v", name, listing, row)
			if row > time.Millisecond {
				t.Errorf("%s: one named row took %v; at most 1ms", name, row)
			}
			if shape.open == 200 && listing > 10*time.Millisecond {
				t.Errorf("%s: the listing took %v; at most 10ms", name, listing)
			}
			at := key{shape.open, analyzed}
			if shape.history == 3_000 {
				small[at] = listing
			} else if listing > 2*small[at] {
				t.Errorf("%s: the listing took %v, %v at 3000 settled; the history must not cost more than that again", name, listing, small[at])
			}
		}
	}
}

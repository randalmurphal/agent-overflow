package store

import (
	"fmt"
	"testing"
	"time"
)

// agentTreeHistory seeds a standing agent's ledger: a chief that spawned
// three managers, which spawned ten workers, owed open sends, holding
// answers settled inside the window and a history of answers settled 20
// days ago, every answer 4 KiB.
func agentTreeHistory(t *testing.T, s *Store, history, open int) (chief string, since int64) {
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
		{"recent", 300, now - 1_000},
		{"open", open, nil},
	} {
		mustExec(t, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
			INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, notify, state, answer, answer_kind,
				settled_at, created_at, updated_at)
			SELECT printf('%s-%06d', ?2, i), ?3, 'send', printf('worker-%d', i % 10), 1,
			       CASE WHEN ?4 IS NULL THEN 'running' ELSE 'finished' END,
			       CASE WHEN ?4 IS NULL THEN NULL ELSE zeroblob(4096) END,
			       CASE WHEN ?4 IS NULL THEN '' ELSE 'final' END, ?4, ?5 + i, ?5 + i FROM seq`,
			batch.n, batch.prefix, chief, batch.settled, created)
		created += int64(batch.n)
	}
	return chief, since
}

// The listing and a read of one named row cost what the listed rows and the
// tree's spawns cost, not the chief's history: at 50,000 settled answers,
// and with 2,000 more open, before and after ANALYZE, the listing stays
// within 10 ms and the named read within 1 ms.
func TestListThreadAgentTreeAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 52,000 requests")
	}
	for _, shape := range []struct {
		history, open int
		limited       bool
	}{
		{3_000, 200, false}, {50_000, 200, true}, {50_000, 2_200, true},
	} {
		s := newTestStore(t)
		chief, since := agentTreeHistory(t, s, shape.history, shape.open)
		read := ThreadAgentTreeRead{Root: chief, SettledSince: since, Depth: 4, OpenDepth: 4, Limit: 128, AnswerHead: 160}
		fewest := func(call func()) time.Duration {
			best := time.Duration(1 << 62)
			for range 5 {
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
			t.Logf("%d settled, %d open, analyzed %v: listing %v, one named row %v", shape.history, shape.open, analyzed, listing, row)
			if shape.limited && (listing > 10*time.Millisecond || row > time.Millisecond) {
				t.Errorf("%d settled, %d open, analyzed %v: listing %v (at most 10ms), one named row %v (at most 1ms)",
					shape.history, shape.open, analyzed, listing, row)
			}
		}
	}
}

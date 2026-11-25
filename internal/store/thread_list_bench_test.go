package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// threadListBenchThreads is how many sidebar threads
// BenchmarkListThreadsLinked lists, each with three settled turns of a
// user and an assistant row.
const threadListBenchThreads = 5_000

// seedThreadListBench seeds threadListBenchThreads listed threads at path.
func seedThreadListBench(b *testing.B, path string) {
	b.Helper()
	if err := copyTestStoreTemplate(b, path); err != nil {
		b.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		b.Fatal(err)
	}
	for i := range threadListBenchThreads {
		id := fmt.Sprintf("list-%06d", i)
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			b.Fatal(err)
		}
		for turn := range 3 {
			completed := int64(turn + 2)
			if err := s.InsertTurn(Turn{TurnID: fmt.Sprintf("%s:%d", id, turn), ThreadID: id, TurnIndex: turn, StartedAt: int64(turn + 1), CompletedAt: &completed}); err != nil {
				b.Fatal(err)
			}
			for index, kind := range []string{"user_text", "assistant_text"} {
				role := map[string]string{"user_text": "user", "assistant_text": "assistant"}[kind]
				if err := s.InsertItem(Item{ID: fmt.Sprintf("%s:%d:%d", id, turn, index), ThreadID: id, TurnIndex: turn, ItemIndex: index,
					Kind: kind, Role: role, Summary: "row", CreatedAt: int64(turn + 1)}); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
}

// linkThreadListBench links the first linked threads to Jira tickets of a
// connection whose mirror keeps a card of each.
func linkThreadListBench(b *testing.B, s *Store, linked int) {
	b.Helper()
	if linked == 0 {
		return
	}
	c, err := s.CreateTrackerConnection(jiraConnection(mirrorSite, "user1@example.com", 10))
	if err != nil {
		b.Fatal(err)
	}
	root, _, err := s.GetOrCreateMirrorRoot(c.ID)
	if err != nil {
		b.Fatal(err)
	}
	mustExec(b, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
		INSERT INTO thread_tickets (thread_id, kind, site, issue_id, issue_key, issue_title, origin, linked_at,
			status_id, status_name, status_group, seen_root)
		SELECT printf('list-%06d', i), 'jira', ?2, 10000 + i, printf('WEB-%d', i), printf('Issue %d of the list', i), 'manual', 1,
			'3', 'In Progress', 2, ?3 FROM seq`,
		linked, mirrorSite, root)
}

// BenchmarkListThreadsLinked times the sidebar's thread list
// (ListThreadsWithItems) over threadListBenchThreads threads, with none
// and with 500 linked to a ticket. The cases share one seeded base,
// copied.
func BenchmarkListThreadsLinked(b *testing.B) {
	base := filepath.Join(b.TempDir(), "base.sqlite")
	seedThreadListBench(b, base)
	for _, linked := range []int{0, 500} {
		b.Run(fmt.Sprintf("%d threads, %d linked", threadListBenchThreads, linked), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "store.sqlite")
			if err := copyStoreFile(base, path); err != nil {
				b.Fatal(err)
			}
			s := openTestStore(b, path)
			linkThreadListBench(b, s, linked)
			threads, err := s.ListThreadsWithItems()
			if err != nil || len(threads) != threadListBenchThreads {
				b.Fatalf("listed %d threads, %v; want %d", len(threads), err, threadListBenchThreads)
			}
			for b.Loop() {
				if _, err := s.ListThreadsWithItems(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

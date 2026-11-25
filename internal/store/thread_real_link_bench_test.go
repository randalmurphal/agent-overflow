package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// The sidebar's listing rule and the empty-draft test with linked
// never-sent threads among them: threadRealLinkBenchThreads threads, of
// which threadRealLinkBenchDrafts have nothing sent, and
// threadRealLinkBenchLinked linked to a ticket, half of them drafts.
const (
	threadRealLinkBenchThreads = 2_000
	threadRealLinkBenchDrafts  = 400
	threadRealLinkBenchLinked  = 200
)

func seedThreadRealLinkBench(b *testing.B) *Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "store.sqlite")
	if err := copyTestStoreTemplate(b, path); err != nil {
		b.Fatal(err)
	}
	s := openTestStore(b, path)
	for i := range threadRealLinkBenchThreads {
		id := fmt.Sprintf("real-%06d", i)
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			b.Fatal(err)
		}
		if i < threadRealLinkBenchDrafts {
			continue
		}
		completed := int64(2)
		if err := s.InsertTurn(Turn{TurnID: id + ":0", ThreadID: id, TurnIndex: 0, StartedAt: 1, CompletedAt: &completed}); err != nil {
			b.Fatal(err)
		}
		if err := s.InsertItem(Item{ID: id + ":0:0", ThreadID: id, Kind: "user_text", Role: "user", Summary: "row", CreatedAt: 1}); err != nil {
			b.Fatal(err)
		}
	}
	// Half the links on drafts (real-000000..), half on sent threads.
	mustExec(b, s.db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
		INSERT INTO thread_tickets (thread_id, kind, site, issue_id, issue_key, issue_title, origin, linked_at,
			status_id, status_name, status_group)
		SELECT printf('real-%06d', CASE WHEN i % 2 = 0 THEN i / 2 ELSE ?2 + i END), 'jira', ?3, 10000 + i, printf('WEB-%d', i),
			printf('Issue %d', i), 'manual', 1, '', '', 0 FROM seq`,
		threadRealLinkBenchLinked, threadRealLinkBenchDrafts, mirrorSite)
	return s
}

// BenchmarkThreadRealWithLinks times the sidebar's thread list
// (ListThreadsWithItems) and the empty-draft test of every thread
// (IsEmptyDraftThread, as a sweep over all of them would run it).
func BenchmarkThreadRealWithLinks(b *testing.B) {
	s := seedThreadRealLinkBench(b)
	b.Run("ListThreadsWithItems", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.ListThreadsWithItems(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("IsEmptyDraftThread of every thread", func(b *testing.B) {
		for b.Loop() {
			for i := range threadRealLinkBenchThreads {
				if _, err := s.IsEmptyDraftThread(fmt.Sprintf("real-%06d", i)); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}

package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A done footer pages its archived threads newest first by a stable
// cursor: every thread once, ties at one instant by id, each page bounded.
func TestArchivedChildrenPageByCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateAssistant(t, s, "agent", "/h/agent")
	const children = 250
	var want []string
	for i := range children {
		id := fmt.Sprintf("child-%03d", i)
		mustCreateThread(t, s, id)
		token := "tok-" + id
		seedThreadRequest(t, s, token, "agent", ThreadRequestSpawn, 10)
		if _, err := s.SetThreadRequestTarget(token, "", id, "", ""); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	if _, err := s.SetThreadPlacement(want, PlacementNested); err != nil {
		t.Fatal(err)
	}
	for _, id := range want {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	// Three instants, so most of a page shares its neighbor's.
	if _, err := s.db.Exec(`UPDATE threads SET updated_at = 1000 + (CAST(substr(id, 7) AS INTEGER) % 3) WHERE id LIKE 'child-%'`); err != nil {
		t.Fatal(err)
	}
	instant := func(id string) int {
		var n int
		if _, err := fmt.Sscanf(id, "child-%d", &n); err != nil {
			t.Fatal(err)
		}
		return n % 3
	}
	slices.SortFunc(want, func(a, b string) int {
		if x, y := instant(a), instant(b); x != y {
			return y - x
		}
		return strings.Compare(b, a)
	})
	var got []string
	after := ArchivedChildrenCursor{}
	for pages := 0; ; pages++ {
		if pages > children {
			t.Fatal("paging does not end")
		}
		page, err := s.ListArchivedSpawnedThreads("agent", []string{defaultTestProjectID}, after, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Threads) > 20 || page.More && len(page.Threads) != 20 {
			t.Fatalf("page %d holds %d threads, more %v", pages, len(page.Threads), page.More)
		}
		for _, thread := range page.Threads {
			got = append(got, thread.ID)
		}
		if !page.More {
			break
		}
		last := page.Threads[len(page.Threads)-1]
		after = ArchivedChildrenCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged %d threads, want %d in order; first %q, want %q", len(got), len(want), got[:min(5, len(got))], want[:5])
	}
	// The card holds none of them.
	if page, err := s.ListArchivedSpawnedThreads("agent", []string{""}, ArchivedChildrenCursor{}, 20); err != nil || len(page.Threads) != 0 || page.More {
		t.Fatalf("card page = %+v %v", page, err)
	}
	for _, bad := range []struct {
		scopes []string
		limit  int
	}{{nil, 20}, {[]string{""}, 0}, {[]string{""}, MaxArchivedChildrenPage + 1}} {
		if _, err := s.ListArchivedSpawnedThreads("agent", bad.scopes, ArchivedChildrenCursor{}, bad.limit); !errors.Is(err, ErrArchivedChildrenPage) {
			t.Errorf("page of %q limit %d: err = %v, want ErrArchivedChildrenPage", bad.scopes, bad.limit, err)
		}
	}
}

// A worker whose spawn row the retention sweep reduced to lineage still
// names its agent, though the ledger no longer knows the token.
func TestStandingAgentSpawnerOfReadsALineageOnlySpawn(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateThread(t, s, "worker")
	seedThreadRequest(t, s, "tok-worker", "agent", ThreadRequestSpawn, 100)
	if _, err := s.SetThreadRequestTarget("tok-worker", "", "worker", "", ""); err != nil {
		t.Fatal(err)
	}
	if applied, err := s.SettleThreadRequest("tok-worker", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 200,
	}); err != nil || !applied {
		t.Fatalf("settle: applied=%v err=%v", applied, err)
	}
	mustCreateAssistant(t, s, "agent", "/home/agent")
	if _, err := s.DeleteThreadRequestsBefore(1000); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetThreadRequest("tok-worker"); err != nil || found {
		t.Fatalf("the spawn row is still in the ledger: found=%v err=%v", found, err)
	}
	if !rawThreadRequest(t, s, "tok-worker").LineageOnly {
		t.Fatal("the spawn row was not kept as lineage")
	}
	if agentID, title, found, err := s.StandingAgentSpawnerOf("worker"); err != nil || !found || agentID != "agent" || title != "Thread agent" {
		t.Errorf("spawner of the worker = %q %q found=%v err=%v; want the agent", agentID, title, found, err)
	}
}

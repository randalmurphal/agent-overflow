package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// An assistant's past threads page newest superseded first by a stable
// cursor: every thread once, ties at one instant by id, each page bounded,
// another assistant's past threads never among them. The page reads the
// past threads' index in its order, with no sort of its own.
func TestAssistantPastThreadsPageByCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateThread(t, s, "other")
	mustCreateAssistant(t, s, "agent", "/h/agent")
	mustCreateAssistant(t, s, "other", "/h/other")
	const past = 75
	var want []string
	for i := range past {
		id := fmt.Sprintf("past-%03d", i)
		homeThread(t, s, id, "")
		// Three instants, so most of a page shares its neighbor's.
		if _, err := s.db.Exec(`INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES (?, 'agent', ?)`, id, 1000+i%3); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	homeThread(t, s, "elsewhere", "")
	if _, err := s.db.Exec(`INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES ('elsewhere', 'other', 1001)`); err != nil {
		t.Fatal(err)
	}
	instant := func(id string) int {
		var n int
		if _, err := fmt.Sscanf(id, "past-%d", &n); err != nil {
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
	after := PastThreadsCursor{}
	for pages := 0; ; pages++ {
		if pages > past {
			t.Fatal("paging does not end")
		}
		page, err := s.ListAssistantPastThreads("agent", after, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Threads) > 20 || page.Next != nil && len(page.Threads) != 20 {
			t.Fatalf("page %d holds %d threads, next %+v", pages, len(page.Threads), page.Next)
		}
		for _, thread := range page.Threads {
			if thread.PastAgentID != "agent" || thread.AgentSupersededAt == nil {
				t.Fatalf("listed %s = past agent %q superseded %v", thread.ID, thread.PastAgentID, thread.AgentSupersededAt)
			}
			got = append(got, thread.ID)
		}
		if page.Next == nil {
			break
		}
		after = *page.Next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged %d threads, want %d in order; first %q, want %q", len(got), len(want), got[:min(5, len(got))], want[:5])
	}
	if page, err := s.ListAssistantPastThreads("nobody", PastThreadsCursor{}, 20); err != nil || len(page.Threads) != 0 || page.Next != nil {
		t.Fatalf("an assistant with no past threads = %+v %v", page, err)
	}
	for _, limit := range []int{0, MaxPastThreadsPage + 1} {
		if _, err := s.ListAssistantPastThreads("agent", PastThreadsCursor{}, limit); !errors.Is(err, ErrPastThreadsPage) {
			t.Errorf("limit %d: err = %v, want ErrPastThreadsPage", limit, err)
		}
	}
	for _, cursor := range []PastThreadsCursor{{}, {SupersededAt: 1001, ThreadID: "past-040"}} {
		query, args := pastThreadsPageQuery("agent", cursor, 20)
		rows := explainPlan(t, s, query, args...)
		searched, sorted := false, false
		for _, row := range rows {
			// Parent 0 is the page's own loop; the row columns' correlated
			// subqueries sort inside themselves.
			if row.parent != 0 {
				continue
			}
			searched = searched || strings.Contains(row.detail, "SEARCH past USING COVERING INDEX idx_agent_past_threads_agent (agent_id=?")
			sorted = sorted || strings.Contains(row.detail, "TEMP B-TREE")
		}
		if !searched || sorted {
			t.Errorf("page after %+v plan:\n%s", cursor, planText(rows))
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

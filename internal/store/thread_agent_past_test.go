package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Past pages a caller's settled spawns and asks and its notify sends,
// newest settled first with ties by token, across pages of any size, once
// each, with the total; open rows, quiet sends, reminders and other
// callers' rows are not its. A spawn the sweep kept as lineage is listed
// with its answer cleared.
func TestListThreadPastAgentRequestsPagesNewestSettledFirst(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "other", "w1", "w2", "w3", "w4", "peer", "old"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "old-spawn", "lead", ThreadRequestSpawn, "old", false, 5)
	settleAgentRequest(t, s, "old-spawn", 10)
	if _, _, err := s.DeleteThreadRequestsBefore(50, everySpawnRests(t, s, 50)); err != nil {
		t.Fatal(err)
	}
	type seed struct {
		token, kind, target string
		notify              bool
		settledAt           int64
	}
	for _, r := range []seed{
		{"spawn-a", ThreadRequestSpawn, "w1", false, 300},
		{"spawn-b", ThreadRequestSpawn, "w2", false, 300},
		{"ask-c", ThreadRequestAsk, "peer", false, 300},
		{"send-d", ThreadRequestSend, "peer", true, 200},
		{"send-quiet", ThreadRequestSend, "peer", false, 250},
		{"spawn-e", ThreadRequestSpawn, "w3", false, 100},
		{"spawn-open", ThreadRequestSpawn, "w4", false, 0},
	} {
		seedAgentRequest(t, s, r.token, "lead", r.kind, r.target, r.notify, 60)
		if r.settledAt != 0 {
			settleAgentRequest(t, s, r.token, r.settledAt)
		}
	}
	seedAgentRequest(t, s, "other-spawn", "other", ThreadRequestSpawn, "peer", false, 60)
	settleAgentRequest(t, s, "other-spawn", 400)
	seedThreadRequest(t, s, "remind", "lead", ThreadRequestRemind, 60)

	want := []string{"spawn-b", "spawn-a", "ask-c", "send-d", "spawn-e", "old-spawn"}
	for _, size := range []int{1, 2, 4, 6, 50} {
		var got []string
		after := PastAgentRowsCursor{}
		for pages := 0; ; pages++ {
			if pages > len(want) {
				t.Fatalf("page size %d: no end after %d pages", size, pages)
			}
			page, err := s.ListThreadPastAgentRequests("lead", after, size, 2, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != len(want) || len(page.Requests) > size {
				t.Fatalf("page size %d: total %d and %d rows", size, page.Total, len(page.Requests))
			}
			for _, row := range page.Requests {
				got = append(got, row.Token)
				if row.Depth != 0 {
					t.Errorf("%s at depth %d", row.Token, row.Depth)
				}
				switch row.Token {
				case "old-spawn":
					if !row.LineageOnly || len(row.AnswerHead) != 0 {
						t.Errorf("the lineage spawn = lineage %v answer %q", row.LineageOnly, row.AnswerHead)
					}
				default:
					if string(row.AnswerHead) != "do" {
						t.Errorf("%s answer head %q, want the first 2 bytes", row.Token, row.AnswerHead)
					}
				}
			}
			if page.Next == nil {
				break
			}
			after = *page.Next
		}
		if !slices.Equal(got, want) {
			t.Errorf("page size %d listed %q, want %q", size, got, want)
		}
	}
	if page, err := s.ListThreadPastAgentRequests("w1", PastAgentRowsCursor{}, 8, 0, nil, nil); err != nil || page.Total != 0 || len(page.Requests) != 0 || page.Next != nil {
		t.Errorf("a thread with no past rows = %+v, %v", page, err)
	}
	for _, bad := range []int{0, MaxPastAgentRowsPage + 1} {
		if _, err := s.ListThreadPastAgentRequests("lead", PastAgentRowsCursor{}, bad, 0, nil, nil); !errors.Is(err, ErrPastAgentRowsPage) {
			t.Errorf("page of %d: err = %v, want ErrPastAgentRowsPage", bad, err)
		}
	}
}

// A page of Past is read in index order from the cursor, never sorted: its
// cost follows the page, not the caller's history.
func TestListThreadPastAgentRequestsReadsItsIndexInOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	held := `{"waiting":[],"live":["w1"]}`
	for _, query := range []string{pastAgentRowsSQL(9), `WITH RECURSIVE ` + agentHeldSQL + ` SELECT count(*) FROM thread_requests r WHERE ` + pastAgentRowSQL} {
		args := []any{"lead", held}
		if strings.Contains(query, "?5") {
			args = append(args, int64(300), "tok", 16)
		}
		assertPlanSearches(t, s, "past agent rows", "r", []string{"idx_thread_requests_caller_settled"}, query, args...)
		// The held work's own union merges its seeds through temporary
		// trees, bounded by that work; the page itself is never sorted.
		plan := explainPlan(t, s, query, args...)
		byID := make(map[int]planRow, len(plan))
		for _, r := range plan {
			byID[r.id] = r
		}
		for _, r := range plan {
			if !strings.Contains(r.detail, "TEMP B-TREE") {
				continue
			}
			inHeld := false
			for id := r.parent; id != 0 && !inHeld; id = byID[id].parent {
				inHeld = byID[id].detail == "MATERIALIZE held"
			}
			if !inHeld {
				t.Errorf("the read sorts:\n%s", planText(plan))
			}
		}
	}
}

// The threads whose listing holds an open row are the callers of open
// requests the listing shows, a parked quiet send included, and the
// threads above open work through a spawn's marks or a remote spawn's last
// report; a thread whose rows all settled is not one. Pages from a cursor
// read the same threads, and each arm searches its own index from the
// cursor.
func TestThreadsWithOpenAgentRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"root", "mid", "leaf", "peer", "parked", "quiet", "done", "remote"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "root-mid", "root", ThreadRequestSpawn, "mid", false, 10)
	settleAgentRequest(t, s, "root-mid", 20)
	seedAgentRequest(t, s, "mid-leaf", "mid", ThreadRequestSpawn, "leaf", false, 30)
	settleAgentRequest(t, s, "mid-leaf", 40)
	seedAgentRequest(t, s, "leaf-peer", "leaf", ThreadRequestSend, "peer", true, 50)
	seedAgentRequest(t, s, "parked-peer", "parked", ThreadRequestSend, "peer", false, 50)
	seedAgentRequest(t, s, "quiet-peer", "quiet", ThreadRequestSend, "peer", false, 50)
	seedAgentRequest(t, s, "done-peer", "done", ThreadRequestSpawn, "peer", false, 50)
	settleAgentRequest(t, s, "done-peer", 60)
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "remote-far", CallerThreadID: "remote", Kind: ThreadRequestSpawn, TargetComputerID: "desk", TargetThreadID: "far",
		State: ThreadRequestRunning, CreatedAt: 50, UpdatedAt: 50,
	}); err != nil {
		t.Fatal(err)
	}
	settleAgentRequest(t, s, "remote-far", 60)
	if _, err := s.db.Exec(`UPDATE thread_requests SET nested_open = 1 WHERE token = 'remote-far'`); err != nil {
		t.Fatal(err)
	}
	waiting := []string{"parked-peer"}
	got, more, err := s.ThreadsWithOpenAgentRows(waiting, nil, "", 64)
	if err != nil || more {
		t.Fatalf("one page: more %v, %v", more, err)
	}
	want := []string{"leaf", "mid", "parked", "remote", "root"}
	if !slices.Equal(got, want) {
		t.Errorf("threads with open rows = %q, want %q", got, want)
	}
	// Paged two at a time from each page's last thread, the pages are the
	// one read's threads, each once, and the last says nothing follows.
	var paged []string
	for after, pages := "", 0; ; pages++ {
		page, more, err := s.ThreadsWithOpenAgentRows(waiting, nil, after, 2)
		if err != nil || len(page) == 0 || len(page) > 2 || pages > 3 {
			t.Fatalf("page after %q = %q, %v", after, page, err)
		}
		paged = append(paged, page...)
		if !more {
			break
		}
		after = page[len(page)-1]
	}
	if !slices.Equal(paged, want) {
		t.Errorf("paged threads = %q, want %q", paged, want)
	}
	assertPlanSearches(t, s, "ThreadsWithOpenAgentRows", "a", []string{
		"idx_thread_requests_open_created", "idx_thread_requests_open_below", "idx_thread_requests_nested_caller",
	}, threadsWithOpenAgentRowsSQL(3), "mid", `{"waiting":["parked-peer"]}`)
	assertPlanSearches(t, s, "ThreadsWithOpenAgentRows", "ps", []string{"idx_thread_requests_spawn_link"},
		threadsWithOpenAgentRowsSQL(3), "mid", `{"waiting":["parked-peer"],"live":["leaf"]}`)
}

// A reply arms its request's wake in the statement that stores it, whether
// or not the caller asked to be told, so a send made without notify that
// was replied to is owed as a message and listed as a sender's agent row:
// in the Agents segment's recent rows and in its Past. One that ended
// without a reply stays a quiet send, owed nothing and listed nowhere.
func TestAReplyToAQuietSendIsListedAsItsWakeIsArmed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "peer"} {
		mustCreateThread(t, s, id)
	}
	for _, token := range []string{"send-replied", "send-finished"} {
		seedAgentRequest(t, s, token, "lead", ThreadRequestSend, "peer", false, 60)
	}
	for token, settlement := range map[string]ThreadRequestSettlement{
		"send-replied":  {State: ThreadRequestReplied, Answer: []byte("done"), AnswerKind: ThreadAnswerReply, SettledAt: 200, ArmWake: true},
		"send-finished": {State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 200},
	} {
		if settled, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), settlement); err != nil || !settled {
			t.Fatalf("settle %s: settled=%v err=%v", token, settled, err)
		}
	}
	if got := undeliveredTokens(t, s); !slices.Equal(got, []string{"send-replied"}) {
		t.Fatalf("owed wakes = %v, want the reply alone", got)
	}
	page, err := s.ListThreadPastAgentRequests("lead", PastAgentRowsCursor{}, 10, 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var past []string
	for _, row := range page.Requests {
		past = append(past, row.Token)
	}
	if !slices.Equal(past, []string{"send-replied"}) || page.Total != 1 {
		t.Fatalf("past = %v (total %d), want the replied send alone", past, page.Total)
	}
	rows, _, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: "lead", SettledSince: 100, Depth: 1, Limit: 10, AnswerHead: 2})
	if err != nil {
		t.Fatal(err)
	}
	var recent []string
	for _, row := range rows {
		recent = append(recent, row.Token)
	}
	if !slices.Equal(recent, []string{"send-replied"}) {
		t.Fatalf("recent agent rows = %v, want the replied send alone", recent)
	}
}

package store

import (
	"strings"
	"testing"

	"agent-overflow/internal/threadmode"
)

func threadIDsOf(threads []Thread) []string {
	ids := make([]string, 0, len(threads))
	for _, thread := range threads {
		ids = append(ids, thread.ID)
	}
	return ids
}

func seedLookupThread(t *testing.T, s *Store, id string, apply func(*Thread)) Thread {
	t.Helper()
	thread := makeThread(id, "claude")
	thread.CreatedAt, thread.UpdatedAt = 1_000, 1_000
	if apply != nil {
		apply(&thread)
	}
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
	return thread
}

// seedCompletedTurn gives a thread a last-activity clock: the tools read
// the newest completed turn, not updated_at.
func seedCompletedTurn(t *testing.T, s *Store, threadID string, turnIndex int, startedAt, completedAt int64) {
	t.Helper()
	turnID := threadID + ":" + string(rune('a'+turnIndex))
	if err := s.InsertTurn(Turn{TurnID: turnID, ThreadID: threadID, TurnIndex: turnIndex, StartedAt: startedAt}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := s.UpdateTurnCompleted(turnID, completedAt, "end_turn", "", "", ""); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
}

// TestResolveThreadPrefixMatchesLiterally pins the id lookup: a byte
// prefix, every mode, archived rows included, ordered and bounded, with
// `_` and `%` matching only themselves.
func TestResolveThreadPrefixMatchesLiterally(t *testing.T) {
	s := newTestStore(t)
	seedLookupThread(t, s, "abcd1111-one", nil)
	seedLookupThread(t, s, "abcd2222-two", nil)
	seedLookupThread(t, s, "ffff0000-other", nil)
	seedLookupThread(t, s, "abcd3333-workflow", func(th *Thread) { th.Mode = threadmode.ModeWorkflow })
	seedLookupThread(t, s, "abcd4444-archived", nil)
	if _, _, err := s.ArchiveThread("abcd4444-archived"); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	matches, err := s.ResolveThreadPrefix("abcd", 10)
	if err != nil {
		t.Fatalf("ResolveThreadPrefix: %v", err)
	}
	ids := threadIDsOf(matches)
	want := []string{"abcd1111-one", "abcd2222-two", "abcd3333-workflow", "abcd4444-archived"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("prefix matches = %v, want %v in id order", ids, want)
	}

	// The limit bounds the answer and keeps the id order.
	limited, err := s.ResolveThreadPrefix("abcd", 2)
	if err != nil || len(limited) != 2 || limited[0].ID != "abcd1111-one" {
		t.Fatalf("limited = %v, %v", threadIDsOf(limited), err)
	}

	// A full id resolves to itself and nothing longer sneaks in above it.
	exact, err := s.ResolveThreadPrefix("abcd1111-one", 10)
	if err != nil || len(exact) != 1 || exact[0].ID != "abcd1111-one" {
		t.Fatalf("exact = %v, %v", threadIDsOf(exact), err)
	}

	if miss, err := s.ResolveThreadPrefix("zzzz", 10); err != nil || len(miss) != 0 {
		t.Fatalf("miss = %v, %v", threadIDsOf(miss), err)
	}
	// An empty prefix is not a listing.
	if all, err := s.ResolveThreadPrefix("", 10); err != nil || len(all) != 0 {
		t.Fatalf("empty prefix = %v, %v", threadIDsOf(all), err)
	}
}

// TestResolveThreadPrefixGivesWildcardsNoMeaning is the escaping case: a
// prefix holding LIKE metacharacters matches them as characters.
func TestResolveThreadPrefixGivesWildcardsNoMeaning(t *testing.T) {
	s := newTestStore(t)
	seedLookupThread(t, s, "a_b-literal", nil)
	seedLookupThread(t, s, "axb-other", nil)
	seedLookupThread(t, s, "100%-literal", nil)
	seedLookupThread(t, s, "100x-other", nil)
	seedLookupThread(t, s, `back\slash-literal`, nil)

	for _, tc := range []struct{ prefix, want string }{
		{"a_b", "a_b-literal"},
		{"100%", "100%-literal"},
		{`back\`, `back\slash-literal`},
	} {
		got, err := s.ResolveThreadPrefix(tc.prefix, 10)
		if err != nil {
			t.Fatalf("ResolveThreadPrefix(%q): %v", tc.prefix, err)
		}
		if len(got) != 1 || got[0].ID != tc.want {
			t.Errorf("prefix %q = %v, want only %s", tc.prefix, threadIDsOf(got), tc.want)
		}
	}

	// A thread this computer moved away is not a match, as everywhere
	// else: the lookup reads `owned_threads`.
	seedLookupThread(t, s, "a_b-moved", nil)
	seedCompletedOutgoingMove(t, s, "a_b-moved")
	got, err := s.ResolveThreadPrefix("a_b", 10)
	if err != nil {
		t.Fatalf("ResolveThreadPrefix after transfer: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a_b-literal" {
		t.Errorf("after the move = %v, want only the local thread", threadIDsOf(got))
	}
}

// TestResolveThreadPrefixWalksTheIdIndex pins the plan: the prefix range
// searches the id primary key instead of scanning the thread table.
func TestResolveThreadPrefixWalksTheIdIndex(t *testing.T) {
	s := newTestStore(t)
	seedLookupThread(t, s, "abcd1111-one", nil)

	plan := explainPlan(t, s,
		`SELECT `+threadColumns+` FROM owned_threads AS threads
		  WHERE threads.id >= ? AND threads.id < ?
		  ORDER BY threads.id ASC
		  LIMIT ?`, "abcd", "abce", 8)
	searched := false
	for _, row := range plan {
		if strings.HasPrefix(row.detail, "SCAN threads") {
			t.Errorf("the prefix lookup scans the thread table\n%s", planText(plan))
		}
		if strings.Contains(row.detail, "SEARCH threads") && strings.Contains(row.detail, "id>?") {
			searched = true
		}
	}
	if !searched {
		t.Errorf("the prefix range does not drive the id index\n%s", planText(plan))
	}
}

// TestListThreadsByActivityOrdersByTheThreadClock pins the listing order
// and its paging: newest activity first, where activity is the newest
// completed turn and updated_at only stands in for a thread that has
// never completed one.
func TestListThreadsByActivityOrdersByTheThreadClock(t *testing.T) {
	s := newTestStore(t)
	// Newest updated_at, oldest completed turn: the clock is the turn.
	seedLookupThread(t, s, "old-work", func(th *Thread) { th.UpdatedAt = 9_000 })
	seedCompletedTurn(t, s, "old-work", 0, 1_000, 1_100)
	seedLookupThread(t, s, "new-work", func(th *Thread) { th.UpdatedAt = 2_000 })
	seedCompletedTurn(t, s, "new-work", 0, 5_000, 5_100)
	// No completed turn at all: updated_at stands in.
	seedLookupThread(t, s, "draft-work", func(th *Thread) { th.UpdatedAt = 3_000 })

	rows, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListThreadsByActivity: %v", err)
	}
	if got := strings.Join(threadIDsOf(rows), ","); got != "new-work,draft-work,old-work" {
		t.Fatalf("order = %s", got)
	}

	// LIMIT and OFFSET page that same order in SQL.
	page, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: 2})
	if err != nil || len(page) != 2 || page[0].ID != "new-work" {
		t.Fatalf("first page = %v, %v", threadIDsOf(page), err)
	}
	next, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: 2, Offset: 2})
	if err != nil || len(next) != 1 || next[0].ID != "old-work" {
		t.Fatalf("second page = %v, %v", threadIDsOf(next), err)
	}
	past, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: 2, Offset: 99})
	if err != nil || len(past) != 0 {
		t.Fatalf("offset past the end = %v, %v", threadIDsOf(past), err)
	}
}

// TestThreadSearchFilterAppliesEveryRowFilter runs the new filter fields
// through both queries they serve, so a filter cannot be right in the
// listing and wrong in the search.
func TestThreadSearchFilterAppliesEveryRowFilter(t *testing.T) {
	s := newTestStore(t)
	seedLookupThread(t, s, "claude-thread", func(th *Thread) { th.Title = "launcher work" })
	seedCompletedTurn(t, s, "claude-thread", 0, 5_000, 5_100)
	seedLookupThread(t, s, "codex-thread", func(th *Thread) {
		th.Title = "launcher packaging"
		th.Provider = "codex"
	})
	seedCompletedTurn(t, s, "codex-thread", 0, 6_000, 6_100)
	seedLookupThread(t, s, "archived-thread", func(th *Thread) { th.Title = "launcher history" })
	seedCompletedTurn(t, s, "archived-thread", 0, 1_000, 1_100)
	if _, _, err := s.ArchiveThread("archived-thread"); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	// A workflow thread, which these answers carry, and a scratch thread,
	// which is out unless the caller names it.
	seedLookupThread(t, s, "workflow-thread", func(th *Thread) {
		th.Title = "launcher workflow"
		th.Mode = threadmode.ModeWorkflow
	})
	seedLookupThread(t, s, "scratch-thread", func(th *Thread) {
		th.Title = "launcher scratch"
		th.Mode = threadmode.ModeScratch
	})
	if err := s.InsertScratchThread(ScratchThread{
		ThreadID: "scratch-thread", SourceThreadID: "claude-thread", ReturnMode: threadmode.ModeChat, RequestToken: "tok-1",
	}); err != nil {
		t.Fatalf("InsertScratchThread: %v", err)
	}
	// A thread the claude thread spawned.
	seedLookupThread(t, s, "spawned-thread", func(th *Thread) { th.Title = "launcher spawn" })
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "spawn-1", CallerThreadID: "claude-thread", Kind: ThreadRequestSpawn,
		TargetThreadID: "spawned-thread", State: ThreadRequestAccepted, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	// A send to another thread must not make it look spawned.
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "send-1", CallerThreadID: "claude-thread", Kind: ThreadRequestSend,
		TargetThreadID: "codex-thread", State: ThreadRequestAccepted, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertThreadRequest(send): %v", err)
	}
	if err := s.BuildSearchIndex(t.Context()); err != nil {
		t.Fatalf("BuildSearchIndex: %v", err)
	}

	no, yes := false, true
	for _, tc := range []struct {
		name   string
		filter ThreadSearchFilter
		want   []string
	}{
		{"unfiltered", ThreadSearchFilter{}, []string{"codex-thread", "claude-thread", "spawned-thread", "archived-thread", "workflow-thread"}},
		{"provider", ThreadSearchFilter{Provider: "codex"}, []string{"codex-thread"}},
		{"not archived", ThreadSearchFilter{Archived: &no}, []string{"codex-thread", "claude-thread", "spawned-thread", "workflow-thread"}},
		{"archived", ThreadSearchFilter{Archived: &yes}, []string{"archived-thread"}},
		{"since", ThreadSearchFilter{SinceUnixMs: 5_100}, []string{"codex-thread", "claude-thread"}},
		{"spawned by", ThreadSearchFilter{SpawnedBy: "claude-thread"}, []string{"spawned-thread"}},
		{"scratch the caller owns", ThreadSearchFilter{ScratchThreadIDs: []string{"scratch-thread"}},
			[]string{"codex-thread", "claude-thread", "spawned-thread", "scratch-thread", "archived-thread", "workflow-thread"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := tc.filter
			filter.Limit = 20
			listed, err := s.ListThreadsByActivity(filter)
			if err != nil {
				t.Fatalf("ListThreadsByActivity: %v", err)
			}
			gotListed := threadIDsOf(listed)
			if len(gotListed) != len(tc.want) {
				t.Fatalf("listing = %v, want %v", gotListed, tc.want)
			}
			for _, id := range tc.want {
				if !strings.Contains(strings.Join(gotListed, ","), id) {
					t.Fatalf("listing = %v, want %v", gotListed, tc.want)
				}
			}

			hits, err := s.SearchThreads("launcher", filter)
			if err != nil {
				t.Fatalf("SearchThreads: %v", err)
			}
			seen := map[string]bool{}
			for _, hit := range hits {
				seen[hit.ThreadID] = true
			}
			if len(seen) != len(tc.want) {
				t.Fatalf("search threads = %v, want %v", seen, tc.want)
			}
			for _, id := range tc.want {
				if !seen[id] {
					t.Fatalf("search threads = %v, want %v", seen, tc.want)
				}
			}
		})
	}

	// The listing and the search agree on the two visibility answers: a
	// workflow thread is a row an agent may find, an unnamed scratch thread
	// is nobody's row, whatever else is asked for.
	listed, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: 20, ThreadIDs: []string{"workflow-thread"}})
	if err != nil || len(listed) != 1 {
		t.Fatalf("workflow thread listed = %v, %v", threadIDsOf(listed), err)
	}
	listed, err = s.ListThreadsByActivity(ThreadSearchFilter{Limit: 20, ThreadIDs: []string{"scratch-thread"}})
	if err != nil || len(listed) != 0 {
		t.Fatalf("unnamed scratch thread listed = %v, %v", threadIDsOf(listed), err)
	}
}

// TestFirstTurnIndexAtOrAfterPastAnyListingCap is the query the `since`
// window anchors on. The thread deliberately has more turns than the
// tools' turn listing cap, because resolving the anchor against a capped
// recent listing is exactly the bug this replaced.
func TestFirstTurnIndexAtOrAfterPastAnyListingCap(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "long-thread")
	const turns = 600
	for index := range turns {
		seedCompletedTurn(t, s, "long-thread", index, int64(1_000+index*10), int64(1_005+index*10))
	}

	// Turn 3 started at 1_030; the anchor lands on it rather than on the
	// oldest turn the last 500 happen to hold.
	index, found, anyTurns, err := s.FirstTurnIndexAtOrAfter("long-thread", 1_021)
	if err != nil || !found || !anyTurns {
		t.Fatalf("FirstTurnIndexAtOrAfter: %d found=%t any=%t err=%v", index, found, anyTurns, err)
	}
	if index != 3 {
		t.Fatalf("anchor = %d, want turn 3", index)
	}
	// An exact start stamp anchors on that turn.
	if index, found, _, err := s.FirstTurnIndexAtOrAfter("long-thread", 1_030); err != nil || !found || index != 3 {
		t.Fatalf("exact stamp = %d found=%t err=%v", index, found, err)
	}
	// Before everything: the first turn.
	if index, found, _, err := s.FirstTurnIndexAtOrAfter("long-thread", 1); err != nil || !found || index != 0 {
		t.Fatalf("before everything = %d found=%t err=%v", index, found, err)
	}
	// After everything: no anchor, but the thread does have turns.
	index, found, anyTurns, err = s.FirstTurnIndexAtOrAfter("long-thread", 99_000)
	if err != nil || found || !anyTurns {
		t.Fatalf("after everything = %d found=%t any=%t err=%v", index, found, anyTurns, err)
	}

	// A thread with no turn rows at all is the other not-found.
	mustCreateThread(t, s, "turnless")
	if _, found, anyTurns, err := s.FirstTurnIndexAtOrAfter("turnless", 1); err != nil || found || anyTurns {
		t.Fatalf("turnless thread: found=%t any=%t err=%v", found, anyTurns, err)
	}
}

// TestListThreadWorktreeWorkspacesNamesEachCheckoutOnce pins the spawn
// catalog's workspace source: one row per checkout, the newest thread's
// branch, archived threads left out.
func TestListThreadWorktreeWorkspacesNamesEachCheckoutOnce(t *testing.T) {
	s := newTestStore(t)
	seedLookupThread(t, s, "wt-old", func(th *Thread) {
		th.WorktreePath = "/tmp/wt-a"
		th.Branch = "old-branch"
		th.UpdatedAt = 1_000
	})
	seedLookupThread(t, s, "wt-new", func(th *Thread) {
		th.WorktreePath = "/tmp/wt-a"
		th.Branch = "new-branch"
		th.UpdatedAt = 2_000
	})
	seedLookupThread(t, s, "wt-second", func(th *Thread) {
		th.WorktreePath = "/tmp/wt-b"
		th.Branch = "second"
		th.UpdatedAt = 3_000
	})
	seedLookupThread(t, s, "wt-archived", func(th *Thread) {
		th.WorktreePath = "/tmp/wt-gone"
		th.Branch = "gone"
		th.UpdatedAt = 4_000
	})
	if _, _, err := s.ArchiveThread("wt-archived"); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	seedLookupThread(t, s, "wt-none", nil)

	rows, err := s.ListThreadWorktreeWorkspaces()
	if err != nil {
		t.Fatalf("ListThreadWorktreeWorkspaces: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("workspaces = %#v, want one per live checkout", rows)
	}
	if rows[0].Path != "/tmp/wt-b" || rows[0].Branch != "second" {
		t.Errorf("newest checkout = %#v", rows[0])
	}
	// One row for the shared checkout, carrying the newest thread's branch.
	if rows[1].Path != "/tmp/wt-a" || rows[1].Branch != "new-branch" {
		t.Errorf("shared checkout = %#v", rows[1])
	}
	if rows[0].ProjectID != defaultTestProjectID {
		t.Errorf("project = %q", rows[0].ProjectID)
	}
}

// seedCompletedOutgoingMove writes the transfer row that takes a thread
// out of `owned_threads`. It writes the row directly because the
// production path is a multi-step handshake with a peer, and what this
// file needs is the settled end state.
func seedCompletedOutgoingMove(t *testing.T, s *Store, threadID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO thread_transfers (
		    id, thread_id, target_thread_id, peer_backend_id, kind, direction, phase,
		    activation_hash, private_state, created_at, updated_at
		 ) VALUES (?, ?, ?, 'peer-backend', 'move', 'outgoing', 'complete', '', '{}', 1, 1)`,
		"transfer-"+threadID, threadID, threadID,
	); err != nil {
		t.Fatalf("seed completed outgoing move for %s: %v", threadID, err)
	}
}

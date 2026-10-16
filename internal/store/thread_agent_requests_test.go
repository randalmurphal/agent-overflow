package store

import (
	"slices"
	"strings"
	"sync"
	"testing"
)

func agentRequestTokens(rows []ThreadRequest) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Token)
	}
	return out
}

// listAgentRequests reads one thread's own Agents rows, as the listing
// reads the rows at its depth bound.
func listAgentRequests(s *Store, caller string, waiting []string, since int64, openDepth, limit int) ([]ThreadRequest, bool, error) {
	rows, more, err := s.ListThreadAgentTree(ThreadAgentTreeRead{
		Root: caller, Waiting: waiting, SettledSince: since, Depth: 1, OpenDepth: openDepth, Limit: limit, AnswerHead: 64,
	})
	out := make([]ThreadRequest, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ThreadRequest)
	}
	return out, more, err
}

func seedAgentRequest(t *testing.T, s *Store, token, caller, kind, target string, notify bool, createdAt int64) {
	t.Helper()
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: kind, TargetThreadID: target, Notify: notify,
		State: ThreadRequestRunning, CreatedAt: createdAt, UpdatedAt: createdAt,
	}); err != nil {
		t.Fatalf("insert request %s: %v", token, err)
	}
}

func settleAgentRequest(t *testing.T, s *Store, token string, at int64) {
	t.Helper()
	settled, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: at,
	})
	if err != nil || !settled {
		t.Fatalf("settle %s: settled=%v err=%v", token, settled, err)
	}
}

// The rail lists every spawn, and a send or ask only while the thread is
// owed its answer: a wake armed, or a call parked on it. Reminders are
// wakes, not agents. A settled request stays only inside the window.
func TestListThreadAgentTreeOwnRowsInclusion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "peer", "idle"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "spawn-quiet", "lead", ThreadRequestSpawn, "child", false, 100)
	seedAgentRequest(t, s, "send-notify", "lead", ThreadRequestSend, "peer", true, 101)
	seedAgentRequest(t, s, "send-quiet", "lead", ThreadRequestSend, "peer", false, 102)
	seedAgentRequest(t, s, "ask-waiting", "lead", ThreadRequestAsk, "peer", false, 103)
	seedAgentRequest(t, s, "settled-old", "lead", ThreadRequestSpawn, "idle", false, 104)
	settleAgentRequest(t, s, "settled-old", 1_000)
	seedAgentRequest(t, s, "settled-new", "lead", ThreadRequestSend, "peer", true, 105)
	settleAgentRequest(t, s, "settled-new", 5_000)
	seedThreadRequest(t, s, "remind", "lead", ThreadRequestRemind, 106)
	seedAgentRequest(t, s, "other-caller", "peer", ThreadRequestSpawn, "child", false, 107)

	rows, more, err := listAgentRequests(s, "lead", []string{"ask-waiting"}, 4_000, 4, 64)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"spawn-quiet", "send-notify", "ask-waiting", "settled-new"}
	if got := agentRequestTokens(rows); !slices.Equal(got, want) || more {
		t.Fatalf("listed %v more=%v; want %v", got, more, want)
	}

	// The lineage-only rows the retention sweep keeps are not requests.
	mustExec(t, s.db, `UPDATE thread_requests SET lineage_only = 1 WHERE token = 'spawn-quiet'`)
	rows, _, err = listAgentRequests(s, "lead", nil, 4_000, 4, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got := agentRequestTokens(rows); !slices.Equal(got, []string{"send-notify", "settled-new"}) {
		t.Fatalf("with a lineage-only spawn and no waits: %v", got)
	}
}

// A finished spawn whose thread, or a thread that thread spawned, still has
// open agent work stays listed past the window: it is what that work nests
// under. One whose descendants are all done does not.
func TestListThreadAgentTreeOwnRowsKeepsSpawnsWithOpenDescendants(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grandchild", "great", "done-child"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "lead-child", "lead", ThreadRequestSpawn, "child", false, 100)
	settleAgentRequest(t, s, "lead-child", 200)
	seedAgentRequest(t, s, "lead-done", "lead", ThreadRequestSpawn, "done-child", false, 101)
	settleAgentRequest(t, s, "lead-done", 200)
	seedAgentRequest(t, s, "child-grand", "child", ThreadRequestSpawn, "grandchild", false, 110)
	settleAgentRequest(t, s, "child-grand", 210)
	seedAgentRequest(t, s, "grand-great", "grandchild", ThreadRequestSpawn, "great", false, 120)

	rows, _, err := listAgentRequests(s, "lead", nil, 10_000, 4, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got := agentRequestTokens(rows); !slices.Equal(got, []string{"lead-child"}) {
		t.Fatalf("with a grandchild's spawn open: %v; want [lead-child]", got)
	}
	settleAgentRequest(t, s, "grand-great", 300)
	rows, _, err = listAgentRequests(s, "lead", nil, 10_000, 4, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("with every descendant done: %v", agentRequestTokens(rows))
	}
}

// A settled spawn chain keeps its rows down to the depth bound while work
// below it is open: open work three spawns down still lists the whole chain
// at the root, and nothing past the bound keeps it.
func TestListThreadAgentTreeOwnRowsKeepsADeepSettledChain(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grand", "great", "ggreat"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
	settleAgentRequest(t, s, "l-c", 200)
	seedAgentRequest(t, s, "c-g", "child", ThreadRequestSpawn, "grand", false, 110)
	settleAgentRequest(t, s, "c-g", 210)
	seedAgentRequest(t, s, "g-gg", "grand", ThreadRequestSpawn, "great", false, 120)
	settleAgentRequest(t, s, "g-gg", 220)
	seedAgentRequest(t, s, "gg-open", "great", ThreadRequestSpawn, "ggreat", false, 130)
	for caller, want := range map[string][]string{"lead": {"l-c"}, "child": {"c-g"}, "grand": {"g-gg"}, "great": {"gg-open"}} {
		rows, _, err := listAgentRequests(s, caller, nil, 10_000, 4, 64)
		if err != nil {
			t.Fatal(err)
		}
		if got := agentRequestTokens(rows); !slices.Equal(got, want) {
			t.Errorf("%s lists %v with gg-open open; want %v", caller, got, want)
		}
	}
	// The open request sits three spawns below the lead: a bound of two
	// levels does not reach it.
	if rows, _, err := listAgentRequests(s, "lead", nil, 10_000, 2, 64); err != nil || len(rows) != 0 {
		t.Errorf("a two-level bound lists %v, %v; want nothing", agentRequestTokens(rows), err)
	}
	settleAgentRequest(t, s, "gg-open", 300)
	if rows, _, err := listAgentRequests(s, "lead", nil, 10_000, 4, 64); err != nil || len(rows) != 0 {
		t.Errorf("with the chain done the lead lists %v, %v", agentRequestTokens(rows), err)
	}
}

// A settled spawn on another computer stays listed past the settled window
// while that computer last reported open work under it, and leaves once it
// reports none; a local spawn above it is kept for it too.
func TestListThreadAgentTreeOwnRowsKeepsARemoteSpawnWithOpenWork(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
	settleAgentRequest(t, s, "l-c", 200)
	if err := s.InsertThreadRequest(ThreadRequest{Token: "remote", CallerThreadID: "child", Kind: ThreadRequestSpawn,
		TargetThreadID: "remote-child", TargetComputerID: "peer-1", State: ThreadRequestRunning, CreatedAt: 110, UpdatedAt: 110}); err != nil {
		t.Fatal(err)
	}
	settleAgentRequest(t, s, "remote", 210)
	list := func(caller string) []string {
		rows, _, err := listAgentRequests(s, caller, nil, 10_000, 4, 64)
		if err != nil {
			t.Fatal(err)
		}
		return agentRequestTokens(rows)
	}
	if got := list("child"); len(got) != 0 {
		t.Fatalf("with nothing open there the settled remote spawn lists %v", got)
	}
	if changed, err := s.SetThreadRequestNestedOpen("remote", true); err != nil || !changed {
		t.Fatalf("SetThreadRequestNestedOpen = %v, %v", changed, err)
	}
	if changed, err := s.SetThreadRequestNestedOpen("remote", true); err != nil || changed {
		t.Fatalf("setting it again = %v, %v; want no change", changed, err)
	}
	if got := list("child"); !slices.Equal(got, []string{"remote"}) {
		t.Errorf("with open work there the child lists %v", got)
	}
	if got := list("lead"); !slices.Equal(got, []string{"l-c"}) {
		t.Errorf("with open work two levels down the lead lists %v", got)
	}
	open, err := s.ListNestedOpenRemoteSpawns(0, "", 8)
	if err != nil || len(open) != 1 || open[0].Token != "remote" {
		t.Fatalf("ListNestedOpenRemoteSpawns = %v, %v", agentRequestTokens(open), err)
	}
	if schedule, err := s.NextThreadRequestWork(); err != nil || !schedule.NestedOpen {
		t.Fatalf("schedule = %+v, %v; want nested open work", schedule, err)
	}
	if _, err := s.SetThreadRequestNestedOpen("remote", false); err != nil {
		t.Fatal(err)
	}
	if got := list("lead"); len(got) != 0 {
		t.Errorf("with the remote work done the lead lists %v", got)
	}
	if schedule, err := s.NextThreadRequestWork(); err != nil || schedule.NestedOpen {
		t.Fatalf("schedule = %+v, %v; want none", schedule, err)
	}
}

// The listing is bounded and says when it is.
func TestListThreadAgentTreeOwnRowsBound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	for i, token := range []string{"a", "b", "c"} {
		seedAgentRequest(t, s, token, "lead", ThreadRequestSpawn, "", false, int64(100+i))
	}
	rows, more, err := listAgentRequests(s, "lead", nil, 0, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := agentRequestTokens(rows); !slices.Equal(got, []string{"a", "b"}) || !more {
		t.Fatalf("bounded listing %v more=%v", got, more)
	}
}

// Every write that changes a source row reports it after it commits, and a
// write that changes nothing does not.
func TestThreadRequestChangesAreReported(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	var mu sync.Mutex
	var got []ThreadRequestChange
	s.OnThreadRequestChanged(func(change ThreadRequestChange) {
		mu.Lock()
		got = append(got, change)
		mu.Unlock()
	})
	take := func() []ThreadRequestChange {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
	expect := func(what string, want ...ThreadRequestChange) {
		t.Helper()
		if changes := take(); !slices.Equal(changes, want) {
			t.Fatalf("%s reported %v; want %v", what, changes, want)
		}
	}

	seedThreadRequest(t, s, "tok", "lead", ThreadRequestSpawn, 100)
	expect("insert", ThreadRequestChange{Token: "tok", CallerThreadID: "lead"})
	if _, err := s.AdvanceThreadRequestState("tok", ThreadRequestUnconfirmed, ThreadRequestAccepted); err != nil {
		t.Fatal(err)
	}
	expect("advance", ThreadRequestChange{Token: "tok"})
	if _, err := s.AdvanceThreadRequestState("tok", ThreadRequestUnconfirmed, ThreadRequestAccepted); err != nil {
		t.Fatal(err)
	}
	expect("an advance that did not apply")
	if _, err := s.SetThreadRequestTarget("tok", "", "child", "", ""); err != nil {
		t.Fatal(err)
	}
	expect("target", ThreadRequestChange{Token: "tok"})
	if existed, err := s.SetThreadRequestTarget("tok", "", "child", "", ""); err != nil || !existed {
		t.Fatalf("restating the target: existed=%v err=%v", existed, err)
	}
	expect("restating the target")
	if _, err := s.SetThreadRequestNotify("tok", true); err != nil {
		t.Fatal(err)
	}
	expect("notify", ThreadRequestChange{Token: "tok"})
	if _, err := s.SettleThreadRequest("tok", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal,
	}); err != nil {
		t.Fatal(err)
	}
	expect("settle", ThreadRequestChange{Token: "tok"})
	if _, err := s.StoreThreadRequestLateReply("tok", []byte("late"), 0, nil); err != nil {
		t.Fatal(err)
	}
	expect("late reply", ThreadRequestChange{Token: "tok"})
	if _, err := s.MarkThreadRequestDelivered("tok", ThreadWakeInline, 0, false); err != nil {
		t.Fatal(err)
	}
	expect("inline delivery", ThreadRequestChange{Token: "tok"})
	seedThreadRequest(t, s, "armed", "lead", ThreadRequestSend, 101)
	expect("second insert", ThreadRequestChange{Token: "armed", CallerThreadID: "lead"})
	if _, err := s.SetThreadRequestNotify("armed", true); err != nil {
		t.Fatal(err)
	}
	expect("arm", ThreadRequestChange{Token: "armed"})
	if _, err := s.SetThreadRequestsNotifyForCaller("lead", false); err != nil {
		t.Fatal(err)
	}
	expect("caller-wide disarm", ThreadRequestChange{CallerThreadID: "lead"})
	s.OnThreadRequestChanged(nil)
	seedThreadRequest(t, s, "after", "lead", ThreadRequestSpawn, 102)
	expect("a cleared hook")
}

// The sweep pages through the settled remote spawns with open work by the
// last row it read, on their partial index, so one that stays open does
// not keep the rest from being asked.
func TestListNestedOpenRemoteSpawnsPagesFromTheLastRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	for i, token := range []string{"a", "b", "c"} {
		if err := s.InsertThreadRequest(ThreadRequest{Token: token, CallerThreadID: "lead", Kind: ThreadRequestSpawn,
			TargetThreadID: "there-" + token, TargetComputerID: "peer-1", State: ThreadRequestRunning, CreatedAt: 100, UpdatedAt: 100}); err != nil {
			t.Fatal(err)
		}
		settleAgentRequest(t, s, token, int64(200+i/2))
		if _, err := s.SetThreadRequestNestedOpen(token, true); err != nil {
			t.Fatal(err)
		}
	}
	page := func(at int64, after string) []string {
		rows, err := s.ListNestedOpenRemoteSpawns(at, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		return agentRequestTokens(rows)
	}
	if got := page(0, ""); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("first page %v", got)
	}
	// b and c settled at the same moment: the token breaks the tie.
	if got := page(200, "b"); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("page after b %v", got)
	}
	plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+nestedOpenRemoteSpawnsSQL(2), int64(0), "")
	if !strings.Contains(plan, "idx_thread_requests_nested_open") {
		t.Fatalf("the page reads %s", plan)
	}
}

// A spawn cycle with nothing open ends the walk and lists nothing.
func TestListThreadAgentTreeOwnRowsEndsASpawnCycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"a", "b"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "a-b", "a", ThreadRequestSpawn, "b", false, 100)
	settleAgentRequest(t, s, "a-b", 200)
	seedAgentRequest(t, s, "b-a", "b", ThreadRequestSpawn, "a", false, 110)
	settleAgentRequest(t, s, "b-a", 210)
	seedAgentRequest(t, s, "b-self", "b", ThreadRequestSpawn, "b", false, 111)
	settleAgentRequest(t, s, "b-self", 211)
	rows, _, err := listAgentRequests(s, "a", nil, 10_000, 4, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got := agentRequestTokens(rows); len(got) != 0 {
		t.Fatalf("a settled cycle lists %v", got)
	}
}

// Forgetting a computer clears open work on every spawn to it, and names
// the callers whose rows changed.
func TestClearThreadRequestsNestedOpenForComputer(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "other"} {
		mustCreateThread(t, s, id)
	}
	for _, seed := range []struct{ token, caller, computer string }{
		{"gone-1", "lead", "gone"}, {"gone-2", "other", "gone"}, {"kept", "lead", "kept"},
	} {
		if err := s.InsertThreadRequest(ThreadRequest{Token: seed.token, CallerThreadID: seed.caller, Kind: ThreadRequestSpawn,
			TargetThreadID: "there", TargetComputerID: seed.computer, State: ThreadRequestRunning, CreatedAt: 100, UpdatedAt: 100}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetThreadRequestNestedOpen(seed.token, true); err != nil {
			t.Fatal(err)
		}
	}
	callers, err := s.ClearThreadRequestsNestedOpenForComputer("gone")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(callers)
	if !slices.Equal(callers, []string{"lead", "other"}) {
		t.Fatalf("callers %v", callers)
	}
	for token, want := range map[string]bool{"gone-1": false, "gone-2": false, "kept": true} {
		row, _, err := s.GetThreadRequest(token)
		if err != nil || row.NestedOpen != want {
			t.Fatalf("%s nested open = %v, %v; want %v", token, row.NestedOpen, err, want)
		}
	}
	if again, err := s.ClearThreadRequestsNestedOpenForComputer("gone"); err != nil || len(again) != 0 {
		t.Fatalf("a second clear = %v, %v; want no change", again, err)
	}
}

package store

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func agentTreeRead(root string, depth int) ThreadAgentTreeRead {
	return ThreadAgentTreeRead{Root: root, SettledSince: 10_000, Depth: depth, OpenDepth: 4, Limit: 64, AnswerHead: 16}
}

// The tree lists each row then the rows its spawn made, siblings oldest
// first, with each row's depth; a local spawn at the bound whose thread has
// rows says so, and a thread already above a row is not walked again.
func TestListThreadAgentTreeNestsInTreeOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grand", "peer"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
	seedAgentRequest(t, s, "l-p", "lead", ThreadRequestSend, "peer", true, 101)
	seedAgentRequest(t, s, "c-g", "child", ThreadRequestSpawn, "grand", false, 110)
	seedAgentRequest(t, s, "c-p", "child", ThreadRequestAsk, "peer", true, 105)
	seedAgentRequest(t, s, "g-p", "grand", ThreadRequestSend, "peer", true, 120)
	seedAgentRequest(t, s, "g-l", "grand", ThreadRequestSpawn, "lead", false, 121)

	read := func(depth int) ([]string, []int, []bool) {
		t.Helper()
		rows, more, err := s.ListThreadAgentTree(agentTreeRead("lead", depth))
		if err != nil || more {
			t.Fatalf("ListThreadAgentTree(depth %d): more=%v err=%v", depth, more, err)
		}
		var tokens []string
		var depths []int
		var hidden []bool
		for _, row := range rows {
			tokens, depths, hidden = append(tokens, row.Token), append(depths, row.Depth), append(hidden, row.NestedHidden)
		}
		return tokens, depths, hidden
	}
	tokens, depths, hidden := read(4)
	if want := []string{"l-c", "c-p", "c-g", "g-p", "g-l", "l-p"}; !slices.Equal(tokens, want) {
		t.Fatalf("tree order %v; want %v", tokens, want)
	}
	if want := []int{0, 1, 1, 2, 2, 0}; !slices.Equal(depths, want) {
		t.Fatalf("depths %v; want %v", depths, want)
	}
	if slices.Contains(hidden, true) {
		t.Fatalf("a row inside the bound is marked hidden: %v", hidden)
	}
	// At two levels the child's spawn nests nothing, and says its thread
	// has rows.
	tokens, _, hidden = read(2)
	if want := []string{"l-c", "c-p", "c-g", "l-p"}; !slices.Equal(tokens, want) {
		t.Fatalf("two-level tree %v; want %v", tokens, want)
	}
	if want := []bool{false, false, true, false}; !slices.Equal(hidden, want) {
		t.Fatalf("two-level hidden marks %v; want %v", hidden, want)
	}
}

// A row carries the head of its newer answer and whether a late reply came,
// and a read by token reads that one row.
func TestListThreadAgentTreeReadsTheAnswerHead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedAgentRequest(t, s, "first", "lead", ThreadRequestSend, "t", true, 100)
	seedAgentRequest(t, s, "late", "lead", ThreadRequestSend, "t", true, 101)
	for _, token := range []string{"first", "late"} {
		if ok, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestFinished, Answer: []byte(strings.Repeat("a", 64)), AnswerKind: ThreadAnswerFinal, SettledAt: 20_000,
		}); err != nil || !ok {
			t.Fatalf("settle %s: %v %v", token, ok, err)
		}
	}
	if ok, err := s.StoreThreadRequestLateReply("late", []byte(strings.Repeat("b", 64)), 21_000, nil); err != nil || !ok {
		t.Fatalf("late reply: %v %v", ok, err)
	}
	rows, _, err := s.ListThreadAgentTree(agentTreeRead("lead", 1))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %d, %v", len(rows), err)
	}
	if got := rows[0]; string(got.AnswerHead) != strings.Repeat("a", 16) || got.HasLateReply || got.Answer != nil || got.LateReply != nil {
		t.Fatalf("first row %+v", got)
	}
	if got := rows[1]; string(got.AnswerHead) != strings.Repeat("b", 16) || !got.HasLateReply || got.LateReply != nil {
		t.Fatalf("late row %+v", got)
	}
	one := agentTreeRead("lead", 1)
	one.Tokens = []string{"late"}
	if rows, _, err := s.ListThreadAgentTree(one); err != nil || len(rows) != 1 || rows[0].Token != "late" {
		t.Fatalf("the read of one token = %+v, %v", rows, err)
	}
}

// A confirmed queued answer names the row its echo named, while the
// caller's rows hold it: a merge's survivor replaces it, an echo naming no
// row leaves it, a give-back clears it, and a row gone names nothing. The
// tool rows' read says the same, in the order asked, for the caller alone.
func TestConfirmedWakesNameTheirRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedSettledThreadRequest(t, s, "tok", "lead", ThreadRequestSend, 100, "the answer")
	seedSettledThreadRequest(t, s, "other", "lead", ThreadRequestSend, 100, "the answer")
	for i, id := range []string{"row-a", "row-b"} {
		if _, err := s.UpsertItem(Item{ID: id, ThreadID: "lead", TurnIndex: i + 1, Kind: "user_text", Role: "user",
			Status: "completed", Summary: "x", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.MarkThreadRequestDelivered("tok", ThreadWakeQueued, 200, false); err != nil || !ok {
		t.Fatalf("mark: %v %v", ok, err)
	}
	wake := func() string {
		t.Helper()
		got, err := s.ThreadRequestDeliveries("lead", []string{"other", "tok", "missing"})
		if err != nil || len(got) != 2 || got[0].Token != "other" || got[1].Token != "tok" {
			t.Fatalf("deliveries %+v, %v", got, err)
		}
		rows, _, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: "lead", SettledSince: 0, Depth: 1, OpenDepth: 4, Limit: 8, AnswerHead: 4})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Token == "tok" && row.WakeItemID != got[1].WakeItemID {
				t.Fatalf("the listing names %q, the states %q", row.WakeItemID, got[1].WakeItemID)
			}
		}
		return got[1].WakeItemID
	}
	if id := wake(); id != "" {
		t.Fatalf("an unconfirmed wake names %q", id)
	}
	if ok, err := s.ConfirmThreadWake("tok", false, "row-a", 300); err != nil || !ok {
		t.Fatalf("confirm: %v %v", ok, err)
	}
	if id := wake(); id != "row-a" {
		t.Fatalf("confirmed wake names %q; want row-a", id)
	}
	if ok, err := s.ConfirmThreadWake("tok", false, "", 310); err != nil || ok {
		t.Fatalf("an echo naming no row changed it: %v %v", ok, err)
	}
	if ok, err := s.ConfirmThreadWake("tok", false, "row-b", 320); err != nil || !ok {
		t.Fatalf("re-point: %v %v", ok, err)
	}
	if row := mustGetThreadRequest(t, s, "tok"); row.ConfirmedAt != 300 {
		t.Fatalf("the re-point moved the confirmation to %d", row.ConfirmedAt)
	}
	if id := wake(); id != "row-b" {
		t.Fatalf("re-pointed wake names %q; want row-b", id)
	}
	// Moved to another caller, as New Thread, Same Assistant moves it, the
	// request names no row of the thread it left.
	mustCreateThread(t, s, "successor")
	if _, err := s.db.Exec(`UPDATE thread_requests SET caller_thread_id = 'successor' WHERE token = 'tok'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ThreadRequestDeliveries("successor", []string{"tok"}); err != nil || len(got) != 1 || got[0].WakeItemID != "" {
		t.Fatalf("the moved request reads %+v, %v; want no wake row", got, err)
	}
	if _, err := s.db.Exec(`UPDATE thread_requests SET caller_thread_id = 'lead' WHERE token = 'tok'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM items WHERE thread_id = 'lead' AND id = 'row-b'`); err != nil {
		t.Fatal(err)
	}
	if id := wake(); id != "" {
		t.Fatalf("a wake whose row is gone names %q", id)
	}
	if err := s.WithdrawThreadWake("tok", false, "queue:none", WithdrawUnsent); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	var cleared int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_requests WHERE token = 'tok' AND wake_item_id = ''`).Scan(&cleared); err != nil || cleared != 1 {
		t.Fatalf("the give-back kept the wake row: %d, %v", cleared, err)
	}
	if got, err := s.ThreadRequestDeliveries("other-thread", []string{"tok"}); err != nil || len(got) != 0 {
		t.Fatalf("another thread read %+v, %v", got, err)
	}
}

// A change frame names the caller of each changed request and of each
// open request this computer made of a changed target.
func TestThreadAgentRowCallersNameTokensAndTargets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "peer"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "direct", "lead", ThreadRequestSend, "peer", true, 100)
	seedAgentRequest(t, s, "running", "peer", ThreadRequestSend, "lead", true, 101)
	seedThreadRequest(t, s, "remind", "lead", ThreadRequestRemind, 102)
	for _, receipt := range []ThreadRequestReceipt{
		{Token: "running", OwnerDeviceID: "local", SourceThreadID: "peer", Kind: ThreadRequestSend, TargetThreadID: "lead", State: ThreadReceiptRunning},
		{Token: "foreign", OwnerDeviceID: "device-2", SourceThreadID: "x", Kind: ThreadRequestSend, TargetThreadID: "lead", State: ThreadReceiptRunning},
	} {
		if _, _, err := s.AcceptThreadRequestReceipt(receipt); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ThreadAgentRowCallers([]string{"direct", "remind", "missing"}, []string{"lead"}, "local")
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b ThreadAgentRowCaller) int { return strings.Compare(a.Token, b.Token) })
	if want := []ThreadAgentRowCaller{{"direct", "lead"}, {"running", "peer"}}; !slices.Equal(got, want) {
		t.Fatalf("callers %+v; want %+v", got, want)
	}
}

// referenceAgentTree is the walk the listing replaced, one thread's own
// rows at a time: each row, then, for a local spawn whose thread no
// earlier row reached, that thread's rows one level down; a spawn at the
// depth bound says whether its thread has rows.
func referenceAgentTree(t *testing.T, s *Store, root string, depth int) []string {
	t.Helper()
	own := func(caller string) []ThreadAgentRequest {
		rows, _, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: caller, SettledSince: 10_000, Depth: 1, OpenDepth: 4, Limit: 1000, AnswerHead: 1})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	visited := map[string]bool{root: true}
	var out []string
	var walk func(caller string, level int)
	walk = func(caller string, level int) {
		for _, row := range own(caller) {
			index := len(out)
			out = append(out, fmt.Sprintf("%s@%d", row.Token, level))
			child := row.TargetThreadID
			if row.Kind != ThreadRequestSpawn || child == "" || row.TargetComputerID != "" || visited[child] {
				continue
			}
			visited[child] = true
			if level+1 >= depth {
				if len(own(child)) > 0 {
					out[index] += "+hidden"
				}
				continue
			}
			walk(child, level+1)
		}
	}
	walk(root, 0)
	return out
}

// The listing matches the walk it replaced on random graphs of spawns and
// sends, a thread reached by several spawns, cycles and settled rows
// included, and lists every request once.
func TestListThreadAgentTreeMatchesTheWalkOnRandomGraphs(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewPCG(7, 155))
	for graph := range 150 {
		s := newTestStore(t)
		threads := make([]string, 3+random.IntN(6))
		for i := range threads {
			threads[i] = fmt.Sprintf("g%d-t%d", graph, i)
			mustCreateThread(t, s, threads[i])
		}
		edges := 2 + random.IntN(3*len(threads))
		for e := range edges {
			token := fmt.Sprintf("g%d-r%02d", graph, e)
			caller, target := threads[random.IntN(len(threads))], threads[random.IntN(len(threads))]
			kind := ThreadRequestSpawn
			if random.IntN(3) == 0 {
				kind = ThreadRequestSend
			}
			seedAgentRequest(t, s, token, caller, kind, target, random.IntN(2) == 0, int64(100+random.IntN(50)))
			if random.IntN(3) == 0 {
				settleAgentRequest(t, s, token, int64(5_000+random.IntN(10_000)))
			}
		}
		for _, depth := range []int{1, 2, 4} {
			want := referenceAgentTree(t, s, threads[0], depth)
			rows, more, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: threads[0], SettledSince: 10_000, Depth: depth, OpenDepth: 4, Limit: 1000, AnswerHead: 1})
			if err != nil || more {
				t.Fatalf("graph %d depth %d: more=%v err=%v", graph, depth, more, err)
			}
			var got []string
			seen := map[string]bool{}
			for _, row := range rows {
				entry := fmt.Sprintf("%s@%d", row.Token, row.Depth)
				if row.NestedHidden {
					entry += "+hidden"
				}
				got = append(got, entry)
				if seen[row.Token] {
					t.Fatalf("graph %d depth %d lists %s twice: %v", graph, depth, row.Token, got)
				}
				seen[row.Token] = true
			}
			if !slices.Equal(got, want) {
				t.Fatalf("graph %d depth %d:\n got %v\nwant %v", graph, depth, got, want)
			}
			// Bounded, the listing is the walk's first rows; named, the
			// walk's rows among those named, each where the walk put it.
			limited := agentTreeRead(threads[0], depth)
			limited.Limit = 1 + random.IntN(len(want)+1)
			rows, more, err = s.ListThreadAgentTree(limited)
			if err != nil {
				t.Fatal(err)
			}
			if got, wantRows := agentTreeEntries(rows), want[:min(len(want), limited.Limit)]; !slices.Equal(got, wantRows) || more != (len(want) > limited.Limit) {
				t.Fatalf("graph %d depth %d limit %d:\n got %v more %v\nwant %v more %v", graph, depth, limited.Limit, got, more, wantRows, len(want) > limited.Limit)
			}
			named := agentTreeRead(threads[0], depth)
			named.Limit, named.Tokens = 1000, []string{}
			var wantNamed []string
			for e := range edges {
				if random.IntN(2) == 0 {
					named.Tokens = append(named.Tokens, fmt.Sprintf("g%d-r%02d", graph, e))
				}
			}
			for _, entry := range want {
				if slices.Contains(named.Tokens, strings.SplitN(entry, "@", 2)[0]) {
					wantNamed = append(wantNamed, entry)
				}
			}
			if rows, _, err = s.ListThreadAgentTree(named); err != nil {
				t.Fatal(err)
			}
			if got := agentTreeEntries(rows); !slices.Equal(got, wantNamed) {
				t.Fatalf("graph %d depth %d named %v:\n got %v\nwant %v", graph, depth, named.Tokens, got, wantNamed)
			}
		}
	}
}

// agentTreeEntries is rows as referenceAgentTree writes them.
func agentTreeEntries(rows []ThreadAgentRequest) []string {
	out := []string{}
	for _, row := range rows {
		entry := fmt.Sprintf("%s@%d", row.Token, row.Depth)
		if row.NestedHidden {
			entry += "+hidden"
		}
		out = append(out, entry)
	}
	return out
}

// A thread first reached at the depth bound nests under no later spawn,
// and the rows such a spawn would have nested count against no limit: a
// bounded listing still fills it.
func TestListThreadAgentTreeFillsItsLimitPastAThreadReachedTwice(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "a", "shared"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-a", "lead", ThreadRequestSpawn, "a", false, 100)
	seedAgentRequest(t, s, "l-s", "lead", ThreadRequestSpawn, "shared", false, 101)
	seedAgentRequest(t, s, "l-x", "lead", ThreadRequestSend, "a", true, 102)
	seedAgentRequest(t, s, "l-y", "lead", ThreadRequestSend, "a", true, 103)
	seedAgentRequest(t, s, "a-s", "a", ThreadRequestSpawn, "shared", false, 110)
	for i := range 3 {
		seedAgentRequest(t, s, fmt.Sprintf("s-w%d", i), "shared", ThreadRequestSend, "a", true, int64(120+i))
	}
	all := []string{"l-a@0", "a-s@1+hidden", "l-s@0", "l-x@0", "l-y@0"}
	if want := referenceAgentTree(t, s, "lead", 2); !slices.Equal(want, all) {
		t.Fatalf("the walk lists %v; the test expects %v", want, all)
	}
	for limit := 1; limit <= len(all)+1; limit++ {
		read := agentTreeRead("lead", 2)
		read.Limit = limit
		rows, more, err := s.ListThreadAgentTree(read)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := agentTreeEntries(rows), all[:min(limit, len(all))]; !slices.Equal(got, want) || more != (limit < len(all)) {
			t.Fatalf("limit %d lists %v more %v; want %v more %v", limit, got, more, want, limit < len(all))
		}
	}
	// Named, a row lists where the walk put it, and one the walk left out
	// is not listed.
	named := agentTreeRead("lead", 2)
	named.Tokens = []string{"s-w0", "l-y", "a-s"}
	rows, _, err := s.ListThreadAgentTree(named)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := agentTreeEntries(rows), []string{"a-s@1+hidden", "l-y@0"}; !slices.Equal(got, want) {
		t.Fatalf("named rows %v; want %v", got, want)
	}
}

// A thread two spawns reach is walked once, under the first.
func TestListThreadAgentTreeWalksADiamondOnce(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "a", "b", "shared"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-a", "lead", ThreadRequestSpawn, "a", false, 100)
	seedAgentRequest(t, s, "l-b", "lead", ThreadRequestSpawn, "b", false, 101)
	seedAgentRequest(t, s, "a-s", "a", ThreadRequestSpawn, "shared", false, 110)
	seedAgentRequest(t, s, "b-s", "b", ThreadRequestSpawn, "shared", false, 111)
	seedAgentRequest(t, s, "s-work", "shared", ThreadRequestSend, "lead", true, 120)
	rows, _, err := s.ListThreadAgentTree(agentTreeRead("lead", 4))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, fmt.Sprintf("%s@%d", row.Token, row.Depth))
	}
	if want := []string{"l-a@0", "a-s@1", "s-work@2", "l-b@0", "b-s@1"}; !slices.Equal(got, want) {
		t.Fatalf("diamond lists %v; want %v", got, want)
	}
}

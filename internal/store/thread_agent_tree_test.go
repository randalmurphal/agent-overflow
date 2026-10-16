package store

import (
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
	one.Token = "late"
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

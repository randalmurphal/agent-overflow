package trackerapp

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// An open ticket's lifetime ends once, in memory, at its retirement
// (retireTicketLocked): its bind ends there, and its row is cleanup. The
// tests below hold the I/O around that end against the fake Jira site.

// waitWritten waits until the gate passed on a ticket write of issue, from
// the write numbered from on, that ok accepts, and returns it.
func waitWritten(t *testing.T, gate *bindGate, issue tracker.IssueID, from int, ok func(store.MirrorTicket) bool) store.MirrorTicket {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		for _, tk := range gate.writesOf(issue, from) {
			if ok(tk) {
				return tk
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no write of %s; the writes %+v", issue, gate.writesOf(issue, from))
		}
		time.Sleep(time.Millisecond)
	}
}

// waitHalted waits until Stop has halted the subscriptions.
func waitHalted(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		s.subMu.Lock()
		halted := s.subs.halted
		s.subMu.Unlock()
		if halted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Stop did not halt the subscriptions")
		}
		time.Sleep(time.Millisecond)
	}
}

// The round's full read of an open ticket is held as it stores its answer,
// with a sibling ticket shown so the round lives; the ticket's last handle
// is released, and its row's drop fails, or has not run yet; the answer
// then lands. The issue's board keeps it, so the row's state decides
// nothing: the write carries the closed ticket's bind, which ended at the
// release, and stores no body and no opening. The row goes once the drop
// succeeds.
func TestAClosedTicketsAnswerStoresNothingWhateverBecameOfItsRow(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "its drop failed", true: "its drop has not run"}[held], func(t *testing.T) {
			t.Parallel()
			s, conn, gate, tp, sub := bindScenario(t)
			root := must[int64](t)(rootOf(s, conn))
			before := must[store.MirrorBody](t)(bodyOrFail(t, s, root))
			bind := s.svc.bindOf(tp)
			answer := gate.holdStore(t, "(*run).details(")
			title := s.renameLater(t, conn)
			s.svc.subMu.Lock()
			s.svc.readNowLocked(tp)
			s.svc.subMu.Unlock()
			waitClosed(t, "the round's answer", answer.arrived)
			var drop *gateHold
			if held {
				drop = gate.holdCall(t, callDrop, "")
			} else {
				gate.mu.Lock()
				gate.refuse = 1
				gate.mu.Unlock()
			}
			s.svc.ReleaseSubscription(sub.ID)
			if held {
				waitClosed(t, "the closed ticket's drop", drop.arrived)
			} else {
				exited(t, tp)
			}
			from := gate.written()
			answer.open()
			waitClosed(t, "the round's write", answer.done)
			got := gate.writesOf("10101", from)
			if len(got) != 1 || got[0].Bind != bind || got[0].Body == nil {
				t.Fatalf("the round's write of 10101 %+v; want its body under the closed ticket's bind", got)
			}
			if outcomes := gate.bodies("10101", bind); outcomes[len(outcomes)-1] != store.MirrorBodyNotKept {
				t.Fatalf("the closed ticket's answer: %v; want not kept", outcomes)
			}
			after := must[store.MirrorBody](t)(bodyOrFail(t, s, root))
			if string(after.Data) != string(before.Data) || after.OpenedAt != before.OpenedAt || strings.Contains(string(after.Data), title) {
				t.Fatalf("the record's body moved from %d %s to %d %s", before.OpenedAt, before.Data, after.OpenedAt, after.Data)
			}
			if held {
				drop.open()
				exited(t, tp)
			}
			quiet(t, s.svc)
			s.svc.payOwedDrops("test")
			if n := s.openTicketRows(t); n != 1 {
				t.Fatalf("%d open-ticket rows; want 10102's", n)
			}
		})
	}
}

// A ticket's first read is held before its keep, or in it, and its last
// handle is released meanwhile; while held before its keep, the ticket
// opens again, binds, stores its body and closes, its row dropped. Its
// pump retired, so the held read keeps nothing (held before its keep) or
// its keep's bind is never made current (held in it), the read writes
// nothing, and the pump's exit drops any row.
func TestARetiredPumpsKeepGivesItNoBind(t *testing.T) {
	t.Parallel()
	for _, inKeep := range []bool{false, true} {
		t.Run(map[bool]string{false: "held before its keep", true: "held in its keep"}[inKeep], func(t *testing.T) {
			t.Parallel()
			s, conn, gate, _, _ := bindScenario(t)
			var h *gateHold
			if inKeep {
				h = gate.holdCall(t, callKeep, "")
			} else {
				h = gate.hold(t, "(*run).bindTickets(")
			}
			first := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, "10105", untied))
			waitClosed(t, "the first read's keep", h.arrived)
			p := s.svc.ticketPumpOf(conn.ID, "10105")
			s.svc.ReleaseSubscription(first.ID)
			if !inKeep {
				again := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, "10105", untied))
				q := s.svc.ticketPumpOf(conn.ID, "10105")
				if q == p {
					t.Fatal("the reopened ticket shares the retired pump")
				}
				waitWritten(t, gate, "10105", 0, func(tk store.MirrorTicket) bool { return tk.Body != nil && !tk.Bind.IsZero() })
				s.closeTicket(t, conn, again, "10105")
			}
			from := gate.written()
			h.open()
			exited(t, p)
			if b := s.svc.bindOf(p); !b.IsZero() {
				t.Fatalf("the retired pump holds the bind %+v", b)
			}
			if got := gate.writesOf("10105", from); len(got) != 0 {
				t.Fatalf("the retired pump's read wrote %+v", got)
			}
			if n := s.openTicketRows(t); n != 2 {
				t.Fatalf("%d open-ticket rows; want 10101's and 10102's", n)
			}
		})
	}
}

// A ticket closed and opened again before the cleanup has its first read
// held before its keep, and its last handle is released meanwhile: the
// retired pump makes no row and never shows the closed ticket's record
// again, its goroutine's drop held or not.
func TestAPumpRetiredBeforeItsKeepMakesNoRowAndShowsNothing(t *testing.T) {
	t.Parallel()
	s, conn, gate, _, _ := bindScenario(t)
	s.closeTicket(t, conn, s.openTicket(t, conn, "10105"), "10105")
	if card, doc := s.kept(t, conn, "10105"); card || doc {
		t.Fatalf("the closed ticket's issue shows its card %v and document %v", card, doc)
	}
	h := gate.hold(t, "(*run).bindTickets(")
	drop := gate.holdCall(t, callDrop, "")
	sub := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, "10105", untied))
	waitClosed(t, "the read's keep", h.arrived)
	p := s.svc.ticketPumpOf(conn.ID, "10105")
	s.svc.ReleaseSubscription(sub.ID)
	h.open()
	select {
	case <-drop.arrived:
	case <-p.exited:
	case <-time.After(20 * time.Second):
		t.Fatal("the retired pump neither dropped its row nor returned")
	}
	if _, ok := gate.kept(p.number); ok {
		t.Error("the retired pump's keep made a row")
	}
	if card, doc := s.kept(t, conn, "10105"); card || doc {
		t.Errorf("the retired pump showed the closed ticket's card %v and document %v", card, doc)
	}
	drop.open()
	exited(t, p)
}

// A ticket closed and opened again before the cleanup has its first keep
// held, and its last handle is released meanwhile: the keep commits and
// shows the closed ticket's record again, but its pump retired, so it
// publishes nothing, and its bind ends never activated: a ticket that
// shows the issue rebuilds nothing until the pump's drop, held here, ends
// the keep.
func TestAKeepWhosePumpRetiredMeanwhilePublishesNothing(t *testing.T) {
	t.Parallel()
	s, conn, gate, other, _ := bindScenario(t)
	s.closeTicket(t, conn, s.openTicket(t, conn, "10105"), "10105")
	s.svc.subMu.Lock()
	other.view.shows = append(other.view.shows, "10105")
	dirty := other.dirty
	s.svc.subMu.Unlock()
	keep := gate.holdCall(t, callKeep, "")
	drop := gate.holdCall(t, callDrop, "")
	sub := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, "10105", untied))
	waitClosed(t, "the read's keep", keep.arrived)
	p := s.svc.ticketPumpOf(conn.ID, "10105")
	s.svc.ReleaseSubscription(sub.ID)
	keep.open()
	waitClosed(t, "the retired pump's drop", drop.arrived)
	if card, _ := s.kept(t, conn, "10105"); !card {
		t.Fatal("the keep did not show the closed ticket's record again")
	}
	s.svc.subMu.Lock()
	moved := other.dirty != dirty
	s.svc.subMu.Unlock()
	if moved {
		t.Error("the retired pump's keep was published")
	}
	gate.mu.Lock()
	bind, ended := gate.keeps[p.number], slices.Clone(gate.ended)
	gate.mu.Unlock()
	if bind.IsZero() || !slices.Contains(ended, bind) || !s.svc.bindOf(p).IsZero() {
		t.Errorf("the retired pump's keep bind %+v; the binds ended %+v; want it ended, not held", bind, ended)
	}
	drop.open()
	exited(t, p)
}

// A ticket whose first paint is held is read by a comment's refresh,
// which keeps its issue and binds it, before its pump launches; the paint
// then fails, so its last handle goes, or Stop comes first. The pump's
// goroutine runs all the same, reads nothing, and drops the row.
func TestAPumpStoppedBeforeItsLaunchDropsTheRowARefreshKept(t *testing.T) {
	t.Parallel()
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "its first paint failed", true: "Stop"}[stop], func(t *testing.T) {
			t.Parallel()
			s, conn, gate, _, _ := bindScenario(t)
			var fail error
			if !stop {
				fail = errors.New("test: the first paint's read failed")
			}
			paint := gate.holdFailing(t, callBodies, "(*Service).SubscribeTicket(", fail)
			subscribed := make(chan error, 1)
			go func() {
				_, err := s.svc.SubscribeTicket(conn.ID, "10105", untied)
				subscribed <- err
			}()
			waitClosed(t, "the first paint", paint.arrived)
			tp := s.svc.ticketPumpOf(conn.ID, "10105")
			must[WriteView](t)(s.svc.AddComment(t.Context(), conn.ID, CommentRequest{Issue: "10105", Markdown: "A comment"}))
			if s.svc.bindOf(tp).IsZero() {
				t.Fatal("the refresh did not bind the ticket before its launch")
			}
			stopped := make(chan struct{})
			if stop {
				go func() {
					s.svc.Stop()
					close(stopped)
				}()
				waitHalted(t, s.svc)
			}
			paint.open()
			if err := <-subscribed; (err != nil) == stop {
				t.Fatalf("the subscribe: %v", err)
			}
			exited(t, tp)
			want := int64(2)
			if stop {
				<-stopped
				want = 0
			}
			if b := s.svc.bindOf(tp); !b.IsZero() {
				t.Fatalf("the stopped pump holds the bind %+v", b)
			}
			if n := s.openTicketRows(t); n != want {
				t.Fatalf("%d open-ticket rows; want %d", n, want)
			}
		})
	}
}

// Stop comes while a ticket's first read keeps its issue: the keep
// commits, makes no bind, and the pump's exit drops the row, as every
// other ticket's.
func TestStopDuringAKeepLeavesNoRowAndNoBind(t *testing.T) {
	t.Parallel()
	s, conn, gate, _, _ := bindScenario(t)
	keep := gate.holdCall(t, callKeep, "")
	must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, "10105", untied))
	waitClosed(t, "the first read's keep", keep.arrived)
	tp := s.svc.ticketPumpOf(conn.ID, "10105")
	stopped := make(chan struct{})
	go func() {
		s.svc.Stop()
		close(stopped)
	}()
	waitHalted(t, s.svc)
	keep.open()
	<-stopped
	if b := s.svc.bindOf(tp); !b.IsZero() {
		t.Fatalf("the stopped pump holds the bind %+v", b)
	}
	if n := s.openTicketRows(t); n != 0 {
		t.Fatalf("%d open-ticket rows after Stop", n)
	}
}

// A restore empties the mirror; the round finds the root gone and makes
// another, whose rehold is held as it begins its keeps; a ticket closes
// meanwhile. Released, the rehold keeps both tickets it found live, and
// binds only the one still open; the closed one's exit drops its row.
func TestARestoresReholdBindsOnlyTheTicketsStillOpen(t *testing.T) {
	t.Parallel()
	s, conn, gate, closed, sub := bindScenario(t)
	open := s.svc.ticketPumpOf(conn.ID, "10102")
	snap := filepath.Join(t.TempDir(), "db.snapshot")
	if err := s.st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.RestoreFrom(snap); err != nil {
		t.Fatal(err)
	}
	rehold := gate.hold(t, "(*Service).keepTicketsAgainLocked(")
	rd := s.svc.roundOf(conn.ID)
	ticked := make(chan ticketsResult, 1)
	go func() { ticked <- s.svc.roundTick(rd) }()
	waitClosed(t, "the rehold's keeps", rehold.arrived)
	s.svc.ReleaseSubscription(sub.ID)
	rehold.open()
	<-ticked
	exited(t, closed)
	if b := s.svc.bindOf(closed); !b.IsZero() {
		t.Fatalf("the closed ticket holds the bind %+v after the rehold", b)
	}
	if s.svc.bindOf(open).IsZero() {
		t.Fatal("the open ticket has no bind after the rehold")
	}
	if n := s.openTicketRows(t); n != 1 {
		t.Fatalf("%d open-ticket rows after the rehold; want the open ticket's", n)
	}
}

// The rehold's keep of a live ticket replaces its bind of the emptied
// root, which ends as the new one becomes current; a rehold whose keep
// fails leaves the old one, which no read carries: the ticket's next read
// keeps and binds it first, and stores its body.
func TestAReholdEndsTheBindOfTheEmptiedRoot(t *testing.T) {
	t.Parallel()
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "its keep succeeds", true: "its keep fails"}[fails], func(t *testing.T) {
			t.Parallel()
			s, conn, gate, tp, sub := bindScenario(t)
			old := s.svc.bindOf(tp)
			snap := filepath.Join(t.TempDir(), "db.snapshot")
			if err := s.st.SnapshotTo(snap); err != nil {
				t.Fatal(err)
			}
			if _, err := s.st.RestoreFrom(snap); err != nil {
				t.Fatal(err)
			}
			if fails {
				gate.mu.Lock()
				gate.refuseKeep = "10101"
				gate.mu.Unlock()
			}
			s.svc.roundTick(s.svc.roundOf(conn.ID))
			if err := s.svc.RefreshSubscription(t.Context(), sub.ID); err != nil {
				t.Fatal(err)
			}
			bind := s.svc.bindOf(tp)
			gate.mu.Lock()
			ended := slices.Contains(gate.ended, old)
			gate.mu.Unlock()
			if bind.IsZero() || bind == old || !ended {
				t.Fatalf("after the rehold the ticket's bind %+v (before %+v), the old one ended %v", bind, old, ended)
			}
			if got := gate.bodies("10101", bind); !slices.Contains(got, store.MirrorBodyStored) {
				t.Fatalf("the ticket's reads under its new bind stored %v", got)
			}
		})
	}
}

// A closed ticket's row whose drop failed stands beside the reopened
// ticket's: at most the owed row and the new one, and the owed row goes
// when its drop next succeeds.
func TestAnOwedRowStandsBesideTheReopenedTicketsUntilItsDrop(t *testing.T) {
	t.Parallel()
	for _, paid := range []bool{false, true} {
		t.Run(map[bool]string{false: "owed", true: "paid"}[paid], func(t *testing.T) {
			t.Parallel()
			s, conn, gate, _, sub := bindScenario(t)
			gate.mu.Lock()
			gate.refuse = 1 << 10
			gate.mu.Unlock()
			s.closeTicket(t, conn, sub, "10101")
			s.openTicket(t, conn, "10101")
			gate.mu.Lock()
			gate.refuse = 0
			gate.mu.Unlock()
			want := int64(3)
			if paid {
				s.svc.payOwedDrops("test")
				want = 2
			}
			if n := s.openTicketRows(t); n != want {
				t.Fatalf("%d open-ticket rows; want %d: 10102's, the reopened ticket's and the owed one until paid", n, want)
			}
		})
	}
}

// retireTicketLocked is the one place a pump leaves the open tickets: no
// other code of the package deletes from or clears the map.
func TestRetirementIsTheOnlyDeleteOfTheOpenTickets(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	removal := regexp.MustCompile(`(delete|clear)\(s\.subs\.tickets`)
	var found []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range removal.FindAllString(string(src), -1) {
			found = append(found, f+": "+m)
		}
	}
	if len(found) != 1 || !strings.HasPrefix(found[0], "pump_ticket.go: delete") {
		t.Fatalf("the removals from the open tickets %v; want retireTicketLocked's delete alone", found)
	}
}

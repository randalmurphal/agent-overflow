package trackerapp

import (
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A ticket read is admitted by the exact bind it was made under (store
// MirrorTicket.Bind): the tests below hold a read of one lifetime of a
// ticket while it closes and opens again, against the fake Jira site.

// bindGate is a store that records every ticket write and, for each frame
// armed, holds until released the next call of a kind whose stack names
// it: BeginMirrorRead (hold), StoreMirrorTickets (holdStore),
// KeepMirrorTicket and DropMirrorTicket (holdCall); the first refuse
// drops of an open ticket fail, and the first keep of refuseKeep.
type bindGate struct {
	*store.Store
	mu         sync.Mutex
	holds      []*gateHold
	writes     []ticketWrite
	refuse     int
	refuseKeep string
	ended      []store.MirrorBind
}

// The kinds of call a bindGate holds.
const (
	callRead  = "read"
	callStore = "store"
	callKeep  = "keep"
	callDrop  = "drop"
)

// gateHold is one armed hold of a bindGate: arrived closes when a call
// arrives at it, which then waits for release and fails with fail when
// set; done closes when the held call returned.
type gateHold struct {
	frame, kind            string
	fail                   error
	arrived, release, done chan struct{}
	once                   sync.Once
}

// open releases the call h holds, once.
func (h *gateHold) open() { h.once.Do(func() { close(h.release) }) }

// ticketWrite is one StoreMirrorTickets call a bindGate passed on, with
// what it did.
type ticketWrite struct {
	tickets []store.MirrorTicket
	out     store.MirrorTicketsWrite
}

// hold arms a hold of the next BeginMirrorRead whose stack names frame.
func (g *bindGate) hold(t *testing.T, frame string) *gateHold { return g.holdCall(t, callRead, frame) }

// holdStore arms a hold of the next StoreMirrorTickets whose stack names
// frame.
func (g *bindGate) holdStore(t *testing.T, frame string) *gateHold {
	return g.holdCall(t, callStore, frame)
}

// holdCall arms a hold of the next call of kind whose stack names frame
// (any, for "").
func (g *bindGate) holdCall(t *testing.T, kind, frame string) *gateHold {
	return g.holdFailing(t, kind, frame, nil)
}

// holdFailing is holdCall of a call that fails with fail once released.
func (g *bindGate) holdFailing(t *testing.T, kind, frame string, fail error) *gateHold {
	h := &gateHold{frame: frame, kind: kind, fail: fail, arrived: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(h.open)
	g.mu.Lock()
	g.holds = append(g.holds, h)
	g.mu.Unlock()
	return h
}

// wait holds the caller at the first armed hold of kind whose frame its
// stack names, until released, and returns the hold (nil for none), whose
// done the caller closes when its call returned.
func (g *bindGate) wait(kind string) *gateHold {
	stack := string(debug.Stack())
	g.mu.Lock()
	i := slices.IndexFunc(g.holds, func(h *gateHold) bool { return h.kind == kind && strings.Contains(stack, h.frame) })
	var h *gateHold
	if i >= 0 {
		h = g.holds[i]
		g.holds = slices.Delete(g.holds, i, i+1)
	}
	g.mu.Unlock()
	if h != nil {
		close(h.arrived)
		<-h.release
	}
	return h
}

func (g *bindGate) BeginMirrorRead(root, now int64) (store.MirrorRead, error) {
	if h := g.wait(callRead); h != nil {
		defer close(h.done)
		if h.fail != nil {
			return store.MirrorRead{}, h.fail
		}
	}
	return g.Store.BeginMirrorRead(root, now)
}

func (g *bindGate) StoreMirrorTickets(root, sweep int64, tickets []store.MirrorTicket, catalog store.MirrorCatalog,
	opened, read store.MirrorRead) (store.MirrorTicketsWrite, error) {
	if h := g.wait(callStore); h != nil {
		defer close(h.done)
	}
	out, err := g.Store.StoreMirrorTickets(root, sweep, tickets, catalog, opened, read)
	if err == nil {
		g.mu.Lock()
		g.writes = append(g.writes, ticketWrite{slices.Clone(tickets), out})
		g.mu.Unlock()
	}
	return out, err
}

func (g *bindGate) KeepMirrorTicket(root int64, issue string, session, pump int64, opened store.MirrorRead) (store.MirrorBind, bool, error) {
	g.mu.Lock()
	refused := issue == g.refuseKeep
	if refused {
		g.refuseKeep = ""
	}
	g.mu.Unlock()
	if refused {
		return store.MirrorBind{}, false, errors.New("test: the store refused the keep")
	}
	if h := g.wait(callKeep); h != nil {
		defer close(h.done)
	}
	return g.Store.KeepMirrorTicket(root, issue, session, pump, opened)
}

func (g *bindGate) DropMirrorTicket(session, pump int64) (bool, error) {
	if h := g.wait(callDrop); h != nil {
		defer close(h.done)
	}
	g.mu.Lock()
	refused := g.refuse > 0
	g.refuse--
	g.mu.Unlock()
	if refused {
		return false, errors.New("test: the store refused the drop")
	}
	return g.Store.DropMirrorTicket(session, pump)
}

func (g *bindGate) EndMirrorBind(b store.MirrorBind) {
	g.mu.Lock()
	if !b.IsZero() {
		g.ended = append(g.ended, b)
	}
	g.mu.Unlock()
	g.Store.EndMirrorBind(b)
}

// writesOf are the tickets of issue the gate's writes stored from the
// write numbered from on.
func (g *bindGate) writesOf(issue tracker.IssueID, from int) []store.MirrorTicket {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []store.MirrorTicket
	for _, w := range g.writes[from:] {
		for _, tk := range w.tickets {
			if tk.Card.IssueID == string(issue) {
				out = append(out, tk)
			}
		}
	}
	return out
}

// written is the number of writes the gate passed on.
func (g *bindGate) written() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.writes)
}

// bodies are the outcomes of the bodies of issue that the writes of the
// tickets read under bind brought.
func (g *bindGate) bodies(issue tracker.IssueID, bind store.MirrorBind) []store.MirrorBodyOutcome {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []store.MirrorBodyOutcome
	for _, w := range g.writes {
		for i, tk := range w.tickets {
			if tk.Card.IssueID == string(issue) && tk.Bind == bind && tk.Body != nil {
				out = append(out, w.out.Tickets[i].Body)
			}
		}
	}
	return out
}

// waitBodies waits until the writes of issue's tickets read under bind
// brought n bodies, and returns their outcomes.
func (g *bindGate) waitBodies(t *testing.T, issue tracker.IssueID, bind store.MirrorBind, n int) []store.MirrorBodyOutcome {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := g.bodies(issue, bind)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the bodies of %s under bind %+v are %v; want %d", issue, bind, got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// bindOf is tp's bind.
func (s *Service) bindOf(tp *ticketPump) store.MirrorBind {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return tp.bind
}

// bindScenario is keepScenario with WEB's board read and the store gated
// (bindGate), and WEB-1 (10101, which the board keeps) open: its pump, its
// bind and its subscription. WEB-2 (10102) is open too, so the
// connection's round runs while 10101 is closed.
func bindScenario(t *testing.T) (*fakeScenario, Connection, *bindGate, *ticketPump, TicketSubscription) {
	t.Helper()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	s.syncBoard(t, scope)
	gate := &bindGate{Store: s.st}
	s.svc.store = gate
	s.openTicket(t, conn, "10102")
	sub := s.openTicket(t, conn, "10101")
	quiet(t, s.svc)
	return s, conn, gate, s.svc.ticketPumpOf(conn.ID, "10101"), sub
}

// heldRoundRead renames 10101 and makes its open ticket tp due a read at
// once (readNowLocked), which wakes the connection's round, and holds the
// round's read of the ticket in full as it begins (run.details), after the
// round's check stored the renamed card under tp's bind. It returns the
// hold, which the caller opens, and the title.
func heldRoundRead(t *testing.T, s *fakeScenario, conn Connection, gate *bindGate, tp *ticketPump) (*gateHold, string) {
	t.Helper()
	h := gate.hold(t, "(*run).details(")
	title := s.renameLater(t, conn)
	s.svc.subMu.Lock()
	s.svc.readNowLocked(tp)
	s.svc.subMu.Unlock()
	waitClosed(t, "the round's read of the ticket in full", h.arrived)
	return h, title
}

// The old pump's read passed its bind, the ticket closes and opens again
// (a new bind, whose first read stores the body), and only then the old
// read begins, of a newer version: the record holds the new lifetime's
// body, not the old read's, though the old read began after the new bind.
func TestAReadOfAClosedTicketBegunAfterTheReopenStoresNoBody(t *testing.T) {
	t.Parallel()
	s, conn, gate, oldPump, old := bindScenario(t)
	oldBind := s.svc.bindOf(oldPump)
	h, _ := heldRoundRead(t, s, conn, gate, oldPump)
	s.closeTicket(t, conn, old, "10101")
	s.openTicket(t, conn, "10101")
	newBind := s.svc.bindOf(s.svc.ticketPumpOf(conn.ID, "10101"))
	if newBind == oldBind || !slices.Contains(gate.bodies("10101", newBind), store.MirrorBodyStored) {
		t.Fatalf("the reopened ticket's bind %+v (the old %+v) stored %v", newBind, oldBind, gate.bodies("10101", newBind))
	}
	s.renameLater(t, conn)
	h.open()
	quiet(t, s.svc)
	if got := gate.bodies("10101", oldBind); len(got) != 2 || got[1] != store.MirrorBodyNotKept {
		t.Fatalf("the old lifetime's bodies %v; want its first read's and the round's not kept", got)
	}
}

// The old pump's read is held, the ticket closes and is not opened again:
// the read's body is not stored, though the board keeps the issue and the
// record holds the old lifetime's own body.
func TestAReadOfAClosedTicketStoresNoBody(t *testing.T) {
	t.Parallel()
	s, conn, gate, oldPump, old := bindScenario(t)
	oldBind := s.svc.bindOf(oldPump)
	h, _ := heldRoundRead(t, s, conn, gate, oldPump)
	s.closeTicket(t, conn, old, "10101")
	h.open()
	quiet(t, s.svc)
	if got := gate.bodies("10101", oldBind); len(got) != 2 || got[1] != store.MirrorBodyNotKept {
		t.Fatalf("the closed lifetime's bodies %v; want its first read's and the round's not kept", got)
	}
}

// The old ticket's drop fails, and fails again at each run's start, so it
// is owed when the ticket opens again, which keeps the issue beside the
// owed row and binds and reads it; the reopened ticket closes first, and
// the old read lands after both: no bind admits it, and the owed drop
// deletes the old row when it is paid.
func TestAReadOfAClosedTicketWhoseDropIsOwedStoresNoBody(t *testing.T) {
	t.Parallel()
	s, conn, gate, oldPump, old := bindScenario(t)
	oldBind := s.svc.bindOf(oldPump)
	h, _ := heldRoundRead(t, s, conn, gate, oldPump)
	// The close's drop and every run's try of it until the test pays it.
	gate.mu.Lock()
	gate.refuse = 1 << 10
	gate.mu.Unlock()
	s.closeTicket(t, conn, old, "10101")
	s.svc.opMu.Lock()
	owed := len(s.svc.owedDrops)
	s.svc.opMu.Unlock()
	if owed != 1 {
		t.Fatalf("%d drops owed; want the old ticket's", owed)
	}
	reopened := s.openTicket(t, conn, "10101")
	if v := must[TicketView](t)(s.svc.TicketDetail(reopened.ID)); !v.Loaded || v.Failure != nil {
		t.Fatalf("the ticket reopened while the old drop is owed shows %+v; want it bound and read", v)
	}
	gate.mu.Lock()
	gate.refuse = 0
	gate.mu.Unlock()
	s.closeTicket(t, conn, reopened, "10101")
	h.open()
	quiet(t, s.svc)
	if got := gate.bodies("10101", oldBind); len(got) != 2 || got[1] != store.MirrorBodyNotKept {
		t.Fatalf("the closed lifetime's bodies %v; want its first read's and the round's not kept", got)
	}
	s.svc.payOwedDrops("test")
	if n := s.openTicketRows(t); n != 1 {
		t.Fatalf("%d open-ticket rows after both closed; want 10102's", n)
	}
}

// A round's check by card of an open ticket (a ticket write with no body)
// is held at the store; the ticket closes and opens again, and the new
// ticket's first read is held after its keep, before it stores anything.
// The old check then lands: it records no opening on the issue's body,
// so the body's opening is not the reopened ticket's until the reopened
// ticket's own read stores it.
func TestACheckByCardOfAClosedTicketRecordsNoOpeningForTheReopenedOne(t *testing.T) {
	t.Parallel()
	s, conn, gate, oldPump, old := bindScenario(t)
	oldBind := s.svc.bindOf(oldPump)
	root := must[int64](t)(rootOf(s, conn))
	check := gate.holdStore(t, "(*run).ticketCards(")
	s.svc.subMu.Lock()
	s.svc.readNowLocked(oldPump)
	s.svc.subMu.Unlock()
	waitClosed(t, "the round's check by card", check.arrived)
	s.closeTicket(t, conn, old, "10101")
	opening := gate.hold(t, "(*run).verifiedDetails(")
	reopened := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, "10101"))
	waitClosed(t, "the reopened ticket's first read", opening.arrived)
	before := must[store.MirrorBody](t)(bodyOrFail(t, s, root))
	check.open()
	waitClosed(t, "the round's check by card to return", check.done)
	gate.mu.Lock()
	last := gate.writes[len(gate.writes)-1].tickets
	gate.mu.Unlock()
	if i := slices.IndexFunc(last, func(tk store.MirrorTicket) bool { return tk.Card.IssueID == "10101" }); i < 0 ||
		last[i].Body != nil || last[i].Bind != oldBind {
		t.Fatalf("the last ticket write %+v; want the closed ticket's check by card under bind %+v", last, oldBind)
	}
	if mid := must[store.MirrorBody](t)(bodyOrFail(t, s, root)); mid.OpenedAt != before.OpenedAt {
		t.Fatalf("the closed ticket's check by card moved the body's opening from %d to %d before the reopened ticket wrote",
			before.OpenedAt, mid.OpenedAt)
	}
	opening.open()
	settled(t, s.svc)
	quiet(t, s.svc)
	if v := must[TicketView](t)(s.svc.TicketDetail(reopened.ID)); !v.Loaded || v.Failure != nil {
		t.Fatalf("the reopened ticket shows %+v", v)
	}
	if after := must[store.MirrorBody](t)(bodyOrFail(t, s, root)); after.OpenedAt <= before.OpenedAt {
		t.Fatalf("the reopened ticket's read left the body's opening at %d; want it later than %d", after.OpenedAt, before.OpenedAt)
	}
}

// A second handle of an open ticket shares its pump and its bind: its
// first paint is the pump's view, it makes no bind, and a refresh through
// it stores its body under the pump's bind.
func TestASecondHandleSharesItsTicketsBind(t *testing.T) {
	t.Parallel()
	s, conn, gate, tp, first := bindScenario(t)
	bind := s.svc.bindOf(tp)
	second := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, "10101"))
	if v := must[TicketView](t)(s.svc.TicketDetail(second.ID)); !v.Loaded || v.Detail.Card.ID != "10101" {
		t.Fatalf("the second handle's first paint %+v", v)
	}
	if got := must[TicketView](t)(s.svc.TicketDetail(first.ID)); !got.Loaded {
		t.Fatalf("the first handle's view %+v", got)
	}
	s.renameLater(t, conn)
	if err := s.svc.RefreshSubscription(t.Context(), second.ID); err != nil {
		t.Fatal(err)
	}
	if s.svc.bindOf(tp) != bind {
		t.Fatalf("the second handle made a bind %+v; want the pump's %+v", s.svc.bindOf(tp), bind)
	}
	if got := gate.bodies("10101", bind); len(got) != 2 || got[1] != store.MirrorBodyStored {
		t.Fatalf("the pump's bodies %v; want the first read's and the refresh's stored", got)
	}
	if n := s.openTicketRows(t); n != 2 {
		t.Fatalf("%d open-ticket rows; want the pump's and 10102's", n)
	}
}

// The old read lands after the ticket opened again and bound, before the
// new pump stored anything: no body of the new lifetime orders it, so the
// bind alone refuses it; the new pump's read then stores its own.
func TestAReadOfAClosedTicketLandingBeforeTheReopenedReadStoresNoBody(t *testing.T) {
	t.Parallel()
	s, conn, gate, oldPump, old := bindScenario(t)
	oldBind := s.svc.bindOf(oldPump)
	root := must[int64](t)(rootOf(s, conn))
	h, title := heldRoundRead(t, s, conn, gate, oldPump)
	s.closeTicket(t, conn, old, "10101")
	opening := gate.hold(t, "(*run).verifiedDetails(")
	reopened := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, "10101"))
	waitClosed(t, "the reopened ticket's first read", opening.arrived)
	newPump := s.svc.ticketPumpOf(conn.ID, "10101")
	newBind := s.svc.bindOf(newPump)
	if newBind.IsZero() || newBind == oldBind {
		t.Fatalf("the reopened ticket's bind %+v (the old %+v); want its own, made before its read", newBind, oldBind)
	}
	h.open()
	if got := gate.waitBodies(t, "10101", oldBind, 2); len(got) != 2 || got[1] != store.MirrorBodyNotKept {
		t.Fatalf("the closed lifetime's bodies %v; want its first read's and the round's not kept", got)
	}
	opening.open()
	settled(t, s.svc)
	quiet(t, s.svc)
	b, ok, err := s.st.GetMirrorBody(root, "10101")
	if err != nil || !ok {
		t.Fatalf("the reopened ticket's body: %v, %v", ok, err)
	}
	if !strings.Contains(string(b.Data), title) {
		t.Fatalf("the record holds %s; want the renamed issue", b.Data)
	}
	if v := must[TicketView](t)(s.svc.TicketDetail(reopened.ID)); !v.Loaded || v.Detail.Card.Title != title {
		t.Fatalf("the reopened ticket shows %+v", v.Detail)
	}
}

// A read no open ticket made (IssueDetail's) held at the tracker across a
// close and a reopen is ordered by its version and read alone: released
// after a rename, while the reopened ticket's round read is held, it
// stores its body, a newer version than the one the reopened ticket
// stored, and no lifetime refuses it.
func TestADirectReadHeldAcrossACloseAndReopenIsOrderedByItsVersion(t *testing.T) {
	t.Parallel()
	s, conn, gate, _, old := bindScenario(t)
	root := must[int64](t)(rootOf(s, conn))
	s.renameLater(t, conn)
	const route = "GET rest/api/3/issue/{issue}"
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: route, Kind: trackerfake.FaultHold, Times: 1}); err != nil {
		t.Fatal(err)
	}
	direct := make(chan error, 1)
	go func() {
		_, _, err := s.svc.RefreshIssueDetail(t.Context(), conn.ID, "10101", Interactive)
		direct <- err
	}()
	waitHeld(t, s.e, route)
	s.closeTicket(t, conn, old, "10101")
	s.openTicket(t, conn, "10101")
	quiet(t, s.svc)
	newBind := s.svc.bindOf(s.svc.ticketPumpOf(conn.ID, "10101"))
	if !slices.Contains(gate.bodies("10101", newBind), store.MirrorBodyStored) {
		t.Fatalf("the reopened ticket stored %v", gate.bodies("10101", newBind))
	}
	before := must[store.MirrorBody](t)(bodyOrFail(t, s, root))
	round, _ := heldRoundRead(t, s, conn, gate, s.svc.ticketPumpOf(conn.ID, "10101"))
	if err := s.e.Release(origin, route, false); err != nil {
		t.Fatal(err)
	}
	if err := <-direct; err != nil {
		t.Fatal(err)
	}
	round.open()
	quiet(t, s.svc)
	if got := gate.bodies("10101", store.MirrorBind{}); len(got) != 1 || got[0] != store.MirrorBodyStored {
		t.Fatalf("the direct read's bodies %v; want it stored", got)
	}
	after := must[store.MirrorBody](t)(bodyOrFail(t, s, root))
	if after.UpdatedAt <= before.UpdatedAt {
		t.Fatalf("the record's body is of %d; want the direct read's newer version over %d", after.UpdatedAt, before.UpdatedAt)
	}
}

// A round takes the read of an open ticket whose bind failed (OPS-1,
// 10105, which no board keeps), held before it binds; the ticket closes
// and opens again, and the new pump binds and stores its body; the issue
// is renamed. The old pump, ended before it was ever bound, has no bind
// to read under: the round answers it ended and writes nothing of it, so
// its read of the newer version is not stored as a read no ticket made.
func TestATicketEndedBeforeItsBindIsNotRead(t *testing.T) {
	t.Parallel()
	s, conn, gate, _, _ := bindScenario(t)
	gate.mu.Lock()
	gate.refuseKeep = "10105"
	gate.mu.Unlock()
	old := s.openTicket(t, conn, "10105")
	if v := must[TicketView](t)(s.svc.TicketDetail(old.ID)); v.Failure == nil {
		t.Fatalf("the ticket whose keep failed shows %+v; want its failure", v)
	}
	oldPump := s.svc.ticketPumpOf(conn.ID, "10105")
	h := gate.hold(t, "(*run).bindTickets(")
	s.svc.subMu.Lock()
	s.svc.readNowLocked(oldPump)
	s.svc.subMu.Unlock()
	waitClosed(t, "the round's bind", h.arrived)
	s.closeTicket(t, conn, old, "10105")
	s.openTicket(t, conn, "10105")
	newBind := s.svc.bindOf(s.svc.ticketPumpOf(conn.ID, "10105"))
	if !slices.Contains(gate.bodies("10105", newBind), store.MirrorBodyStored) {
		t.Fatalf("the reopened ticket stored %v", gate.bodies("10105", newBind))
	}
	s.renameIssueLater(t, conn, "10105")
	h.open()
	quiet(t, s.svc)
	if got := gate.bodies("10105", store.MirrorBind{}); len(got) != 0 {
		t.Fatalf("the ended ticket's read stored bodies %v", got)
	}
}

// renameLater moves the fake's clock a second and renames 10101 to a
// title no earlier rename gave it, so its update time moves; it returns
// the title.
func (s *fakeScenario) renameLater(t *testing.T, conn Connection) string {
	t.Helper()
	return s.renameIssueLater(t, conn, "10101")
}

// renameIssueLater is renameLater of issue.
func (s *fakeScenario) renameIssueLater(t *testing.T, conn Connection, issue tracker.IssueID) string {
	t.Helper()
	s.clock.advance(time.Second)
	title := fmt.Sprintf("Renamed %s at %d", issue, s.clock.Now().Unix())
	edit := tracker.EditInput{Issue: issue, Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues(title)}}}
	applied(t, "edit", s.provider(conn.ID).Edit(t.Context(), edit))
	return title
}

// bodyOrFail is the body the record of 10101 holds.
func bodyOrFail(t *testing.T, s *fakeScenario, root int64) (store.MirrorBody, error) {
	t.Helper()
	b, ok, err := s.st.GetMirrorBody(root, "10101")
	if err == nil && !ok {
		err = errors.New("the record holds no body")
	}
	return b, err
}

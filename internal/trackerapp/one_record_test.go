package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The issue record is the one record of an issue: a ticket's read and a
// board's read of the same issue are admitted by the same store code, so
// what one shows the other shows, and an open ticket's lifetime is its
// keep row alone.

// renamedAtTheSameUpdate is seededCard renamed to APP-1 and relabeled,
// changes a tracker makes without moving the issue's update time.
func renamedAtTheSameUpdate() tracker.Card {
	c := seededCard()
	c.Key = "APP-1"
	c.Labels = []tracker.Choice{{ID: "frontend-web", Name: "frontend-web"}}
	return c
}

// checkRenamed fails unless v shows the renamed card with its own URL.
func checkRenamed(t *testing.T, what string, v CardView, url string) {
	t.Helper()
	if v.Key != "APP-1" || len(v.Labels) != 1 || v.Labels[0].Name != "frontend-web" {
		t.Fatalf("%s shows %s labeled %+v; want APP-1 labeled frontend-web", what, v.Key, v.Labels)
	}
	if url != "" && !strings.HasSuffix(url, "/APP-1") {
		t.Fatalf("%s links %s; want the URL of APP-1", what, url)
	}
}

// A key and a label renamed at an unchanged update time show on the board
// and on the ticket whichever read lands last: the ticket's read, which
// began after the board's, or a board read that began before the
// ticket's and lands after it.
func TestARenameAtAnUnchangedUpdateTimeShowsEverywhere(t *testing.T) {
	t.Parallel()
	t.Run("the ticket's read lands last", func(t *testing.T) {
		t.Parallel()
		m := newMirrorFixture(t)
		m.seedBoard(t)
		m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(renamedAtTheSameUpdate()), nil }
		v, _ := m.openDetail(t, "10001")
		checkRenamed(t, "the ticket", v.Card, v.URL)
		checkRenamed(t, "the board", must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")).Card, "")
	})
	t.Run("the board's read lands last", func(t *testing.T) {
		t.Parallel()
		m := newMirrorFixture(t)
		m.seedBoard(t)
		// A board read that began before the rename, held at the tracker.
		before := tracker.Row{Card: seededCard(), Order: "a"}
		arrived, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		m.p.set(func(p *scripted) {
			p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
				return tracker.RowPage{Rows: []tracker.Row{before}, Next: cursorAfter("10001")}, nil
			}
			p.block = func(ctx context.Context, name string) error {
				if name != "ChangedRows" {
					return nil
				}
				close(arrived)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
		done := make(chan error, 1)
		go func() {
			_, err := m.svc.tickDelta(context.Background(), m.scope(), Background)
			done <- err
		}()
		<-arrived
		m.p.set(func(p *scripted) {
			p.block = nil
			p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(renamedAtTheSameUpdate()), nil }
		})
		v, _ := m.openDetail(t, "10001")
		checkRenamed(t, "the ticket", v.Card, v.URL)
		once.Do(func() { close(release) })
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		checkRenamed(t, "the board after the late board read", must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")).Card, "")
		v, _ = m.openDetail(t, "10001")
		checkRenamed(t, "the ticket after the late board read", v.Card, v.URL)
	})
}

// A read that began after another but whose tracker answered an older
// version, landing after the other stored the newer one, never shows: the
// open ticket keeps the newer content.
func TestALateOlderReadNeverReplacesANewerTicket(t *testing.T) {
	t.Parallel()
	m, st, issues, subs := newRoundFixture(t, 1, false)
	m.settledTickets(t)
	issue := issues[0]
	st.edit(issue, func(c *tracker.Card) { c.Title = "Second" })
	older := must[tracker.Detail](t)(st.detail(issue))
	st.edit(issue, func(c *tracker.Card) { c.Title = "Third" })
	// The first Details call after this is the refresh's, the second the
	// round's; each is held until released.
	arrived := []chan struct{}{make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var ends [2]sync.Once
	for i := range release {
		t.Cleanup(func() { ends[i].Do(func() { close(release[i]) }) })
	}
	var mu sync.Mutex
	calls := 0
	m.p.set(func(p *scripted) {
		p.block = func(ctx context.Context, name string) error {
			if name != "Details" {
				return nil
			}
			mu.Lock()
			n := calls
			calls++
			mu.Unlock()
			if n > 1 {
				return nil
			}
			close(arrived[n])
			select {
			case <-release[n]:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	refreshed := make(chan error, 1)
	go func() {
		v, _, err := m.svc.RefreshIssueDetail(context.Background(), m.conn.ID, issue, Interactive)
		if err == nil && v.Card.Title != "Third" {
			err = fmt.Errorf("the refresh shows %q", v.Card.Title)
		}
		refreshed <- err
	}()
	<-arrived[0]
	m.p.set(func(p *scripted) { p.detail = func(tracker.IssueID) (tracker.Detail, error) { return older, nil } })
	rounded := make(chan ticketsResult, 1)
	go func() { rounded <- m.round(t) }()
	<-arrived[1]
	m.p.set(func(p *scripted) { p.detail = st.detail })
	ends[0].Do(func() { close(release[0]) })
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	ends[1].Do(func() { close(release[1]) })
	<-rounded
	quiet(t, m.svc)
	if v := m.ticketView(t, subs[0].ID); v.Detail == nil || v.Detail.Card.Title != "Third" {
		t.Fatalf("after the late older read the open ticket shows %+v; want Third", v.Detail.Card)
	}
	if v, _, err := m.svc.MirroredDetail(m.conn.ID, issue); err != nil || v.Card.Title != "Third" {
		t.Fatalf("after the late older read the mirror's copy shows %q, %v; want Third", v.Card.Title, err)
	}
}

// statusHistory makes m's history reads say issue entered status s1 at
// began, complete through now.
func (m *mirrorFixture) statusHistory(issue tracker.IssueID, began time.Time) {
	m.p.set(func(p *scripted) {
		p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
			var h tracker.History
			for _, id := range q.Issues {
				ih := tracker.IssueHistory{Issue: id, CompleteTo: m.now()}
				if id == issue {
					ih.Status = []tracker.StatusChange{{Change: tracker.Change{At: began}, From: "s2", To: "s1"}}
				}
				h.Issues = append(h.Issues, ih)
			}
			return h, nil
		}
	})
}

// A status time a final history read stored is not erased by a document
// read inside the tracker's settle margin, whose history may lack the
// change: the ticket and the board show the final time.
func TestAFinalStatusTimeOutlivesADocumentReadInsideTheMargin(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	m.statusHistory("10001", statusBegan)
	must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	want := statusBegan.UnixMilli()
	// The document holds no status change and is not final (no HistoryTo).
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(seededCard()), nil }
	v, _ := m.openDetail(t, "10001")
	if v.Card.StatusSince != want {
		t.Fatalf("the ticket shows since %d; want the final %d", v.Card.StatusSince, want)
	}
	if c := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")).Card; c.StatusSince != want {
		t.Fatalf("the board shows since %d; want the final %d", c.StatusSince, want)
	}
}

// A ticket of an issue no board keeps, whose history is complete from its
// creation and final, holding no status change, shows its creation as
// the time in its status; one whose history starts after the creation
// (Linear's folded opening) shows none.
func TestAnUnkeptTicketWithNoStatusChangeShowsItsCreation(t *testing.T) {
	t.Parallel()
	for name, fromCreation := range map[string]bool{"complete from creation": true, "folded opening": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.seedBoard(t)
			c := card("10009", 1_799_000_000_000)
			m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
				d := detailOfCard(c)
				d.HistoryTo = c.Updated.Add(time.Minute)
				if !fromCreation {
					d.HistoryFrom = c.Created.Add(time.Second)
				}
				return d, nil
			}
			v, _ := m.openDetail(t, "10009")
			want := int64(0)
			if fromCreation {
				want = millis(c.Created)
			}
			if v.Card.StatusSince != want {
				t.Fatalf("the ticket shows since %d; want %d", v.Card.StatusSince, want)
			}
		})
	}
}

// A round's stored change of one ticket shows, and is told, whatever
// fails after its transaction: a cards read and the history read after it.
func TestACommittedTicketChangeOutlivesALaterFailure(t *testing.T) {
	t.Parallel()
	m, st, issues, subs := newRoundFixture(t, 2, false)
	m.settledTickets(t)
	before := m.issueEventCount()
	st.comment(issues[0], "Second")
	st.edit(issues[1], func(c *tracker.Card) { c.Title = "Edited" })
	failed := errors.New("test: the tracker failed")
	m.p.set(func(p *scripted) {
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) { return tracker.Cards{}, failed }
		p.history = func(tracker.HistoryQuery) (tracker.History, error) { return tracker.History{}, failed }
	})
	m.round(t)
	quiet(t, m.svc)
	v := m.ticketView(t, subs[0].ID)
	if v.Detail == nil || len(v.Detail.Comments) == 0 || v.Detail.Comments[0].Body != "Second" {
		t.Fatalf("the first ticket shows %+v; want its new comment", v.Detail)
	}
	if !slices.ContainsFunc(m.issueEventsSince(before), func(e IssueEvent) bool { return e.Issue == issues[0] && e.Changed }) {
		t.Fatalf("the first ticket's change was not told: %+v", m.issueEventsSince(before))
	}
}

// After a restore emptied the mirror, the first run that makes its new
// root keeps every live ticket again, one whose keep failed before it
// included, before any read of a ticket; and the next automatic round
// stores the bodies of the open tickets again. The refused ticket's own
// retry is an hour away, so the keeps counted are the restore's alone.
func TestARestoredMirrorStoresTheOpenTicketsBodiesAtTheNextRound(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	keeps := &keepsMade{Store: s.st, refuse: "10104"}
	s.svc.store = keeps
	s.openTicket(t, conn, "10105")
	s.openTicket(t, conn, "10104")
	keeps.since()
	snap := filepath.Join(t.TempDir(), "db.snapshot")
	if err := s.st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.RestoreFrom(snap); err != nil {
		t.Fatal(err)
	}
	// A board's read makes the new root; it reads no ticket.
	must[Outcome](t)(s.svc.tickDelta(t.Context(), scope, Background))
	if got := keeps.since(); !slices.Equal(got, []string{"10104", "10105"}) {
		t.Fatalf("the board's read after the restore kept the open tickets %v; want both live tickets", got)
	}
	rd := s.svc.roundOf(conn.ID)
	if rd == nil {
		t.Fatal("no ticket round runs")
	}
	if res := s.svc.roundTick(rd); res.failed {
		t.Fatalf("the round after the restore %+v", res)
	}
	settled(t, s.svc)
	for _, issue := range []tracker.IssueID{"10105", "10104"} {
		if card, doc := s.kept(t, conn, issue); !card || !doc {
			t.Fatalf("after the restore and a round the mirror keeps %s's card %v and body %v", issue, card, doc)
		}
	}
	if n := s.openTicketRows(t); n != 2 {
		t.Fatalf("%d open-ticket rows after the restore; want the two live tickets'", n)
	}
}

// A ticket whose first read found no card of its issue in the mirror has
// its body stored by a later automatic round.
func TestATicketWithoutACardAtItsOpeningIsStoredByALaterRound(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	m.seedBoard(t)
	c := card("10009", 1_799_000_000_000)
	var answer sync.Mutex
	cardsKnown := false
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			answer.Lock()
			defer answer.Unlock()
			if !cardsKnown {
				return tracker.Cards{}, nil
			}
			return cardsAnswering(&c)(asked)
		}
	})
	must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10009"))
	m.settledTickets(t)
	answer.Lock()
	cardsKnown = true
	answer.Unlock()
	m.round(t)
	if _, ok, err := m.st.GetMirrorBody(m.root(), "10009"); err != nil || !ok {
		t.Fatalf("after a round the mirror holds the body: %v, %v", ok, err)
	}
}

// A ticket whose keep fails shows a failure that says it tries again, and
// its next read once the store accepts it stores its body.
func TestAFailedKeepIsARetryableFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	m.seedBoard(t)
	c := card("10009", 1_799_000_000_000)
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	})
	m.refuseKeeps()
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10009"))
	m.settledTickets(t)
	if v := m.ticketView(t, sub.ID); v.Failure == nil || v.Failure.Kind != string(FailureInternal) {
		t.Fatalf("a ticket whose keep failed shows %+v; want the failure that it tries again", v)
	}
	m.svc.store = m.st
	if err := m.svc.RefreshSubscription(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if v := m.ticketView(t, sub.ID); v.Failure != nil || !v.Loaded {
		t.Fatalf("after the store accepted the keep the ticket shows %+v", v)
	}
	if _, ok, err := m.st.GetMirrorBody(m.root(), "10009"); err != nil || !ok {
		t.Fatalf("after the store accepted the keep the mirror holds the body: %v, %v", ok, err)
	}
}

// Closing the ticket of a parent no board keeps ends the mirror's keep of
// it, and the open child's ticket, which showed the parent's key from the
// mirror's card, is told its view changed and shows it without the key.
func TestClosingAParentsTicketRebuildsItsOpenChild(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.syncBoard(t, scope)
	child := s.openTicket(t, conn, "10101")
	parent := s.openTicket(t, conn, "10104")
	quiet(t, s.svc)
	if v := must[TicketView](t)(s.svc.TicketDetail(child.ID)); v.Detail == nil || v.Detail.Card.Parent == nil || v.Detail.Card.Parent.Key != "WEB-4" {
		t.Fatalf("the child shows its parent %+v", v.Detail)
	}
	_, told, _ := s.events.snapshot()
	s.closeTicket(t, conn, parent, "10104")
	quiet(t, s.svc)
	s.events.waitFor(t, "the child's change", func(_ []BoardEvent, i []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(i[len(told):], func(e IssueEvent) bool { return e.Issue == "10101" && e.Changed })
	})
	if v := must[TicketView](t)(s.svc.TicketDetail(child.ID)); v.Detail == nil || v.Detail.Card.Parent == nil || v.Detail.Card.Parent.Key != "" {
		t.Fatalf("after the parent's ticket closed the child shows its parent %+v; want no key the mirror no longer keeps", v.Detail.Card.Parent)
	}
}

// A shown ticket's issue is read first by the running round's history
// read, however many of the connection's issues are due before it.
func TestAShownTicketsHistoryIsReadFirst(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	var rows []tracker.Row
	for i := range 260 {
		rows = append(rows, row(fmt.Sprint(10001+i), 1_799_000_000_000, fmt.Sprintf("a%03d", i)))
	}
	m.tracks(rows)
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	last := rows[len(rows)-1].Card
	var mu sync.Mutex
	var asked [][]tracker.IssueID
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(last), nil }
		p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
			mu.Lock()
			asked = append(asked, slices.Clone(q.Issues))
			mu.Unlock()
			return tracker.History{}, nil
		}
	})
	must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, last.ID))
	m.settledTickets(t)
	if m.recordOf(t, string(last.ID)).StatusFinal {
		t.Fatal("the shown ticket's status time is final; the round would read no history")
	}
	mu.Lock()
	asked = nil
	mu.Unlock()
	m.readSoon(last.ID)
	quiet(t, m.svc)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 {
		t.Fatal("the round read no history")
	}
	if !slices.Contains(asked[0], last.ID) {
		t.Fatalf("the first history read names %d issues without the shown ticket's %s", len(asked[0]), last.ID)
	}
	// The shown ticket's read shares the board's batches: no read of its
	// own, the first full, and no more reads than a history tick makes.
	if len(asked[0]) != historyBatch || len(asked) > historyBatches {
		t.Fatalf("the round read history in %d reads, the first of %d issues; want the shown ticket in a full batch of %d and at most %d reads",
			len(asked), len(asked[0]), historyBatch, historyBatches)
	}
}

// A ticket read's final status evidence decides the record's status time
// and leaves the issue's history due: the board's sprint, estimate and
// assignee history needs the history read, which the evidence does not
// discharge.
func TestStatusEvidenceLeavesTheBoardsHistoryDue(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	m.statusHistory("10001", statusBegan)
	must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	if slices.Contains(m.due(), "10001") {
		t.Fatal("the issue is due a history read after its history read")
	}
	c := seededCard()
	c.Updated = c.Updated.Add(time.Hour)
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{{Card: c, Order: "a"}}, Next: cursorAfter("10001")}, nil
		}
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			d := detailOfCard(c)
			d.StatusChanged = tracker.StatusChange{Change: tracker.Change{At: statusBegan}, From: "s2", To: c.Status}
			d.HistoryTo = c.Updated.Add(time.Minute)
			return d, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.openDetail(t, "10001")
	if r := m.recordOf(t, "10001"); !r.StatusFinal || r.StatusSince != statusBegan.UnixMilli() {
		t.Fatalf("the record after a final ticket read holds since %d final %v; want %d final", r.StatusSince, r.StatusFinal, statusBegan.UnixMilli())
	}
	if !slices.Contains(m.due(), "10001") {
		t.Fatalf("the issue's history is not due after the status evidence: %v", m.due())
	}
}

// A shown ticket whose read decided, finally, that its status time is
// unknown (Linear's folded opening) costs no further read: the rounds
// after it read no history for it.
func TestAFinalUnknownStatusTimeCostsNoFurtherRead(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	m.seedBoard(t)
	c := seededCard()
	var mu sync.Mutex
	reads := 0
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			d := detailOfCard(c)
			d.HistoryFrom, d.HistoryTo = c.Created.Add(time.Second), c.Updated.Add(time.Minute)
			return d, nil
		}
		p.history = func(tracker.HistoryQuery) (tracker.History, error) {
			mu.Lock()
			reads++
			mu.Unlock()
			return tracker.History{}, nil
		}
	})
	must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, c.ID))
	m.settledTickets(t)
	quiet(t, m.svc)
	if r := m.recordOf(t, "10001"); !r.StatusFinal || r.StatusSince != 0 {
		t.Fatalf("the record holds since %d final %v; want a final unknown", r.StatusSince, r.StatusFinal)
	}
	for range 2 {
		m.readSoon(c.ID)
		quiet(t, m.svc)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 0 {
		t.Fatalf("the rounds of a ticket whose status time is a final unknown read history %d times; want none", reads)
	}
}

// statusRead makes m's detail reads answer c, a version of issue 10001,
// whose history is not final through its update (its read was inside the
// tracker's settle margin) and whose latest status change entered c's
// status at latest.
func (m *mirrorFixture) statusRead(c tracker.Card, latest time.Time) {
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			d := detailOfCard(c)
			d.StatusChanged = tracker.StatusChange{Change: tracker.Change{At: latest}, From: "s2", To: c.Status}
			d.HistoryTo = c.Updated.Add(-10 * time.Second)
			return d, nil
		}
	})
}

// recordOf is the issue record's card of issue: its status time and
// whether a final read decided it for its update.
func (m *mirrorFixture) recordOf(t *testing.T, issue string) store.MirrorCard {
	t.Helper()
	cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{issue}))
	if len(cards) != 1 {
		t.Fatalf("the mirror keeps no card of %s", issue)
	}
	return cards[0]
}

// A read inside the tracker's settle margin may lack the newest status
// change, so its status time is provisional. The issue was in s1 since
// Monday, a final history read decided; it left s1 and came back 10
// seconds before a ticket read whose history shows neither move (the
// Monday case): the ticket and the board show the time unknown, not
// Monday; a read of the same version once its history is final shows the
// return it holds and is told as a status time moved. A return the read's
// history shows, within the margin of the update, shows at once; the history read after the margin makes it
// final; and a provisional read of the same version never replaces the
// final time.
func TestAStatusTimeInsideTheSettleMargin(t *testing.T) {
	t.Parallel()
	monday := statusBegan
	setup := func(t *testing.T) (*mirrorFixture, tracker.Card) {
		t.Helper()
		m := newMirrorFixture(t)
		m.p.set(func(p *scripted) { p.caps.HistorySettle = time.Minute })
		m.seedBoard(t)
		m.statusHistory("10001", monday)
		must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
		if r := m.recordOf(t, "10001"); r.StatusSince != monday.UnixMilli() || !r.StatusFinal {
			t.Fatalf("after the history read the record holds since %d final %v; want Monday's, final", r.StatusSince, r.StatusFinal)
		}
		c := seededCard()
		c.Updated = c.Updated.Add(time.Hour)
		return m, c
	}
	t.Run("the Monday case", func(t *testing.T) {
		t.Parallel()
		m, c := setup(t)
		m.statusRead(c, monday)
		v, _ := m.openDetail(t, "10001")
		if v.Card.StatusSince != 0 {
			t.Fatalf("the ticket shows since %d; want unknown, not Monday's %d", v.Card.StatusSince, monday.UnixMilli())
		}
		if b := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")).Card; b.StatusSince != 0 {
			t.Fatalf("the board shows since %d; want unknown", b.StatusSince)
		}
		// A read of the same version once its history is final shows the
		// return it holds, and reports the status time moved with no card
		// change, so every view of the card is told.
		back := c.Updated.Add(-30 * time.Minute)
		m.p.set(func(p *scripted) {
			p.detail = func(tracker.IssueID) (tracker.Detail, error) {
				d := detailOfCard(c)
				d.StatusChanged = tracker.StatusChange{Change: tracker.Change{At: back}, From: "s2", To: c.Status}
				d.HistoryTo = c.Updated
				return d, nil
			}
		})
		v, ch := m.openDetail(t, "10001")
		if v.Card.StatusSince != back.UnixMilli() || len(ch.Cards) != 0 || !slices.Equal(ch.History, ids("10001")) {
			t.Fatalf("the settled read shows since %d, changes %+v; want the return %d, told as a status time moved", v.Card.StatusSince, ch, back.UnixMilli())
		}
	})
	t.Run("a return within the margin, then the settle re-read", func(t *testing.T) {
		t.Parallel()
		m, c := setup(t)
		back := c.Updated.Add(-10 * time.Second)
		m.statusRead(c, back)
		v, _ := m.openDetail(t, "10001")
		if v.Card.StatusSince != back.UnixMilli() {
			t.Fatalf("the ticket shows since %d; want the return %d at once", v.Card.StatusSince, back.UnixMilli())
		}
		if r := m.recordOf(t, "10001"); r.StatusSince != back.UnixMilli() || r.StatusFinal {
			t.Fatalf("the record holds since %d final %v; want the return, provisional", r.StatusSince, r.StatusFinal)
		}
		// The history read after the margin, final through the update.
		m.statusHistory("10001", back)
		must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
		if r := m.recordOf(t, "10001"); r.StatusSince != back.UnixMilli() || !r.StatusFinal {
			t.Fatalf("after the settle re-read the record holds since %d final %v; want the return, final", r.StatusSince, r.StatusFinal)
		}
		// A provisional read of the same version, whose history shows only
		// Monday's entry.
		m.statusRead(c, monday)
		if v, _ := m.openDetail(t, "10001"); v.Card.StatusSince != back.UnixMilli() {
			t.Fatalf("after a provisional read of the final version the ticket shows since %d; want the final %d", v.Card.StatusSince, back.UnixMilli())
		}
		if r := m.recordOf(t, "10001"); r.StatusSince != back.UnixMilli() || !r.StatusFinal {
			t.Fatalf("after a provisional read of the final version the record holds since %d final %v", r.StatusSince, r.StatusFinal)
		}
	})
}

// An issue in s1 leaves it and comes back between two card reads. The
// second card read finds the same status at a newer update time: the
// board keeps the time it showed, but no final read decided it for the
// new version, so the issue is due a history read, and that read ends the
// shown time at the return to s1.
func TestAStatusLeftAndReturnedToBetweenCardReads(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	m.statusHistory("10001", statusBegan)
	must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	c := seededCard()
	left, back := c.Updated.Add(20*time.Minute), c.Updated.Add(40*time.Minute)
	c.Updated = back
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{{Card: c, Order: "a"}}, Next: cursorAfter("10001")}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if r := m.recordOf(t, "10001"); r.StatusSince != statusBegan.UnixMilli() || r.StatusFinal {
		t.Fatalf("after the second card read the record holds since %d final %v; want the time it showed, not final", r.StatusSince, r.StatusFinal)
	}
	due, _ := must2[[]store.MirrorHistoryDue, bool](t)(m.st.ListMirrorHistoryDue(m.root(), m.svc.steady().UnixMilli()))
	if !slices.ContainsFunc(due, func(d store.MirrorHistoryDue) bool { return d.IssueID == "10001" }) {
		t.Fatalf("the issue is not due a history read: %+v", due)
	}
	m.p.set(func(p *scripted) {
		p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
			var h tracker.History
			for _, id := range q.Issues {
				ih := tracker.IssueHistory{Issue: id, CompleteTo: m.now()}
				if id == "10001" {
					ih.Status = []tracker.StatusChange{
						{Change: tracker.Change{At: statusBegan}, From: "s2", To: "s1"},
						{Change: tracker.Change{At: left}, From: "s1", To: "s2"},
						{Change: tracker.Change{At: back}, From: "s2", To: "s1"},
					}
				}
				h.Issues = append(h.Issues, ih)
			}
			return h, nil
		}
	})
	must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	if b := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")).Card; b.StatusSince != back.UnixMilli() {
		t.Fatalf("the board shows since %d; want the return %d", b.StatusSince, back.UnixMilli())
	}
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	if v, _ := m.openDetail(t, "10001"); v.Card.StatusSince != back.UnixMilli() {
		t.Fatalf("the ticket shows since %d; want the return %d", v.Card.StatusSince, back.UnixMilli())
	}
}

// A ticket's read sends the shared catalog only what its card names, its
// status and assignee: the body keeps the rest of the catalog it read, and
// the board's names of other users and statuses stay as the reads that
// named them stored them. The ticket names what the shared catalog holds
// as the board does (the move's status) and from its own catalog only what
// the shared one lacks (the comment's author).
func TestATicketsReadNamesOnlyItsCardsUsersAndStatusInTheSharedCatalog(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	before := must[store.MirrorCatalog](t)(m.st.GetMirrorCatalog(m.root(), []string{"acc-user2"}, []string{"s3"}))
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
		d := detailOfCard(seededCard())
		renamed1, renamed2 := user1, user2
		renamed1.Name, renamed2.Name = "User 1 renamed", "User 2 renamed"
		d.Catalog = tracker.Catalog{Users: []tracker.User{renamed1, renamed2},
			Statuses: []tracker.Status{{ID: "s1", Name: "Doing", Group: tracker.GroupInProgress}, {ID: "s3", Name: "Shipped", Group: tracker.GroupDone}}}
		return d, nil
	}
	v, _ := m.openDetail(t, "10001")
	if v.Card.Assignee.Name != "User 1 renamed" || v.Comments[0].Author.Name != "User 2 renamed" || v.Moves[0].To.Name != "Done" {
		t.Fatalf("the ticket shows assignee %q, comment author %q, move to %q; want the shared names, its read's for the author the board lacks",
			v.Card.Assignee.Name, v.Comments[0].Author.Name, v.Moves[0].To.Name)
	}
	shared := must[store.MirrorCatalog](t)(m.st.GetMirrorCatalog(m.root(), []string{"acc-user1", "acc-user2"}, []string{"s1", "s3"}))
	names := map[string]string{}
	for _, u := range shared.Users {
		names[u.ID] = u.Name
	}
	for _, st := range shared.Statuses {
		names[st.ID] = st.Name
	}
	if names["acc-user1"] != "User 1 renamed" || names["s1"] != "Doing" {
		t.Fatalf("the shared catalog holds %v; want the card's assignee and status renamed", names)
	}
	was := map[string]string{}
	for _, u := range before.Users {
		was[u.ID] = u.Name
	}
	for _, st := range before.Statuses {
		was[st.ID] = st.Name
	}
	if names["acc-user2"] != was["acc-user2"] || names["s3"] != was["s3"] {
		t.Fatalf("the shared catalog names the comment's author %q and the move's status %q; want them as they were, %v", names["acc-user2"], names["s3"], was)
	}
}

// A Jira round's check of an open ticket whose cards read began before a
// board read stored a newer version (the tracker answered it the version
// the ticket shows, landing after the board's): the board's write tells
// the ticket its card changed after the check began, so the ticket stays
// due at once (readNowLocked, dueLocked) and the next read, finding the
// record newer than the ticket's body (mirrorMoved), reads it in full and
// shows that version's body with nothing behind.
func TestAJiraCheckRacingABoardWriteReadsTheTicketInFull(t *testing.T) {
	t.Parallel()
	m, st, issues, subs := newRoundFixture(t, 1, true)
	quiet(t, m.svc)
	shown := st.cards[issues[0]]
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	m.p.set(func(p *scripted) {
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			// The answer is the version the ticket shows, held at the tracker.
			out, err := cardsAnswering(&shown)(asked)
			close(arrived)
			<-release
			return out, err
		}
	})
	details, cards := m.p.count("Details"), m.p.count("Cards")
	m.readSoon(issues[0])
	waitClosed(t, "the check's cards read", arrived)
	st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
	st.comment(issues[0], "Second")
	m.tracks([]tracker.Row{{Card: st.cards[issues[0]], Order: "a000"}})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) { p.cards = st.cardsRead })
	once.Do(func() { close(release) })
	quiet(t, m.svc)
	v := m.ticketView(t, subs[0].ID)
	if v.Detail == nil || v.Detail.Card.Title != "Renamed" || v.Detail.BodyBehind || len(v.Detail.Comments) == 0 || v.Detail.Comments[0].Body != "Second" {
		t.Fatalf("after the check raced the board write the ticket shows %+v", v.Detail)
	}
	if d, c := m.p.count("Details")-details, m.p.count("Cards")-cards; d != 1 || c != 1 {
		t.Fatalf("the rounds made %d full reads and %d cards reads; want 1 of each: the check, then the read the board's write asked", d, c)
	}
}

// A user renamed with no update of the ticket's issue (a board read of
// another issue names the new name) shows renamed on the open ticket as on
// the board: the ticket's held body named the old name, and the shared
// catalog's name comes first.
func TestARenameWithNoIssueUpdateShowsOnTheOpenTicket(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	m.seedBoard(t)
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			d := detailOfCard(seededCard())
			d.Users = []tracker.User{user1, user2}
			return d, nil
		}
	})
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001"))
	m.settledTickets(t)
	quiet(t, m.svc)
	if v := m.ticketView(t, sub.ID); v.Detail == nil || v.Detail.Card.Assignee == nil || v.Detail.Card.Assignee.Name != "User 1" {
		t.Fatalf("the ticket shows %+v; want its assignee User 1", v.Detail)
	}
	renamed := user1
	renamed.Name = "User 1 renamed"
	other := row("10002", 1_799_000_100_000, "b")
	m.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{other}, Next: q.From, Catalog: tracker.Catalog{Users: []tracker.User{renamed}}}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	quiet(t, m.svc)
	if b := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")).Card; b.Assignee == nil || b.Assignee.Name != "User 1 renamed" {
		t.Fatalf("the board shows assignee %+v; want the new name", b.Assignee)
	}
	v := m.ticketView(t, sub.ID)
	if v.Detail == nil || v.Detail.Card.Assignee == nil || v.Detail.Card.Assignee.Name != "User 1 renamed" {
		t.Fatalf("the open ticket shows assignee %+v; want the name the board shows", v.Detail)
	}
	if v.Detail.History[0].Author == nil || v.Detail.History[0].Author.Name != "User 1 renamed" {
		t.Fatalf("the open ticket's history names its author %+v; want the new name", v.Detail.History[0].Author)
	}
}

// A board read that reparents the ticket's issue after the ticket's body
// read (the body names the old parent, the record's newer card the new
// one): the ticket's view shows the record's card with its new parent's
// key and title, and the issues it shows, which its pump follows, are the
// rendered card's: the new parent, not the body's old one.
func TestAReparentAfterTheBodyReadShowsAndFollowsTheNewParent(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	m.seedBoard(t)
	read := seededCard()
	moved := read
	moved.Parent, moved.Updated = "10003", read.Updated.Add(time.Minute)
	m.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{{Card: moved, Order: "a"}}, Next: q.From, Catalog: tracker.Catalog{Users: []tracker.User{user1}}}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	row := must[store.TrackerConnection](t)(m.svc.viewRow(m.conn.ID))
	views, shows, err := m.svc.detailViews(m.root(), row, []ticketDoc{{d: detailOfCard(read), version: read.Updated.UnixMilli()}})
	if err != nil {
		t.Fatal(err)
	}
	if p := views[0].Card.Parent; p == nil || p.ID != "10003" || p.Key != "WEB-3" || p.Title != "Issue 10003" {
		t.Fatalf("the ticket shows its parent %+v; want the record's, WEB-3 with its title", p)
	}
	if !views[0].BodyBehind {
		t.Fatal("the ticket's body is of an older version than its card and is not marked behind")
	}
	if !slices.Contains(shows[0], "10003") || slices.Contains(shows[0], "10004") {
		t.Fatalf("the ticket follows %v; want the new parent 10003 and not the old 10004", shows[0])
	}
}

package trackerapp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/tracker"
)

// ticket is issue of the fixture's connection as a link names it.
func (m *mirrorFixture) ticket(issue string) store.TicketRef {
	row := m.row(m.conn.ID)
	return store.TicketRef{Kind: row.Kind, Site: row.Site, IssueID: issue}
}

// rowLink is threadID's link as its thread row shows it, nil for none.
func (m *mirrorFixture) rowLink(threadID string) *store.ThreadTicketLink {
	m.t.Helper()
	row, err := m.st.GetThread(threadID)
	if err != nil {
		m.t.Fatal(err)
	}
	return row.TicketLink
}

// refuseCards makes every card read of the tracker fail the test.
func (m *mirrorFixture) refuseCards(t *testing.T) {
	m.p.set(func(p *scripted) {
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			t.Errorf("read the cards of %v from the tracker", asked)
			return tracker.Cards{}, errors.New("no read expected")
		}
	})
}

// A card a tick stores tells the threads whose link it moved, with no
// subscription open; the same card again tells nothing; a read that does
// not return the issue tells them again, and their rows show it not found.
func TestCardWritesTellTheLinksTheyMove(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.linkThread("thread-1", "10005")
	m.linkThread("thread-2", "10006")
	if link := m.rowLink("thread-1"); link == nil || link.State != store.TicketLinkCurrent || link.Title != "Issue 10005" {
		t.Fatalf("the linked row = %+v", link)
	}
	_, before := m.events.linkedThreads()
	renamed := card("10005", 1_799_000_000_000)
	renamed.Title = "Renamed"
	m.p.set(func(p *scripted) {
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) {
			return tracker.Cards{Cards: []tracker.Card{renamed, card("10006", 1_700_000_000_000)}}, nil
		}
	})
	must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	if got, calls := m.events.linkedThreads(); !slices.Equal(got, []string{"thread-1"}) || calls != before+1 {
		t.Fatalf("a renamed card told %v in %d calls; want thread-1 in one", got, calls-before)
	}
	if link := m.rowLink("thread-1"); link.Title != "Renamed" {
		t.Fatalf("the renamed row = %+v", link)
	}

	must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	if _, calls := m.events.linkedThreads(); calls != before+1 {
		t.Fatalf("the same cards again told %d more calls; want none", calls-before-1)
	}

	m.p.set(func(p *scripted) {
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) {
			return tracker.Cards{Cards: []tracker.Card{renamed}, Missing: ids("10006")}, nil
		}
	})
	m.pass(time.Second)
	must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	if got, calls := m.events.linkedThreads(); !slices.Equal(got, []string{"thread-1", "thread-2"}) || calls != before+2 {
		t.Fatalf("a missing issue told %v in %d calls", got, calls-before)
	}
	if link := m.rowLink("thread-2"); link.State != store.TicketLinkNotFound {
		t.Fatalf("the missing issue's row = %+v; want not found", link)
	}
}

// A link from a ticket the mirror holds makes no request; one it does not
// hold reads the card once and stores it with the link; a ticket the
// tracker does not return, and a site with no connection on, are refused
// with the link as it was. Each link reports its own change, which its
// caller announces, and tells no thread whose link it did not move.
func TestLinkThreadFromTheMirrorOrOneRead(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.linkThread("held", "10005")
	m.thread("a")
	m.refuseCards(t)
	_, before := m.events.linkedThreads()
	if changed, err := m.svc.LinkThread(t.Context(), "a", m.ticket("10005"), store.TicketOriginManual); err != nil || !changed {
		t.Fatalf("a link from the mirror: %v, %v", changed, err)
	}
	if link := m.rowLink("a"); link == nil || link.State != store.TicketLinkCurrent || link.Key != "WEB-5" || link.Title != "Issue 10005" {
		t.Fatalf("a link from the mirror = %+v", link)
	}
	if changed, err := m.svc.LinkThread(t.Context(), "a", m.ticket("10005"), store.TicketOriginManual); err != nil || changed {
		t.Fatalf("the same link again: %v, %v; want no change", changed, err)
	}
	if _, calls := m.events.linkedThreads(); calls != before {
		t.Fatalf("links from the mirror told %d calls; want none", calls-before)
	}

	var asked [][]tracker.IssueID
	m.p.set(func(p *scripted) {
		p.cards = func(q []tracker.IssueID) (tracker.Cards, error) {
			asked = append(asked, q)
			if q[0] == "10009" {
				return tracker.Cards{Missing: q}, nil
			}
			c := card(string(q[0]), 1_799_000_000_000)
			c.Title = "Read from the tracker"
			return tracker.Cards{Cards: []tracker.Card{c}}, nil
		}
	})
	if changed, err := m.svc.LinkThread(t.Context(), "a", m.ticket("10007"), store.TicketOriginStartWork); err != nil || !changed {
		t.Fatalf("a link from a read: %v, %v", changed, err)
	}
	if !slices.EqualFunc(asked, [][]tracker.IssueID{ids("10007")}, slices.Equal) {
		t.Fatalf("card reads = %v; want one of 10007", asked)
	}
	link := m.rowLink("a")
	if link == nil || link.IssueID != "10007" || link.Title != "Read from the tracker" || link.State != store.TicketLinkCurrent || link.Origin != store.TicketOriginStartWork {
		t.Fatalf("a link from a read = %+v", link)
	}
	if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{"10007"})); len(cards) != 1 {
		t.Fatalf("the read's card is not in the mirror: %+v", cards)
	}

	var refusal *LinkRefusal
	if _, err := m.svc.LinkThread(t.Context(), "a", m.ticket("10009"), store.TicketOriginManual); !errors.As(err, &refusal) || refusal.Code != LinkNotFound {
		t.Fatalf("a link to a ticket the tracker did not return: %v", err)
	}
	other := m.ticket("10001")
	other.Site = "https://other.example.com"
	if _, err := m.svc.LinkThread(t.Context(), "a", other, store.TicketOriginManual); !errors.As(err, &refusal) || refusal.Code != LinkNoConnection {
		t.Fatalf("a link to a site with no connection: %v", err)
	}
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.LinkThread(t.Context(), "a", m.ticket("10005"), store.TicketOriginManual); !errors.As(err, &refusal) || refusal.Code != LinkConnectionOff {
		t.Fatalf("a link through a connection turned off: %v", err)
	}
	if got := m.rowLink("a"); got == nil || got.IssueID != "10007" || got.Title != "Read from the tracker" {
		t.Fatalf("the link after the refusals = %+v; want it as it was", got)
	}
	if _, err := m.svc.LinkThread(t.Context(), "missing", m.ticket("10005"), store.TicketOriginManual); err == nil {
		t.Fatal("a link of a missing thread was made")
	}
}

// An unlink, and a link replaced, tell the boards that show the issue the
// link was the last to keep as a parent of their rows.
func TestEndedLinksTellTheBoardsOfTheirChildren(t *testing.T) {
	t.Parallel()
	for _, end := range []struct {
		name string
		run  func(m *mirrorFixture) error
	}{
		{"unlink", func(m *mirrorFixture) error {
			had, err := m.svc.UnlinkThread("thread-1")
			if err == nil && !had {
				err = errors.New("the unlink found no link")
			}
			return err
		}},
		{"replace", func(m *mirrorFixture) error {
			changed, err := m.svc.LinkThread(m.t.Context(), "thread-1", m.ticket("10002"), store.TicketOriginManual)
			if err == nil && !changed {
				err = errors.New("the replace changed nothing")
			}
			return err
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
			child := row("10002", 1_799_000_000_000, "a")
			child.Card.Parent = "10009"
			m.tracks([]tracker.Row{child})
			m.linkThread("thread-1", "10009")
			m.subscribe(t, testBoard)
			idleBoards(t, m.svc, m.p)
			before, _, _ := m.events.snapshot()
			if err := end.run(m); err != nil {
				t.Fatal(err)
			}
			if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{"10009"})); len(cards) != 0 {
				t.Fatalf("the parent is still kept: %+v", cards)
			}
			m.events.waitFor(t, "the child's row", func(b []BoardEvent, _ []IssueEvent, _ []error) bool {
				return slices.ContainsFunc(b[len(before):], func(e BoardEvent) bool { return slices.Contains(e.Changed, "10002") })
			})
		})
	}
}

// A link another computer's thread carried, which names no mirror here,
// takes the card the mirror holds and tells its thread; one whose ticket
// the mirror does not hold stays not checked.
func TestAdoptLinksTakesTheMirrorsCard(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.linkThread("held", "10005")
	m.linkThread("unheld", "10006")
	for _, c := range []struct{ thread, issue string }{{"carried", "10005"}, {"carried-unheld", "10008"}} {
		receiveLinkedThread(t, m.st, c.thread, m.ticket(c.issue))
	}
	if link := m.rowLink("carried"); link == nil || link.State != store.TicketLinkNotChecked {
		t.Fatalf("a carried link = %+v; want not checked", link)
	}
	_, before := m.events.linkedThreads()
	if err := m.svc.AdoptLinks([]string{"carried", "carried-unheld", "held"}); err != nil {
		t.Fatal(err)
	}
	if link := m.rowLink("carried"); link.State != store.TicketLinkCurrent || link.Title != "Issue 10005" || link.Origin != store.TicketOriginStartWork {
		t.Fatalf("the adopted link = %+v", link)
	}
	if link := m.rowLink("carried-unheld"); link.State != store.TicketLinkNotChecked {
		t.Fatalf("a link the mirror does not hold = %+v; want not checked", link)
	}
	if got, calls := m.events.linkedThreads(); !slices.Contains(got, "carried") || slices.Contains(got, "held") || calls != before+1 {
		t.Fatalf("told %v in %d calls; want carried in one", got, calls-before)
	}
}

// receiveLinkedThread commits an incoming copy of thread threadID whose
// manifest carries a link to ticket, origin start_work, as another
// computer's thread row shows it: its copy names no mirror of this one.
func receiveLinkedThread(t *testing.T, st *store.Store, threadID string, ticket store.TicketRef) {
	t.Helper()
	secret := bytes.Repeat([]byte{0xa5}, 32)
	hash := sha256.Sum256(secret)
	digest := strings.Repeat("b", 64)
	transfer := store.ThreadTransfer{ID: entityid.New(), ThreadID: threadID, PeerBackendID: entityid.New(), Kind: "copy", Direction: "incoming",
		ActivationHash: hex.EncodeToString(hash[:]), PrivateState: json.RawMessage(`{"grant":"test-grant"}`)}
	if _, err := st.CreateThreadTransfer(transfer); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceThreadTransfer(transfer.ID, "prepared", digest); err != nil {
		t.Fatal(err)
	}
	target := store.Thread{ID: threadID, ProjectID: "project-1", Title: "Carried", Provider: "claude", Model: "test-model",
		WorkspacePath: "/tmp/web", CreatedAt: 1, UpdatedAt: 1,
		TicketLink: &store.ThreadTicketLink{Kind: ticket.Kind, Site: ticket.Site, IssueID: ticket.IssueID, Key: "WEB-x",
			Title: "As the sender saw it", Origin: store.TicketOriginStartWork, LinkedAt: 5,
			State: store.TicketLinkCurrent, ConnectionID: "sender-connection"}}
	source := storetest.Clone(t)
	if _, err := source.CreateProject(store.Project{ID: "project-1", Path: "/tmp/web", Name: "web", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := source.CreateThread(target); err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	if err := source.ExportThreadHistory(t.Context(), threadID, &history); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitIncomingThreadTransfer(t.Context(), transfer.ID, digest, secret, target, &history); err != nil {
		t.Fatal(err)
	}
}

// A connection added, turned off, turned on and removed tells the threads
// linked to its site, whose rows derive their state from its connections.
func TestConnectionChangesTellTheirSitesLinks(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.linkThread("thread-1", "10005")
	told := func(what, state string) {
		t.Helper()
		_, before := m.events.linkedThreads()
		m.events.mu.Lock()
		last := slices.Clone(m.events.links[before-1])
		m.events.mu.Unlock()
		if !slices.Equal(last, []string{"thread-1"}) {
			t.Fatalf("%s told %v; want thread-1", what, last)
		}
		if link := m.rowLink("thread-1"); link == nil || link.State != state {
			t.Fatalf("%s: the row = %+v; want %s", what, link, state)
		}
	}
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	told("a turn off", store.TicketLinkTurnedOff)
	if _, err := m.svc.SetEnabled(m.conn.ID, true); err != nil {
		t.Fatal(err)
	}
	told("a turn on", store.TicketLinkNotChecked)
	if err := m.svc.Remove(m.conn.ID); err != nil {
		t.Fatal(err)
	}
	told("a removal", store.TicketLinkNoConnection)
	m.addJira()
	told("an add", store.TicketLinkNotChecked)
	_, calls := m.events.linkedThreads()
	m.addLinear()
	if _, after := m.events.linkedThreads(); after != calls {
		t.Fatal("a connection to another site told its links")
	}
}

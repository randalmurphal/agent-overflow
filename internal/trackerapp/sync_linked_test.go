package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// linkThread makes a thread this computer owns and links it to issue on
// the fixture's connection.
func (m *mirrorFixture) linkThread(threadID string, issue tracker.IssueID) {
	m.t.Helper()
	m.thread(threadID)
	linkOldCard(m.t, m.st, m.conn.ID, threadID, issue)
}

// thread makes thread threadID in the fixture's project, with no link.
func (m *mirrorFixture) thread(threadID string) {
	m.t.Helper()
	project := store.Project{ID: "project-1", Path: "/tmp/web", Name: "web", CreatedAt: 1, UpdatedAt: 1}
	if _, err := m.st.GetProject(project.ID); err != nil {
		if _, err := m.st.CreateProject(project); err != nil {
			m.t.Fatal(err)
		}
	}
	if err := m.st.CreateThread(store.Thread{
		ID: threadID, ProjectID: project.ID, Title: "Thread", Provider: "claude", Model: "test-model",
		WorkspacePath: project.Path, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		m.t.Fatal(err)
	}
}

// linkOldCard links threadID to issue of connection id as a link made
// before the test: from the card the mirror holds of it, as LinkThread
// links, or else from a read of the issue's card older than every card a
// test's tracker answers with, which the mirror holds from then on.
func linkOldCard(tb testing.TB, st *store.Store, id, threadID string, issue tracker.IssueID) {
	tb.Helper()
	root, _, err := st.GetOrCreateMirrorRoot(id)
	if err != nil {
		tb.Fatal(err)
	}
	held, err := st.ListMirrorCards(root, []string{string(issue)})
	if err != nil {
		tb.Fatal(err)
	}
	if len(held) == 1 {
		c, ok := cardOf(held[0])
		if !ok {
			tb.Fatalf("the mirror's card of %s has no blob this version reads", issue)
		}
		w := store.ThreadTicketWrite{ThreadID: threadID, Origin: store.TicketOriginManual, LinkedAt: 1, Root: root, Card: held[0]}
		w.Card.Title = c.Title
		if _, err := st.SetThreadTicket(w); err != nil {
			tb.Fatal(err)
		}
		return
	}
	sync, _, err := st.GetMirrorSync(root, "", 1)
	if err != nil {
		tb.Fatal(err)
	}
	read, err := st.BeginMirrorRead(root, 1)
	if err != nil {
		tb.Fatal(err)
	}
	old, err := mirrorCard(card(string(issue), 1_700_000_000_000))
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := st.SetThreadTicket(store.ThreadTicketWrite{ThreadID: threadID, Origin: store.TicketOriginManual, LinkedAt: 1,
		Root: root, Card: old, Read: read, Sweep: sync.Sweep}); err != nil {
		tb.Fatal(err)
	}
}

// The cards unit reads the cards of the linked issues, those with a row
// on a board no pump polls included, and records the read in the linked
// scope's sync state; a failure is recorded and settled like a board's.
func TestLinkedCardsReadsTheLinkedIssues(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.linkThread("thread-1", "10005")
	m.linkThread("thread-2", "10001")
	var asked [][]tracker.IssueID
	m.p.cards = func(q []tracker.IssueID) (tracker.Cards, error) {
		asked = append(asked, q)
		return tracker.Cards{Cards: []tracker.Card{card("10005", 1_799_000_000_000)}}, nil
	}
	before := m.read()
	out := must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	after := m.read()
	if out.Failure != nil || out.More() || !slices.Equal(out.Changes().Cards, ids("10005")) || len(asked) != 1 || !slices.Equal(asked[0], ids("10001", "10005")) {
		t.Fatalf("cards tick %+v, asked %v", out, asked)
	}
	if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{"10001", "10005"})); len(cards) != 2 {
		t.Fatalf("the mirror holds %+v", cards)
	}
	// The read is recorded as it began: a begin time the tick took.
	if st, _, _ := m.st.GetMirrorSync(m.root(), "", m.svc.steady().UnixMilli()); st.ReadAt <= before.At() || st.ReadAt >= after.At() || st.Failure.Kind != "" {
		t.Fatalf("linked sync state %+v, the tick's reads began after %d and before %d", st, before.At(), after.At())
	}
	m.p.set(func(p *scripted) {
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) {
			return tracker.Cards{}, &trackerapi.StatusError{Tracker: "jira", Status: 403}
		}
	})
	out = must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	if out.Failure == nil || out.Failure.Kind != FailureForbidden || m.problem() != FailureForbidden {
		t.Fatalf("a failed cards tick %+v, problem %q", out, m.problem())
	}
	if st, _, _ := m.st.GetMirrorSync(m.root(), "", m.svc.steady().UnixMilli()); st.Failure.Kind != store.MirrorFailureForbidden {
		t.Fatalf("linked sync state after the failure %+v", st)
	}
	if _, err := m.svc.tickCards(t.Context(), "", Background); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("linked unit of no connection: %v", err)
	}
}

// One tick reads every page of the store's listing of linked issues (200
// a page), readBatch issues a read, and records the read once it has.
func TestLinkedCardsReadEveryPageInOneTick(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	n := 2*200 + 5
	for i := range n {
		m.linkThread(fmt.Sprintf("thread-%d", i), tracker.IssueID(fmt.Sprintf("%05d", 40000+i)))
	}
	var asked []tracker.IssueID
	reads := 0
	m.p.cards = func(q []tracker.IssueID) (tracker.Cards, error) {
		reads++
		if len(q) > readBatch {
			t.Errorf("one read named %d issues", len(q))
		}
		asked = append(asked, q...)
		return tracker.Cards{}, nil
	}
	out := must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	if out.More() || out.Failure != nil || len(asked) != n || reads != 5 {
		t.Fatalf("tick %+v: %d asked in %d reads", out, len(asked), reads)
	}
	if !slices.IsSorted(asked) || asked[0] != "40000" || asked[n-1] != tracker.IssueID(fmt.Sprintf("%05d", 40000+n-1)) {
		t.Fatalf("asked from %v to %v", asked[0], asked[n-1])
	}
	if st, _, _ := m.st.GetMirrorSync(m.root(), "", m.svc.steady().UnixMilli()); st.ReadAt != m.now().UnixMilli() {
		t.Fatalf("the walk recorded no read: %+v", st)
	}
}

// The card read stores the cards of linked issues and leaves alone those
// the mirror does not keep and those the tracker does not return.
func TestCardsResourceStoresKeptIssues(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.linkThread("thread-1", "10005")
	m.p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
		return tracker.Cards{Cards: []tracker.Card{card("10005", 1_799_000_000_000), card("10006", 1_799_000_000_000)},
			Missing: ids("10007"), Catalog: tracker.Catalog{Users: []tracker.User{user1}}}, nil
	}
	r := m.begun(t, Scope{Connection: m.conn.ID})
	ch, _, err := r.cards(ids("10005", "10006", "10007"))
	r.finish()
	if err != nil || !slices.Equal(ch.Cards, ids("10005")) || !ch.Catalog {
		t.Fatalf("cards %+v, %v", ch, err)
	}
	if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{"10005", "10006", "10007"})); len(cards) != 1 {
		t.Fatalf("the mirror holds %+v", cards)
	}
	m.p.set(func(p *scripted) {
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) {
			return tracker.Cards{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	r = m.begun(t, Scope{Connection: m.conn.ID})
	_, _, err = r.cards(ids("10005"))
	r.finish()
	if _, ok := errors.AsType[*trackerapi.TransientError](err); !ok {
		t.Fatalf("a failed card read: %v", err)
	}
}

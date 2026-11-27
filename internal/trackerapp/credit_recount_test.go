package trackerapp

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
	"agent-overflow/internal/trackerdash"
)

// rowsToldSince is the rows the fixture board's events after the first n
// named as changed.
func (m *mirrorFixture) rowsToldSince(n int) []tracker.IssueID {
	boards, _, _ := m.events.snapshot()
	var out []tracker.IssueID
	for _, e := range boards[n:] {
		if e.Scope == m.scope() {
			out = append(out, e.Changed...)
		}
	}
	return out
}

// An evidence read that changes what credit counts of a ticket tells
// each open board holding it, which counts its credit again: the first
// read of a finished ticket's evidence names its row; the same read again
// names nothing.
func TestAnEvidenceReadTellsTheBoardsHoldingItsTickets(t *testing.T) {
	m, _, _ := evidenceFixture(t, 1)
	m.countCredit(t, m.conn.ID, true)
	m.subscribeShown(t, m.project())
	n := m.boardEventCount()
	m.evidenceTick(t)
	if got := m.rowsToldSince(n); !slices.Contains(got, "20001") {
		t.Fatalf("after the evidence read the board was told %v; want ticket 20001's row", got)
	}
	n = m.boardEventCount()
	m.evidenceTick(t)
	if got := m.rowsToldSince(n); len(got) != 0 {
		t.Fatalf("a tick with nothing due told %v", got)
	}
}

// evidenceFailureOf is why the evidence read failed as the fixture
// board's dashboard reads it while its project counts credit.
func (m *mirrorFixture) evidenceFailureOf(t *testing.T) string {
	t.Helper()
	p := must[store.Project](t)(m.st.GetProject(m.project()))
	groups := must[StatusGroups](t)(m.svc.boardStatusGroups(setupKey{slug: p.Slug, site: siteKey{m.conn.Kind, m.conn.Site}}))
	b, err := m.svc.dashboardBoard(m.scope(), groups)
	if err != nil {
		t.Fatal(err)
	}
	m.svc.leave()
	return b.EvidenceFailure
}

// recountSince reports whether the fixture board's events after the
// first n asked its dashboards to count again (BoardEvent.Recount).
func (m *mirrorFixture) recountSince(n int) bool {
	boards, _, _ := m.events.snapshot()
	return slices.ContainsFunc(boards[n:], func(e BoardEvent) bool { return e.Scope == m.scope() && e.Recount })
}

// The credit sentence says the evidence read's own failure: a linked tick
// that fails before the evidence read (an off-board linked card Jira
// refuses) says none; an evidence read that fails as a whole says why
// and marks no ticket's source failed, so the next reads the ticket, and
// that read, to its end, clears it. Each change of it has an open board's
// dashboards count again; a failure that repeats does not.
func TestTheCreditSentenceSaysTheEvidenceReadsOwnFailure(t *testing.T) {
	m, _, asked := evidenceFixture(t, 1)
	m.countCredit(t, m.conn.ID, true)
	m.subscribeShown(t, m.project())
	m.linkThread("thread-1", "50005")
	var read func(tracker.EvidenceQuery) (tracker.Evidence, error)
	m.p.set(func(p *scripted) {
		read = p.evidence
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) {
			return tracker.Cards{}, &trackerapi.StatusError{Status: 403}
		}
	})
	n := m.boardEventCount()
	out := must[Outcome](t)(m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{Cards: true, Evidence: true}, Background))
	if m.recountSince(n) {
		t.Fatal("a tick that failed before the evidence read had the dashboards count again")
	}
	if out.Failure == nil || slices.ContainsFunc(out.Ran, func(u UnitResult) bool { return u.Unit == UnitEvidence }) {
		t.Fatalf("the linked tick %+v; want it failed at its cards, before the evidence read", out)
	}
	if got := m.evidenceFailureOf(t); got != "" {
		t.Fatalf("after a failure before the evidence read, the sentence's failure is %q; want none", got)
	}
	m.p.set(func(p *scripted) {
		p.cards = nil
		p.evidence = func(tracker.EvidenceQuery) (tracker.Evidence, error) {
			return tracker.Evidence{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	n = m.boardEventCount()
	out = must[Outcome](t)(m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{Evidence: true}, Background))
	if out.Failure == nil {
		t.Fatal("the failing evidence read did not fail its tick")
	}
	if got := m.evidenceFailureOf(t); got == "" || !strings.Contains(got, out.Failure.Message) {
		t.Fatalf("after the evidence read failed, the sentence's failure is %q; want %q", got, out.Failure.Message)
	}
	if !m.recountSince(n) {
		t.Fatal("the evidence read's failure did not have the open board's dashboards count again")
	}
	n = m.boardEventCount()
	if out = must[Outcome](t)(m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{Evidence: true}, Background)); out.Failure == nil {
		t.Fatal("the failing evidence read did not fail its tick again")
	}
	if m.recountSince(n) {
		t.Fatal("the same failure again had the dashboards count again")
	}
	m.p.set(func(p *scripted) { p.evidence = read })
	n = m.boardEventCount()
	m.evidenceTick(t)
	if !m.recountSince(n) {
		t.Fatal("the evidence read that cleared the failure did not have the open board's dashboards count again")
	}
	whole := func(in tracker.EvidenceIssue) bool { return in.Issue == "20001" && in.Comments && in.Worklogs }
	if reads := asked(); len(reads) == 0 || !slices.ContainsFunc(reads[len(reads)-1], whole) {
		t.Fatalf("after the whole read failed, the next read asked %v; want both sources of ticket 20001, which the failure did not mark failed", reads)
	}
	if got := m.evidenceFailureOf(t); got != "" {
		t.Fatalf("after an evidence read that stored, the sentence's failure is %q; want none", got)
	}
}

// Turning the connection's Tempo connection on or off, which drops its
// worklogs (credit's evidence), reads its boards again.
func TestATimeSourceChangeReadsTheBoardsAgain(t *testing.T) {
	m, _, _ := evidenceFixture(t, 1)
	m.subscribeShown(t, m.project())
	n := m.boardEventCount()
	tempo := m.addTempo(m.conn.ID)
	if !m.readAgainSince(n) {
		t.Fatal("adding Tempo did not read the board again")
	}
	n = m.boardEventCount()
	if _, err := m.svc.SetEnabled(tempo.ID, false); err != nil {
		t.Fatal(err)
	}
	if !m.readAgainSince(n) {
		t.Fatal("turning Tempo off did not read the board again")
	}
}

// An edit of a finished ticket moves its card past its history, so who
// had it when it finished is not known and its review is not counted;
// the history read for the edit tells the open board its row, and the
// review counts again with no other change.
func TestAHistoryReadForAnEditCountsTheTicketsCreditAgain(t *testing.T) {
	m, _, _ := evidenceFixture(t, 1)
	m.countCredit(t, m.conn.ID, true)
	m.subscribeShown(t, m.project())
	review := openTickets()
	review.Metric.Title, review.Metric.SQL = "Review credit", `SELECT ticket, value, share FROM credit WHERE kind = 'review'`
	review.Metric.Reduce, review.Metric.Unit = trackerdash.ReduceSum, trackerdash.UnitEstimate
	credit := func() string {
		tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), review, TileMarks{}, "thread-1", false)
		if err != nil {
			t.Fatal(err)
		}
		return numText(t, tried.Value.Value)
	}
	m.evidenceTick(t)
	if got := credit(); got != `{"v":0}` {
		t.Fatalf("with its history and evidence read, the review is %s; want counted", got)
	}
	edited := row("20001", 1_799_000_100_000, "00000")
	edited.Card.Status, edited.Card.Sprints = statusDone.ID, []tracker.SprintID{"1"}
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{edited}, Next: cursorAfter("end")}, nil
		}
		// The evidence as the tracker answers it for the edited card.
		read := p.evidence
		p.evidence = func(q tracker.EvidenceQuery) (tracker.Evidence, error) {
			ev, err := read(q)
			for i := range ev.Read {
				ev.Read[i].Updated = edited.Card.Updated
			}
			return ev, err
		}
	})
	must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Delta: true}, Background))
	m.evidenceTick(t)
	if got := credit(); got != `{"nc":1}` {
		t.Fatalf("after the edit, with its evidence read again and not its history, the review is %s; want not counted", got)
	}
	n := m.boardEventCount()
	must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{History: true}, Background))
	if got := m.rowsToldSince(n); !slices.Contains(got, "20001") {
		t.Fatalf("the history read told the board %v; want ticket 20001's row", got)
	}
	if got := credit(); got != `{"v":0}` {
		t.Fatalf("with its history read for the edit, the review is %s; want counted", got)
	}
}

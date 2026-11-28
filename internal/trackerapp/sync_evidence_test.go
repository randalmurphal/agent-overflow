package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// evidenceFixture is a mirror fixture whose board holds rows, each in the
// active sprint and done, read by a delta with their histories, and
// whose tracker reads
// comments and worklogs of each ticket asked: one comment by User 2 and
// one hour logged by User 1, and rows extra. asked collects the tickets of
// each read.
func evidenceFixture(t *testing.T, rows int, extra ...tracker.Row) (m *mirrorFixture, cs *countingStore, asked func() [][]tracker.EvidenceIssue) {
	t.Helper()
	m = newMirrorFixture(t)
	var page []tracker.Row
	for i := range rows {
		r := row(fmt.Sprint(20001+i), 1_799_000_000_000, fmt.Sprintf("%05d", i))
		r.Card.Status, r.Card.Sprints = statusDone.ID, []tracker.SprintID{"1"}
		page = append(page, r)
	}
	page = append(page, extra...)
	m.p.set(func(p *scripted) {
		p.caps = tracker.Capabilities{Evidence: true}
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: page, Next: cursorAfter("end")}, nil
		}
		// Each ticket's history is read, final and with no change: who had
		// it is who has it.
		p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
			var h tracker.History
			for _, id := range q.Issues {
				h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: time.UnixMilli(1_900_000_000_000)})
			}
			return h, nil
		}
	})
	must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true, History: true}, Background))
	var reads [][]tracker.EvidenceIssue
	m.p.set(func(p *scripted) {
		p.evidence = func(q tracker.EvidenceQuery) (tracker.Evidence, error) {
			reads = append(reads, slices.Clone(q.Issues))
			var ev tracker.Evidence
			for _, in := range q.Issues {
				at := time.UnixMilli(1_799_000_000_000).UTC()
				if in.Comments {
					ev.Read = append(ev.Read, tracker.SourceEvidence{Issue: in.Issue, Source: tracker.EvidenceComments, Updated: at,
						Acts: []tracker.Act{{ID: "c-" + string(in.Issue), Author: user2.ID, At: at}}})
				}
				if in.Worklogs {
					ev.Read = append(ev.Read, tracker.SourceEvidence{Issue: in.Issue, Source: tracker.EvidenceWorklogs, Updated: at,
						Acts: []tracker.Act{{ID: "w-" + string(in.Issue), Author: user1.ID, At: at, Spent: time.Hour}}})
				}
			}
			ev.Users = []tracker.User{user1, user2}
			return ev, nil
		}
	})
	cs = newCountingStore(m.st)
	m.svc.store = cs
	return m, cs, func() [][]tracker.EvidenceIssue { return reads }
}

// countCredit turns credit on or off for the project that lists conn's
// boards (listBoards), in the setup's credit section, with support, the
// section's support entries as JSON, when it is given.
func (f *fixture) countCredit(t *testing.T, conn string, on bool, support ...string) {
	t.Helper()
	f.writeSetupFile(t, "boards-"+conn, fmt.Sprintf(`{"credit": {"version": 1, "on": %t, "rules": [{"from": 0, "review_share": 10, "review_need": 1,
		"review_actions": ["commented", "logged_time"], "hours_per_point": 8}], "support": [%s]}}`, on, strings.Join(support, ",")))
	f.svc.setupMu.Lock()
	clear(f.svc.setups)
	f.svc.setupMu.Unlock()
}

func (m *mirrorFixture) evidenceTick(t *testing.T) Outcome {
	t.Helper()
	out := must[Outcome](t)(m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{Evidence: true}, Background))
	if out.Failure != nil {
		t.Fatalf("evidence tick failed: %+v", out.Failure)
	}
	return out
}

// With no project counting credit, the evidence unit lists nothing and
// asks the tracker nothing; nor does a section that is off. It reads the
// connection's own board listings alone, never the board picker's
// listing of every project.
func TestEvidenceWithoutCreditReadsNothing(t *testing.T) {
	m, cs, asked := evidenceFixture(t, 1)
	m.evidenceTick(t)
	m.countCredit(t, m.conn.ID, false)
	m.evidenceTick(t)
	calls := cs.take()
	if calls["ListMirrorActsDue"]+calls["ReplaceMirrorActs"]+calls["ListTrackerBoardChoices"] != 0 || len(asked()) != 0 {
		t.Fatalf("store calls %v, reads %v", calls, asked())
	}
	if calls["ListConnectionProjectBoards"] != 2 {
		t.Fatalf("store calls %v; want the connection's listings read once a tick", calls)
	}
}

// A board of another connection with the same ID, listed by a project
// counting credit, counts nothing on this connection.
func TestEvidenceCountsOnlyTheConnectionsOwnBoards(t *testing.T) {
	m, _, asked := evidenceFixture(t, 1)
	other := m.addLinear()
	m.listBoards(other.ID, testBoard)
	m.countCredit(t, other.ID, true)
	m.evidenceTick(t)
	if len(asked()) != 0 {
		t.Fatalf("reads %v", asked())
	}
}

// A tracker that reads no evidence is asked nothing, and its listing not
// made, whether or not credit counts.
func TestEvidenceOfATrackerWithoutEvidenceReadsNothing(t *testing.T) {
	m, cs, asked := evidenceFixture(t, 1)
	m.p.set(func(p *scripted) { p.caps.Evidence = false })
	m.countCredit(t, m.conn.ID, true)
	m.evidenceTick(t)
	if calls := cs.take(); calls["ListMirrorActsDue"] != 0 || len(asked()) != 0 {
		t.Fatalf("store calls %v, reads %v", calls, asked())
	}
}

// With credit on, the unit reads the due tickets' evidence once, stores
// it with its receipts and the authors, and reads a ticket again only
// when its card moves.
func TestEvidenceIsReadOncePerUpdate(t *testing.T) {
	m, cs, asked := evidenceFixture(t, 2)
	m.countCredit(t, m.conn.ID, true)
	out := m.evidenceTick(t)
	if got := units(out); !slices.Equal(got, []Unit{UnitEvidence}) || out.More() || !out.Changes().Catalog {
		t.Fatalf("outcome %+v", out)
	}
	if fmt.Sprint(asked()) != "[[{20001 true true 0001-01-01 00:00:00 +0000 UTC} {20002 true true 0001-01-01 00:00:00 +0000 UTC}]]" {
		t.Fatalf("reads %v", asked())
	}
	root := m.root()
	got, _, err := m.st.ListMirrorActsDue(root, []store.MirrorActsBoard{{BoardID: string(testBoard)}}, 0, millis(m.svc.now()))
	if err != nil || len(got) != 0 {
		t.Fatalf("due after the read: %v, %v", got, err)
	}
	cs.take()
	m.evidenceTick(t)
	if calls := cs.take(); calls["ListMirrorActsDue"] != 1 || calls["ReplaceMirrorActs"] != 0 || len(asked()) != 1 {
		t.Fatalf("an idle tick: store calls %v, reads %d", calls, len(asked()))
	}
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			r := row("20002", 1_799_000_100_000, "00001")
			r.Card.Status, r.Card.Sprints = statusDone.ID, []tracker.SprintID{"1"}
			return tracker.RowPage{Rows: []tracker.Row{r}, Next: cursorAfter("end2")}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.evidenceTick(t)
	if reads := asked(); len(reads) != 2 || len(reads[1]) != 1 || reads[1][0].Issue != "20002" {
		t.Fatalf("reads %v", reads)
	}
}

// A unit reads one listing's page a tick, 200 tickets in 4 reads, and
// reports More while tickets are due.
func TestEvidenceReadsABoundedPageATick(t *testing.T) {
	const page, reads = 200, 4
	m, _, asked := evidenceFixture(t, page+1)
	m.countCredit(t, m.conn.ID, true)
	if out := m.evidenceTick(t); !out.More() || len(asked()) != reads {
		t.Fatalf("first tick: more %v, %d reads", out.More(), len(asked()))
	}
	if out := m.evidenceTick(t); out.More() || len(asked()) != reads+1 || len(asked()[reads]) != 1 {
		t.Fatalf("second tick: more %v, %d reads", out.More(), len(asked()))
	}
}

// A read during which the connection's time source changed stores its
// comments and no worklog, and the unit reports More so the next tick
// reads under the new source.
func TestEvidenceReadAcrossATimeSourceChangeIsReadAgain(t *testing.T) {
	m, _, asked := evidenceFixture(t, 1)
	m.countCredit(t, m.conn.ID, true)
	m.p.set(func(p *scripted) {
		inner := p.evidence
		p.evidence = func(q tracker.EvidenceQuery) (tracker.Evidence, error) {
			if len(asked()) == 0 {
				if _, err := m.st.CreateTrackerConnection(store.NewTrackerConnection{Kind: store.TrackerKindTempo, Site: "https://api.tempo.io",
					ParentID: m.conn.ID, Label: "Tempo", CreatedAt: 2}); err != nil {
					t.Error(err)
				}
			}
			return inner(q)
		}
	})
	if out := m.evidenceTick(t); !out.More() {
		t.Fatalf("outcome %+v", out)
	}
	rows, _, err := m.st.ListMirrorActsDue(m.root(), []store.MirrorActsBoard{{BoardID: string(testBoard)}}, 0, millis(m.svc.now()))
	if err != nil || fmt.Sprint(rows) != "[{20001 false true 0}]" {
		t.Fatalf("due after the refused read: %v, %v; want its worklogs alone", rows, err)
	}
}

// A ticket whose read failed for it alone is recorded with why and not
// asked again for a day, so the next tick reaches the tickets behind it
// and the unit stops reporting More; the attempts are reported to the
// loop's failure report. The tickets of the first page asked again a day
// later are read.
func TestEvidenceSkipsAFailedTicketForADay(t *testing.T) {
	m, _, asked := evidenceFixture(t, 201)
	m.countCredit(t, m.conn.ID, true)
	m.p.set(func(p *scripted) {
		inner := p.evidence
		p.evidence = func(q tracker.EvidenceQuery) (tracker.Evidence, error) {
			ev, err := inner(q)
			if len(asked()) <= 4 {
				ev.Read = nil
				for _, in := range q.Issues {
					ev.Failed = append(ev.Failed, tracker.SourceFailure{Issue: in.Issue, Source: tracker.EvidenceComments, Err: errors.New("Jira is busy")},
						tracker.SourceFailure{Issue: in.Issue, Source: tracker.EvidenceWorklogs, Err: errors.New("Jira is busy")})
				}
			}
			return ev, err
		}
	})
	if out := m.evidenceTick(t); !out.More() || len(asked()) != 4 {
		t.Fatalf("first tick: more %v, %d reads", out.More(), len(asked()))
	}
	_, _, errs := m.events.snapshot()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "could not read 400 tickets' comments or worklogs") || !strings.Contains(errs[0].Error(), "Jira is busy") {
		t.Fatalf("reported %v", errs)
	}
	if n := m.storeCount(t, `SELECT count(*) FROM tracker_cache_issue_act_reads WHERE tried_reason = 'Jira is busy' AND read_at = 0`); n != 400 {
		t.Fatalf("%d attempts recorded", n)
	}
	if out := m.evidenceTick(t); out.More() || len(asked()) != 5 || fmt.Sprint(asked()[4]) != "[{20201 true true 0001-01-01 00:00:00 +0000 UTC}]" {
		t.Fatalf("second tick: more %v, reads %v", out.More(), asked()[4:])
	}
	if out := m.evidenceTick(t); out.More() || len(asked()) != 5 {
		t.Fatalf("third tick: more %v, %d reads", out.More(), len(asked()))
	}
	m.pass(24*time.Hour + time.Second)
	if out := m.evidenceTick(t); !out.More() || len(asked()) != 9 {
		t.Fatalf("a day later: more %v, %d reads", out.More(), len(asked()))
	}
}

// A read that fails for every ticket after part of it was read stores
// that part, with its receipts, and fails the tick.
func TestEvidenceStoresWhatAFailedReadReturned(t *testing.T) {
	m, _, _ := evidenceFixture(t, 2)
	m.countCredit(t, m.conn.ID, true)
	m.p.set(func(p *scripted) {
		inner := p.evidence
		p.evidence = func(q tracker.EvidenceQuery) (tracker.Evidence, error) {
			ev, _ := inner(q)
			ev.Read = ev.Read[:1]
			return ev, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	out := must[Outcome](t)(m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{Evidence: true}, Background))
	if out.Failure == nil {
		t.Fatalf("outcome %+v; want the tick failed", out)
	}
	if n := m.storeCount(t, `SELECT count(*) FROM tracker_cache_issue_act_reads WHERE issue_id = '20001' AND source = 1 AND read_at > 0`); n != 1 {
		t.Fatalf("the comments read before the failure: %d receipts", n)
	}
}

// The support entry of the project's credit section reaches the listing:
// a support ticket in no sprint is asked for its worklogs from the
// window's start and not its comments, and a done member of the active
// sprint that is also support is asked for both, every worklog. With no
// support entry the support ticket is not asked.
func TestEvidenceReadsSupportTicketsOfTheCreditSetup(t *testing.T) {
	m, _, asked := evidenceFixture(t, 1, row("20002", 1_799_000_000_000, "z"))
	m.countCredit(t, m.conn.ID, true)
	m.evidenceTick(t)
	if fmt.Sprint(asked()) != "[[{20001 true true 0001-01-01 00:00:00 +0000 UTC}]]" {
		t.Fatalf("without support: reads %v", asked())
	}
	m.countCredit(t, m.conn.ID, true, fmt.Sprintf(`{"tracker": "jira", "site": %q, "boards": [%q]}`, m.conn.Site, testBoard))
	m.evidenceTick(t)
	window := m.svc.window().UTC().Truncate(time.Millisecond)
	want := []tracker.EvidenceIssue{{Issue: "20002", Worklogs: true, WorklogsFrom: window}}
	if reads := asked(); len(reads) != 2 || !slices.Equal(reads[1], want) {
		t.Fatalf("with support: reads %v; want %v", reads, want)
	}
}

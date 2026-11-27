package trackerapp

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// evidenceFixture is a mirror fixture whose board holds rows, each in the
// active sprint and done, read by a delta, and whose tracker reads
// comments and worklogs of each ticket asked: one comment by User 2 and
// one hour logged by User 1. asked collects the tickets of each read.
func evidenceFixture(t *testing.T, rows int) (m *mirrorFixture, cs *countingStore, asked func() [][]tracker.EvidenceIssue) {
	t.Helper()
	m = newMirrorFixture(t)
	var page []tracker.Row
	for i := range rows {
		r := row(fmt.Sprint(20001+i), 1_799_000_000_000, fmt.Sprintf("%05d", i))
		r.Card.Status, r.Card.Sprints = statusDone.ID, []tracker.SprintID{"1"}
		page = append(page, r)
	}
	m.p.set(func(p *scripted) {
		p.caps = tracker.Capabilities{Evidence: tracker.EvidenceKinds{Comments: true, Worklogs: true}}
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: page, Next: cursorAfter("end")}, nil
		}
	})
	must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true}, Background))
	var reads [][]tracker.EvidenceIssue
	m.p.set(func(p *scripted) {
		p.evidence = func(q tracker.EvidenceQuery) (tracker.Evidence, error) {
			reads = append(reads, slices.Clone(q.Issues))
			var ev tracker.Evidence
			for _, in := range q.Issues {
				at := time.UnixMilli(1_799_000_000_000).UTC()
				ev.Issues = append(ev.Issues, tracker.IssueEvidence{Issue: in.Issue, Updated: at,
					Comments: []tracker.Act{{ID: "c-" + string(in.Issue), Author: user2.ID, At: at}},
					Worklogs: []tracker.Act{{ID: "w-" + string(in.Issue), Author: user1.ID, At: at, Spent: time.Hour}}})
			}
			ev.Users = []tracker.User{user1, user2}
			return ev, nil
		}
	})
	cs = newCountingStore(m.st)
	m.svc.store = cs
	return m, cs, func() [][]tracker.EvidenceIssue { return reads }
}

// countCredit turns credit on for the fixture's project, in the setup's
// credit section.
func (m *mirrorFixture) countCredit(t *testing.T, on bool) {
	t.Helper()
	m.writeSetupFile(t, "boards-"+m.conn.ID, fmt.Sprintf(`{"credit": {"version": 1, "on": %t}}`, on))
	m.svc.setupMu.Lock()
	clear(m.svc.setups)
	m.svc.setupMu.Unlock()
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
// asks the tracker nothing; nor does a section that is off.
func TestEvidenceWithoutCreditReadsNothing(t *testing.T) {
	m, cs, asked := evidenceFixture(t, 1)
	m.evidenceTick(t)
	m.countCredit(t, false)
	m.evidenceTick(t)
	if calls := cs.take(); calls["ListMirrorActsDue"]+calls["ReplaceMirrorActs"] != 0 || len(asked()) != 0 {
		t.Fatalf("store calls %v, reads %v", calls, asked())
	}
}

// With credit on, the unit reads the due tickets' evidence once, stores
// it with its receipts and the authors, and reads a ticket again only
// when its card moves.
func TestEvidenceIsReadOncePerUpdate(t *testing.T) {
	m, cs, asked := evidenceFixture(t, 2)
	m.countCredit(t, true)
	out := m.evidenceTick(t)
	if got := units(out); !slices.Equal(got, []Unit{UnitEvidence}) || out.More() || !out.Changes().Catalog {
		t.Fatalf("outcome %+v", out)
	}
	if fmt.Sprint(asked()) != "[[{20001 0001-01-01 00:00:00 +0000 UTC} {20002 0001-01-01 00:00:00 +0000 UTC}]]" {
		t.Fatalf("reads %v", asked())
	}
	root := m.root()
	got, _, err := m.st.ListMirrorActsDue(root, []store.MirrorActsBoard{{BoardID: string(testBoard)}}, true, 0)
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

// A unit reads at most evidenceBatches batches a tick and reports More
// while tickets are due.
func TestEvidenceReadsABoundedPageATick(t *testing.T) {
	m, _, asked := evidenceFixture(t, evidenceBatch*evidenceBatches+1)
	m.countCredit(t, true)
	if out := m.evidenceTick(t); !out.More() || len(asked()) != evidenceBatches {
		t.Fatalf("first tick: more %v, %d reads", out.More(), len(asked()))
	}
	if out := m.evidenceTick(t); out.More() || len(asked()) != evidenceBatches+1 || len(asked()[evidenceBatches]) != 1 {
		t.Fatalf("second tick: more %v, %d reads", out.More(), len(asked()))
	}
}

// While the tracker reads no worklogs (Tempo is the time), only comments
// are read and due: a ticket with a comment receipt is not due again.
func TestEvidenceWithoutWorklogsStoresComments(t *testing.T) {
	m, _, asked := evidenceFixture(t, 1)
	m.p.set(func(p *scripted) { p.caps.Evidence.Worklogs = false })
	m.countCredit(t, true)
	m.evidenceTick(t)
	m.evidenceTick(t)
	if len(asked()) != 1 {
		t.Fatalf("%d reads", len(asked()))
	}
	var sources []string
	rows, _, err := m.st.ListMirrorActsDue(m.root(), []store.MirrorActsBoard{{BoardID: string(testBoard)}}, true, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("due with worklogs read: %v, %v", rows, err)
	}
	for _, r := range rows {
		sources = append(sources, r.IssueID)
	}
	if strings.Join(sources, ",") != "20001" {
		t.Fatalf("due %v", sources)
	}
}

// A read during which the connection's time source changed stores
// nothing, and the unit reports More so the next tick reads under the new
// source.
func TestEvidenceReadAcrossATimeSourceChangeIsReadAgain(t *testing.T) {
	m, _, asked := evidenceFixture(t, 1)
	m.countCredit(t, true)
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
	rows, _, err := m.st.ListMirrorActsDue(m.root(), []store.MirrorActsBoard{{BoardID: string(testBoard)}}, true, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("due after the refused read: %v, %v", rows, err)
	}
}

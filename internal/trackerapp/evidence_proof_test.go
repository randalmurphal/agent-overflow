package trackerapp

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// Proofs of the reviewers' findings on the evidence unit, against the API
// of eb733523b.

// A2, B3: with the first 200 of 201 due tickets withheld, the second tick
// must reach ticket 201.
func TestProofWithheldTicketsStallTheQueue(t *testing.T) {
	m, _, asked := evidenceFixture(t, 201)
	m.countCredit(t, m.conn.ID, true)
	withheld := map[tracker.IssueID]bool{}
	m.p.set(func(p *scripted) {
		inner := p.evidence
		p.evidence = func(q tracker.EvidenceQuery) (tracker.Evidence, error) {
			ev, err := inner(q)
			if len(asked()) <= 4 {
				for _, in := range q.Issues {
					withheld[in.Issue] = true
				}
				ev.Issues = nil
			}
			return ev, err
		}
	})
	m.evidenceTick(t)
	m.evidenceTick(t)
	reads := asked()
	for _, in := range reads[len(reads)-1] {
		if !withheld[in.Issue] {
			return
		}
	}
	t.Fatalf("the second tick asked only withheld tickets again: %d reads, last %v", len(reads), reads[len(reads)-1])
}

// creditTempoScenario is a credit scenario of one finished sprint whose
// first ticket holds a Tempo worklog of User 1 of 1,800 seconds, with
// Tempo on.
func creditTempoScenario(t *testing.T) (*fakeScenario, Connection, Scope, Connection) {
	t.Helper()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		site := jiraSeed(at)
		completed := at.Add(-7 * 24 * time.Hour)
		finishedSprint(&site, 101, completed)
		site.Tempo = &trackerfake.TempoAccount{Token: tempoTokenFake, Worklogs: []trackerfake.TempoWorklog{
			{ID: 7001, Issue: "110100", Author: "acc-user1", StartDate: completed.Add(-48 * time.Hour).Format(time.DateOnly), StartTime: "09:00:00", Spent: 1800}}}
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	tempo := s.addTempo(conn.ID)
	s.writeToken(tempo, tempoTokenFake)
	scope := Scope{Connection: conn.ID, Board: "1"}
	s.listBoards(conn.ID, scope.Board)
	s.countCredit(t, conn.ID, true)
	s.readBoard(t, scope)
	return s, conn, scope, tempo
}

// A4: the app's own Tempo edit reaches the evidence.
func TestProofTempoEditReachesTheEvidence(t *testing.T) {
	s, conn, _, _ := creditTempoScenario(t)
	s.runLinked(t, conn)
	if n := s.storeCount(t, `SELECT seconds FROM tracker_cache_issue_acts WHERE source = 2 AND act_id = '7001'`); n != 1800 {
		t.Fatalf("collected %d seconds", n)
	}
	day := s.clock.Now().Add(-9 * 24 * time.Hour).Format(time.DateOnly)
	got := must[WorklogDays](t)(s.svc.RefreshWorklogs(t.Context(), conn.ID, "acc-user1", day, day))
	if len(got.Worklogs) != 1 {
		t.Fatalf("worklogs %+v", got)
	}
	spent := int64(3600)
	must[WriteView](t)(s.svc.EditWorklog(t.Context(), conn.ID, WorklogEditRequest{From: got.Worklogs[0], SpentSeconds: &spent}))
	s.runLinked(t, conn)
	if n := s.storeCount(t, `SELECT seconds FROM tracker_cache_issue_acts WHERE source = 2 AND act_id = '7001'`); n != 3600 {
		t.Fatalf("after the edit the evidence holds %d seconds", n)
	}
}

// Turning Tempo off while its search is held: the tick reports no
// failure and More.
func TestProofTempoOffDuringARead(t *testing.T) {
	s, conn, _, tempo := creditTempoScenario(t)
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerTempo}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: "POST 4/worklogs/search", Kind: trackerfake.FaultHold, Times: 1}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := s.svc.SyncLinked(context.Background(), conn.ID, LinkedDue{Evidence: true}, Background)
		done <- result{out, err}
	}()
	waitHeld(t, s.e, "POST 4/worklogs/search")
	must[Connection](t)(s.svc.SetEnabled(tempo.ID, false))
	if err := s.e.Release(origin, "POST 4/worklogs/search", false); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.out.Failure != nil || !r.out.More() {
		t.Fatalf("the tick %+v, %v; want no failure and More", r.out, r.err)
	}
}

// A1: an evidence read's 429 and the board's next interactive read.
func TestProofEvidenceRateLimitAndTheBoard(t *testing.T) {
	s, conn, scope := creditScenario(t)
	if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"},
		Route: "POST rest/api/3/issue/bulkfetch", Kind: trackerfake.FaultRateLimit, RetryAfter: 120, Times: 1}); err != nil {
		t.Fatal(err)
	}
	out := must[Outcome](t)(s.svc.SyncLinked(t.Context(), conn.ID, LinkedDue{Evidence: true}, Background))
	t.Logf("evidence tick: failure %+v, retry at %v", out.Failure, out.RetryAt)
	since := s.lastSeq()
	start := time.Now()
	board, err := s.svc.tickDelta(t.Context(), scope, Interactive)
	t.Logf("board delta (interactive) after %v: failure %+v, err %v, requests %d", time.Since(start), board.Failure, err, len(s.e.Requests(since).Requests))
	if board.Failure != nil || err != nil {
		t.Fatalf("the board's read failed after an evidence read's 429: %+v, %v", board.Failure, err)
	}
}

// B7: with credit off, the unit lists no board choices.
type choicesCountingStore struct {
	*countingStore
}

func (c *choicesCountingStore) ListTrackerBoardChoices(projectID string) ([]store.TrackerBoardChoice, error) {
	c.count("ListTrackerBoardChoices")
	return c.Store.ListTrackerBoardChoices(projectID)
}

func TestProofCreditOffListsNoBoardChoices(t *testing.T) {
	m, cs, _ := evidenceFixture(t, 1)
	cc := &choicesCountingStore{countingStore: cs}
	m.svc.store = cc
	m.countCredit(t, m.conn.ID, false)
	cs.take()
	m.evidenceTick(t)
	if n := cs.take()["ListTrackerBoardChoices"]; n != 0 {
		t.Fatalf("credit off: %d board choice listings", n)
	}
}

var _ = http.StatusOK
var _ = fmt.Sprint

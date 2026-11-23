package trackerapp

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// sameUpdate gives issue the title title in the fake, its update time
// left as it is, as a change Linear records with no update would.
func (s *fakeScenario) sameUpdate(t *testing.T, issue tracker.IssueID, title string) {
	t.Helper()
	f := s.e.Snapshot()
	ws := f.Linear[0]
	ws.APIKey = "lin_api_fake00001"
	i := slices.IndexFunc(ws.Issues, func(is trackerfake.LinearIssue) bool { return is.ID == string(issue) })
	updated := ws.Issues[i].UpdatedAt
	ws.Issues[i].Title = title
	if err := s.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
		t.Fatal(err)
	}
	if got := s.e.Snapshot().Linear[0].Issues[i]; got.Title != title || !got.UpdatedAt.Equal(updated) {
		t.Fatalf("the fake holds %q updated %v; want %q at %v", got.Title, got.UpdatedAt, title, updated)
	}
}

// Linear reads an on-board issue's card at an update time the mirror
// already holds on each of these paths: a reconcile's read by ID of a
// listed issue with no row, the delta's first walk from the window, a
// delta walk that starts again within the overlap of its last row, and a
// ticket round, which reads an open ticket in full each time (Linear has
// no check). On each, a card identical to the record's is stored with no
// change and tells nothing; a card whose content differs at the same
// update time is the later read's, so it is stored and told.
func TestLinearReadsACardAtAnUnchangedUpdateTime(t *testing.T) {
	t.Parallel()
	for _, differs := range []bool{false, true} {
		t.Run(map[bool]string{false: "identical", true: "differs"}[differs], func(t *testing.T) {
			t.Parallel()
			s := newFakeScenarioWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour}, func(at time.Time) trackerfake.Fixture {
				return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
			})
			conn := s.addLinear()
			s.writeToken(conn, "lin_api_fake00001")
			scope := Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
			s.listBoards(conn.ID, scope.Board)
			issue := tracker.IssueID(lid(60))
			title := func() string { return must[IssueView](t)(s.svc.Issue(conn.ID, issue)).Card.Title }
			// step changes the fake's title at the same update time when the
			// card differs, runs path, and checks what the path stored and
			// told of the issue.
			n := 0
			step := func(what string, path func() Changes) {
				t.Helper()
				want := title()
				if differs {
					n++
					want = want + " " + string(rune('a'+n))
					s.sameUpdate(t, issue, want)
				}
				ch := path()
				if got := title(); got != want {
					t.Fatalf("%s: the record's title %q; want %q", what, got, want)
				}
				if told := slices.Contains(ch.Cards, issue); told != differs {
					t.Fatalf("%s: told the card %v (%+v); want %v", what, told, ch, differs)
				}
				// A card told makes the open ticket due a read at once, which
				// settles before the tracker changes again.
				quiet(t, s.svc)
			}
			tick := func(unit func() (Outcome, error)) Changes {
				out := must[Outcome](t)(unit())
				if out.Failure != nil || out.More() {
					t.Fatalf("the unit ended %+v", out)
				}
				return out.Changes()
			}
			must[Outcome](t)(s.svc.tickStructure(t.Context(), scope, Background))
			// The open ticket stores the issue's card before any row of the
			// board.
			sub := s.openTicket(t, conn, issue)

			step("a reconcile's read by ID", func() Changes {
				ch := tick(func() (Outcome, error) { return s.svc.tickReconcile(t.Context(), scope, Background) })
				if !slices.Contains(ch.Rows, issue) {
					t.Fatalf("the reconcile placed no row of the issue: %+v", ch)
				}
				return ch
			})
			step("the delta's first walk", func() Changes {
				return tick(func() (Outcome, error) { return s.svc.tickDelta(t.Context(), scope, Background) })
			})
			// An edit the walk delivers within the overlap of its start makes
			// the next walk start again before it.
			s.clock.advance(time.Minute)
			applied(t, "edit", s.provider(conn.ID).Edit(t.Context(), tracker.EditInput{Issue: issue,
				Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Edited")}}}))
			quiet(t, s.svc)
			if ch := tick(func() (Outcome, error) { return s.svc.tickDelta(t.Context(), scope, Background) }); !slices.Contains(ch.Cards, issue) {
				t.Fatalf("the walk after the edit did not deliver it: %+v", ch)
			}
			quiet(t, s.svc)
			step("a walk started again", func() Changes {
				return tick(func() (Outcome, error) { return s.svc.tickDelta(t.Context(), scope, Background) })
			})

			// The round's read is told through the funnel: a board subscriber
			// hears of a changed card.
			board := must[BoardSubscription](t)(s.svc.SubscribeBoard("boards-"+conn.ID, scope, []Section{{Kind: SectionBacklog}}))
			if err := s.svc.RefreshSubscription(t.Context(), board.ID); err != nil {
				t.Fatal(err)
			}
			step("a ticket round", func() Changes {
				boards, _, _ := s.events.snapshot()
				before := len(boards)
				rd := s.svc.roundOf(conn.ID)
				if rd == nil {
					t.Fatal("no ticket round runs")
				}
				if res := s.svc.roundTick(rd); res.failed {
					t.Fatalf("the round %+v", res)
				}
				quiet(t, s.svc)
				boards, _, _ = s.events.snapshot()
				var ch Changes
				for _, e := range boards[before:] {
					if slices.Contains(e.Changed, issue) {
						ch.Cards = ids(string(issue))
					}
				}
				return ch
			})
			s.closeTicket(t, conn, sub, issue)
		})
	}
}

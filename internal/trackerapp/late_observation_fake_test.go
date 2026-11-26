package trackerapp

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// A ticket read held at the tracker answers after a later read of the
// same ticket stored, with what the tracker holds then: the observation
// the later read stored, arriving late. It replaces nothing the later
// read wrote (the record's card, its status time and finality, the body
// and its bookkeeping, the read order) and tells nothing. Both reads are
// a person's refresh of the issue's detail, inside the tracker's settle
// margin after a status change, so each gives provisional status
// evidence.
func TestALateIdenticalObservationReplacesNothing(t *testing.T) {
	t.Parallel()
	routes := map[string]trackerfake.Origin{
		"jira":   {Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"},
		"linear": {Tracker: trackerfake.TrackerLinear},
	}
	route := map[string]string{"jira": "GET rest/api/3/issue/{issue}", "linear": "IssueDetails"}
	for name, tc := range statusSinceTrackers() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, conn, scope := tc.open(t)
			s.listBoards(conn.ID, scope.Board)
			s.syncBoard(t, scope)
			tc.leaveAndReturn(t, s, conn)
			must[Outcome](t)(s.svc.tickDelta(t.Context(), scope, Background))
			root := must[int64](t)(rootOf(s, conn))

			origin := routes[name]
			if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: route[name], Kind: trackerfake.FaultHold, Times: 1}); err != nil {
				t.Fatal(err)
			}
			type answer struct {
				ch  Changes
				err error
			}
			older := make(chan answer, 1)
			go func() {
				_, ch, err := s.svc.issueDetail(t.Context(), conn.ID, tc.issue, Interactive, true, true)
				older <- answer{ch, err}
			}()
			waitHeld(t, s.e, route[name])
			if _, _, err := s.svc.issueDetail(t.Context(), conn.ID, tc.issue, Interactive, true, true); err != nil {
				t.Fatal(err)
			}
			card := recordOfFake(t, s, conn, tc.issue)
			body, ok, err := getMirrorBody(s.st, root, string(tc.issue))
			if err != nil || !ok {
				t.Fatalf("the later read stored no body: %v, %v", ok, err)
			}
			if card.StatusSince == 0 || card.StatusFinal {
				t.Fatalf("the later read's status time %d final %v; want a provisional one", card.StatusSince, card.StatusFinal)
			}

			if err := s.e.Release(origin, route[name], false); err != nil {
				t.Fatal(err)
			}
			got := <-older
			if got.err != nil {
				t.Fatal(got.err)
			}
			named := func(ids []tracker.IssueID) bool { return slices.Contains(ids, tc.issue) }
			if named(got.ch.Cards) || named(got.ch.Details) || named(got.ch.History) {
				t.Fatalf("the late observation told %+v", got.ch)
			}
			if after := recordOfFake(t, s, conn, tc.issue); !reflect.DeepEqual(after, card) {
				t.Fatalf("the late observation replaced the record's card\n%+v\nwith\n%+v", card, after)
			}
			after, ok, err := getMirrorBody(s.st, root, string(tc.issue))
			if err != nil || !ok {
				t.Fatalf("the body went: %v, %v", ok, err)
			}
			// The older read's caller is shown the record's body, the later
			// read's (issueDetail), and that showing is a
			// person's opening of it now: the opening time alone moves, and
			// only forward.
			if after.OpenedAt < body.OpenedAt {
				t.Fatalf("the late observation moved the body's opening back from %d to %d", body.OpenedAt, after.OpenedAt)
			}
			after.OpenedAt = body.OpenedAt
			if !reflect.DeepEqual(after, body) {
				t.Fatalf("the late observation replaced the body: read %d wall %d updated %d; want the later read's %d %d %d",
					after.ReadAt, after.Wall, after.UpdatedAt, body.ReadAt, body.Wall, body.UpdatedAt)
			}
		})
	}
}

// waitHeld waits until a request on route is held.
func waitHeld(t *testing.T, e *trackerfake.Engine, route string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; {
		if slices.ContainsFunc(e.Requests(0).Requests, func(r trackerfake.Request) bool { return r.Route == route && r.Held }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no request on %s was held", route)
		}
		time.Sleep(time.Millisecond)
	}
}

package jiraprovider

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// The routes a rows read by ID makes, as the fake logs them.
const (
	boardListRoute   = "GET rest/software/1.0/board/{board}/issue"
	boardSprintRoute = "GET rest/agile/1.0/board/{board}/sprint"
	sprintRoute      = "GET rest/agile/1.0/sprint/{sprint}"
	matchRoute       = "POST rest/api/3/jql/match"
)

// routeCounts are the requests the fake recorded after since, by route.
func routeCounts(e *trackerfake.Engine, since int) map[string]int {
	out := map[string]int{}
	for _, r := range e.Requests(since).Requests {
		out[r.Route]++
	}
	return out
}

// lastSeq is the number of the fake's latest recorded request.
func lastSeq(e *trackerfake.Engine) int {
	rs := e.Requests(0).Requests
	if len(rs) == 0 {
		return 0
	}
	return rs[len(rs)-1].Seq
}

// isRefusal reports err as a rows read the tracker refused for its rule,
// wrapping the 400 that refused it.
func isRefusal(err error) bool {
	status, ok := errors.AsType[*trackerapi.StatusError](err)
	return errors.Is(err, tracker.ErrRuleRefused) && ok && status.Status == http.StatusBadRequest
}

// A rows read by ID is refused for its rule only on evidence: after the
// 400, a lookup of a sprint the rule names answers 404. A sprint deleted
// since the listing is refused that way, and so is one the rule names
// that never existed; the read under the listing's own rule reads the
// rows. The lookups stop at the first 404.
func TestARowsReadWhoseRuleNamesADeletedSprintIsRefusedTyped(t *testing.T) {
	s := newScenario(t, false)
	window := s.clock.Now().Add(-scenarioWindow)
	listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, "1", window))
	if len(listing.Rule.Sprints) == 0 {
		t.Fatalf("the listing's rule %+v names no sprint", listing.Rule)
	}
	if rows := scenarioMust[tracker.Rows](t)(s.p.BoardRows(s.ctx, "1", listing.IDs, listing.Rule)); len(rows.Rows) != len(listing.IDs) {
		t.Fatalf("the rows under the listing's rule %+v", rows)
	}
	never := listing.Rule
	never.Sprints = append(never.Sprints[:len(never.Sprints):len(never.Sprints)], "999")
	since := lastSeq(s.e)
	if _, err := s.p.BoardRows(s.ctx, "1", listing.IDs, never); !isRefusal(err) {
		t.Fatalf("a rule naming a sprint the board does not have: %v; want the typed refusal wrapping the 400", err)
	}
	if got := routeCounts(s.e, since); got[sprintRoute] != len(never.Sprints) || got[boardSprintRoute] != 0 {
		t.Fatalf("the refusal of a rule naming %d sprints made %v; want a lookup of each and no listing", len(never.Sprints), got)
	}
	if err := s.p.jira.DeleteSprint(s.ctx, 1); err != nil {
		t.Fatal(err)
	}
	since = lastSeq(s.e)
	if _, err := s.p.BoardRows(s.ctx, "1", listing.IDs, listing.Rule); !isRefusal(err) {
		t.Fatalf("the listing's rule after its sprint was deleted: %v; want the typed refusal wrapping the 400", err)
	}
	if got := routeCounts(s.e, since); got[sprintRoute] != 1 || got[boardSprintRoute] != 0 {
		t.Fatalf("the refusal of a rule whose first sprint is deleted made %v; want one lookup and no listing", got)
	}
}

// A rule naming more sprints than a page of the board's sprint list
// holds is judged at most one lookup per sprint it names, stopping at the
// first 404, with no listing.
func TestARuleRefusalCostsAtMostOneLookupPerSprint(t *testing.T) {
	s := newScenario(t, false)
	for i := range 49 {
		if _, err := s.p.jira.CreateSprint(s.ctx, jira.SprintInput{Board: 1, Name: fmt.Sprintf("Sprint %d", i+3)}); err != nil {
			t.Fatal(err)
		}
	}
	listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, "1", s.clock.Now().Add(-scenarioWindow)))
	if len(listing.Rule.Sprints) != 51 {
		t.Fatalf("the listing's rule names %d sprints; want 51", len(listing.Rule.Sprints))
	}
	for _, at := range []int{50, 25} {
		n, err := sprintNumber(listing.Rule.Sprints[at])
		if err != nil {
			t.Fatal(err)
		}
		if err := s.p.jira.DeleteSprint(s.ctx, n); err != nil {
			t.Fatal(err)
		}
		since := lastSeq(s.e)
		if _, err := s.p.BoardRows(s.ctx, "1", listing.IDs, listing.Rule); !isRefusal(err) {
			t.Fatalf("sprint %d of 51 deleted: %v; want the typed refusal wrapping the 400", at+1, err)
		}
		if got := routeCounts(s.e, since); got[sprintRoute] != at+1 || got[boardSprintRoute] != 0 {
			t.Fatalf("sprint %d of 51 deleted: the refusal made %v; want %d lookups and no listing", at+1, got, at+1)
		}
	}
}

// A 400 on the rows read while every sprint the rule names still exists
// is the tracker's answer, not the rule's: it is returned as it is, at the
// cost of one lookup per sprint the rule names, however many times it
// repeats.
func TestARowsReadRefusedWithEverySprintValidIsNotARuleRefusal(t *testing.T) {
	s := newScenario(t, false)
	listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, "1", s.clock.Now().Add(-scenarioWindow)))
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: boardListRoute, Kind: trackerfake.FaultRefuse,
		Status: http.StatusBadRequest, Body: []byte(`{"errorMessages":["Bad request"]}`)}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		since := lastSeq(s.e)
		_, err := s.p.BoardRows(s.ctx, "1", listing.IDs, listing.Rule)
		status, ok := errors.AsType[*trackerapi.StatusError](err)
		if errors.Is(err, tracker.ErrRuleRefused) || !ok || status.Status != http.StatusBadRequest {
			t.Fatalf("a 400 with every sprint valid: %v; want the 400 alone", err)
		}
		if got := routeCounts(s.e, since); got[sprintRoute] != len(listing.Rule.Sprints) || got[boardSprintRoute] != 0 || got[matchRoute] != 0 {
			t.Fatalf("the refused read made %v; want a lookup of each of the rule's %d sprints, no listing and no match", got, len(listing.Rule.Sprints))
		}
	}
	// A rule naming no sprint is not judged at all.
	since := lastSeq(s.e)
	_, err := s.p.BoardRows(s.ctx, "1", listing.IDs, tracker.Eligibility{Window: listing.Rule.Window})
	if errors.Is(err, tracker.ErrRuleRefused) || routeCounts(s.e, since)[sprintRoute] != 0 {
		t.Fatalf("a 400 under a rule naming no sprint: %v, %v", err, routeCounts(s.e, since))
	}
}

// The rows read's list leaves an issue out, and the sprint the rule names
// is deleted between the list and the match, which Jira then refuses: the
// read is refused for its rule as when the list refused it. The same
// refused match with every sprint valid is the tracker's answer.
func TestASprintDeletedBetweenTheListAndTheMatchIsARuleRefusal(t *testing.T) {
	for _, deleted := range []bool{true, false} {
		var mu sync.Mutex
		var armed bool
		var s *scenario
		s = newObservedScenario(t, false, func(r trackerfake.Request) {
			mu.Lock()
			fire := armed && r.Route == boardListRoute
			armed = armed && !fire
			mu.Unlock()
			if fire && deleted {
				if err := s.p.jira.DeleteSprint(s.ctx, 1); err != nil {
					t.Error(err)
				}
			}
		})
		listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, "1", s.clock.Now().Add(-scenarioWindow)))
		origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}
		if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: matchRoute, Kind: trackerfake.FaultRefuse,
			Status: http.StatusBadRequest, Body: []byte(`{"errorMessages":["Bad request"]}`)}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		armed = true
		mu.Unlock()
		// 10199 names no issue: the list leaves it out, and only the match
		// judges it.
		_, err := s.p.BoardRows(s.ctx, "1", []tracker.IssueID{"10101", "10199"}, listing.Rule)
		if got := isRefusal(err); got != deleted {
			t.Fatalf("sprint deleted %v: the refused match %v; typed refusal %v", deleted, err, got)
		}
	}
}

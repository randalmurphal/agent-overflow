package jiraprovider

import (
	"errors"
	"net/http"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
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

// isStatus reports an error that is the tracker's answer with status.
func isStatus(err error, status int) bool {
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	return ok && se.Status == status
}

// isRefusal reports err as a rows read the tracker refused for its rule,
// wrapping the 400 that refused it.
func isRefusal(err error) bool {
	status, ok := errors.AsType[*trackerapi.StatusError](err)
	return errors.Is(err, tracker.ErrRuleRefused) && ok && status.Status == http.StatusBadRequest
}

// A read by ID that Jira refuses with a 400 under a rule naming sprints
// is a refusal of the rule, whatever its cause: a sprint deleted since the
// listing, one that never existed, or a 400 of its own with every sprint
// valid. No sprint is read to judge it. Under a rule naming no sprint the
// 400 is the tracker's answer.
func TestARefusedReadUnderASprintRuleIsARuleRefusal(t *testing.T) {
	s := newScenario(t, false)
	window := s.clock.Now().Add(-scenarioWindow)
	listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, "1", window))
	if len(listing.Rule.Sprints) == 0 {
		t.Fatalf("the listing's rule %+v names no sprint", listing.Rule)
	}
	if rows := scenarioMust[tracker.Rows](t)(s.p.BoardRows(s.ctx, "1", listing.IDs, listing.Rule)); len(rows.Rows) != len(listing.IDs) {
		t.Fatalf("the rows under the listing's rule %+v", rows)
	}
	refused := func(what string, rule tracker.Eligibility, want bool) {
		t.Helper()
		since := lastSeq(s.e)
		_, err := s.p.BoardRows(s.ctx, "1", listing.IDs, rule)
		status, ok := errors.AsType[*trackerapi.StatusError](err)
		if !ok || status.Status != http.StatusBadRequest || errors.Is(err, tracker.ErrRuleRefused) != want {
			t.Fatalf("%s: %v; want the 400, a refusal of the rule %v", what, err, want)
		}
		if got := routeCounts(s.e, since); got[sprintRoute]+got[boardSprintRoute] != 0 {
			t.Fatalf("%s made %v; want no sprint read", what, got)
		}
	}
	never := listing.Rule
	never.Sprints = append(never.Sprints[:len(never.Sprints):len(never.Sprints)], "999")
	refused("a rule naming a sprint that never existed", never, true)
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: boardListRoute, Kind: trackerfake.FaultRefuse,
		Status: http.StatusBadRequest, Body: []byte(`{"errorMessages":["Bad request"]}`)}); err != nil {
		t.Fatal(err)
	}
	refused("a 400 with every sprint valid", listing.Rule, true)
	refused("a 400 under a rule naming no sprint", tracker.Eligibility{Window: listing.Rule.Window}, false)
	s.e.ClearFaults(origin, boardListRoute)
	if err := s.p.jira.DeleteSprint(s.ctx, 1); err != nil {
		t.Fatal(err)
	}
	refused("the listing's rule after its sprint was deleted", listing.Rule, true)
}

// The rows read's list leaves an issue out and Jira refuses the match
// that judges it with a 400: under a rule naming sprints the read is
// refused for its rule, with no sprint read; under one naming none the
// 400 is the tracker's answer.
func TestARefusedMatchUnderASprintRuleIsARuleRefusal(t *testing.T) {
	s := newScenario(t, false)
	listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, "1", s.clock.Now().Add(-scenarioWindow)))
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: matchRoute, Kind: trackerfake.FaultRefuse,
		Status: http.StatusBadRequest, Body: []byte(`{"errorMessages":["Bad request"]}`)}); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []tracker.Eligibility{listing.Rule, {Window: listing.Rule.Window}} {
		since := lastSeq(s.e)
		// 10199 names no issue: the list leaves it out, and only the match
		// judges it.
		_, err := s.p.BoardRows(s.ctx, "1", []tracker.IssueID{"10101", "10199"}, rule)
		if got, want := isRefusal(err), len(rule.Sprints) > 0; got != want {
			t.Fatalf("the refused match under %+v: %v; typed refusal %v, want %v", rule, err, got, want)
		}
		if got := routeCounts(s.e, since); got[sprintRoute]+got[boardSprintRoute] != 0 || got[matchRoute] != 1 {
			t.Fatalf("the refused match made %v; want one match and no sprint read", got)
		}
	}
}

package jiraprovider

import (
	"errors"
	"net/http"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// A rows read by ID whose rule names a sprint the board issue list refuses
// (measured: a sprint ID that names nothing answers 400 there; here one
// deleted since the listing) fails with tracker.ErrRuleRefused, the 400
// wrapped. The same read under the listing's own rule reads the rows, and
// a 400 under a rule that names no sprint is the tracker's answer, not a
// refusal of the rule.
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
	gone := listing.Rule
	gone.Sprints = append(gone.Sprints[:len(gone.Sprints):len(gone.Sprints)], "999")
	_, err := s.p.BoardRows(s.ctx, "1", listing.IDs, gone)
	status, ok := errors.AsType[*trackerapi.StatusError](err)
	if !errors.Is(err, tracker.ErrRuleRefused) || !ok || status.Status != http.StatusBadRequest {
		t.Fatalf("the rows read under a rule naming a deleted sprint: %v; want the typed refusal wrapping the 400", err)
	}

	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: "GET rest/software/1.0/board/{board}/issue", Kind: trackerfake.FaultRefuse,
		Status: http.StatusBadRequest, Body: []byte(`{"errorMessages":["Bad request"]}`)}); err != nil {
		t.Fatal(err)
	}
	_, err = s.p.BoardRows(s.ctx, "1", listing.IDs, tracker.Eligibility{Window: window})
	if status, ok := errors.AsType[*trackerapi.StatusError](err); errors.Is(err, tracker.ErrRuleRefused) || !ok || status.Status != http.StatusBadRequest {
		t.Fatalf("a 400 under a rule naming no sprint: %v; want the 400 alone", err)
	}
}

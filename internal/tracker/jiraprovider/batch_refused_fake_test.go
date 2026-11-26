package jiraprovider

import (
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// A batch write Jira refuses whole, a rank, a sprint move or a backlog
// move as a person's Board makes it, says Jira's sentence for each issue,
// as any other refused write does: not the request it refused, whose URL
// names the site's host.
func TestFakeABatchRefusedWholeSaysJirasSentence(t *testing.T) {
	s := newScenario(t, false)
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}
	two := []tracker.IssueID{"10101", "10102"}
	for _, c := range []struct {
		name, route string
		write       func() tracker.Result
	}{
		{"rank", "PUT rest/agile/1.0/issue/rank", func() tracker.Result {
			return s.p.Rank(s.ctx, tracker.RankInput{Board: "1", Issues: two, Next: "10103"})
		}},
		{"sprint move", "POST rest/agile/1.0/sprint/{sprint}/issue", func() tracker.Result {
			return s.p.MoveToSprint(s.ctx, tracker.SprintMoveInput{Board: "1", Sprint: "2", Issues: two})
		}},
		{"backlog move", "POST rest/agile/1.0/backlog/{board}/issue", func() tracker.Result {
			return s.p.MoveToSprint(s.ctx, tracker.SprintMoveInput{Board: "1", Issues: two})
		}},
	} {
		said := "Jira cannot take this " + c.name + "."
		if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: c.route, Kind: trackerfake.FaultRefuse, Times: 1,
			Status: http.StatusBadRequest, Body: []byte(`{"errorMessages":["` + said + `"],"errors":{}}`)}); err != nil {
			t.Fatal(err)
		}
		res := c.write()
		if len(res.Parts) != len(two) {
			t.Fatalf("%s: %d parts", c.name, len(res.Parts))
		}
		for _, p := range res.Parts {
			r, ok := p.Outcome.Refusal()
			if !ok || r.Reason != tracker.ReasonTracker || r.Message != said || strings.Contains(r.Message, "http") {
				t.Errorf("%s of %s: %s %+v; want refused saying %q", c.name, p.Issue, p.Outcome.State(), r, said)
			}
		}
	}
}

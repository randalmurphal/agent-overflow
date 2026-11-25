package trackerapp

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
)

// The tracker's refusals of a worklog read reach the refresh's failure
// with their sentence, from what the fake answers on the wire: Jira's 404
// for a person it does not have and its 403 for the lookup, a person whose
// profile hides the time zone (Jira answers 200), its 400 for the search,
// and Tempo's 403 and 400. None is the connection's problem.
func TestWorklogRefusalsOnTheWire(t *testing.T) {
	t.Parallel()
	s := newTimeScenario(t)
	jira := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
	tempoOrigin := trackerfake.Origin{Tracker: trackerfake.TrackerTempo}
	refuse := func(o trackerfake.Origin, route string, status int) func() {
		return func() {
			body := json.RawMessage(`{"errorMessages":["refused"],"errors":{}}`)
			if o.Tracker == trackerfake.TrackerTempo {
				body = json.RawMessage(`{"errors":[{"message":"refused"}]}`)
			}
			if err := s.e.AddFault(trackerfake.Fault{Origin: o, Route: route, Kind: trackerfake.FaultRefuse, Times: 1, Status: status, Body: body}); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func(what, author string, arm func(), kind FailureKind, message string) {
		t.Helper()
		arm()
		got := must[WorklogDays](t)(s.svc.RefreshWorklogs(t.Context(), s.conn.ID, author, s.from, s.to))
		if got.Failure == nil || got.Failure.Kind != string(kind) || got.Failure.Message != message {
			t.Fatalf("%s: the refresh answered %+v; want %s %q", what, got.Failure, kind, message)
		}
		s.svc.mu.Lock()
		problem := s.svc.problems[s.conn.ID]
		s.svc.mu.Unlock()
		if problem.Kind != "" {
			t.Fatalf("%s: the connection's problem is %+v", what, problem)
		}
	}
	read("a person Jira does not have", "acc-nobody", func() {}, FailureTracker,
		"Jira has no person with account ID acc-nobody, or the token's user may not see them; choose another person")
	read("a person whose profile hides the time zone", "acc-user3", func() {}, FailureTracker,
		"Jira does not show which time zone acc-user3 is in, so Agent Overflow cannot tell which day their time falls on; choose another person, or ask them to show their time zone in their Atlassian profile")
	read("the person's lookup refused", "acc-user2", refuse(jira, "GET rest/api/3/user", 403), FailureForbidden,
		"Jira refused to look up the person acc-user2 (HTTP 403): the token's user needs the Browse users and groups permission to read another person's time")
	read("the search refused", "acc-user2", refuse(jira, "POST rest/api/3/search/jql", 400), FailureTracker,
		"Jira refused the search for acc-user2's worklogs from "+s.from+" to "+s.to+" (HTTP 400); check the person and the days")
	s.addTempoOn(t)
	read("Tempo's 403", "acc-user2", refuse(tempoOrigin, "GET 4/worklogs/user/{account}", 403), FailureForbidden,
		"Tempo refused the worklogs of acc-user2 (HTTP 403): the linked Tempo connection's token may not read that person's time")
	read("Tempo's 400", "acc-user2", refuse(tempoOrigin, "GET 4/worklogs/user/{account}", 400), FailureTracker,
		"Tempo refused the worklogs of acc-user2 from "+s.from+" to "+s.to+" (HTTP 400); check the person and the days")
}

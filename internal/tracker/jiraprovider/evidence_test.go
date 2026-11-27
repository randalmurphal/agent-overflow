package jiraprovider

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// evidenceScenario is the scenario's site with WEB-1 holding comments of
// User 2, of an app and of a customer, and 25 worklogs of User 1 an hour
// apart from start, more than Jira embeds; requests records the routes
// each request reached.
func evidenceScenario(t *testing.T, withTempo bool) (s *scenario, start time.Time, requests func() []string) {
	t.Helper()
	var mu sync.Mutex
	var routes []string
	s = newEditedScenario(t, func(r trackerfake.Request) {
		mu.Lock()
		defer mu.Unlock()
		routes = append(routes, r.Route)
	}, func(site *trackerfake.JiraSite) {
		if !withTempo {
			site.Tempo = nil
		}
		site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-app", DisplayName: "Automation", AccountType: "app"},
			trackerfake.JiraUser{AccountID: "acc-customer", DisplayName: "Customer 1", AccountType: "customer"})
		is := &site.Issues[0]
		start = is.Created
		is.Comments = append(is.Comments, trackerfake.JiraComment{ID: "40002", Author: "acc-app", Body: scenarioDoc("deployed"), Created: start},
			trackerfake.JiraComment{ID: "40003", Author: "acc-customer", Body: scenarioDoc("thanks"), Created: start})
		for i := range 25 {
			is.Worklogs = append(is.Worklogs, trackerfake.JiraWorklog{ID: fmt.Sprint(50001 + i), Author: "acc-user1", Started: start.Add(time.Duration(i) * time.Hour),
				TimeSpent: 600})
		}
	})
	return s, start, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(routes)
	}
}

// Evidence reads an issue's comments and its worklogs while Jira's own are
// the time, the worklogs Jira did not embed through one read of the
// issue's worklog list from the issue's bound; an app's and a customer's
// comments are left out, the authors named, and an issue Jira does not
// return is left out.
func TestEvidenceReadsCommentsAndJirasWorklogs(t *testing.T) {
	s, start, requests := evidenceScenario(t, false)
	if caps := s.p.Capabilities().Evidence; caps != (tracker.EvidenceKinds{Comments: true, Worklogs: true}) {
		t.Fatalf("evidence kinds %+v", caps)
	}
	from := start.Add(10 * time.Hour)
	ev, err := s.p.Evidence(s.ctx, tracker.EvidenceQuery{Issues: []tracker.EvidenceIssue{{Issue: "10101", WorklogsFrom: from}, {Issue: "10102"}, {Issue: "99999"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Issues) != 2 || ev.Issues[0].Issue != "10101" || ev.Issues[1].Issue != "10102" || ev.Issues[0].Updated.IsZero() {
		t.Fatalf("issues %+v", ev.Issues)
	}
	one := ev.Issues[0]
	if len(one.Comments) != 1 || one.Comments[0].ID != "40001" || one.Comments[0].Author != "acc-user2" {
		t.Errorf("comments %+v", one.Comments)
	}
	if len(one.Worklogs) != 15 || one.Worklogs[0].ID != "50011" || !one.Worklogs[0].At.Equal(from) || one.Worklogs[0].Spent != 10*time.Minute {
		t.Errorf("%d worklogs from the bound, first %+v", len(one.Worklogs), one.Worklogs[0])
	}
	if len(ev.Issues[1].Comments)+len(ev.Issues[1].Worklogs) != 0 {
		t.Errorf("WEB-2 %+v", ev.Issues[1])
	}
	var users []string
	for _, u := range ev.Users {
		users = append(users, string(u.ID)+":"+u.Name)
	}
	if fmt.Sprint(users) != "[acc-user2:User 2 acc-user1:User 1]" {
		t.Errorf("users %v", users)
	}
	if got := fmt.Sprint(requests()); got != "[POST rest/api/3/issue/bulkfetch GET rest/api/3/issue/{issue}/worklog]" {
		t.Errorf("requests %s", got)
	}
}

// While Tempo is the time, Evidence reads no worklog: the bulk fetch asks
// for none, and no issue has any.
func TestEvidenceReadsNoWorklogWhileTempoIsTheTime(t *testing.T) {
	s, _, requests := evidenceScenario(t, true)
	if caps := s.p.Capabilities().Evidence; caps != (tracker.EvidenceKinds{Comments: true}) {
		t.Fatalf("evidence kinds %+v", caps)
	}
	ev, err := s.p.Evidence(s.ctx, tracker.EvidenceQuery{Issues: []tracker.EvidenceIssue{{Issue: "10101"}}})
	if err != nil || len(ev.Issues) != 1 || len(ev.Issues[0].Comments) != 1 || ev.Issues[0].Worklogs != nil {
		t.Fatalf("evidence %+v, %v", ev, err)
	}
	if got := fmt.Sprint(requests()); got != "[POST rest/api/3/issue/bulkfetch]" {
		t.Errorf("requests %s", got)
	}
}

// A query Evidence cannot send is refused before any request.
func TestEvidenceRefusesAnInvalidQuery(t *testing.T) {
	s, _, requests := evidenceScenario(t, false)
	for name, q := range map[string]tracker.EvidenceQuery{
		"no issue":    {},
		"a key":       {Issues: []tracker.EvidenceIssue{{Issue: "WEB-1"}}},
		"twice":       {Issues: []tracker.EvidenceIssue{{Issue: "10101"}, {Issue: "10101"}}},
		"over a read": {Issues: make([]tracker.EvidenceIssue, tracker.MaxEvidenceIssues+1)},
	} {
		if _, err := s.p.Evidence(s.ctx, q); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(requests()); n != 0 {
		t.Errorf("%d requests", n)
	}
}

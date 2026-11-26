package trackerfake

import (
	"errors"
	"strings"
	"testing"
)

func TestRequestLogBound(t *testing.T) {
	var l requestLog
	for range maxLogged + 10 {
		l.add(Request{Method: "GET"})
	}
	if len(l.entries) != maxLogged || l.dropped != 10 || l.entries[0].Seq != 11 {
		t.Fatalf("%d entries, %d dropped, first %d", len(l.entries), l.dropped, l.entries[0].Seq)
	}
	l.update(Request{Seq: 5, Method: "POST"})
	if slicesHaveMethod(l.entries, "POST") {
		t.Fatal("an evicted entry was updated back in")
	}
	l.reset()
	if len(l.entries) != 0 || l.dropped != 0 {
		t.Fatal("reset kept entries")
	}
	if r := l.add(Request{}); r.Seq != maxLogged+11 {
		t.Fatalf("numbering restarted at %d", r.Seq)
	}
}

func slicesHaveMethod(rs []Request, method string) bool {
	for _, r := range rs {
		if r.Method == method {
			return true
		}
	}
	return false
}

func TestRequestsSinceAndCopies(t *testing.T) {
	w := newWorld(t)
	w.jira(t, "GET", "/rest/api/3/myself", "")
	w.jira(t, "POST", "/rest/api/3/issue/WEB-1/comment", `{"body":`+testDoc+`}`)
	all := w.e.Requests(0)
	if len(all.Requests) != 2 || all.Requests[1].Route != commentRoute || all.Requests[1].Status != 201 || all.Requests[1].Tenant != testSite {
		t.Fatalf("log %+v", all)
	}
	later := w.e.Requests(all.Requests[0].Seq)
	if len(later.Requests) != 1 || later.Requests[0].Seq != all.Requests[1].Seq {
		t.Fatalf("since %d: %+v", all.Requests[0].Seq, later)
	}
	later.Requests[0].Body[0] = 'X'
	if w.e.Requests(0).Requests[1].Body[0] == 'X' {
		t.Fatal("a returned entry shares the log's memory")
	}
}

// TestSeededIDsStayUnique seeds a Linear ID of the generated form; the
// IDs the engine generates afterwards, in the same seed and later, skip
// it.
func TestSeededIDsStayUnique(t *testing.T) {
	e := New(Options{})
	defer e.Close()
	f := testFixture()
	taken := mediaID(firstGeneratedID + 1)
	f.Linear[0].Users[0].ID, f.Linear[0].Viewer = taken, taken
	f.Linear[0].Teams[0].Members = []string{taken}
	f.Linear[0].States = append(f.Linear[0].States, LinearState{Team: "t1", Name: "Done", Type: "completed"})
	if err := e.Seed(f); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.Snapshot().Linear[0].States {
		if s.ID == taken {
			t.Fatalf("a generated ID repeats the seeded %s", taken)
		}
	}
}

// TestLinearCharging follows the measured rule: an issue list costs by
// what it returns, a catalog list by its page size.
func TestLinearCharging(t *testing.T) {
	w := newWorld(t)
	cost := func(op, query string) string {
		r := w.linear(t, testLinearKey, linearBody(op, query, nil))
		if r.status != 200 {
			t.Fatalf("%s answered %d %s", op, r.status, r.body)
		}
		return r.header.Get("X-Complexity")
	}
	small, large := cost("Teams", `query Teams { teams(first: 5) { nodes { id } } }`), cost("Teams", `query Teams { teams(first: 50) { nodes { id } } }`)
	if small == large {
		t.Fatalf("a catalog list cost %s at first 5 and at first 50", small)
	}
	small, large = cost("Issues", `query Issues { issues(first: 5) { nodes { id } } }`), cost("Issues", `query Issues { issues(first: 250) { nodes { id } } }`)
	if small != large {
		t.Fatalf("an issue list cost %s at first 5 and %s at first 250", small, large)
	}
}

func TestJQLForms(t *testing.T) {
	for _, q := range []string{
		`updated >= 1700000000000 ORDER BY updated ASC, id ASC`,
		`updated > 1700000000000`,
		`id > 10 AND id != 11`,
		`id in (10101, 10102)`,
		`key in ("WEB-1", "WEB-2") ORDER BY key DESC`,
		`statusCategory = Done OR statusCategory != "To Do"`,
		`sprint in (1, 2)`,
		`sprint in openSprints() AND assignee = currentUser() AND status = "In Review"`,
		`text ~ "login error"`,
		`project = "WEB" AND (worklogAuthor = "acc-user1" AND worklogDate >= "2026-11-01" AND worklogDate <= "2026-11-30")`,
		`ORDER BY created DESC`,
		``,
	} {
		if _, err := parseJQL(q, "acc-user1"); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func TestJQLRefusals(t *testing.T) {
	for _, q := range []string{
		`assignee = "acc-user1"`,
		`assignee = currentUser("x")`,
		`sprint in closedSprints()`,
		`status = 10001`,
		`updated >= -1d`,
		`id < 10`,
		`statusCategory = Blocked`,
		`text ~ "a AND b"`,
		`text ~ "wild*"`,
		`project = WEB ORDER BY rank`,
		`key in ("WEB-1"`,
		`(id > 1`,
		`summary ~ "x"`,
		`worklogDate >= "tomorrow"`,
	} {
		_, err := parseJQL(q, "acc-user1")
		var je *jqlError
		if !errors.As(err, &je) {
			t.Errorf("%q: %v, want a JQL refusal", q, err)
		}
	}
}

// currentUser() with no one asking is not a form the fake reads: a
// board's sub-query is read when it is seeded.
func TestJQLCurrentUserNeedsAViewer(t *testing.T) {
	var je *jqlError
	if _, err := parseJQL(`assignee = currentUser()`, ""); !errors.As(err, &je) {
		t.Errorf("currentUser() with no viewer = %v, want a JQL refusal", err)
	}
}

func TestJQLRefusalIsUnhandled(t *testing.T) {
	w := newWorld(t)
	r := w.jira(t, "POST", "/rest/api/3/search/jql", `{"jql":"id < 10"}`)
	if r.status != 501 || !strings.Contains(r.body, "not implemented") {
		t.Fatalf("answered %d %s", r.status, r.body)
	}
	r = w.jira(t, "POST", "/rest/api/3/search/jql", `{"jql":"key in (\"WEB-1\")","fields":["summary"]}`)
	if r.status != 200 || !strings.Contains(r.body, `"key":"WEB-1"`) {
		t.Fatalf("answered %d %s", r.status, r.body)
	}
}

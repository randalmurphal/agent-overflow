package jiraprovider

import (
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// People lists the site's active people the text names, leaving out the
// accounts that are no person's; asks the project's assignable users when
// the site's search names none or is withheld, and only when a project is
// named; asks nothing for empty text; and fails on any other failure.
func TestPeople(t *testing.T) {
	const site = `[{"accountId":"acc-u1","displayName":"User 1","active":true,"accountType":"atlassian"},
		{"accountId":"acc-app","displayName":"User App","active":true,"accountType":"app"},
		{"accountId":"acc-gone","displayName":"User Gone","active":false,"accountType":"atlassian"},
		{"accountId":"acc-cust","displayName":"User Customer","active":true,"accountType":"customer"}]`
	// What each project's assignable search answers: User 2 may be
	// assigned in both, User 3 in the second alone.
	assignable := map[string]string{
		"10000": `[{"accountId":"acc-u2","displayName":"User 2","active":true,"accountType":"atlassian"}]`,
		"10001": `[{"accountId":"acc-u2","displayName":"User 2","active":true,"accountType":"atlassian"},
			{"accountId":"acc-u3","displayName":"User 3","active":true,"accountType":"atlassian"}]`,
	}
	u1, u2, u3 := tracker.User{ID: "acc-u1", Name: "User 1", Active: true}, tracker.User{ID: "acc-u2", Name: "User 2", Active: true}, tracker.User{ID: "acc-u3", Name: "User 3", Active: true}
	one, two := []tracker.ProjectID{"10000"}, []tracker.ProjectID{"10000", "10001"}
	for _, c := range []struct {
		name         string
		projects     []tracker.ProjectID
		text         string
		status       int
		body         string
		want         []tracker.User
		more         bool
		searches     int
		assignables  int
		fails        bool
		assignStatus int
	}{
		{name: "the site's people", projects: one, text: " user ", status: 200, body: site, want: []tracker.User{u1}, searches: 1},
		{name: "no person named, with a project", projects: one, text: "user", status: 200, body: `[]`, want: []tracker.User{u2}, more: true, searches: 1, assignables: 1},
		{name: "only accounts that are no person's", projects: one, text: "user", status: 200,
			body: `[{"accountId":"acc-app","displayName":"User App","active":true,"accountType":"app"}]`, want: []tracker.User{u2}, more: true, searches: 1, assignables: 1},
		{name: "the search withheld, with a project", projects: one, text: "user", status: 403, body: `{"errorMessages":["You do not have permission to browse users."]}`,
			want: []tracker.User{u2}, more: true, searches: 1, assignables: 1},
		{name: "no person named, with two projects", projects: two, text: "user", status: 200, body: `[]`, want: []tracker.User{u2, u3}, more: true, searches: 1, assignables: 2},
		{name: "the site's people, with two projects", projects: two, text: "user", status: 200, body: site, want: []tracker.User{u1}, searches: 1},
		{name: "no person named, with no project", text: "user", status: 200, body: `[]`, more: true, searches: 1},
		{name: "the search withheld, with no project", text: "user", status: 403, body: `{}`, more: true, searches: 1},
		{name: "empty text", projects: one, text: "  ", more: true},
		{name: "the search failing", projects: one, text: "user", status: 500, body: `{}`, searches: 1, fails: true},
		{name: "the assignable search failing", projects: one, text: "user", status: 200, body: `[]`, searches: 1, assignables: 1, fails: true, assignStatus: 500},
		{name: "a project that is no ID", projects: []tracker.ProjectID{"10000", "WEB 1"}, text: "user", fails: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newSite(t)
			searches, assignables := 0, 0
			f.On("GET", "/rest/api/3/user/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
				searches++
				if q := r.URL.Query().Get("query"); q != "user" {
					t.Errorf("searched %q; want the text trimmed", q)
				}
				jiratest.Reply(c.status, c.body)(w, r, b)
			})
			f.On("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
				assignables++
				q := r.URL.Query()
				body, ok := assignable[q.Get("project")]
				if !ok || q.Get("project") != string(c.projects[assignables-1]) || q.Get("query") != "user" {
					t.Errorf("asked the assignable users of %q for %q", q.Get("project"), q.Get("query"))
				}
				if c.assignStatus != 0 {
					jiratest.Reply(c.assignStatus, `{}`)(w, r, b)
					return
				}
				jiratest.Reply(200, body)(w, r, b)
			})
			p := newProvider(t, f, nil)
			got, err := p.People(t.Context(), c.projects, c.text)
			if (err != nil) != c.fails {
				t.Fatalf("People = %+v, %v; want failing %t", got, err, c.fails)
			}
			if err == nil && (!reflect.DeepEqual(got.Users, c.want) || got.More != c.more) {
				t.Fatalf("People = %+v; want %+v, more %t", got, c.want, c.more)
			}
			if searches != c.searches || assignables != c.assignables {
				t.Fatalf("%d site searches and %d assignable searches; want %d and %d", searches, assignables, c.searches, c.assignables)
			}
		})
	}
}

// A site search that fills its window may hold more people than listed.
func TestPeopleSaysWhenTheSiteMayHoldMore(t *testing.T) {
	f, _ := newSite(t)
	f.On("GET", "/rest/api/3/user/search", jiratest.Reply(200, usersJSON(peopleWindow)))
	got, err := newProvider(t, f, nil).People(t.Context(), nil, "user")
	if err != nil || len(got.Users) != pickLimit || !got.More {
		t.Fatalf("People = %d users, more %t, %v; want %d and more", len(got.Users), got.More, err, pickLimit)
	}
}

// When the site's search names nobody, the assignable users of the first
// peopleProjects projects are asked, and of no project past them.
func TestPeopleAsksTheAssignableUsersOfAFewProjects(t *testing.T) {
	f, _ := newSite(t)
	f.On("GET", "/rest/api/3/user/search", jiratest.Reply(200, `[]`))
	var asked []string
	f.On("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		asked = append(asked, r.URL.Query().Get("project"))
		jiratest.Reply(200, `[]`)(w, r, b)
	})
	var projects, want []tracker.ProjectID
	for i := range peopleProjects + 2 {
		projects = append(projects, tracker.ProjectID(strconv.Itoa(10000+i)))
	}
	want = projects[:peopleProjects]
	got, err := newProvider(t, f, nil).People(t.Context(), projects, "user")
	if err != nil || len(got.Users) != 0 || !got.More || len(asked) != len(want) {
		t.Fatalf("People = %+v, %v after asking %v; want nobody, more, and the first %d projects asked", got, err, asked, peopleProjects)
	}
	for i, p := range want {
		if asked[i] != string(p) {
			t.Fatalf("asked %v; want %v", asked, want)
		}
	}
}

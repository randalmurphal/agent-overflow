package jiraprovider

import (
	"net/http"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// PersonNamed finds the one person with a display name, case ignored;
// names nobody when none or several have it, or when Jira withholds the
// user search; and fails on any other failure of the search.
func TestPersonNamed(t *testing.T) {
	for _, c := range []struct {
		name   string
		ask    string
		status int
		body   string
		want   tracker.User
		ok     bool
		fails  bool
	}{
		{"one person", "user 2", 200, usersJSON(3), tracker.User{ID: "acc-u2", Name: "User 2", Active: true}, true, false},
		{"no person", "User 9", 200, usersJSON(3), tracker.User{}, false, false},
		{"two people", "User 1", 200, `[{"accountId":"acc-a","displayName":"User 1","active":true,"accountType":"atlassian"},
			{"accountId":"acc-b","displayName":"user 1","active":true,"accountType":"atlassian"}]`, tracker.User{}, false, false},
		{"an app with the name", "User 1", 200, `[{"accountId":"acc-a","displayName":"User 1","active":true,"accountType":"app"}]`, tracker.User{}, false, false},
		{"the search withheld", "User 2", 403, `{"errorMessages":["You do not have permission to browse users."]}`, tracker.User{}, false, false},
		{"the search failing", "User 2", 500, `{}`, tracker.User{}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newSite(t)
			searches := 0
			f.On("GET", "/rest/api/3/user/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
				searches++
				if q := r.URL.Query().Get("query"); q != c.ask {
					t.Errorf("searched %q; want %q", q, c.ask)
				}
				// The walk's next page is empty: the search is at its end.
				if r.URL.Query().Get("startAt") != "0" {
					jiratest.Reply(200, `[]`)(w, r, b)
					return
				}
				jiratest.Reply(c.status, c.body)(w, r, b)
			})
			p := newProvider(t, f, nil)
			got, ok, err := p.PersonNamed(t.Context(), c.ask)
			if (err != nil) != c.fails || ok != c.ok || got != c.want {
				t.Fatalf("PersonNamed = %+v, %t, %v; want %+v, %t, failing %t", got, ok, err, c.want, c.ok, c.fails)
			}
			if searches == 0 {
				t.Fatal("no search was made")
			}
		})
	}
}

package forgefake

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The management fixture is validated at seed: what belongs to the other
// forge, an unknown feature, a pull label or team the repository lacks.
// A seeded closed GitLab merge request gets its state event.
func TestManageSeedValidation(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		`{"repos":[{"forge":"gitlab","project":"g/p","teams":[{"slug":"core"}]}]}`,
		`{"repos":[{"forge":"github","project":"o/r","serverLacks":["rereview"]}]}`,
		`{"repos":[{"forge":"gitlab","project":"g/p","serverLacks":["epics"]}]}`,
		`{"repos":[{"forge":"github","project":"o/r","pulls":[{"number":1,"title":"t","labels":["bug"]}]}]}`,
		`{"repos":[{"forge":"github","project":"o/r","teams":[{"slug":"core"}],"pulls":[{"number":1,"title":"t","teamReviewers":["other/core"]}]}]}`,
		`{"repos":[{"forge":"github","project":"o/r","pulls":[{"number":1,"title":"t","changesOverridden":true}]}]}`,
		`{"repos":[{"forge":"gitlab","project":"g/p","labels":[{"name":"a,b"}]}]}`,
	} {
		if _, err := seedJSON(New(Options{APIToken: testAPIToken}), bad); err == nil {
			t.Errorf("seeded %s", bad)
		}
	}
	got, err := seedJSON(New(Options{APIToken: testAPIToken}), `{"repos":[{"forge":"gitlab","project":"g/p","pulls":[{"number":1,"title":"t","author":"a","state":"closed","closedBy":"b","closedAt":"2026-01-02T00:00:00Z"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if events := got.Repos[0].Pulls[0].StateEvents; len(events) != 1 || events[0].State != "closed" || events[0].Actor != "b" || events[0].ID == 0 {
		t.Fatalf("state events = %+v", events)
	}
}

const manageFixture = `{
  "viewer": "alice",
  "repos": [
    {"forge": "github", "project": "acme/m", "organization": true, "members": [{"login": "bob"}], "teams": [{"slug": "core"}], "labels": [{"name": "bug"}],
     "pulls": [{"number": 1, "title": "T", "author": "octo", "reviewers": ["bob"]}]},
    {"forge": "gitlab", "project": "grp/m", "id": 90, "members": [{"login": "bob", "id": 11}], "labels": [{"name": "bug"}],
     "pulls": [{"number": 1, "title": "T", "author": "octo", "reviewers": ["bob"], "changeRequests": ["bob"]}]}
  ]
}`

// Each write route decodes its body strictly, refuses what its forge
// refuses, and a refusal set on the route answers before the handler,
// changing nothing.
func TestManageRoutesRefuseAndDecodeStrictly(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, manageFixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	gl := http.Header{"Private-Token": {testAPIToken}}
	pull := func(project string) *Pull {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, r := range e.repos {
			if r.Project == project {
				return r.pull(1)
			}
		}
		t.Fatalf("no %s", project)
		return nil
	}

	for _, tc := range []struct {
		name, method, host, path, body string
		header                         http.Header
		status                         int
	}{
		{"unknown reviewers key", "POST", "github.com", "/github/rest/repos/acme/m/pulls/1/requested_reviewers", `{"reviewers":["bob"],"draft":true}`, nil, http.StatusNotFound},
		{"author as reviewer", "POST", "github.com", "/github/rest/repos/acme/m/pulls/1/requested_reviewers", `{"reviewers":["octo"]}`, nil, http.StatusUnprocessableEntity},
		{"non-member reviewer", "POST", "github.com", "/github/rest/repos/acme/m/pulls/1/requested_reviewers", `{"reviewers":["eve"]}`, nil, http.StatusUnprocessableEntity},
		{"unknown team", "POST", "github.com", "/github/rest/repos/acme/m/pulls/1/requested_reviewers", `{"team_reviewers":["ops"]}`, nil, http.StatusUnprocessableEntity},
		{"unknown assignees key", "POST", "github.com", "/github/rest/repos/acme/m/issues/1/assignees", `{"assignee":"bob"}`, nil, http.StatusNotFound},
		{"unknown update key", "PUT", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fm/merge_requests/1", `{"milestone_id":3}`, gl, http.StatusNotFound},
		{"empty title", "PUT", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fm/merge_requests/1", `{"title":" "}`, gl, http.StatusBadRequest},
		{"unknown state event", "PUT", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fm/merge_requests/1", `{"state_event":"merge"}`, gl, http.StatusNotFound},
	} {
		if got := httpRequest(t, srv, tc.method, tc.host, tc.path, tc.body, tc.header); got.status != tc.status {
			t.Errorf("%s: %d %s, want %d", tc.name, got.status, got.body, tc.status)
		}
	}
	if p := pull("acme/m"); !slices.Equal(p.Reviewers, []string{"bob"}) || len(p.TeamReviewers) != 0 || len(p.Assignees) != 0 {
		t.Fatalf("GitHub pull after refusals = %+v %+v %+v", p.Reviewers, p.TeamReviewers, p.Assignees)
	}
	if p := pull("grp/m"); p.Title != "T" {
		t.Fatalf("GitLab title after refusals = %q", p.Title)
	}

	// A refusal on the route answers in place of the handler.
	if err := e.SetRefusal(Refusal{Route: "gh api remove reviewers", Status: 422, Times: 1, Body: []byte(`{"message":"refused"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := httpRequest(t, srv, "DELETE", "github.com", "/github/rest/repos/acme/m/pulls/1/requested_reviewers", `{"reviewers":["bob"]}`, nil); got.status != 422 || !slices.Equal(pull("acme/m").Reviewers, []string{"bob"}) {
		t.Fatalf("refused remove = %d, reviewers %v", got.status, pull("acme/m").Reviewers)
	}
	if got := httpRequest(t, srv, "DELETE", "github.com", "/github/rest/repos/acme/m/pulls/1/requested_reviewers", `{"reviewers":["bob"]}`, nil); got.status != 200 || len(pull("acme/m").Reviewers) != 0 {
		t.Fatalf("remove after the refusal = %d, reviewers %v", got.status, pull("acme/m").Reviewers)
	}
	if err := e.SetRefusal(Refusal{Route: "glab graphql PRBypassChanges", Status: 200, Times: 1, Body: []byte(`{"errors":[{"message":"refused"}]}`)}); err != nil {
		t.Fatal(err)
	}
	bypass := func() httpAnswer {
		return httpRequest(t, srv, "POST", "gitlab.com", "/gitlab/api/graphql", mustJSON(t, map[string]any{
			"query":         "mutation PRBypassChanges($projectPath: ID!, $iid: String!) { x }",
			"operationName": "PRBypassChanges", "variables": map[string]any{"projectPath": "grp/m", "iid": "1"},
		}), gl)
	}
	if got := bypass(); !strings.Contains(got.body, "refused") || pull("grp/m").ChangesOverridden {
		t.Fatalf("refused bypass = %s, overridden %v", got.body, pull("grp/m").ChangesOverridden)
	}
	if got := bypass(); !strings.Contains(got.body, `"errors":[]`) || !pull("grp/m").ChangesOverridden {
		t.Fatalf("bypass = %s, overridden %v", got.body, pull("grp/m").ChangesOverridden)
	}

	// The body of a REST write is recorded on its invocation.
	log := e.Invocations(0).Invocations
	var recorded string
	for _, inv := range log {
		if inv.Route == "gh api remove reviewers" {
			recorded = string(inv.Body)
		}
	}
	if recorded != `{"reviewers":["bob"]}` {
		t.Fatalf("recorded body = %q", recorded)
	}
}

package forgefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		`{"repos":[{"forge":"gitlab","project":"g/p","stateEvents":[]}]}`,
		`{"repos":[{"forge":"gitlab","project":"g/p","pulls":[{"number":1,"title":"t","stateEvents":[{"state":"locked"}]}]}]}`,
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
		{"empty update", "PUT", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fm/merge_requests/1", `{}`, gl, http.StatusBadRequest},
		{"null update", "PUT", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fm/merge_requests/1", `null`, gl, http.StatusBadRequest},
		{"reviewer ids, no longer sent", "PUT", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fm/merge_requests/1", `{"reviewer_ids":[11]}`, gl, http.StatusNotFound},
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
			"query":         testDocument("mutation", "PRBypassChanges", "($projectPath: ID!, $iid: String!)"),
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

func glabGraphQL(t *testing.T, srv *httptest.Server, op, kind string, vars map[string]any) map[string]any {
	t.Helper()
	params := []string{}
	for name := range vars {
		params = append(params, "$"+name+": String")
	}
	slices.Sort(params)
	got := httpRequest(t, srv, "POST", "gitlab.com", "/gitlab/api/graphql", mustJSON(t, map[string]any{
		"query": testDocument(kind, op, "("+strings.Join(params, ", ")+")"), "operationName": op, "variables": vars,
	}), http.Header{"Private-Token": {testAPIToken}})
	var out map[string]any
	if err := json.Unmarshal([]byte(got.body), &out); err != nil {
		t.Fatalf("%s: %d %s", op, got.status, got.body)
	}
	return out
}

// The GitLab set mutations append and remove by username or label id,
// skip what GitLab skips (a non-member, an unknown label id), answer
// the list as it stands, and deny a viewer without adminMergeRequest;
// the draft mutation moves the title's prefix; a reopen is recorded as
// the state event "opened".
func TestGitLabSetMutations(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	fixture, err := seedJSON(e, manageFixture)
	if err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	bug := fixture.Repos[1].Labels[0].ID
	pull := func() *Pull {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, r := range e.repos {
			if r.Project == "grp/m" {
				return r.pull(1)
			}
		}
		return nil
	}
	mr := map[string]any{"projectPath": "grp/m", "iid": "1"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range mr {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	got := glabGraphQL(t, srv, "PRSetAssignees", "mutation", with(map[string]any{"usernames": []string{"bob", "eve"}, "mode": "APPEND"}))
	if nodes := mustJSON(t, got["data"].(map[string]any)["mergeRequestSetAssignees"].(map[string]any)["mergeRequest"]); nodes != `{"assignees":{"nodes":[{"username":"bob"}],"pageInfo":{"hasNextPage":false}}}` {
		t.Fatalf("append assignees = %s", nodes)
	}
	glabGraphQL(t, srv, "PRSetReviewers", "mutation", with(map[string]any{"usernames": []string{"bob"}, "mode": "REMOVE"}))
	if p := pull(); len(p.Reviewers) != 0 || len(p.ChangeRequests) != 0 {
		t.Fatalf("after removing bob: reviewers %v, change requests %v", p.Reviewers, p.ChangeRequests)
	}
	glabGraphQL(t, srv, "PRSetLabels", "mutation", with(map[string]any{"labelIds": []string{bug, "gid://gitlab/ProjectLabel/999"}, "mode": "APPEND"}))
	if p := pull(); !slices.Equal(p.Labels, []string{"bug"}) {
		t.Fatalf("labels = %v", p.Labels)
	}
	glabGraphQL(t, srv, "PRSetDraft", "mutation", with(map[string]any{"draft": true}))
	if p := pull(); !p.Draft || p.Title != "Draft: T" {
		t.Fatalf("draft = %v %q", p.Draft, p.Title)
	}
	glabGraphQL(t, srv, "PRSetDraft", "mutation", with(map[string]any{"draft": false}))
	if p := pull(); p.Draft || p.Title != "T" {
		t.Fatalf("ready = %v %q", p.Draft, p.Title)
	}
	gl := http.Header{"Private-Token": {testAPIToken}}
	for _, event := range []string{"close", "reopen"} {
		if got := httpRequest(t, srv, "PUT", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fm/merge_requests/1", `{"state_event":"`+event+`"}`, gl); got.status != 200 {
			t.Fatalf("%s: %d %s", event, got.status, got.body)
		}
	}
	events := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fm/merge_requests/1/resource_state_events?per_page=100", "", gl)
	if !strings.Contains(events.body, `"state":"closed"`) || !strings.Contains(events.body, `"state":"opened"`) {
		t.Fatalf("state events = %s", events.body)
	}

	if _, err := seedJSON(e, strings.Replace(manageFixture, `"id": 90,`, `"id": 90, "merge": {"permission": "reporter"},`, 1)); err != nil {
		t.Fatal(err)
	}
	denied := glabGraphQL(t, srv, "PRSetReviewers", "mutation", with(map[string]any{"usernames": []string{"bob"}, "mode": "REMOVE"}))
	if denied["data"].(map[string]any)["mergeRequestSetReviewers"] != nil || !strings.Contains(mustJSON(t, denied["errors"]), "permission") || len(pull().Reviewers) != 1 {
		t.Fatalf("a reporter's change = %v, reviewers %v", denied, pull().Reviewers)
	}
}

// GitHub keeps ten assignees and ignores the rest without an error.
func TestGitHubAssigneesStopAtTen(t *testing.T) {
	t.Parallel()
	members := []string{}
	for i := range 12 {
		members = append(members, fmt.Sprintf(`{"login":"m%d"}`, i))
	}
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"repos":[{"forge":"github","project":"acme/m","members":[`+strings.Join(members, ",")+`],"pulls":[{"number":1,"title":"T","author":"octo"}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	logins := []string{}
	for i := range 12 {
		logins = append(logins, fmt.Sprintf("m%d", i))
	}
	got := httpRequest(t, srv, "POST", "github.com", "/github/rest/repos/acme/m/issues/1/assignees", mustJSON(t, map[string]any{"assignees": logins}), nil)
	var answer struct {
		Assignees []string `json:"assignees"`
	}
	decodeBodyStatus(t, got, http.StatusCreated, &answer)
	if len(answer.Assignees) != 10 {
		t.Fatalf("assignees = %v, want the first ten", answer.Assignees)
	}
}

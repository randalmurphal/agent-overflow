package forgefake

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// reviewStateFixture is a GitLab merge request the viewer alice reviews,
// on a host running version, on a tier with or without Request changes.
func reviewStateFixture(version string, noChangeRequests bool) string {
	tier := ""
	if noChangeRequests {
		tier = `, "noChangeRequests": true`
	}
	return `{"viewer": "alice", "repos": [{"forge": "gitlab", "project": "grp/r", "id": 91, "serverVersion": "` + version + `"` + tier + `,
	  "pulls": [{"number": 1, "title": "T", "author": "octo", "reviewers": ["alice", "bob"]}]}]}`
}

// bulk_publish takes reviewer_state from 19.3: requested_changes sets the
// viewer's state, reviewed on a tier without Request changes, and a host
// before 19.3 does not take it. A thread posted without a position is a
// general resolvable thread.
func TestGitLabReviewStateAndGeneralThread(t *testing.T) {
	t.Parallel()
	gl := http.Header{"Private-Token": {testAPIToken}}
	const mr = "/gitlab/api/v4/projects/grp%2Fr/merge_requests/1"
	state := func(t *testing.T, e *Engine) string {
		t.Helper()
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, r := range e.repos {
			if r.Project == "grp/r" {
				return gitlabReviewState(r.pull(1), "alice")
			}
		}
		t.Fatal("no grp/r")
		return ""
	}
	for _, tc := range []struct {
		name, version string
		noChanges     bool
		status        int
		want          string
	}{
		{"19.6 requests changes", "19.6.0-ee", false, http.StatusNoContent, "requested_changes"},
		{"19.3 requests changes", "19.3.0", false, http.StatusNoContent, "requested_changes"},
		{"a tier without Request changes sets reviewed", "19.6.0-ee", true, http.StatusNoContent, "reviewed"},
		{"19.2 does not take it", "19.2.4", false, http.StatusNotFound, "unreviewed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := New(Options{APIToken: testAPIToken})
			if _, err := seedJSON(e, reviewStateFixture(tc.version, tc.noChanges)); err != nil {
				t.Fatal(err)
			}
			srv := testServer(t, e)
			got := httpRequest(t, srv, "POST", "gitlab.com", mr+"/draft_notes/bulk_publish", `{"reviewer_state":"requested_changes"}`, gl)
			if got.status != tc.status {
				t.Fatalf("bulk_publish = %d %s, want %d", got.status, got.body, tc.status)
			}
			if s := state(t, e); s != tc.want {
				t.Fatalf("alice's state = %q, want %q", s, tc.want)
			}
		})
	}

	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, reviewStateFixture("19.6.0-ee", false)); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	if got := httpRequest(t, srv, "POST", "gitlab.com", mr+"/draft_notes/bulk_publish", `{"reviewer_state":"approved"}`, gl); got.status != http.StatusBadRequest {
		t.Fatalf("an unknown reviewer_state = %d %s", got.status, got.body)
	}
	created := httpRequest(t, srv, "POST", "gitlab.com", mr+"/discussions", `{"body":"Changes requested:\n\nRename it."}`, gl)
	if created.status != http.StatusCreated || !strings.Contains(created.body, `"type":"DiscussionNote"`) || !strings.Contains(created.body, `"resolvable":true`) {
		t.Fatalf("create a general thread = %d %s", created.status, created.body)
	}
	var discussions []struct {
		ID    string `json:"id"`
		Notes []struct {
			Body       string `json:"body"`
			Type       string `json:"type"`
			Resolvable bool   `json:"resolvable"`
			Position   any    `json:"position"`
		} `json:"notes"`
	}
	listed := httpRequest(t, srv, "GET", "gitlab.com", mr+"/discussions?per_page=100", "", gl)
	if err := json.Unmarshal([]byte(listed.body), &discussions); err != nil {
		t.Fatalf("discussions = %s: %v", listed.body, err)
	}
	found := false
	for _, d := range discussions {
		for _, n := range d.Notes {
			if n.Body == "Changes requested:\n\nRename it." {
				found = n.Type == "DiscussionNote" && n.Resolvable && n.Position == nil
			}
		}
	}
	if !found {
		t.Fatalf("the general thread is not a resolvable unpositioned discussion: %s", listed.body)
	}
	if s := state(t, e); s != "reviewed" {
		t.Fatalf("alice after her thread = %q, want reviewed", s)
	}
}

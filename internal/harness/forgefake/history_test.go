package forgefake

import (
	"strings"
	"testing"
)

const historyFixture = `{"repos": [{"forge": "gitlab", "project": "grp/hist", "id": 7100, "pulls": [
  {"number": 1, "title": "Old", "createdAt": "2026-03-01T00:00:00Z", "updatedAt": "2026-03-02T00:00:00Z",
   "reviews": [{"author": "bob", "authorName": "Bob Li", "state": "APPROVED", "submittedAt": "2026-03-03T10:00:00Z"}],
   "comments": [{"author": "carol", "body": "ok", "createdAt": "2026-03-04T10:00:00Z"}]},
  {"number": 2, "title": "New", "createdAt": "2026-03-05T00:00:00Z", "updatedAt": "2026-03-06T00:00:00Z"}
]}]}`

// The merge request list orders by update time either way and filters by
// it inclusively; the project events answer one action's events, after
// or before a day, in either order; an action or filter the handler does
// not implement is unhandled.
func TestGitLabHistoryRoutes(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, historyFixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	iids := func(path string) string {
		var got []map[string]any
		decodeBody(t, glabGET(t, srv, "gitlab.com", path), &got)
		var out []string
		for _, mr := range got {
			out = append(out, mustJSON(t, mr["iid"]))
		}
		return strings.Join(out, ",")
	}
	for path, want := range map[string]string{
		"projects/grp%2Fhist/merge_requests?state=all&order_by=updated_at&sort=asc":                                      "1,2",
		"projects/grp%2Fhist/merge_requests?state=all&order_by=updated_at&sort=desc":                                     "2,1",
		"projects/grp%2Fhist/merge_requests?state=all&order_by=updated_at&sort=asc&updated_after=2026-03-06T00:00:00Z":   "2",
		"projects/grp%2Fhist/merge_requests?state=all&order_by=updated_at&sort=desc&updated_before=2026-03-02T00:00:00Z": "1",
	} {
		if got := iids(path); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	events := func(path string) []map[string]any {
		var got []map[string]any
		decodeBody(t, glabGET(t, srv, "gitlab.com", path), &got)
		return got
	}
	approved := events("projects/7100/events?action=approved&sort=asc&after=2026-03-02&per_page=100")
	if len(approved) != 1 || approved[0]["action_name"] != "approved" || approved[0]["target_iid"] != float64(1) || mustJSON(t, approved[0]["author"]) != `{"name":"Bob Li","username":"bob"}` {
		t.Errorf("approved = %v", mustJSON(t, approved))
	}
	if none := events("projects/7100/events?action=approved&sort=asc&after=2026-03-03&per_page=100"); len(none) != 0 {
		t.Errorf("approved after its day = %v", mustJSON(t, none))
	}
	commented := events("projects/7100/events?action=commented&sort=desc&before=2026-03-05&per_page=100")
	if len(commented) != 1 || mustJSON(t, commented[0]["note"]) != `{"noteable_iid":1,"noteable_type":"MergeRequest","system":false,"type":null}` {
		t.Errorf("commented = %v", mustJSON(t, commented))
	}
	for _, path := range []string{
		"projects/7100/events?action=pushed",
		"projects/7100/events?action=approved&target_type=merge_request",
		"projects/grp%2Fhist/merge_requests?state=all&sort=asc",
	} {
		if answer := glabGET(t, srv, "gitlab.com", path); answer.status != 404 {
			t.Errorf("%s = %d, want unhandled", path, answer.status)
		}
	}
}

package trackerfake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// readWorld is changeWorld with more of WEB-1: changes, comments and
// worklogs, and more issues on board 1.
func readWorld(t *testing.T, changes, comments, worklogs, issues int) *world {
	t.Helper()
	w := newWorld(t)
	f := changeFixture()
	is := &f.Jira[0].Issues[0]
	for i := range changes {
		is.Changelog = append(is.Changelog, JiraChange{ID: strconv.Itoa(70000 + i), Author: "acc-user1", Created: testStart.Add(time.Duration(i) * time.Minute),
			Items: []JiraChangeItem{{Field: "summary", FieldType: "jira", ToString: ptr(strconv.Itoa(i))}}})
	}
	for i := range comments {
		is.Comments = append(is.Comments, JiraComment{ID: strconv.Itoa(80000 + i), Author: "acc-user1", Body: json.RawMessage(testDoc),
			Created: testStart.Add(time.Duration(i) * time.Minute)})
	}
	for i := range worklogs {
		is.Worklogs = append(is.Worklogs, JiraWorklog{ID: strconv.Itoa(90000 + i), Author: "acc-user1", Started: testStart, TimeSpent: 60})
	}
	for i := range issues {
		id := 30000 + i
		f.Jira[0].Issues = append(f.Jira[0].Issues, JiraIssue{ID: strconv.Itoa(id), Key: "WEB-" + strconv.Itoa(100+i), Project: "WEB", Type: "10001", Summary: "More"})
	}
	if err := w.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	return w
}

func ptr(s string) *string { return &s }

// decodeReply decodes r's body into v.
func decodeReply(t *testing.T, r reply, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
}

// TestJiraDateIsTheEngineClock proves every Jira answer's Date header is
// the fake's clock, ahead of the computer's or behind it.
func TestJiraDateIsTheEngineClock(t *testing.T) {
	w := changeWorld(t)
	for _, at := range []time.Time{time.Now().Add(-time.Hour), time.Now().Add(time.Hour)} {
		w.clock.set(at)
		for _, r := range []reply{w.jira(t, "GET", "/rest/api/3/myself", ""), w.jira(t, "GET", "/rest/api/3/issue/NOPE-1", "")} {
			got, err := http.ParseTime(r.header.Get("Date"))
			if err != nil || !got.Equal(at.Truncate(time.Second)) {
				t.Errorf("Date %q (%d), want %v", r.header.Get("Date"), r.status, at)
			}
		}
	}
}

// TestIssueExpansions: each expansion embeds what its own read answers,
// and the changelog only its newest 100 entries, newest first, with the
// total (measured).
func TestIssueExpansions(t *testing.T) {
	w := readWorld(t, 120, 0, 0, 0)
	var v struct {
		Fields    map[string]json.RawMessage
		EditMeta  struct{ Fields map[string]json.RawMessage }
		Changelog struct {
			StartAt, MaxResults, Total int
			Histories                  []struct{ ID string }
		}
		Transitions []struct {
			ID     string
			Fields map[string]json.RawMessage
		}
	}
	decodeReply(t, w.write(t, "GET", "/rest/api/3/issue/WEB-1?fields=summary&expand=editmeta,changelog,transitions,transitions.fields", "", 200), &v)
	if len(v.Fields) != 1 || v.EditMeta.Fields["summary"] == nil || len(v.Transitions) == 0 || v.Transitions[0].Fields == nil {
		t.Fatalf("expanded %+v", v)
	}
	h := v.Changelog
	if h.StartAt != 0 || h.Total != 120 || h.MaxResults != 100 || len(h.Histories) != 100 || h.Histories[0].ID != "70119" || h.Histories[99].ID != "70020" {
		t.Fatalf("changelog %d from %d of %d, first %s", len(h.Histories), h.StartAt, h.Total, h.Histories[0].ID)
	}
	if r := w.jira(t, "GET", "/rest/api/3/issue/WEB-1?expand=renderedFields", ""); r.status != http.StatusNotImplemented {
		t.Fatalf("an expansion the fake lacks answered %d", r.status)
	}
	if len(w.e.Unhandled()) != 1 {
		t.Fatal("an unimplemented expansion was not recorded")
	}
}

// TestCommentsNewestFirst: orderBy=-created lists the newest first
// (measured).
func TestCommentsNewestFirst(t *testing.T) {
	w := readWorld(t, 0, 3, 0, 0)
	var page struct {
		Total    int
		Comments []struct{ ID string }
	}
	decodeReply(t, w.write(t, "GET", "/rest/api/3/issue/WEB-1/comment?orderBy=-created&maxResults=2", "", 200), &page)
	if page.Total != 3 || len(page.Comments) != 2 || page.Comments[0].ID != "80002" || page.Comments[1].ID != "80001" {
		t.Fatalf("newest first %+v", page)
	}
}

// TestWorklogFieldEmbedsTwenty: the worklog field holds the first 20
// with the total and maxResults 20 (measured).
func TestWorklogFieldEmbedsTwenty(t *testing.T) {
	w := readWorld(t, 0, 0, 25, 0)
	var page struct {
		Issues []struct {
			Fields struct {
				Worklog struct {
					StartAt, MaxResults, Total int
					Worklogs                   []struct{ ID string }
				}
			}
		}
	}
	decodeReply(t, w.write(t, "POST", "/rest/api/3/search/jql", `{"jql":"key in (\"WEB-1\")","fields":["worklog"]}`, 200), &page)
	wl := page.Issues[0].Fields.Worklog
	if wl.Total != 25 || wl.MaxResults != 20 || len(wl.Worklogs) != 20 || wl.Worklogs[0].ID != "90000" {
		t.Fatalf("embedded worklogs %+v", wl)
	}
}

// TestBoardListIsUnclamped: the board issue list holds up to 5000 with
// fields (measured); search and the other lists stop at 100.
func TestBoardListIsUnclamped(t *testing.T) {
	w := readWorld(t, 0, 0, 0, 150)
	var page struct{ Issues []struct{ ID string } }
	decodeReply(t, w.write(t, "GET", "/rest/software/1.0/board/1/issue?fields=summary&maxResults=5000", "", 200), &page)
	if len(page.Issues) != 153 {
		t.Fatalf("the board list held %d of 153", len(page.Issues))
	}
	decodeReply(t, w.write(t, "GET", "/rest/software/1.0/board/1/backlog?fields=summary&maxResults=5000", "", 200), &page)
	if len(page.Issues) != 100 {
		t.Fatalf("the backlog held %d, want the clamp of 100", len(page.Issues))
	}
	decodeReply(t, w.write(t, "POST", "/rest/api/3/search/jql", `{"jql":"project = WEB","fields":["summary"],"maxResults":5000}`, 200), &page)
	if len(page.Issues) != 100 {
		t.Fatalf("search held %d, want the clamp of 100", len(page.Issues))
	}
}

// TestIndexedLate: an issue not indexed yet is left out of searches and
// lists and read by key, and shows once the clock reaches Indexed.
func TestIndexedLate(t *testing.T) {
	w := newWorld(t)
	f := changeFixture()
	f.Jira[0].Issues[1].Updated = testStart
	f.Jira[0].Issues[1].Indexed = testStart.Add(time.Minute)
	if err := w.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	keys := func() (search, list int) {
		var page struct{ Issues []struct{ Key string } }
		decodeReply(t, w.write(t, "POST", "/rest/api/3/search/jql", `{"jql":"project = WEB","fields":["id"]}`, 200), &page)
		search = len(page.Issues)
		decodeReply(t, w.write(t, "GET", "/rest/software/1.0/board/1/issue?fields=id", "", 200), &page)
		return search, len(page.Issues)
	}
	if s, l := keys(); s != 2 || l != 2 {
		t.Fatalf("before it is indexed: search %d, list %d of 3", s, l)
	}
	w.write(t, "GET", "/rest/api/3/issue/WEB-2", "", 200)
	w.clock.set(testStart.Add(time.Minute))
	if s, l := keys(); s != 3 || l != 3 {
		t.Fatalf("once indexed: search %d, list %d of 3", s, l)
	}
}

// TestJiraAccountHeader: an answer to an authenticated request names the
// account it was made for, a 404 included, and no answer to an anonymous
// one does; an anonymous search answers 200 with no issues (measured).
func TestJiraAccountHeader(t *testing.T) {
	w := changeWorld(t)
	for _, r := range []reply{
		w.jira(t, "POST", "/rest/api/3/search/jql", `{"jql":"project = WEB","fields":["id"]}`),
		w.jira(t, "GET", "/rest/api/3/issue/NOPE-1", ""),
		w.jira(t, "GET", "/rest/agile/1.0/board/1/configuration", ""),
	} {
		if got := r.header.Get("X-AAccountId"); got != "acc-user1" {
			t.Errorf("an authenticated %d answer names %q", r.status, got)
		}
	}
	anonymous := func(method, path, body string) reply {
		t.Helper()
		r, err := send(context.Background(), method, w.srv.JiraURL(testSite)+path, withJSON(map[string]string{}, body), body)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	search := anonymous("POST", "/rest/api/3/search/jql", `{"jql":"project = WEB","fields":["id"]}`)
	var page struct{ Issues []json.RawMessage }
	decodeReply(t, search, &page)
	if search.status != 200 || len(page.Issues) != 0 {
		t.Errorf("an anonymous search: %d %s", search.status, search.body)
	}
	for _, r := range []reply{search, anonymous("GET", "/rest/api/3/issue/WEB-1", ""), anonymous("GET", "/rest/api/3/myself", "")} {
		if got := r.header.Get("X-AAccountId"); got != "" {
			t.Errorf("an anonymous %d answer names %q", r.status, got)
		}
	}
}

// TestSprintThatNamesNothing: a sprint ID no sprint has fails the board
// issue list with a 400 and matches nothing on a search (measured).
func TestSprintThatNamesNothing(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "GET", "/rest/software/1.0/board/1/issue?fields=id&jql="+url.QueryEscape("sprint in (1, 999)"), "", 400)
	w.write(t, "GET", "/rest/software/1.0/board/1/issue?fields=id&jql="+url.QueryEscape("sprint in (1)"), "", 200)
	var page struct{ Issues []json.RawMessage }
	decodeReply(t, w.write(t, "POST", "/rest/api/3/search/jql", `{"jql":"sprint in (999)","fields":["id"]}`, 200), &page)
	if len(page.Issues) != 0 {
		t.Errorf("a search of a sprint that names nothing found %d", len(page.Issues))
	}
}

// TestBoardLocationIDIsAString: a board configuration's location names
// its project's ID as a string (measured).
func TestBoardLocationIDIsAString(t *testing.T) {
	w := changeWorld(t)
	var cfg struct {
		Location map[string]json.RawMessage
	}
	decodeReply(t, w.write(t, "GET", "/rest/agile/1.0/board/1/configuration", "", 200), &cfg)
	var id string
	if err := json.Unmarshal(cfg.Location["id"], &id); err != nil || id == "" {
		t.Errorf("location id %s: %v", cfg.Location["id"], err)
	}
}

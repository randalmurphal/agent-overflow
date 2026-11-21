package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// TestCatalogIndex proves the catalog finds a field by ID and the first
// field of a custom type, both in Jira's order, and owns a copy of what
// it was built from.
func TestCatalogIndex(t *testing.T) {
	const sprintType = "com.pyxis.greenhopper.jira:gh-sprint"
	fields := []Field{
		{ID: "summary", Name: "Summary"},
		{ID: "customfield_10020", Name: "Sprint", Custom: true, Schema: Schema{Custom: sprintType}},
		{ID: "customfield_10021", Name: "Old sprint", Custom: true, Schema: Schema{Custom: sprintType}},
		{ID: "summary", Name: "A second summary"},
	}
	cat := NewCatalog(fields)
	fields[0].Name = "changed by the caller"
	if f, ok := cat.Field("summary"); !ok || f.Name != "Summary" {
		t.Errorf("by ID, the first of a repeated ID, not the caller's change: %+v %v", f, ok)
	}
	if f, ok := cat.OfCustomType(sprintType); !ok || f.ID != "customfield_10020" {
		t.Errorf("the first of a custom type: %+v %v", f, ok)
	}
	if _, ok := cat.OfCustomType("com.example:none"); ok {
		t.Error("a custom type no field has")
	}
	var ids []string
	for f := range cat.All() {
		ids = append(ids, f.ID)
	}
	if cat.Len() != 4 || strings.Join(ids, ",") != "summary,customfield_10020,customfield_10021,summary" {
		t.Errorf("all %d: %v", cat.Len(), ids)
	}
}

// meSite counts the sign-in checks and answers each with User 1.
func meSite(t *testing.T) (*jiratest.Server, *Client) {
	f := jiratest.NewServer(t)
	f.On("GET", myselfURL, jiratest.Reply(200, userOneJSON))
	return f, newTestClient(t, f)
}

// TestMeIsReadOncePerCredential proves Me answers from the last sign-in
// check until the credential is forgotten or refused, while Myself asks
// every time.
func TestMeIsReadOncePerCredential(t *testing.T) {
	f, c := meSite(t)
	me := func(want int) {
		t.Helper()
		if u, err := c.Me(t.Context()); err != nil || u.AccountID != "acc-user1" {
			t.Fatalf("me: %+v %v", u, err)
		}
		if n := f.Count("GET", myselfURL); n != want {
			t.Errorf("%d sign-in checks, want %d", n, want)
		}
	}
	me(1)
	me(1)
	if _, err := c.Myself(t.Context()); err != nil {
		t.Fatal(err)
	}
	me(2)
	c.ForgetCredential()
	me(3)
	// A refused credential drops it too.
	f.On("GET", "/rest/api/3/field", jiratest.Reply(401, `{"errorMessages":["Client must be authenticated"]}`))
	if _, err := c.Fields(t.Context()); !errors.As(err, new(*trackerapi.AuthError)) {
		t.Fatalf("a refused credential: %v", err)
	}
	me(4)
}

// TestMeRetiredByAnotherCredentialRead proves an answer drawn under
// another read of the credential (the transport read it again after a
// 401 and the retry succeeded) retires what Me holds.
func TestMeRetiredByAnotherCredentialRead(t *testing.T) {
	f := markSite(t, map[string]int{"user1@example.com": 200, "user2@example.com": 200})
	f.On("GET", "/rest/api/3/field", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if user, _, _ := r.BasicAuth(); user == "user1@example.com" {
			jiratest.Reply(401, `{"errorMessages":["Client must be authenticated"]}`)(w, r, b)
			return
		}
		jiratest.Reply(200, `[]`)(w, r, b)
	})
	src := &rotatingCredentials{}
	c := markClient(t, f, src, nil)
	for range 2 {
		if _, err := c.Me(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Fields(t.Context()); err != nil || src.reads.Load() != 2 {
		t.Fatalf("the retry under the second read: %v, %d reads", err, src.reads.Load())
	}
	if _, err := c.Me(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("GET", myselfURL); n != 2 {
		t.Errorf("%d sign-in checks, want 2: one per credential read", n)
	}
}

// TestForgetBoardDropsOnlyThatBoard proves ForgetBoard drops one board's
// configuration and sprint listing and keeps every other board's, and a
// held listing is the caller's own, stamped with Jira's clock.
func TestForgetBoardDropsOnlyThatBoard(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	date := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	for _, n := range []int{3, 4} {
		f.On("GET", fmt.Sprintf("/rest/agile/1.0/board/%d/configuration", n), jiratest.Reply(200, fmt.Sprintf(`{"id":%d,"name":"b","type":"scrum"}`, n)))
		f.On("GET", fmt.Sprintf("/rest/agile/1.0/board/%d/sprint", n), func(w http.ResponseWriter, r *http.Request, b []byte) {
			if r.URL.Query().Has("state") {
				t.Errorf("a board's held listing is filtered: %s", r.URL.RawQuery)
			}
			w.Header().Set("Date", date.Format(http.TimeFormat))
			jiratest.Reply(200, `{"isLast":true,"values":[{"id":7,"name":"Sprint 1","state":"active"}]}`)(w, r, b)
		})
	}
	read := func() {
		t.Helper()
		for _, n := range []int64{3, 4} {
			if _, err := c.BoardConfig(t.Context(), n); err != nil {
				t.Fatal(err)
			}
			l, err := c.BoardSprints(t.Context(), n)
			if err != nil || len(l.Sprints) != 1 || !l.Complete || !l.Read.Equal(date) {
				t.Fatalf("board %d sprints: %+v %v", n, l, err)
			}
			l.Sprints[0].Name = "changed by a caller"
		}
	}
	read()
	read()
	c.ForgetBoard(3)
	read()
	for path, want := range map[string]int{
		"/rest/agile/1.0/board/3/configuration": 2, "/rest/agile/1.0/board/3/sprint": 2,
		"/rest/agile/1.0/board/4/configuration": 1, "/rest/agile/1.0/board/4/sprint": 1,
	} {
		if n := f.Count("GET", path); n != want {
			t.Errorf("%s read %d times, want %d", path, n, want)
		}
	}
	if l, _ := c.BoardSprints(t.Context(), 4); l.Sprints[0].Name != "Sprint 1" {
		t.Errorf("a caller's change reached the held listing: %+v", l.Sprints[0])
	}
	if _, err := c.BoardSprints(t.Context(), 0); !trackerapi.WriteNotApplied(err) {
		t.Errorf("board 0: %v", err)
	}
}

// TestBoardConfigLocation proves where a board is reads with its ID as
// the string Jira sends (measured), only a project location names a
// project, and an ID of another JSON type fails the read rather than
// read as some other project.
func TestBoardConfigLocation(t *testing.T) {
	for _, tc := range []struct {
		location string
		want     ConfigLocation
		project  string
		fails    bool
	}{
		{`{"type":"project","id":"10000","key":"WEB","name":"Web"}`, ConfigLocation{Type: LocatedInProject, ID: "10000", Key: "WEB", Name: "Web"}, "10000", false},
		{`{"type":"user","id":"acc-user1","name":"User 1"}`, ConfigLocation{Type: LocatedOnUser, ID: "acc-user1", Name: "User 1"}, "", false},
		{`null`, ConfigLocation{}, "", false},
		{`{"type":"project","id":10000,"key":"WEB"}`, ConfigLocation{}, "", true},
	} {
		f := jiratest.NewServer(t)
		c := newTestClient(t, f)
		f.On("GET", boardConfigURL, jiratest.Reply(200, `{"id":3,"name":"b","type":"scrum","location":`+tc.location+`}`))
		cfg, err := c.BoardConfig(t.Context(), 3)
		if (err != nil) != tc.fails || cfg.Location != tc.want || cfg.Location.ProjectID() != tc.project {
			t.Errorf("%s: %+v %v", tc.location, cfg.Location, err)
		}
	}
}

// dated answers with body and the Date header at, or none when at is
// zero (net/http would add the wall clock's).
func dated(at time.Time, body string) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		if at.IsZero() {
			w.Header()["Date"] = nil
		} else {
			w.Header().Set("Date", at.Format(http.TimeFormat))
		}
		jiratest.Reply(200, body)(w, r, b)
	}
}

// TestAnswerDates proves a search page, a search run to its end, an
// issue list page and a bulk changelog hand up Jira's clock from the
// Date header (the first answer's when there are several), and zero when
// the answer has none.
func TestAnswerDates(t *testing.T) {
	first := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	later := first.Add(time.Minute)
	for _, at := range []time.Time{first, {}} {
		f := jiratest.NewServer(t)
		c := newTestClient(t, f)
		f.On("POST", "/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request, b []byte) {
			if strings.Contains(string(b), `"t1"`) {
				dated(later, `{"issues":[{"id":"2","key":"WEB-2"}],"isLast":true}`)(w, r, b)
				return
			}
			dated(at, `{"issues":[{"id":"1","key":"WEB-1"}],"nextPageToken":"t1","isLast":false}`)(w, r, b)
		})
		f.On("GET", backlogURL, dated(at, `{"issues":[],"isLast":true}`))
		f.On("POST", bulkChangelogURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
			if strings.Contains(string(b), `"t1"`) {
				dated(later, `{"issueChangeLogs":[]}`)(w, r, b)
				return
			}
			dated(at, `{"issueChangeLogs":[{"issueId":"10001","changeHistories":[`+changeJSON(1)+`]}],"nextPageToken":"t1"}`)(w, r, b)
		})
		page, err := c.Search(t.Context(), SearchQuery{JQL: `project = "WEB"`})
		if err != nil || !page.Date.Equal(at) {
			t.Errorf("a search page: %v %v, want %v", page.Date, err, at)
		}
		all, err := c.SearchAll(t.Context(), SearchQuery{JQL: `project = "WEB"`}, 0)
		if err != nil || len(all.Issues) != 2 || !all.Date.Equal(at) {
			t.Errorf("a search to its end: %v %v, want the first page's %v", all.Date, err, at)
		}
		list, err := c.ListIssues(t.Context(), ListQuery{List: BacklogIssues(3)})
		if err != nil || !list.Date.Equal(at) {
			t.Errorf("a list page: %v %v, want %v", list.Date, err, at)
		}
		logs, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: []string{"WEB-1"}})
		if err != nil || !logs.Date.Equal(at) {
			t.Errorf("a bulk changelog: %v %v, want the first answer's %v", logs.Date, err, at)
		}
		if n := f.Count("POST", bulkChangelogURL); n != 2 {
			t.Errorf("%d changelog pages, want 2", n)
		}
	}
}

// detailIssue is WEB-1 read for IssueDetail with a changelog of total
// entries, the newest embedded ones (IDs from total down) starting at
// startAt.
func detailIssue(t *testing.T, total, embedded, startAt int) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "fields", detailFields)
		jiratest.WantQuery(t, r, "expand", detailExpand)
		var hs []string
		for id := total; id > total-embedded; id-- {
			hs = append(hs, changeJSON(id))
		}
		jiratest.Reply(200, fmt.Sprintf(`{"id":"10001","key":"WEB-1","fields":{"summary":"a","comment":null},
			"editmeta":{"fields":{"summary":{"name":"Summary","schema":{"type":"string","system":"summary"},"operations":["set"]}}},
			"transitions":[{"id":"31","name":"Done","to":{"id":"10001","name":"Done"},"fields":{}}],
			"changelog":{"startAt":%d,"maxResults":%d,"total":%d,"histories":[%s]}}`, startAt, embedded, total, strings.Join(hs, ",")))(w, r, b)
	}
}

func changeJSON(id int) string {
	return fmt.Sprintf(`{"id":"%d","created":"2026-11-%02dT09:00:00.000+0000","items":[{"field":"summary","fieldId":"summary","toString":"v%d"}]}`, id, 1+id%28, id)
}

// olderChanges answers the changelog read oldest first with entries 1 to
// n, of which it sends sent.
func olderChanges(t *testing.T, n, sent int) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "startAt", "0")
		var vs []string
		for id := 1; id <= sent; id++ {
			vs = append(vs, changeJSON(id))
		}
		jiratest.Reply(200, fmt.Sprintf(`{"startAt":0,"maxResults":100,"total":%d,"isLast":true,"values":[%s]}`, n, strings.Join(vs, ",")))(w, r, b)
	}
}

// TestIssueDetail proves one read carries the issue, its edit screen,
// its transitions and its newest changes, and only a changelog longer
// than what is embedded reads the older entries, joined oldest first.
func TestIssueDetail(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1", detailIssue(t, 2, 2, 0))
	d, err := c.IssueDetail(t.Context(), "WEB-1")
	if err != nil || d.Issue.Summary != "a" || len(d.Edit.Fields) != 1 || len(d.Transitions) != 1 || d.Transitions[0].To.ID != "10001" {
		t.Fatalf("got %+v %v", d, err)
	}
	if ids := entryIDs(d.Changelog); !d.Changelog.Complete || ids != "1,2" || len(f.Requests()) != 1 {
		t.Errorf("an embedded changelog: %s, requests %v", ids, f.Requests())
	}
	f.On("GET", "/rest/api/3/issue/WEB-1", detailIssue(t, 5, 2, 0))
	f.On("GET", "/rest/api/3/issue/WEB-1/changelog", olderChanges(t, 5, 3))
	d, err = c.IssueDetail(t.Context(), "WEB-1")
	if ids := entryIDs(d.Changelog); err != nil || ids != "1,2,3,4,5" || f.Count("GET", "/rest/api/3/issue/WEB-1/changelog") != 1 {
		t.Errorf("an older part: %s %v", ids, err)
	}
	// A changelog that does not join the embedded entries fails the read.
	for name, route := range map[string][2]jiratest.Handler{
		"embedded from elsewhere":         {detailIssue(t, 5, 2, 3), olderChanges(t, 5, 3)},
		"more embedded than the total":    {detailIssue(t, 1, 2, 0), olderChanges(t, 1, 0)},
		"an older part shorter than told": {detailIssue(t, 5, 2, 0), olderChanges(t, 5, 2)},
	} {
		f.On("GET", "/rest/api/3/issue/WEB-1", route[0])
		f.On("GET", "/rest/api/3/issue/WEB-1/changelog", route[1])
		if _, err := c.IssueDetail(t.Context(), "WEB-1"); !errors.As(err, new(*AnswerError)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func entryIDs(l Changelog) string {
	ids := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		ids[i] = e.ID
	}
	return strings.Join(ids, ",")
}

// TestLatestComments proves the newest comments are read newest first in
// one request and handed up oldest first with where they start, and an
// answer that is not the newest page fails.
func TestLatestComments(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	comment := func(id int) string {
		return fmt.Sprintf(`{"id":"%d","body":null,"created":"2026-11-%02dT10:00:00.000+0000"}`, id, id)
	}
	answer := func(startAt, total int, ids ...int) jiratest.Handler {
		return func(w http.ResponseWriter, r *http.Request, b []byte) {
			jiratest.WantQuery(t, r, "orderBy", "-created")
			jiratest.WantQuery(t, r, "startAt", "0")
			var cs []string
			for _, id := range ids {
				cs = append(cs, comment(id))
			}
			jiratest.Reply(200, fmt.Sprintf(`{"startAt":%d,"total":%d,"comments":[%s]}`, startAt, total, strings.Join(cs, ",")))(w, r, b)
		}
	}
	const path = "/rest/api/3/issue/WEB-1/comment"
	f.On("GET", path, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "maxResults", "2")
		answer(0, 5, 5, 4)(w, r, b)
	})
	page, err := c.LatestComments(t.Context(), "WEB-1", 2)
	if err != nil || page.StartAt != 3 || page.Total != 5 || len(page.Comments) != 2 || page.Comments[0].ID != "4" || page.Comments[1].ID != "5" {
		t.Fatalf("got %+v %v", page, err)
	}
	f.On("GET", path, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "maxResults", "100")
		answer(0, 1, 1)(w, r, b)
	})
	if page, err := c.LatestComments(t.Context(), "WEB-1", 0); err != nil || page.StartAt != 0 || len(page.Comments) != 1 {
		t.Errorf("zero asks for the most: %+v %v", page, err)
	}
	if _, err := c.LatestComments(t.Context(), "WEB-1", -1); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a negative page: %v", err)
	}
	for name, h := range map[string]jiratest.Handler{
		"another start":            answer(1, 5, 4, 3),
		"fewer than asked":         answer(0, 5, 5),
		"more than the total":      answer(0, 1, 2, 1),
		"more than the total says": answer(0, 0, 1),
	} {
		f.On("GET", path, h)
		if _, err := c.LatestComments(t.Context(), "WEB-1", 2); !errors.As(err, new(*AnswerError)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestWorklogField proves the worklog field an issue embeds reads as its
// worklogs, complete only when they are all of the issue's.
func TestWorklogField(t *testing.T) {
	wl := func(id string) string {
		return `{"id":"` + id + `","issueId":"10001","started":"2026-11-16T09:30:00.000+0100","timeSpentSeconds":60}`
	}
	for _, tc := range []struct {
		raw      string
		n        int
		complete bool
		err      bool
	}{
		{`{"startAt":0,"maxResults":20,"total":2,"worklogs":[` + wl("1") + `,` + wl("2") + `]}`, 2, true, false},
		{`{"startAt":0,"maxResults":1,"total":2,"worklogs":[` + wl("1") + `]}`, 1, false, false},
		{`{"startAt":1,"maxResults":1,"total":2,"worklogs":[` + wl("2") + `]}`, 1, false, false},
		{`{"startAt":0,"total":0,"worklogs":[]}`, 0, true, false},
		{`{"startAt":0,"worklogs":[]}`, 0, false, true},
		{`[]`, 0, false, true},
	} {
		list, err := FieldValue(json.RawMessage(tc.raw)).Worklogs()
		if (err != nil) != tc.err || len(list.Worklogs) != tc.n || list.Complete != tc.complete {
			t.Errorf("%s: %+v %v", tc.raw, list, err)
		}
	}
}

// TestServerEmbedsExpansions proves the test server embeds a GET's routed
// expansions, unwrapping a member of the same name and keeping one the
// answer holds, counts only the GET, and fails the GET with a
// sub-resource's failure.
func TestServerEmbedsExpansions(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	const path = "/rest/api/3/issue/WEB-1"
	f.On("GET", path, jiratest.Reply(200, `{"id":"10001","changelog":{"total":0}}`))
	f.On("GET", path+"/editmeta", jiratest.Reply(200, `{"fields":{}}`))
	f.On("GET", path+"/transitions", jiratest.Reply(200, `{"expand":"transitions","transitions":[{"id":"31"}]}`))
	f.On("GET", path+"/changelog", func(w http.ResponseWriter, r *http.Request, b []byte) {
		t.Error("an expansion the answer holds was read")
		jiratest.Reply(500, `{}`)(w, r, b)
	})
	var got map[string]json.RawMessage
	if err := c.get(t.Context(), "issue/WEB-1", map[string][]string{"expand": {"editmeta,transitions,changelog,names"}}, &got); err != nil {
		t.Fatal(err)
	}
	want := `{"id":"10001","changelog":{"total":0},"editmeta":{"fields":{}},"transitions":[{"id":"31"}]}`
	if b, _ := json.Marshal(got); !jsonEqual(string(b), want) {
		t.Errorf("got %s, want %s", b, want)
	}
	if want := []string{"GET " + path}; !slices.Equal(f.Requests(), want) {
		t.Errorf("requests %v", f.Requests())
	}
	f.On("GET", path+"/editmeta", jiratest.Reply(403, `{"errorMessages":["no"]}`))
	if err := c.get(t.Context(), "issue/WEB-1", map[string][]string{"expand": {"editmeta"}}, &got); err == nil {
		t.Error("a sub-resource's failure was not the answer")
	}
}

func jsonEqual(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

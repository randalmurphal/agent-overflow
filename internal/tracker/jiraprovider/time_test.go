package jiraprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/tempo/tempotest"
	"agent-overflow/internal/tracker/trackerapi"
)

const worklogsPath = "/rest/api/3/issue/10001/worklog"

func day(t *testing.T, y int, m time.Month, d int) tracker.Date {
	t.Helper()
	out, err := tracker.NewDate(y, m, d)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// jiraWorklog is a Jira worklog of issue 10001 by author started at
// started.
func jiraWorklog(id, author, started string, spent int, comment string) string {
	return fmt.Sprintf(`{"id":%q,"issueId":"10001","author":%s,"started":%q,"timeSpentSeconds":%d,"comment":%s,"updated":%q}`,
		id, author, started, spent, mustJSON(adf.Doc(para(comment))), updatedAt)
}

// worklogSite is newSite with User 1's profile and the worklogs of
// issue 10001: User 1's at 09:00 and 23:30 New York time on 10
// November, and one by User 2.
func worklogSite(t *testing.T) (*jiratest.Server, *Provider) {
	f, p := newSite(t)
	f.On("GET", "/rest/api/3/user", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "accountId", "acc-user1")
		jiratest.Reply(200, user1JSON)(w, r, b)
	})
	f.On("GET", worklogsPath, jiratest.Reply(200, `{"startAt":0,"total":3,"worklogs":[`+
		jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, "Review")+","+
		jiraWorklog("301", user1JSON, "2026-11-10T23:30:00.000-0500", 1800, "Late")+","+
		jiraWorklog("302", user2JSON, "2026-11-10T10:00:00.000+0100", 600, "Theirs")+`]}`))
	f.On("GET", worklogsPath+"/300", jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, "Review")))
	return f, p
}

// TestJiraWorklogs proves a worklog's day is the day its start falls on
// in its author's zone, and that only the author's are listed, the one
// starting at the next day's midnight left out whether or not Jira's
// bound includes it.
func TestJiraWorklogs(t *testing.T) {
	f, p := worklogSite(t)
	f.On("GET", worklogsPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		from := time.Date(2026, 11, 10, 0, 0, 0, 0, time.FixedZone("EST", -5*3600))
		jiratest.WantQuery(t, r, "startedAfter", fmt.Sprint(from.UnixMilli()-1))
		jiratest.WantQuery(t, r, "startedBefore", fmt.Sprint(from.AddDate(0, 0, 1).UnixMilli()))
		jiratest.Reply(200, `{"startAt":0,"total":4,"worklogs":[`+
			jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, "Review")+","+
			jiraWorklog("301", user1JSON, "2026-11-10T23:30:00.000-0500", 1800, "Late")+","+
			jiraWorklog("302", user2JSON, "2026-11-10T10:00:00.000+0100", 600, "Theirs")+","+
			jiraWorklog("304", user1JSON, "2026-11-11T00:00:00.000-0500", 600, "Next day")+`]}`)(w, r, b)
	})
	f.On("POST", searchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		v := bodyOf(t, b)
		if jql := v["jql"]; jql != `worklogAuthor = "acc-user1" AND worklogDate >= "2026-11-09" AND worklogDate <= "2026-11-11" ORDER BY id ASC` {
			t.Errorf("jql %v", jql)
		}
		if fmt.Sprint(v["fields"]) != "[worklog]" {
			t.Errorf("fields %v", v["fields"])
		}
		// 10001 has more worklogs than Jira embeds, so it is read again;
		// 10002's are all embedded.
		jiratest.Reply(200, page("",
			issue("10001", "WEB-1", updatedAt, map[string]any{"worklog": json.RawMessage(`{"startAt":0,"maxResults":20,"total":4,"worklogs":[` +
				jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, "Review") + `]}`)}),
			issue("10002", "WEB-2", updatedAt, map[string]any{"worklog": json.RawMessage(`{"startAt":0,"maxResults":20,"total":2,"worklogs":[` +
				jiraWorklog("310", user1JSON, "2026-11-10T12:00:00.000-0500", 900, "Embedded") + "," +
				jiraWorklog("311", user2JSON, "2026-11-10T12:00:00.000-0500", 900, "Theirs") + `]}`)}),
		))(w, r, b)
	})
	got, err := p.Time().Worklogs(t.Context(), tracker.WorklogQuery{Author: "acc-user1", From: day(t, 2026, 11, 10), To: day(t, 2026, 11, 10)})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, w := range got {
		ids = append(ids, fmt.Sprintf("%s %s %s %v %q", w.ID, w.Issue, w.Day, w.Spent, strings.TrimSpace(w.Description)))
	}
	if want := []string{`300 10001 2026-11-10 1h0m0s "Review"`, `301 10001 2026-11-10 30m0s "Late"`, `310 10002 2026-11-10 15m0s "Embedded"`}; !slices.Equal(ids, want) {
		t.Errorf("worklogs %q", ids)
	}
	if n := f.Count("GET", worklogsPath); n != 1 {
		t.Errorf("%d worklog reads, want one for the issue with more than Jira embeds", n)
	}
	f.On("POST", searchPath, jiratest.Reply(200, `{"isLast":true,"issues":[],"warnings":[{"type":"x","message":"partial"}]}`))
	if _, err := p.Time().Worklogs(t.Context(), tracker.WorklogQuery{Author: "acc-user1", From: day(t, 2026, 11, 10), To: day(t, 2026, 11, 10)}); err == nil {
		t.Error("an incomplete search was taken")
	}
}

func TestJiraWorklogWrites(t *testing.T) {
	f, p := worklogSite(t)
	tt := p.Time()
	f.On("POST", worklogsPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "adjustEstimate", "auto")
		if started := bodyOf(t, b)["started"]; started != "2026-11-10T09:00:00.000-0500" {
			t.Errorf("started %v", started)
		}
		jiratest.Reply(201, jiraWorklog("303", user1JSON, "2026-11-10T09:00:00.000-0500", 5400, "New"))(w, r, b)
	})
	d := day(t, 2026, 11, 10)
	res := checked(t, tt.AddWorklog(t.Context(), tracker.WorklogInput{Issue: "10001", Day: d, Spent: 90 * time.Minute, Description: "New"}))
	want := []tracker.Resource{tracker.DetailOf("10001"), tracker.WorklogsOf("acc-user1", d)}
	if states(res) != "worklog:applied" || !slices.Equal(res.Parts[0].Changed, want) {
		t.Errorf("add: %s %+v %v", states(res), res.Parts[0].Changed, res.Parts[0].Outcome.Err())
	}

	from := tracker.Worklog{ID: "300", Issue: "10001", Author: "acc-user1", Day: d, Spent: time.Hour, Description: adf.ToMarkdown(adf.Doc(para("Review"))), Updated: jiraRevision}
	next := day(t, 2026, 11, 11)
	f.On("PUT", worklogsPath+"/300", func(w http.ResponseWriter, r *http.Request, b []byte) {
		v := bodyOf(t, b)
		if v["started"] != "2026-11-11T09:00:00.000-0500" || v["timeSpentSeconds"] != 7200.0 {
			t.Errorf("edit %s", b)
		}
		jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-11T09:00:00.000-0500", 7200, "Review"))(w, r, b)
	})
	res = checked(t, tt.EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Day: &next, Spent: ptr(2 * time.Hour)}))
	want = []tracker.Resource{tracker.DetailOf("10001"), tracker.WorklogsOf("acc-user1", d), tracker.WorklogsOf("acc-user1", next)}
	if states(res) != "worklog:applied" || !slices.Equal(res.Parts[0].Changed, want) {
		t.Errorf("edit: %s %+v %v", states(res), res.Parts[0].Changed, res.Parts[0].Outcome.Err())
	}
	stale := from
	stale.Spent = 30 * time.Minute
	res = checked(t, tt.EditWorklog(t.Context(), tracker.WorklogEdit{From: stale, Spent: ptr(2 * time.Hour)}))
	if reason(res, 0) != tracker.ReasonStale || f.Count("PUT", worklogsPath+"/300") != 1 {
		t.Errorf("a stale edit: %s", states(res))
	}
	f.On("DELETE", worklogsPath+"/300", func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jiratest.WantQuery(t, r, "adjustEstimate", "auto")
		w.WriteHeader(http.StatusNoContent)
	})
	if res := checked(t, tt.DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from})); states(res) != "worklog:applied" {
		t.Errorf("delete: %s", states(res))
	}
	bad := from
	bad.ID = "300/x"
	if res := checked(t, tt.DeleteWorklog(t.Context(), tracker.WorklogDelete{From: bad})); reason(res, 0) != tracker.ReasonInvalid {
		t.Errorf("a worklog that is not an ID: %s", states(res))
	}
	f.On("POST", worklogsPath, jiratest.Reply(500, `{}`))
	if res := checked(t, tt.AddWorklog(t.Context(), tracker.WorklogInput{Issue: "10001", Day: d, Spent: time.Hour})); states(res) != "worklog:unknown" {
		t.Errorf("an unanswered add: %s", states(res))
	}
}

// tempoWL is a Tempo worklog of issue in Tempo's wire shape.
// jiraRevision and tempoRevision are the revisions jiraWorklog and tempoWL
// answer (updatedAt, and Tempo's updatedAt below): a copy as read carries
// them.
var (
	jiraRevision  = time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	tempoRevision = time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC)
)

func tempoWL(id, issue int64, date, start string, spent int64, desc string) string {
	m := map[string]any{
		"self": fmt.Sprintf("https://api.example.com/4/worklogs/%d", id), "tempoWorklogId": id,
		"issue":     map[string]any{"id": issue, "self": "https://example.atlassian.net/rest/api/2/issue/1"},
		"author":    map[string]any{"accountId": "acc-user1", "self": "https://example.atlassian.net/rest/api/2/user"},
		"startDate": date, "startTime": start, "startDateTimeUtc": date + "T" + start + "Z",
		"timeSpentSeconds": spent, "billableSeconds": spent, "description": desc,
		"attributes": map[string]any{"self": "https://api.example.com/4/worklogs/1/work-attribute-values", "values": []any{}},
		"createdAt":  "2026-11-02T08:00:00Z", "updatedAt": "2026-11-03T09:30:00Z",
	}
	return mustJSON(m)
}

func tempoPage(items ...string) string {
	return fmt.Sprintf(`{"self":"https://api.example.com/4/x","metadata":{"count":%d,"offset":0,"limit":1000},"results":[%s]}`, len(items), strings.Join(items, ","))
}

func values(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Add(pairs[i], pairs[i+1])
	}
	return v
}

// tempoSite is a fake Jira and Tempo with a Provider built with Tempo;
// the Tempo client is paced as in production.
func tempoSite(t *testing.T) (*jiratest.Server, *tempotest.Server, *Provider) {
	f, _ := newSite(t)
	f.On("POST", bulkFetchPath, jiratest.Reply(200, bulk(issue("10001", "WEB-1", updatedAt, nil))))
	ts := tempotest.NewServer(t, "fake-tempo-token")
	jc := newJira(t, f)
	tc, err := tempo.New(tempo.Options{
		BaseURL: ts.URL(), Version: "test", Issues: TempoIssues(jc),
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte("fake-tempo-token")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	p, err := New(Options{Jira: jc, Tempo: tc, Site: "https://example.atlassian.net", SprintsFrom: allSprints})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Time().(*tempoTime); !ok {
		t.Fatalf("with Tempo, Time is %T", p.Time())
	}
	return f, ts, p
}

func TestTempoWorklogs(t *testing.T) {
	t.Parallel()
	_, ts, p := tempoSite(t)
	ts.On("GET", "/4/worklogs/user/acc-user1", values("from", "2026-11-10", "to", "2026-11-11", "limit", "1000", "offset", "0"), "", 200,
		tempoPage(tempoWL(70, 10001, "2026-11-10", "13:00:00", 3600, "Planning"), tempoWL(71, 10001, "2026-11-11", "09:00:00", 1800, "Review")))
	got, err := p.Time().Worklogs(t.Context(), tracker.WorklogQuery{Author: "acc-user1", From: day(t, 2026, 11, 10), To: day(t, 2026, 11, 11)})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
	if w := got[0]; w.ID != "70" || w.Issue != "10001" || w.Author != "acc-user1" || w.Day != day(t, 2026, 11, 10) || w.Spent != time.Hour || w.Description != "Planning" {
		t.Errorf("worklog %+v", w)
	}
}

func TestTempoWorklogWrites(t *testing.T) {
	t.Parallel()
	_, ts, p := tempoSite(t)
	tt := p.Time()
	d, next := day(t, 2026, 11, 10), day(t, 2026, 11, 11)
	ts.On("GET", "/4/work-attributes", values("offset", "0", "limit", "1000"), "", 200, tempoPage()).Repeat = true
	for _, dd := range []string{"2026-11-10", "2026-11-11"} {
		ts.On("GET", "/4/timesheet-approvals/user/acc-user1", values("from", dd, "to", dd), "", 200,
			`{"self":"x","actions":{},"worklogs":{"self":"x"},"user":{"accountId":"acc-user1","self":"x"},"period":{"from":"2026-11-09","to":"2026-11-15"},
			"requiredSeconds":144000,"timeSpentSeconds":72000,"status":{"key":"OPEN","actor":{"accountId":"acc-user1","self":"x"},"updatedAt":"2026-11-08T10:00:00Z"}}`).Repeat = true
	}
	ts.On("POST", "/4/worklogs", nil, `{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-10","timeSpentSeconds":5400,"description":"Review","attributes":[]}`,
		200, tempoWL(72, 10001, "2026-11-10", "00:00:00", 5400, "Review"))
	res := checked(t, tt.AddWorklog(t.Context(), tracker.WorklogInput{Issue: "10001", Day: d, Spent: 90 * time.Minute, Description: "Review"}))
	if states(res) != "worklog:applied" || !slices.Equal(res.Parts[0].Changed, []tracker.Resource{tracker.DetailOf("10001"), tracker.WorklogsOf("acc-user1", d)}) {
		t.Errorf("add: %s %v", states(res), res.Parts[0].Outcome.Err())
	}

	ts.On("GET", "/4/worklogs/70", nil, "", 200, tempoWL(70, 10001, "2026-11-10", "13:00:00", 3600, "Planning")).Repeat = true
	ts.On("PUT", "/4/worklogs/70", nil,
		`{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-11","startTime":"13:00:00","timeSpentSeconds":3600,"billableSeconds":3600,"description":"Planning","attributes":[]}`,
		200, tempoWL(70, 10001, "2026-11-11", "13:00:00", 3600, "Planning"))
	from := tracker.Worklog{ID: "70", Issue: "10001", Author: "acc-user1", Day: d, Spent: time.Hour, Description: "Planning", Updated: tempoRevision}
	res = checked(t, tt.EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Day: &next}))
	if states(res) != "worklog:applied" || len(res.Parts[0].Changed) != 3 {
		t.Errorf("edit: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
	stale := from
	stale.Description = "Something else"
	res = checked(t, tt.EditWorklog(t.Context(), tracker.WorklogEdit{From: stale, Description: ptr("x")}))
	if reason(res, 0) != tracker.ReasonStale {
		t.Errorf("a stale edit: %s", states(res))
	}
	ts.On("DELETE", "/4/worklogs/70", nil, "", 204, "")
	if res := checked(t, tt.DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from})); states(res) != "worklog:applied" {
		t.Errorf("delete: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
	if res := checked(t, tt.AddWorklog(t.Context(), tracker.WorklogInput{Issue: "WEB-1", Day: d, Spent: time.Hour})); reason(res, 0) != tracker.ReasonInvalid {
		t.Errorf("a key for an ID: %s", states(res))
	}
}

// TestTempoIssues proves the adapter maps keys to IDs by the issue's
// current key, case ignored, leaving out what Jira does not return, and
// fails on a failed read.
func TestTempoIssues(t *testing.T) {
	f, _ := newSite(t)
	f.On("POST", bulkFetchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issueIdsOrKeys":["web-1","WEB-5"],"fields":["status"]}`)
		moved := issue("10005", "APP-5", updatedAt, nil)
		jiratest.Reply(200, bulk(issue("10001", "WEB-1", updatedAt, nil), moved))(w, r, b)
	})
	ti := TempoIssues(newJira(t, f))
	ids, err := ti.IDs(t.Context(), []string{"web-1", "WEB-5", "not a key"})
	if err != nil || len(ids) != 1 || ids["web-1"] != 10001 {
		t.Errorf("IDs %v %v", ids, err)
	}
	f.On("POST", bulkFetchPath, jiratest.Reply(400, `{}`))
	if _, err := ti.IDs(t.Context(), []string{"WEB-1"}); err == nil {
		t.Error("a failed read was taken")
	}
}

// A Jira connection's time is logged in Tempo once Jira says who logs it:
// a Tempo refusal of its token after Jira answered is a refusal whose
// error names Tempo, which the sentence a person reads is built from.
func TestATempoRefusalAfterJiraNamesTempo(t *testing.T) {
	t.Parallel()
	_, ts, p := tempoSite(t)
	refused := `{"errors":[{"message":"invalid token"}]}`
	ts.On("GET", "/4/work-attributes", values("offset", "0", "limit", "1000"), "", 401, refused).Repeat = true
	ts.On("GET", "/4/timesheet-approvals/user/acc-user1", values("from", "2026-11-10", "to", "2026-11-10"), "", 401, refused).Repeat = true
	res := checked(t, p.Time().AddWorklog(t.Context(), tracker.WorklogInput{Issue: "10001", Day: day(t, 2026, 11, 10), Spent: time.Hour}))
	auth, ok := errors.AsType[*trackerapi.AuthError](res.Parts[0].Outcome.Err())
	if reason(res, 0) != tracker.ReasonTracker || !ok || auth.Tracker != "tempo" {
		t.Fatalf("a Tempo refusal: %s, %v", states(res), res.Parts[0].Outcome.Err())
	}
}

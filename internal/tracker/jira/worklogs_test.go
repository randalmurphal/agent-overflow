package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	worklogsURL = "/rest/api/3/issue/WEB-1/worklog"
	worklog300  = worklogsURL + "/300"
)

// worklogJSONWith is worklog 300 with comment (JSON) and visibility.
func worklogJSONWith(comment, visibility string) string {
	return `{"id":"300","issueId":"10001","author":` + userOneJSON + `,"comment":` + comment + `,
		"started":"2026-11-16T09:30:00.000+0100","timeSpentSeconds":5400,"visibility":` + visibility + `}`
}

func TestAddWorklog(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	started := time.Date(2026, 11, 16, 9, 30, 0, 0, time.FixedZone("", 3600))
	f.On("POST", worklogsURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "adjustEstimate", "manual")
		jiratest.WantQuery(t, r, "reduceBy", "90m")
		jiratest.WantJSON(t, b, `{"started":"2026-11-16T09:30:00.000+0100","timeSpentSeconds":5400,"comment":`+docJSON(t, adf.Doc(para("Fixed it")))+`}`)
		jiratest.Reply(201, worklogJSONWith(docJSON(t, adf.Doc(para("Fixed it"))), "null"))(w, r, b)
	})
	wl, err := c.AddWorklog(t.Context(), WorklogInput{Issue: "WEB-1", Started: started, TimeSpent: 90 * time.Minute, Markdown: "Fixed it", Estimate: ReduceRemaining(90 * time.Minute)})
	if err != nil || wl.ID != "300" || wl.TimeSpent != 90*time.Minute || !wl.Started.Equal(started) || wl.Comment == nil {
		t.Fatalf("got %+v %v", wl, err)
	}
	for name, in := range map[string]WorklogInput{
		"no start":          {Issue: "WEB-1", TimeSpent: time.Minute},
		"no time":           {Issue: "WEB-1", Started: started},
		"part of a second":  {Issue: "WEB-1", Started: started, TimeSpent: 1500 * time.Millisecond},
		"a local image":     {Issue: "WEB-1", Started: started, TimeSpent: time.Minute, Markdown: "![x](x.png)"},
		"increase on add":   {Issue: "WEB-1", Started: started, TimeSpent: time.Minute, Estimate: IncreaseRemaining(time.Minute)},
		"part of a minute":  {Issue: "WEB-1", Started: started, TimeSpent: time.Minute, Estimate: SetRemaining(90 * time.Second)},
		"a negative remain": {Issue: "WEB-1", Started: started, TimeSpent: time.Minute, Estimate: SetRemaining(-time.Minute)},
		"a bad issue":       {Issue: "x/y", Started: started, TimeSpent: time.Minute},
	} {
		if _, err := c.AddWorklog(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := f.Count("POST", worklogsURL); n != 1 {
		t.Errorf("%d writes", n)
	}
}

// TestWorklogAnswerForms proves a worklog write is applied only on its
// documented answer naming the worklog: one that names it but does not
// decode in full is applied with its ID, a field that does not parse is a
// problem on a stored worklog, and any other 2xx is an unknown outcome
// reporting no worklog.
func TestWorklogAnswerForms(t *testing.T) {
	for body, want := range map[string]struct {
		status  int
		id      string
		applied bool
		problem string
		err     bool
	}{
		`{"id":"300","started":"soon","timeSpentSeconds":60}`: {201, "300", false, "started", false},
		`{"id":"300","created":"yesterday"}`:                  {201, "300", false, "created", false},
		`{"id":"300","comment":5}`:                            {201, "300", false, "comment", false},
		`{"id":300,"comment":5}`:                              {201, "300", true, "", true},
		`{"id":"300","timeSpentSeconds":"long"}`:              {201, "300", true, "", true},
		`{"started":"2026-11-16T09:30:00.000+0100"}`:          {201, "", false, "", true},
		`<html>ok</html>`:                                     {201, "", false, "", true},
		`{"id":"300"}`:                                        {200, "", false, "", true},
	} {
		f := jiratest.NewServer(t)
		c := newTestClient(t, f)
		f.On("POST", worklogsURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
			jiratest.WantJSON(t, b, `{"started":"2026-11-16T09:30:00.000+0000","timeSpentSeconds":60}`)
			jiratest.Reply(want.status, body)(w, r, b)
		})
		wl, err := c.AddWorklog(t.Context(), WorklogInput{Issue: "WEB-1", Started: time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC), TimeSpent: time.Minute})
		if (err != nil) != want.err || wl.ID != want.id || (problemIn(wl.Problems, want.problem) != nil) != (want.problem != "") {
			t.Errorf("%s: got %+v %v", body, wl, err)
			continue
		}
		if err == nil {
			continue
		}
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepWorklog || se.Applied() != want.applied || se.OutcomeUnknown() == want.applied || trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// worklogSite serves worklog 300 as stored holds it, and its update.
type worklogSite struct {
	t      *testing.T
	f      *jiratest.Server
	c      *Client
	stored atomic.Pointer[string]
	put    func(r *http.Request, b []byte) string
}

func newWorklogSite(t *testing.T, comment, visibility string) *worklogSite {
	s := &worklogSite{t: t, f: jiratest.NewServer(t)}
	s.c = newTestClient(t, s.f)
	body := worklogJSONWith(comment, visibility)
	s.stored.Store(&body)
	s.f.On("GET", worklog300, func(w http.ResponseWriter, r *http.Request, b []byte) { jiratest.Reply(200, *s.stored.Load())(w, r, b) })
	s.f.On("PUT", worklog300, func(w http.ResponseWriter, r *http.Request, b []byte) { jiratest.Reply(200, s.put(r, b))(w, r, b) })
	return s
}

// TestUpdateWorklog proves an edit sends only what changes, keeps the
// worklog's visibility and what Markdown cannot show, and is refused when
// it would lose content or its base changed.
func TestUpdateWorklog(t *testing.T) {
	visibility := `{"type":"group","value":"web-team"}`
	s := newWorklogSite(t, docJSON(t, layoutDoc()), visibility)
	read, err := s.c.Worklog(t.Context(), "WEB-1", "300")
	if err != nil {
		t.Fatal(err)
	}
	md := strings.Replace(adf.ToMarkdown(layoutDoc()), "left", "LEFT", 1)
	edited := strings.Replace(docJSON(t, layoutDoc()), "left", "LEFT", 1)
	s.put = func(r *http.Request, b []byte) string {
		jiratest.WantQuery(t, r, "adjustEstimate", "leave")
		jiratest.WantJSON(t, b, `{"timeSpentSeconds":3600,"comment":`+edited+`,"visibility":`+visibility+`}`)
		return worklogJSONWith(edited, visibility)
	}
	in := WorklogEdit{Issue: "WEB-1", ID: "300", From: &read, TimeSpent: time.Hour, Markdown: &md, Estimate: LeaveEstimate()}
	if res, err := s.c.UpdateWorklog(t.Context(), in); err != nil || res.Unchanged || res.Worklog.ID != "300" {
		t.Fatalf("got %+v %v", res, err)
	}
	flattened := "intro\n\nleft\n\nright\n"
	_, err = s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Unchecked: true, Markdown: &flattened})
	if _, ok := errors.AsType[*adf.LossError](err); !ok || !trackerapi.WriteNotApplied(err) {
		t.Errorf("a lossy edit: %v", err)
	}
	older := read
	older.Comment = adf.Doc(para("older"))
	_, err = s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", From: &older, Markdown: &md})
	if _, ok := errors.AsType[*StaleError](err); !ok || !trackerapi.WriteNotApplied(err) {
		t.Errorf("a stale edit: %v", err)
	}
	same := adf.ToMarkdown(layoutDoc())
	res, err := s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", From: &read, TimeSpent: 90 * time.Minute, Markdown: &same})
	if err != nil || !res.Unchanged || res.Worklog.ID != "300" {
		t.Errorf("an edit asking for what is stored: %+v %v", res, err)
	}
	for name, bad := range map[string]WorklogEdit{
		"reduce on an update": {Issue: "WEB-1", ID: "300", Unchecked: true, Estimate: ReduceRemaining(time.Minute)},
		"no base":             {Issue: "WEB-1", ID: "300", TimeSpent: time.Hour},
		"both bases":          {Issue: "WEB-1", ID: "300", From: &read, Unchecked: true, TimeSpent: time.Hour},
		"another worklog":     {Issue: "WEB-1", ID: "301", From: &read, TimeSpent: time.Hour},
		"nothing asked":       {Issue: "WEB-1", ID: "300", From: &read},
		"only leave":          {Issue: "WEB-1", ID: "300", From: &read, Estimate: LeaveEstimate()},
	} {
		if _, err := s.c.UpdateWorklog(t.Context(), bad); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := s.f.Count("PUT", worklog300); n != 1 {
		t.Errorf("%d writes", n)
	}
}

// TestUpdateWorklogAbsentComment proves a base read with no comment is
// checked: a comment added since makes the edit stale.
func TestUpdateWorklogAbsentComment(t *testing.T) {
	s := newWorklogSite(t, "null", "null")
	read, err := s.c.Worklog(t.Context(), "WEB-1", "300")
	if err != nil || read.Comment != nil {
		t.Fatalf("got %+v %v", read, err)
	}
	added := worklogJSONWith(docJSON(t, adf.Doc(para("added meanwhile"))), "null")
	s.stored.Store(&added)
	md := "mine"
	_, err = s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", From: &read, Markdown: &md})
	if _, ok := errors.AsType[*StaleError](err); !ok || s.f.Count("PUT", worklog300) != 0 {
		t.Fatalf("a comment added since an absent one was read: %v", err)
	}
}

// TestUpdateWorklogStaleParts proves each part an edit can overwrite is
// checked against the base: a start, a time spent or a comment that
// changed since the read (a comment that no longer decodes included)
// makes the edit stale, and nothing is written.
func TestUpdateWorklogStaleParts(t *testing.T) {
	base := worklogJSONWith("null", "null")
	for name, now := range map[string]string{
		"the start":                strings.Replace(base, "09:30:00.000+0100", "10:30:00.000+0100", 1),
		"the time spent":           strings.Replace(base, "5400", "7200", 1),
		"a comment that is no doc": worklogJSONWith("5", "null"),
	} {
		s := newWorklogSite(t, "null", "null")
		read, err := s.c.Worklog(t.Context(), "WEB-1", "300")
		if err != nil {
			t.Fatal(err)
		}
		s.stored.Store(&now)
		_, err = s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", From: &read, TimeSpent: time.Hour})
		if _, ok := errors.AsType[*StaleError](err); !ok || !trackerapi.WriteNotApplied(err) || s.f.Count("PUT", worklog300) != 0 {
			t.Errorf("%s changed: %v", name, err)
		}
	}
}

// TestUpdateWorklogUnreadableComment proves a comment edit of a worklog
// whose stored comment did not decode is refused before anything is sent,
// since the edit cannot keep what it cannot read; a change to another
// field goes through.
func TestUpdateWorklogUnreadableComment(t *testing.T) {
	s := newWorklogSite(t, "5", "null")
	read, err := s.c.Worklog(t.Context(), "WEB-1", "300")
	if err != nil || problemIn(read.Problems, "comment") == nil {
		t.Fatalf("got %+v %v", read, err)
	}
	md := "mine"
	for name, in := range map[string]WorklogEdit{
		"based on the read": {Issue: "WEB-1", ID: "300", From: &read, Markdown: &md},
		"unchecked":         {Issue: "WEB-1", ID: "300", Unchecked: true, Markdown: &md},
	} {
		_, err = s.c.UpdateWorklog(t.Context(), in)
		if _, stale := errors.AsType[*StaleError](err); err == nil || stale || !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s.put = func(r *http.Request, b []byte) string {
		jiratest.WantQuery(t, r, "adjustEstimate", "auto")
		jiratest.WantJSON(t, b, `{"timeSpentSeconds":3600}`)
		return worklogJSONWith("5", "null")
	}
	if _, err := s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", From: &read, TimeSpent: time.Hour}); err != nil {
		t.Errorf("a time change: %v", err)
	}
	if n := s.f.Count("PUT", worklog300); n != 1 {
		t.Errorf("%d writes", n)
	}
}

// TestUpdateWorklogStoredValues proves an edit asking only for values the
// worklog holds sends nothing and says so.
func TestUpdateWorklogStoredValues(t *testing.T) {
	s := newWorklogSite(t, "null", "null")
	s.put = func(*http.Request, []byte) string {
		t.Error("an edit of stored values was sent")
		return worklogJSONWith("null", "null")
	}
	read, err := s.c.Worklog(t.Context(), "WEB-1", "300")
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]WorklogEdit{
		"the start":      {Issue: "WEB-1", ID: "300", From: &read, Started: read.Started.UTC()},
		"the time spent": {Issue: "WEB-1", ID: "300", From: &read, TimeSpent: read.TimeSpent},
	} {
		res, err := s.c.UpdateWorklog(t.Context(), in)
		if err != nil || !res.Unchanged || res.Worklog.ID != "300" {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
}

// TestUpdateWorklogAnswerNamesAnother proves an update answered with
// another worklog is an unknown outcome reporting no worklog.
func TestUpdateWorklogAnswerNamesAnother(t *testing.T) {
	s := newWorklogSite(t, "null", "null")
	s.put = func(r *http.Request, b []byte) string {
		jiratest.WantJSON(t, b, `{"timeSpentSeconds":3600}`)
		return strings.Replace(worklogJSONWith("null", "null"), `"id":"300"`, `"id":"301"`, 1)
	}
	res, err := s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Unchecked: true, TimeSpent: time.Hour})
	se, ok := errors.AsType[*StepError](err)
	if !ok || se.Step != StepWorklog || se.Applied() || !se.OutcomeUnknown() || res.Worklog.ID != "" {
		t.Fatalf("got %+v %v", res, err)
	}
}

// TestUpdateWorklogChecksLast proves the stale check is the last read
// before the write: a change made while a mention resolves is caught.
func TestUpdateWorklogChecksLast(t *testing.T) {
	s := newWorklogSite(t, docJSON(t, adf.Doc(para("old"))), "null")
	read, err := s.c.Worklog(t.Context(), "WEB-1", "300")
	if err != nil {
		t.Fatal(err)
	}
	s.f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		changed := worklogJSONWith(docJSON(t, adf.Doc(para("changed while the mention resolved"))), "null")
		s.stored.Store(&changed)
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	md := "thanks @[me]"
	_, err = s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", From: &read, Markdown: &md})
	if _, ok := errors.AsType[*StaleError](err); !ok || s.f.Count("PUT", worklog300) != 0 {
		t.Fatalf("a change during the mention's read: %v", err)
	}
	if got := s.f.Requests(); got[len(got)-1] != "GET "+worklog300 || got[len(got)-2] != "GET "+myselfURL {
		t.Errorf("requests %v", got)
	}
}

// TestUpdateWorklogEstimateOnly proves setting the remaining estimate is
// a change on its own: the write carries it with the stored time spent.
func TestUpdateWorklogEstimateOnly(t *testing.T) {
	s := newWorklogSite(t, "null", "null")
	s.put = func(r *http.Request, b []byte) string {
		jiratest.WantQuery(t, r, "adjustEstimate", "new")
		jiratest.WantQuery(t, r, "newEstimate", "120m")
		jiratest.WantJSON(t, b, `{"timeSpentSeconds":5400}`)
		return worklogJSONWith("null", "null")
	}
	res, err := s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Unchecked: true, Estimate: SetRemaining(2 * time.Hour)})
	if err != nil || res.Unchanged || s.f.Count("PUT", worklog300) != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestDeleteWorklog(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("DELETE", worklog300, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jiratest.WantQuery(t, r, "adjustEstimate", "manual")
		jiratest.WantQuery(t, r, "increaseBy", "30m")
		w.WriteHeader(204)
	})
	if err := c.DeleteWorklog(t.Context(), "WEB-1", "300", IncreaseRemaining(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"reduce on a delete": c.DeleteWorklog(t.Context(), "WEB-1", "300", ReduceRemaining(time.Minute)),
		"a bad ID":           c.DeleteWorklog(t.Context(), "WEB-1", "3x", AdjustAuto()),
	} {
		if !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWorklogs(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	after := time.UnixMilli(1762074000000)
	f.On("GET", worklogsURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "startedAfter", "1762074000000")
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		var logs []string
		for i := start; i < min(start+2, 3); i++ {
			logs = append(logs, fmt.Sprintf(`{"id":"%d","started":"2026-11-16T09:30:00.000+0100","timeSpentSeconds":60}`, 300+i))
		}
		jiratest.Reply(200, fmt.Sprintf(`{"startAt":%d,"total":3,"worklogs":[%s]}`, start, strings.Join(logs, ",")))(w, r, b)
	})
	list, err := c.Worklogs(t.Context(), WorklogQuery{Issue: "WEB-1", StartedAfter: after})
	if err != nil || !list.Complete || len(list.Worklogs) != 3 || list.Worklogs[2].ID != "302" {
		t.Fatalf("got %+v %v", list, err)
	}
	for name, body := range map[string]string{
		"no total":      `{"worklogs":[{"id":"300","started":"2026-11-16T09:30:00.000+0100"}]}`,
		"an empty page": `{"total":3,"worklogs":[]}`,
	} {
		f.On("GET", worklogsURL, jiratest.Reply(200, body))
		if list, err := c.Worklogs(t.Context(), WorklogQuery{Issue: "WEB-1"}); err == nil || list.Complete {
			t.Errorf("%s: %+v %v", name, list, err)
		}
	}
}

func TestWorklogChanges(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/worklog/updated", func(w http.ResponseWriter, r *http.Request, b []byte) {
		switch r.URL.Query().Get("since") {
		case "1000":
			jiratest.Reply(200, `{"values":[{"worklogId":300,"updatedTime":1500}],"since":1000,"until":2000,"lastPage":false}`)(w, r, b)
		case "2000":
			jiratest.Reply(200, `{"values":[{"worklogId":301,"updatedTime":2500}],"since":2000,"until":3000,"lastPage":true}`)(w, r, b)
		default:
			t.Errorf("since %s", r.URL.Query().Get("since"))
		}
	})
	got, err := c.UpdatedWorklogs(t.Context(), time.UnixMilli(1000))
	if err != nil || !got.Complete || len(got.Changes) != 2 || got.Changes[1].ID != 301 || got.Until.UnixMilli() != 3000 {
		t.Fatalf("got %+v %v", got, err)
	}
	f.On("GET", "/rest/api/3/worklog/deleted", jiratest.Reply(200, `{"values":[],"since":1000,"until":1000,"lastPage":false}`))
	if got, err := c.DeletedWorklogs(t.Context(), time.UnixMilli(1000)); err == nil || got.Complete {
		t.Errorf("a page that does not move on: %+v %v", got, err)
	}
	if _, err := c.UpdatedWorklogs(t.Context(), time.UnixMilli(-1)); !trackerapi.WriteNotApplied(err) {
		t.Errorf("before 1970: %v", err)
	}
}

func TestWorklogsByID(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	ids := make([]int64, 1001)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	f.On("POST", "/rest/api/3/worklog/list", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var req struct{ IDs []int64 }
		if err := json.Unmarshal(b, &req); err != nil || len(req.IDs) > 1000 {
			t.Errorf("a request of %d IDs: %v", len(req.IDs), err)
		}
		var logs []string
		for _, id := range req.IDs {
			logs = append(logs, fmt.Sprintf(`{"id":"%d","started":"2026-11-16T09:30:00.000+0100"}`, id))
		}
		// One answer names worklog 1 again, which is not counted twice.
		logs = append(logs, `{"id":"1","started":"2026-11-16T09:30:00.000+0100"}`)
		jiratest.Reply(200, "["+strings.Join(logs, ",")+"]")(w, r, b)
	})
	list, err := c.WorklogsByID(t.Context(), ids)
	if err != nil || !list.Complete || len(list.Worklogs) != 1001 {
		t.Fatalf("got %d worklogs, complete %v, %v", len(list.Worklogs), list.Complete, err)
	}
	f.On("POST", "/rest/api/3/worklog/list", jiratest.Reply(200, `[{"id":"1","started":"2026-11-16T09:30:00.000+0100"},{"id":"1","started":"2026-11-16T09:30:00.000+0100"}]`))
	if list, err := c.WorklogsByID(t.Context(), []int64{1, 2}); err != nil || list.Complete {
		t.Errorf("one worklog twice for two named: %+v %v", list, err)
	}
	for name, bad := range map[string][]int64{"none": nil, "a repeat": {1, 1}, "zero": {0}} {
		if _, err := c.WorklogsByID(t.Context(), bad); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

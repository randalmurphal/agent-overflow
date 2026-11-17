package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/adf"
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
	f := newFake(t)
	c := newTestClient(t, f)
	started := time.Date(2026, 11, 16, 9, 30, 0, 0, time.FixedZone("", 3600))
	f.on("POST", worklogsURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "adjustEstimate", "manual")
		wantQuery(t, r, "reduceBy", "90m")
		wantJSON(t, b, `{"started":"2026-11-16T09:30:00.000+0100","timeSpentSeconds":5400,"comment":`+docJSON(t, adf.Doc(para("Fixed it")))+`}`)
		reply(201, worklogJSONWith(docJSON(t, adf.Doc(para("Fixed it"))), "null"))(w, r, b)
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
	if n := f.count("POST", worklogsURL); n != 1 {
		t.Errorf("%d writes", n)
	}
}

// TestWorklogUnreadableAnswer proves a worklog stored whose answer does
// not decode still names the worklog by ID, as applied.
func TestWorklogUnreadableAnswer(t *testing.T) {
	for body, wantID := range map[string]string{
		`{"id":"300","started":"soon","timeSpentSeconds":60}`: "300",
		`{"id":300,"comment":5}`:                              "300",
		`{"started":"2026-11-16T09:30:00.000+0100"}`:          "",
	} {
		f := newFake(t)
		c := newTestClient(t, f)
		f.on("POST", worklogsURL, reply(201, body))
		wl, err := c.AddWorklog(t.Context(), WorklogInput{Issue: "WEB-1", Started: time.Now(), TimeSpent: time.Minute})
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepWorklog || !se.Applied() || wl.ID != wantID {
			t.Errorf("%s: got %+v %v", body, wl, err)
		}
	}
}

// TestUpdateWorklog proves an edit sends only what changes, keeps the
// worklog's visibility and what Markdown cannot show, and is refused
// when it would lose content or the comment changed meanwhile.
func TestUpdateWorklog(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	visibility := `{"type":"group","value":"web-team"}`
	stored := worklogJSONWith(docJSON(t, layoutDoc()), visibility)
	f.on("GET", worklog300, reply(200, stored))
	md := strings.Replace(adf.ToMarkdown(layoutDoc()), "left", "LEFT", 1)
	edited := strings.Replace(docJSON(t, layoutDoc()), "left", "LEFT", 1)
	f.on("PUT", worklog300, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "adjustEstimate", "leave")
		wantJSON(t, b, `{"timeSpentSeconds":3600,"comment":`+edited+`,"visibility":`+visibility+`}`)
		reply(200, worklogJSONWith(edited, visibility))(w, r, b)
	})
	in := WorklogEdit{Issue: "WEB-1", ID: "300", TimeSpent: time.Hour, Markdown: &md, From: layoutDoc(), Estimate: LeaveEstimate()}
	if _, err := c.UpdateWorklog(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	flattened := "intro\n\nleft\n\nright\n"
	_, err := c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Markdown: &flattened})
	if _, ok := errors.AsType[*adf.LossError](err); !ok || !trackerapi.WriteNotApplied(err) {
		t.Errorf("a lossy edit: %v", err)
	}
	_, err = c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Markdown: &md, From: adf.Doc(para("older"))})
	if _, ok := errors.AsType[*StaleError](err); !ok || !trackerapi.WriteNotApplied(err) {
		t.Errorf("a stale edit: %v", err)
	}
	same := adf.ToMarkdown(layoutDoc())
	if wl, err := c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", TimeSpent: 90 * time.Minute, Markdown: &same}); err != nil || wl.ID != "300" {
		t.Errorf("an edit that changes nothing: %v", err)
	}
	if _, err := c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Estimate: ReduceRemaining(time.Minute)}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("reduce on an update: %v", err)
	}
	if n := f.count("PUT", worklog300); n != 1 {
		t.Errorf("%d writes", n)
	}
}

func TestDeleteWorklog(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("DELETE", worklog300, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		wantQuery(t, r, "adjustEstimate", "manual")
		wantQuery(t, r, "increaseBy", "30m")
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
	f := newFake(t)
	c := newTestClient(t, f)
	after := time.UnixMilli(1762074000000)
	f.on("GET", worklogsURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "startedAfter", "1762074000000")
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		var logs []string
		for i := start; i < min(start+2, 3); i++ {
			logs = append(logs, fmt.Sprintf(`{"id":"%d","started":"2026-11-16T09:30:00.000+0100","timeSpentSeconds":60}`, 300+i))
		}
		reply(200, fmt.Sprintf(`{"startAt":%d,"total":3,"worklogs":[%s]}`, start, strings.Join(logs, ",")))(w, r, b)
	})
	list, err := c.Worklogs(t.Context(), WorklogQuery{Issue: "WEB-1", StartedAfter: after})
	if err != nil || !list.Complete || len(list.Worklogs) != 3 || list.Worklogs[2].ID != "302" {
		t.Fatalf("got %+v %v", list, err)
	}
	for name, body := range map[string]string{
		"no total":      `{"worklogs":[{"id":"300","started":"2026-11-16T09:30:00.000+0100"}]}`,
		"an empty page": `{"total":3,"worklogs":[]}`,
	} {
		f.on("GET", worklogsURL, reply(200, body))
		if list, err := c.Worklogs(t.Context(), WorklogQuery{Issue: "WEB-1"}); err == nil || list.Complete {
			t.Errorf("%s: %+v %v", name, list, err)
		}
	}
}

func TestWorklogChanges(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/worklog/updated", func(w http.ResponseWriter, r *http.Request, b []byte) {
		switch r.URL.Query().Get("since") {
		case "1000":
			reply(200, `{"values":[{"worklogId":300,"updatedTime":1500}],"since":1000,"until":2000,"lastPage":false}`)(w, r, b)
		case "2000":
			reply(200, `{"values":[{"worklogId":301,"updatedTime":2500}],"since":2000,"until":3000,"lastPage":true}`)(w, r, b)
		default:
			t.Errorf("since %s", r.URL.Query().Get("since"))
		}
	})
	got, err := c.UpdatedWorklogs(t.Context(), time.UnixMilli(1000))
	if err != nil || !got.Complete || len(got.Changes) != 2 || got.Changes[1].ID != 301 || got.Until.UnixMilli() != 3000 {
		t.Fatalf("got %+v %v", got, err)
	}
	f.on("GET", "/rest/api/3/worklog/deleted", reply(200, `{"values":[],"since":1000,"until":1000,"lastPage":false}`))
	if got, err := c.DeletedWorklogs(t.Context(), time.UnixMilli(1000)); err == nil || got.Complete {
		t.Errorf("a page that does not move on: %+v %v", got, err)
	}
	if _, err := c.UpdatedWorklogs(t.Context(), time.UnixMilli(-1)); !trackerapi.WriteNotApplied(err) {
		t.Errorf("before 1970: %v", err)
	}
}

func TestWorklogsByID(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	ids := make([]int64, 1001)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	f.on("POST", "/rest/api/3/worklog/list", func(w http.ResponseWriter, r *http.Request, b []byte) {
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
		reply(200, "["+strings.Join(logs, ",")+"]")(w, r, b)
	})
	list, err := c.WorklogsByID(t.Context(), ids)
	if err != nil || !list.Complete || len(list.Worklogs) != 1001 {
		t.Fatalf("got %d worklogs, complete %v, %v", len(list.Worklogs), list.Complete, err)
	}
	f.on("POST", "/rest/api/3/worklog/list", reply(200, `[{"id":"1","started":"2026-11-16T09:30:00.000+0100"},{"id":"1","started":"2026-11-16T09:30:00.000+0100"}]`))
	if list, err := c.WorklogsByID(t.Context(), []int64{1, 2}); err != nil || list.Complete {
		t.Errorf("one worklog twice for two named: %+v %v", list, err)
	}
	for name, bad := range map[string][]int64{"none": nil, "a repeat": {1, 1}, "zero": {0}} {
		if _, err := c.WorklogsByID(t.Context(), bad); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

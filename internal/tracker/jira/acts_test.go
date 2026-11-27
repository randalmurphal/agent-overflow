package jira

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/jira/jiratest"
)

const actsAnswer = `{"issues":[{"id":"10001","key":"WEB-1","fields":{
	"updated":"2026-01-05T10:00:00.000+0000",
	"comment":{"startAt":0,"maxResults":2,"total":3,"comments":[
		{"id":"c1","author":{"accountId":"acc-user1","displayName":"User 1","accountType":"atlassian","avatarUrls":{"48x48":"https://example.com/1.png","16x16":"x"}},
		 "body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Looks good"}]}]},
		 "created":"2026-01-02T10:00:00.000+0000","updated":"2026-01-02T10:00:00.000+0000"},
		{"id":"c2","created":"2026-01-03T10:00:00.000+0000"}]},
	"worklog":{"startAt":0,"maxResults":20,"total":1,"worklogs":[
		{"id":"w1","author":{"accountId":"acc-user2","displayName":"User 2"},"comment":{"type":"doc","version":1,"content":[]},
		 "started":"2026-01-04T09:00:00.000+0000","timeSpentSeconds":3600}]}}}],"issueErrors":[]}`

// A bulk read of acts asks for the comment and update fields, and the
// worklog field only when asked, and returns each issue's update time and
// its embedded acts with whether they are all of them; an author with no
// account is kept as none.
func TestBulkActsReadTheEmbeddedActs(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	var fields []string
	f.On("POST", "/rest/api/3/issue/bulkfetch", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var req struct {
			Fields []string `json:"fields"`
		}
		_ = json.Unmarshal(b, &req)
		fields = req.Fields
		jiratest.Reply(200, actsAnswer)(w, r, b)
	})
	got, err := c.BulkActs(t.Context(), []string{"10001", "10002"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fields, ",") != "comment,updated,worklog" {
		t.Errorf("fields %v", fields)
	}
	day := func(d, h int) time.Time { return time.Date(2026, 1, d, h, 0, 0, 0, time.UTC) }
	if len(got) != 1 || got[0].ID != "10001" || !got[0].Updated.Equal(day(5, 10)) {
		t.Fatalf("issues %+v", got)
	}
	cm, wl := got[0].Comments, got[0].Worklogs
	if cm.Complete || len(cm.Acts) != 2 || cm.Acts[0].ID != "c1" || cm.Acts[0].Author.AccountID != "acc-user1" ||
		cm.Acts[0].Author.AvatarURL != "https://example.com/1.png" || !cm.Acts[0].At.Equal(day(2, 10)) || cm.Acts[1].Author != nil {
		t.Errorf("comments %+v", cm)
	}
	if !wl.Complete || len(wl.Acts) != 1 || wl.Acts[0].Author.AccountID != "acc-user2" || wl.Acts[0].Spent != time.Hour || !wl.Acts[0].At.Equal(day(4, 9)) {
		t.Errorf("worklogs %+v", wl)
	}
	if _, err := c.BulkActs(t.Context(), []string{"10001"}, false); err != nil || strings.Join(fields, ",") != "comment,updated" {
		t.Errorf("without worklogs: fields %v, %v", fields, err)
	}
}

// An act or an issue that cannot be counted as sent fails the read as a
// malformed answer rather than being left out.
func TestBulkActsRefuseWhatCannotBeCounted(t *testing.T) {
	for name, answer := range map[string]string{
		"an act with no time":  strings.Replace(actsAnswer, `"created":"2026-01-03T10:00:00.000+0000"`, `"created":""`, 1),
		"an act with no ID":    strings.Replace(actsAnswer, `"id":"c2"`, `"id":""`, 1),
		"a list with no total": strings.Replace(actsAnswer, `"maxResults":2,"total":3,`, ``, 1),
		"no worklog field":     strings.Replace(actsAnswer, `"worklog":`, `"other":`, 1),
		"no update time":       strings.Replace(actsAnswer, `"updated":"2026-01-05T10:00:00.000+0000",`, ``, 1),
	} {
		f := jiratest.NewServer(t)
		c := newTestClient(t, f)
		f.On("POST", "/rest/api/3/issue/bulkfetch", jiratest.Reply(200, answer))
		if _, err := c.BulkActs(t.Context(), []string{"10001"}, true); !isAnswerErr(err) {
			t.Errorf("%s: %v; want a malformed answer", name, err)
		}
	}
}

// The comment and worklog walks read every page to the total, from the
// first, and fail at a page that names no total.
func TestActWalksReadEveryPage(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/10001/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.URL.Query().Get("startAt") == "0" {
			jiratest.Reply(200, `{"startAt":0,"total":2,"comments":[{"id":"c1","created":"2026-01-02T10:00:00.000+0000","body":{"type":"doc"}}]}`)(w, r, b)
			return
		}
		jiratest.Reply(200, `{"startAt":1,"total":2,"comments":[{"id":"c2","created":"2026-01-03T10:00:00.000+0000"}]}`)(w, r, b)
	})
	acts, err := c.CommentActs(t.Context(), "10001")
	if err != nil || len(acts) != 2 || acts[1].ID != "c2" {
		t.Fatalf("comments %+v, %v", acts, err)
	}
	var after string
	f.On("GET", "/rest/api/3/issue/10001/worklog", func(w http.ResponseWriter, r *http.Request, b []byte) {
		after = r.URL.Query().Get("startedAfter")
		jiratest.Reply(200, `{"startAt":0,"total":1,"worklogs":[{"id":"w1","started":"2026-01-04T09:00:00.000+0000","timeSpentSeconds":60}]}`)(w, r, b)
	})
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wl, err := c.WorklogActs(t.Context(), WorklogQuery{Issue: "10001", StartedAfter: from})
	if err != nil || len(wl) != 1 || wl[0].Spent != time.Minute || after != "1767225600000" {
		t.Fatalf("worklogs %+v, %v, startedAfter %q", wl, err, after)
	}
	f.On("GET", "/rest/api/3/issue/10001/comment", jiratest.Reply(200, `{"startAt":0,"comments":[]}`))
	if _, err := c.CommentActs(t.Context(), "10001"); !isAnswerErr(err) {
		t.Fatalf("a page with no total: %v", err)
	}
}

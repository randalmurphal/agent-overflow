package jira

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// TestEditCommentCurrent proves an edit based on the comment as just read
// reads it no more, unless a mention made the conversion read Jira: then
// the comment is read again last and checked against the base.
func TestEditCommentCurrent(t *testing.T) {
	const commentURL = "/rest/api/3/issue/WEB-1/comment/900"
	f, c := editCommentSite(t, adf.Doc(para("first")))
	f.On("PUT", commentURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		var body struct {
			Body       json.RawMessage `json:"body"`
			Visibility json.RawMessage `json:"visibility"`
		}
		_ = json.Unmarshal(b, &body)
		if string(body.Visibility) != `{"type":"role","value":"Developers"}` {
			t.Errorf("the visibility read is not kept: %s", b)
		}
		storedComment(200, string(body.Body))(w, r, b)
	})
	read, err := c.Comment(t.Context(), "WEB-1", "900")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: "second", Current: &read}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"GET " + commentURL, "PUT " + commentURL}; !slices.Equal(f.Requests(), want) {
		t.Errorf("requests %v, want %v", f.Requests(), want)
	}
	// A mention reads Jira, so the comment is read again after it.
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		f.On("GET", commentURL, jiratest.Reply(200, `{"id":"900","body":`+docJSON(t, adf.Doc(para("changed")))+`}`))
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	_, err = c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: "thanks @[me]", Current: &read})
	if _, ok := errors.AsType[*StaleError](err); !ok || f.Count("PUT", commentURL) != 1 {
		t.Errorf("a change while the mention resolved: %v", err)
	}
	for name, in := range map[string]CommentEdit{
		"Current and From":        {Issue: "WEB-1", ID: "900", Markdown: "x", Current: &read, From: &read},
		"Current and Unchecked":   {Issue: "WEB-1", ID: "900", Markdown: "x", Current: &read, Unchecked: true},
		"Current another comment": {Issue: "WEB-1", ID: "901", Markdown: "x", Current: &read},
	} {
		if _, err := c.EditComment(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestUpdateWorklogCurrent proves the same of a worklog edit.
func TestUpdateWorklogCurrent(t *testing.T) {
	s := newWorklogSite(t, docJSON(t, adf.Doc(para("old"))), "null")
	s.put = func(_ *http.Request, b []byte) string {
		jiratest.WantJSON(t, b, `{"timeSpentSeconds":3600}`)
		return worklogJSONWith(docJSON(t, adf.Doc(para("old"))), "null")
	}
	read, err := s.c.Worklog(t.Context(), "WEB-1", "300")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Current: &read, TimeSpent: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"GET " + worklog300, "PUT " + worklog300}; !slices.Equal(s.f.Requests(), want) {
		t.Errorf("requests %v, want %v", s.f.Requests(), want)
	}
	// What the edit asks for is compared with the base: the stored time
	// sends nothing.
	if res, err := s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Current: &read, TimeSpent: read.TimeSpent}); err != nil || !res.Unchanged {
		t.Errorf("the stored time: %+v %v", res, err)
	}
	s.f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		changed := worklogJSONWith(docJSON(t, adf.Doc(para("changed while the mention resolved"))), "null")
		s.stored.Store(&changed)
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	md := "thanks @[me]"
	_, err = s.c.UpdateWorklog(t.Context(), WorklogEdit{Issue: "WEB-1", ID: "300", Current: &read, Markdown: &md})
	if _, ok := errors.AsType[*StaleError](err); !ok || s.f.Count("PUT", worklog300) != 1 {
		t.Errorf("a change while the mention resolved: %v", err)
	}
	for name, in := range map[string]WorklogEdit{
		"Current and From":        {Issue: "WEB-1", ID: "300", Current: &read, From: &read, TimeSpent: time.Hour},
		"Current and Unchecked":   {Issue: "WEB-1", ID: "300", Current: &read, Unchecked: true, TimeSpent: time.Hour},
		"Current another worklog": {Issue: "WEB-1", ID: "301", Current: &read, TimeSpent: time.Hour},
	} {
		if _, err := s.c.UpdateWorklog(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestLinkTypeByID proves a link type named by its ID is sent with no
// read of the link types.
func TestLinkTypeByID(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("POST", "/rest/api/3/issueLink", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"type":{"id":"10000"},"outwardIssue":{"key":"WEB-1"},"inwardIssue":{"key":"WEB-2"}}`)
		w.WriteHeader(201)
	})
	if err := c.LinkIssues(t.Context(), LinkInput{Type: "10000", Outward: "WEB-1", Inward: "WEB-2"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"POST /rest/api/3/issueLink"}; !slices.Equal(f.Requests(), want) {
		t.Errorf("requests %v", f.Requests())
	}
}

// TestCreateIssueTypeByID proves an issue type named by its ID reads only
// the create screen, which names it, and one the project cannot create
// fails there before anything is sent.
func TestCreateIssueTypeByID(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/1", jiratest.Reply(200, `{"total":2,"fields":[
		{"fieldId":"summary","key":"summary","name":"Summary","required":true,"schema":{"type":"string","system":"summary"},"operations":["set"]},
		{"fieldId":"issuetype","key":"issuetype","name":"Issue Type","required":true,"schema":{"type":"issuetype","system":"issuetype"},"operations":[],"allowedValues":[{"id":"1","name":"Task"}]}]}`))
	f.On("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/99", jiratest.Reply(404, `{"errorMessages":["Issue type with id 99 not found"]}`))
	plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "1", Fields: []FieldInput{{Field: "Summary", Value: Text("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Preview().IssueType; got.ID != "1" || got.Name != "Task" {
		t.Errorf("the type is %+v, want Task from the screen", got)
	}
	if _, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "99", Fields: []FieldInput{{Field: "Summary", Value: Text("x")}}}); err == nil || !trackerapi.WriteNotApplied(err) {
		t.Errorf("a type the project lacks: %v", err)
	}
	if f.Count("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes") != 0 {
		t.Error("the issue types were listed")
	}
}

// TestAddCommentPublicReadsNoProject proves a public comment, which every
// project takes, is sent with no read of the issue's project.
func TestAddCommentPublicReadsNoProject(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("POST", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var body struct {
			Body       json.RawMessage  `json:"body"`
			Properties []map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(b, &body)
		if len(body.Properties) > 0 {
			t.Errorf("a public comment carries properties: %s", b)
		}
		storedCommentReply(string(body.Body))(w, r, b)
	})
	if _, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "x", Audience: AudiencePublic}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"POST /rest/api/3/issue/WEB-1/comment"}; !slices.Equal(f.Requests(), want) {
		t.Errorf("requests %v", f.Requests())
	}
}

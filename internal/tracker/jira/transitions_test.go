package jira

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

const transitionsJSON = `{"transitions":[
 {"id":"21","name":"Start","to":{"id":"3","name":"In Progress","statusCategory":{"key":"indeterminate"}},"hasScreen":false,"isAvailable":true,"fields":{}},
 {"id":"31","name":"Resolve","to":{"id":"5","name":"Done","statusCategory":{"key":"done"}},"hasScreen":true,"isAvailable":true,"fields":{
   "resolution":{"key":"resolution","name":"Resolution","required":true,"schema":{"type":"resolution","system":"resolution"},"operations":["set"],"allowedValues":[{"id":"1","name":"Fixed"},{"id":"2","name":"Won't Do"}]}}},
 {"id":"41","name":"Reopen","to":{"id":"1","name":"To Do","statusCategory":{"key":"new"}},"isAvailable":false}
]}`

func transitionSite(t *testing.T, typeKey string) (*jiratest.Server, *Client) {
	f, c := commentSite(t, typeKey)
	f.On("GET", "/rest/api/3/issue/WEB-1/transitions", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "expand", "transitions.fields")
		jiratest.Reply(200, transitionsJSON)(w, r, b)
	})
	return f, c
}

func TestTransitionsDecode(t *testing.T) {
	_, c := transitionSite(t, "software")
	ts, err := c.Transitions(t.Context(), "WEB-1")
	if err != nil || len(ts) != 3 || ts[1].To.Category.Key != "done" || len(ts[1].Fields.Fields) != 1 || ts[2].Available {
		t.Fatalf("got %+v %v", ts, err)
	}
}

func TestTransitionWithFieldAndComment(t *testing.T) {
	f, c := transitionSite(t, "software")
	f.On("POST", "/rest/api/3/issue/WEB-1/transitions", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"transition":{"id":"31"},"fields":{"resolution":{"id":"1"}},
			"update":{"comment":[{"add":{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Done now"}]}]}}}]}}`)
		w.WriteHeader(204)
	})
	res, err := c.Transition(t.Context(), TransitionInput{Issue: "WEB-1", Transition: "resolve",
		Fields: []FieldInput{{Field: "Resolution", Value: Text("Fixed")}}, Comment: "Done now"})
	if err != nil || !res.Moved || res.Comment != nil {
		t.Fatalf("got %+v %v", res, err)
	}
}

// TestTransitionOnServiceDesk proves the comment follows the move as its
// own request, with its audience, and that its failure says the issue
// moved.
func TestTransitionOnServiceDesk(t *testing.T) {
	f, c := transitionSite(t, "service_desk")
	f.On("POST", "/rest/api/3/issue/WEB-1/transitions", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"transition":{"id":"21"}}`)
		w.WriteHeader(204)
	})
	f.On("POST", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), "sd.public.comment") {
			t.Errorf("comment %s", b)
		}
		jiratest.Reply(500, `{}`)(w, r, b)
	})
	_, err := c.Transition(t.Context(), TransitionInput{Issue: "WEB-1", Transition: "Start", Comment: "Working on it"})
	if !trackerapi.WriteNotApplied(err) {
		t.Errorf("a service desk comment without an audience: %v", err)
	}
	if n := f.Count("POST", "/rest/api/3/issue/WEB-1/transitions"); n != 0 {
		t.Fatalf("moved before the audience was checked: %d", n)
	}
	res, err := c.Transition(t.Context(), TransitionInput{Issue: "WEB-1", Transition: "21", Comment: "Working on it", Audience: AudienceInternal})
	se, ok := errors.AsType[*StepError](err)
	if !ok || se.Step != StepComment || !se.OutcomeUnknown() || !res.Moved {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestTransitionRefusals(t *testing.T) {
	f, c := transitionSite(t, "software")
	for name, in := range map[string]TransitionInput{
		"unavailable":    {Issue: "WEB-1", Transition: "Reopen"},
		"unknown":        {Issue: "WEB-1", Transition: "Close"},
		"bad value":      {Issue: "WEB-1", Transition: "Resolve", Fields: []FieldInput{{Field: "Resolution", Value: Text("Duplicate")}}},
		"off the screen": {Issue: "WEB-1", Transition: "Start", Fields: []FieldInput{{Field: "Resolution", Value: Text("Fixed")}}},
		"required clear": {Issue: "WEB-1", Transition: "Resolve", Fields: []FieldInput{{Field: "Resolution", Value: Clear()}}},
		"set twice": {Issue: "WEB-1", Transition: "Resolve", Fields: []FieldInput{
			{Field: "Resolution", Value: Text("Fixed")}, {Field: "resolution", Value: Text("Won't Do")}}},
		"blank comment":   {Issue: "WEB-1", Transition: "Start", Comment: "<!-- -->"},
		"bad issue in id": {Issue: "WEB-1/x", Transition: "Start"},
	} {
		if _, err := c.Transition(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := f.Count("POST", "/rest/api/3/issue/WEB-1/transitions"); n != 0 {
		t.Errorf("%d moves sent", n)
	}
}

// TestTransitionLossCheck proves a rich text field on a transition screen
// gets the same loss check as an edit.
func TestTransitionLossCheck(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/transitions", jiratest.Reply(200, `{"transitions":[{"id":"31","name":"Resolve","isAvailable":true,"fields":{
		"description":{"key":"description","name":"Description","schema":{"type":"string","system":"description"},"operations":["set"]}}}]}`))
	f.On("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "fields", "description")
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"description":`+docJSON(t, layoutDoc())+`}}`)(w, r, b)
	})
	f.On("POST", "/rest/api/3/issue/WEB-1/transitions", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	flattened := strings.NewReplacer("<!-- layout -->\n\n", "", "<!-- column 50% -->\n\n", "", "<!-- /column -->\n\n", "", "<!-- /layout -->\n", "").
		Replace(adf.ToMarkdown(layoutDoc()))
	in := TransitionInput{Issue: "WEB-1", Transition: "Resolve", Fields: []FieldInput{{Field: "description", Value: Text(flattened)}}}
	if _, err := c.Transition(t.Context(), in); !errors.As(err, new(*adf.LossError)) || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("loss: %v", err)
	}
	if n := f.Count("POST", "/rest/api/3/issue/WEB-1/transitions"); n != 0 {
		t.Fatalf("%d moves sent", n)
	}
	in.AcceptLoss = true
	if res, err := c.Transition(t.Context(), in); err != nil || !res.Moved {
		t.Fatalf("accepted loss: %+v %v", res, err)
	}
}

// TestTransitionChecksLast proves the rich text field's stale check is
// the last read before the move: the comment's mention is read first, so
// a change to the field made while it is read is caught, and nothing is
// moved.
func TestTransitionChecksLast(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	var desc atomic.Pointer[string]
	first := docJSON(t, adf.Doc(para("first")))
	desc.Store(&first)
	f.On("GET", "/rest/api/3/issue/WEB-1/transitions", jiratest.Reply(200, `{"transitions":[{"id":"31","name":"Resolve","isAvailable":true,"fields":{
		"description":{"key":"description","name":"Description","schema":{"type":"string","system":"description"},"operations":["set"]}}}]}`))
	f.On("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		switch fields := r.URL.Query().Get("fields"); fields {
		case "description":
			jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"description":`+*desc.Load()+`}}`)(w, r, b)
		case "project":
			jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","projectTypeKey":"software"}}}`)(w, r, b)
		default:
			t.Errorf("fields %q", fields)
		}
	})
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		changed := docJSON(t, adf.Doc(para("changed")))
		desc.Store(&changed)
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	in := TransitionInput{Issue: "WEB-1", Transition: "Resolve", Fields: []FieldInput{{Field: "description", Value: Text("new text")}}, Comment: "thanks @[me]"}
	_, err := c.Transition(t.Context(), in)
	if _, ok := errors.AsType[*StaleError](err); !ok || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("got %v", err)
	}
	got := f.Requests()
	if tail := strings.Join(got[len(got)-2:], " | "); tail != "GET "+myselfURL+" | GET /rest/api/3/issue/WEB-1" {
		t.Errorf("requests %s", strings.Join(got, " | "))
	}
}

// TestTransitionRequiredFieldsBeforeUploads proves a required screen
// field the move leaves provably empty is refused before any image is
// uploaded, and one the issue already holds is let through.
func TestTransitionRequiredFieldsBeforeUploads(t *testing.T) {
	f, c := transitionSite(t, "software")
	var resolution atomic.Pointer[string]
	empty, fixed := `null`, `{"id":"1","name":"Fixed"}`
	resolution.Store(&empty)
	f.On("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.URL.Query().Get("fields") == "project" {
			jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","projectTypeKey":"software"}}}`)(w, r, b)
			return
		}
		jiratest.WantQuery(t, r, "fields", "resolution")
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"resolution":`+*resolution.Load()+`}}`)(w, r, b)
	})
	var uploads atomic.Int32
	imageRoutes(t, f, "WEB-1", &uploads)
	f.On("POST", "/rest/api/3/issue/WEB-1/transitions", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	in := TransitionInput{Issue: "WEB-1", Transition: "Resolve", Comment: "![a](a.png)", ImageDir: imageDir(t, map[string][]byte{"a.png": pngBytes})}
	_, err := c.Transition(t.Context(), in)
	if mf, ok := errors.AsType[*MissingFieldsError](err); !ok || mf.Fields[0].ID != "resolution" || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("got %v", err)
	}
	if uploads.Load() != 0 {
		t.Fatal("an image was uploaded before the refusal")
	}
	resolution.Store(&fixed)
	if res, err := c.Transition(t.Context(), in); err != nil || !res.Moved || uploads.Load() != 1 {
		t.Fatalf("a field the issue holds: %+v %v", res, err)
	}
}

func TestAssignAndUnassign(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	var bodies []string
	f.On("PUT", "/rest/api/3/issue/WEB-1/assignee", func(w http.ResponseWriter, r *http.Request, b []byte) {
		bodies = append(bodies, string(b))
		w.WriteHeader(204)
	})
	if err := c.Assign(t.Context(), "WEB-1", "acc-user1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Unassign(t.Context(), "WEB-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Assign(t.Context(), "WEB-1", ""); !trackerapi.WriteNotApplied(err) {
		t.Errorf("an empty account: %v", err)
	}
	if len(bodies) != 2 || bodies[0] != `{"accountId":"acc-user1"}` || bodies[1] != `{"accountId":null}` {
		t.Errorf("bodies %v", bodies)
	}
}

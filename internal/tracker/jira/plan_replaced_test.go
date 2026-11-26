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

// notesSite serves WEB-1 with a plain text field, Notes, whose stored value
// notes holds, and its rich description.
func notesSite(t *testing.T, notes *atomic.Pointer[string]) (*jiratest.Server, *Client) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/editmeta", jiratest.Reply(200, `{"fields":{
		"description":{"key":"description","name":"Description","required":false,"schema":{"type":"string","system":"description"},"operations":["set"]},
		"customfield_10050":{"key":"customfield_10050","name":"Notes","required":false,"schema":{"type":"string","custom":"com.atlassian.jira.plugin.system.customfieldtypes:textfield","customId":10050},"operations":["set"]}}}`))
	issue := func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"summary":"S","description":`+docJSON(t, adf.Doc(para("old")))+`,"customfield_10050":`+*notes.Load()+`}}`)(w, r, b)
	}
	f.On("GET", "/rest/api/3/issue/WEB-1", issue)
	f.On("GET", "/rest/api/3/issue/10001", issue)
	return f, c
}

// A plain text edit or clear written from what the field held is planned
// only while the field still holds it, whether it is named by ID or by
// name; a field holding nothing holds no text.
func TestAReplacedPlainTextIsCompared(t *testing.T) {
	var notes atomic.Pointer[string]
	stored := `"Some notes."`
	notes.Store(&stored)
	f, c := notesSite(t, &notes)
	for _, field := range []string{"customfield_10050", "Notes"} {
		for _, in := range []Input{Text("New notes.").Replacing("Some notes."), Clear().Replacing("Some notes.")} {
			if _, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: field, Value: in}}}); err != nil {
				t.Fatalf("%s written from what it holds: %v", field, err)
			}
		}
		_, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: field, Value: Text("New notes.").Replacing("Other notes.")}}})
		if _, stale := errors.AsType[*StaleError](err); !stale {
			t.Fatalf("%s written from other text: %v", field, err)
		}
	}
	empty := "null"
	notes.Store(&empty)
	if _, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "Notes", Value: Text("First.").Replacing()}}}); err != nil {
		t.Fatalf("an empty field written from nothing: %v", err)
	}
	if _, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "Notes", Value: Text("First.").Replacing("Some notes.")}}}); err == nil {
		t.Fatal("an empty field written from text was planned")
	}
	if n := f.Count("PUT", "/rest/api/3/issue/10001"); n != 0 {
		t.Fatalf("a plan wrote %d times", n)
	}
	// What it replaces is a plain text's alone.
	if _, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Clear().Replacing("old")}}}); err == nil {
		t.Fatal("a rich text clear written from plain texts was planned")
	}
}

// A clear of a rich text written from Markdown is planned only while the
// stored document renders to it.
func TestAClearOfRichTextFromMarkdownIsCompared(t *testing.T) {
	var notes atomic.Pointer[string]
	empty := "null"
	notes.Store(&empty)
	_, c := notesSite(t, &notes)
	if _, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Clear().EditedFrom(adf.ToMarkdown(adf.Doc(para("old"))))}}}); err != nil {
		t.Fatalf("a clear from the stored text: %v", err)
	}
	_, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Clear().EditedFrom("other\n")}}})
	if _, stale := errors.AsType[*StaleError](err); !stale {
		t.Fatalf("a clear from other text: %v", err)
	}
}

// A create's value replaces nothing.
func TestACreateValueReplacesNothing(t *testing.T) {
	_, c := createSite(t)
	for _, fi := range []FieldInput{
		{Field: "summary", Value: Text("S").Replacing("y")},
		{Field: "description", Value: Text("x").EditedFrom("")},
	} {
		fields := []FieldInput{fi}
		if fi.Field != "summary" {
			fields = append(fields, FieldInput{Field: "summary", Value: Text("S")})
		}
		_, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "1", Fields: fields})
		if err == nil || !strings.Contains(err.Error(), "a create's value replaces nothing") || !trackerapi.WriteNotApplied(err) {
			t.Fatalf("a create's %s written from something = %v, want it refused as replacing nothing", fi.Field, err)
		}
	}
}

// A clear of rich text is an edit of it: a document holding what Markdown
// cannot show is refused for that loss until the loss is accepted, on an
// edit and on a transition's screen.
func TestAClearOfRichTextNeedsItsLossAccepted(t *testing.T) {
	desc := storedDesc(t, layoutDoc())
	_, c := editSite(t, desc)
	shown := adf.ToMarkdown(layoutDoc())
	in := EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Clear().EditedFrom(shown)}}}
	if _, err := c.PlanEdit(t.Context(), in); !errors.As(err, new(*adf.LossError)) || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("a clear of a layout = %v, want a loss refusal", err)
	}
	in.AcceptLoss = true
	if _, err := c.PlanEdit(t.Context(), in); err != nil {
		t.Fatalf("a clear with its loss accepted: %v", err)
	}

	f := jiratest.NewServer(t)
	c = newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/transitions", jiratest.Reply(200, `{"transitions":[{"id":"31","name":"Resolve","isAvailable":true,"fields":{
		"description":{"key":"description","name":"Description","schema":{"type":"string","system":"description"},"operations":["set"]}}}]}`))
	f.On("GET", "/rest/api/3/issue/WEB-1", jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"description":`+docJSON(t, layoutDoc())+`}}`))
	f.On("POST", "/rest/api/3/issue/WEB-1/transitions", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	move := TransitionInput{Issue: "WEB-1", Transition: "Resolve", Fields: []FieldInput{{Field: "description", Value: Clear().EditedFrom(shown)}}}
	if _, err := c.Transition(t.Context(), move); !errors.As(err, new(*adf.LossError)) || f.Count("POST", "/rest/api/3/issue/WEB-1/transitions") != 0 {
		t.Fatalf("a transition clearing a layout = %v, want a loss refusal and no move", err)
	}
	move.AcceptLoss = true
	if res, err := c.Transition(t.Context(), move); err != nil || !res.Moved {
		t.Fatalf("a transition clearing with its loss accepted: %+v, %v", res, err)
	}
}

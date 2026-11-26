package jira

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
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
	for _, in := range []Input{Text("x").Replacing("y"), Text("x").EditedFrom("y")} {
		if _, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "1", Fields: []FieldInput{{Field: "summary", Value: Text("S")}, {Field: "description", Value: in}}}); err == nil {
			t.Fatal("a create's value written from something was planned")
		}
	}
}

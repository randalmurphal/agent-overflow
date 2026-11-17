package jira

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/trackerapi"
)

// rawDesc is an edit of the description to raw.
func rawDesc(raw string, accept bool) EditInput {
	return EditInput{Issue: "WEB-1", AcceptLoss: accept, Fields: []FieldInput{{Field: "Description", Value: RawJSON(json.RawMessage(raw))}}}
}

func wantLoss(t *testing.T, what string, err error) {
	t.Helper()
	if _, ok := errors.AsType[*adf.LossError](err); !ok || !trackerapi.WriteNotApplied(err) {
		t.Errorf("%s: got %v, want a loss refusal", what, err)
	}
}

// TestRawRichTextKeepsTheProtections proves a raw document is a rich
// text write like Markdown: replacing a stored document with content
// needs loss acceptance, and the plan rechecks the stored document
// before writing.
func TestRawRichTextKeepsTheProtections(t *testing.T) {
	desc := storedDesc(t, layoutDoc())
	f, c := editSite(t, desc)
	raw := docJSON(t, adf.Doc(para("new")))
	_, err := c.PlanEdit(t.Context(), rawDesc(raw, false))
	wantLoss(t, "a raw document over content", err)
	_, err = c.PlanEdit(t.Context(), rawDesc(`null`, false))
	wantLoss(t, "a raw null over content", err)

	plan, err := c.PlanEdit(t.Context(), rawDesc(raw, true))
	if err != nil || len(plan.Preview().Fields) != 1 || len(plan.Preview().Fields[0].Warnings) == 0 {
		t.Fatalf("accepted: %+v %v", plan, err)
	}
	other := docJSON(t, adf.Doc(para("written meanwhile")))
	desc.Store(&other)
	_, err = c.ApplyEdit(t.Context(), plan)
	se, ok := errors.AsType[*StepError](err)
	if _, stale := errors.AsType[*StaleError](err); !ok || se.Step != StepCheck || !stale || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("a raw document over a concurrent change: %v", err)
	}
	if n := f.count("PUT", "/rest/api/3/issue/10001"); n != 0 {
		t.Errorf("%d writes over a concurrent change", n)
	}
}

// TestRawRichTextOverNothing proves a raw document over an empty field
// needs no acceptance, is written as given, and one equal to the stored
// document is unchanged.
func TestRawRichTextOverNothing(t *testing.T) {
	desc := storedDesc(t, nil)
	f, c := editSite(t, desc)
	raw := docJSON(t, adf.Doc(para("new")))
	f.on("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"fields":{"description":`+raw+`}}`)
		reply(200, `{"id":"10001","key":"WEB-1","fields":{"description":`+raw+`}}`)(w, r, b)
	})
	plan, err := c.PlanEdit(t.Context(), rawDesc(raw, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyEdit(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	desc.Store(&raw)
	plan, err = c.PlanEdit(t.Context(), rawDesc(raw, false))
	if err != nil || len(plan.Preview().Unchanged) != 1 {
		t.Fatalf("the stored document again: %+v %v", plan, err)
	}
}

// TestRawRichTextMustBeADocument proves a raw value for rich text is a
// valid document or null, and required text is never emptied raw.
func TestRawRichTextMustBeADocument(t *testing.T) {
	desc := storedDesc(t, nil)
	f, c := editSite(t, desc)
	for name, raw := range map[string]string{
		"a paragraph":     `{"type":"paragraph","content":[]}`,
		"an unknown node": `{"type":"doc","version":1,"content":[{"type":"marquee"}]}`,
		"no version":      `{"type":"doc","content":[]}`,
		"a string":        `"text"`,
		"an empty object": `{}`,
	} {
		_, err := c.PlanEdit(t.Context(), rawDesc(raw, true))
		if err == nil || !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	for _, raw := range []string{`null`, `[]`, `""`, `{}`} {
		in := EditInput{Issue: "WEB-1", AcceptLoss: true, Fields: []FieldInput{{Field: "Summary", Value: RawJSON(json.RawMessage(raw))}}}
		if _, err := c.PlanEdit(t.Context(), in); err == nil || !trackerapi.WriteNotApplied(err) {
			t.Errorf("summary %s: got %v", raw, err)
		}
	}
	if n := f.count("PUT", "/rest/api/3/issue/10001"); n != 0 {
		t.Errorf("%d writes", n)
	}
}

// TestRequiredRichTextIsNotEmptied proves a required rich text field is
// judged empty on content, raw and Markdown alike: a null, an empty
// document, empty paragraphs or whitespace are all refused, while an
// optional field takes the empty document.
func TestRequiredRichTextIsNotEmptied(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	env := meta("environment", "Environment", Schema{Type: "string", System: "environment"})
	env.Required = true
	empty := map[string]Input{
		"a raw null":              RawJSON(json.RawMessage(`null`)),
		"a raw empty document":    RawJSON(json.RawMessage(`{"type":"doc","version":1,"content":[]}`)),
		"raw empty paragraphs":    RawJSON(json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph"},{"type":"paragraph","content":[{"type":"hardBreak"}]}]}`)),
		"raw whitespace":          RawJSON(json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":" \t "}]}]}`)),
		"Markdown whitespace":     Text("  \n\n "),
		"Markdown with a comment": Text("<!-- -->"),
	}
	for name, value := range empty {
		if _, err := c.Value(t.Context(), env, value, ValueOptions{}); err == nil || !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	env.Required = false
	if v, err := c.Value(t.Context(), env, empty["a raw empty document"], ValueOptions{}); err != nil || string(v.Fields) != `{"type":"doc","version":1,"content":[]}` {
		t.Errorf("an optional field emptied raw: %+v %v", v, err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests", n)
	}
}

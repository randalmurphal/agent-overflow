package jiraprovider

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// pngBytes is enough of a PNG for content sniffing.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRinvented-image")

const (
	createMetaPath = "/rest/api/3/issue/createmeta/10000/issuetypes/10002"
	createPath     = "/rest/api/3/issue"
	issuePath      = "/rest/api/3/issue/10001"
	editMetaPath   = "/rest/api/3/issue/10001/editmeta"
	uploadPath     = "/rest/api/3/issue/10001/attachments"
)

// metaFields are the fields of the create and edit screens, by ID.
var metaFields = map[string]string{
	"summary":           `{"key":"summary","name":"Summary","required":true,"schema":{"type":"string","system":"summary"},"operations":["set"]}`,
	"description":       `{"key":"description","name":"Description","required":false,"schema":{"type":"string","system":"description"},"operations":["set"]}`,
	"priority":          `{"key":"priority","name":"Priority","required":false,"schema":{"type":"priority","system":"priority"},"operations":["set"],"allowedValues":[{"id":"1","name":"High"},{"id":"3","name":"Medium"},{"id":"4","name":"Low"}]}`,
	"labels":            `{"key":"labels","name":"Labels","required":false,"schema":{"type":"array","items":"string","system":"labels"},"operations":["add","set","remove"]}`,
	"customfield_10050": `{"key":"customfield_10050","name":"Severity","required":false,"schema":{"type":"option","custom":"com.atlassian.jira.plugin.system.customfieldtypes:select","customId":10050},"operations":["set"],"allowedValues":[{"id":"20","value":"Minor"},{"id":"21","value":"MINOR"},{"id":"22","value":"Major"}]}`,
	"customfield_10016": `{"key":"customfield_10016","name":"Story points","required":false,"schema":{"type":"number","custom":"com.atlassian.jira.plugin.system.customfieldtypes:float","customId":10016},"operations":["set"]}`,
	"customfield_10070": `{"key":"customfield_10070","name":"Region","required":false,"schema":{"type":"option-with-child","custom":"com.atlassian.jira.plugin.system.customfieldtypes:cascadingselect","customId":10070},"operations":["set"],"allowedValues":[{"id":"30","value":"North","children":[{"id":"31","value":"East"}]}]}`,
	"customfield_10020": `{"key":"customfield_10020","name":"Sprint","required":false,"schema":{"type":"array","items":"json","custom":"com.pyxis.greenhopper.jira:gh-sprint","customId":10020},"operations":["set"]}`,
}

// createMeta is the create screen of WEB's stories with ids, and with a
// required Severity when required is set.
func createMeta(required bool, ids ...string) string {
	var fields []string
	for _, id := range ids {
		meta := metaFields[id]
		if required && id == "customfield_10050" {
			meta = strings.Replace(meta, `"required":false`, `"required":true`, 1)
		}
		fields = append(fields, `{"fieldId":"`+id+`",`+meta[1:])
	}
	return `{"total":` + decimal(int64(len(fields))) + `,"fields":[` + strings.Join(fields, ",") + `]}`
}

// createSite is newSite with the create screen of WEB's stories.
func createSite(t *testing.T, required bool) (*jiratest.Server, *Provider) {
	f, p := newSite(t)
	f.On("GET", "/rest/api/3/issue/createmeta/10000/issuetypes", jiratest.Reply(200, `{"issueTypes":[{"id":"10002","name":"Story"}],"total":1}`))
	f.On("GET", createMetaPath, jiratest.Reply(200, createMeta(required, "summary", "description", "priority", "labels", "customfield_10050", "customfield_10070", "customfield_10020")))
	return f, p
}

// layoutDoc is a description with a two-column layout, which Markdown
// writes with comment markers.
func layoutDoc() *adf.Node {
	col := func(s string) *adf.Node {
		return &adf.Node{Type: "layoutColumn", Attrs: map[string]any{"width": 50.0}, Content: []*adf.Node{para(s)}}
	}
	return adf.Doc(para("intro"), &adf.Node{Type: "layoutSection", Content: []*adf.Node{col("left"), col("right")}})
}

func para(s string) *adf.Node {
	return &adf.Node{Type: "paragraph", Content: []*adf.Node{{Type: "text", Text: s}}}
}

// editSite is newSite with issue 10001's edit screen, its description
// read from desc on every request.
func editSite(t *testing.T, desc *atomic.Pointer[adf.Node]) (*jiratest.Server, *Provider) {
	f, p := newSite(t)
	var fields []string
	for _, id := range []string{"summary", "description", "priority", "labels", "customfield_10050", "customfield_10016"} {
		fields = append(fields, `"`+id+`":`+metaFields[id])
	}
	f.On("GET", editMetaPath, jiratest.Reply(200, `{"fields":{`+strings.Join(fields, ",")+`}}`))
	f.On("GET", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		d, err := json.Marshal(desc.Load())
		if err != nil {
			t.Error(err)
		}
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"summary":"S","description":`+string(d)+`}}`)(w, r, b)
	})
	return f, p
}

func storedDesc(doc *adf.Node) *atomic.Pointer[adf.Node] {
	var p atomic.Pointer[adf.Node]
	p.Store(doc)
	return &p
}

func TestEdit(t *testing.T) {
	desc := storedDesc(adf.Doc(para("old")))
	f, p := editSite(t, desc)
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"fields":{"summary":"New","priority":{"id":"4"},"customfield_10050":{"id":"21"},"customfield_10016":5,"labels":[],
			"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"new"}]}]}}}`)
		w.WriteHeader(http.StatusNoContent)
	})
	res := checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{
		{Field: tracker.FieldTitle, Value: tracker.TypedValues("New")},
		{Field: "priority", Value: tracker.TypedValues("LOW")},
		{Field: "customfield_10050", Value: tracker.PickValues("21")},
		{Field: "customfield_10016", Value: tracker.TypedValues("5")},
		{Field: "labels", Value: tracker.ClearField()},
		{Field: "description", Value: tracker.MarkdownEdit("new", adf.ToMarkdown(adf.Doc(para("old"))))},
	}}))
	if states(res) != "edit:applied" || res.Parts[0].Issue != "10001" {
		t.Fatalf("got %s: %v", states(res), res.Parts[0].Outcome.Err())
	}
}

// parentMeta is the parent as an edit screen offers it: a set, with no
// allowed values.
const parentMeta = `{"key":"parent","name":"Parent","required":false,"schema":{"type":"issuelink","system":"parent"},"operations":["set"]}`

// TestEditParent proves the parent is edited by issue ID, cleared by
// the jira client's update operation, and that a parent Jira does not take is refused by its answer
// with the parent's message.
func TestEditParent(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", editMetaPath, jiratest.Reply(200, `{"fields":{"parent":`+parentMeta+`}}`))
	for _, is := range []string{`"10001","key":"WEB-1","fields":{"summary":"Story"}`, `"10005","key":"WEB-5","fields":{"summary":"Epic"}`} {
		f.On("GET", "/rest/api/3/issue/"+is[1:6], func(w http.ResponseWriter, r *http.Request, b []byte) {
			// The edit's own read asks for the fields it names by ID; the
			// parent's read, for its summary.
			if want := map[string]string{"10001": "summary,parent", "10005": "summary"}[is[1:6]]; r.URL.Query().Get("fields") != want {
				t.Errorf("%s: fields %q, want %q", r.URL.Path, r.URL.Query().Get("fields"), want)
			}
			jiratest.Reply(200, `{"id":`+is+`}`)(w, r, b)
		})
	}
	var bodies []string
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		bodies = append(bodies, string(b))
		if len(bodies) == 3 {
			jiratest.Reply(400, `{"errorMessages":[],"errors":{"parent":"Given parent work item does not belong to appropriate hierarchy."}}`)(w, r, b)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	edit := func(v tracker.Edit) tracker.Result {
		return checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: tracker.FieldParent, Value: v}}}))
	}
	for _, v := range []tracker.Edit{tracker.PickValues("10005"), tracker.ClearField()} {
		if res := edit(v); states(res) != "edit:applied" {
			t.Fatalf("%+v: %s %v", v, states(res), res.Parts[0].Outcome.Err())
		}
	}
	res := edit(tracker.PickValues("10005"))
	r, _ := res.Parts[0].Outcome.Refusal()
	if r.Reason != tracker.ReasonTracker || len(r.Fields) != 1 || r.Fields[0].Field != tracker.FieldParent {
		t.Errorf("a parent Jira does not take: %s %+v", states(res), r)
	}
	want := []string{`{"fields":{"parent":{"id":"10005"}}}`, `{"fields":{},"update":{"parent":[{"set":{"none":true}}]}}`, `{"fields":{"parent":{"id":"10005"}}}`}
	if len(bodies) != len(want) {
		t.Fatalf("sent %q", bodies)
	}
	for i := range want {
		jiratest.WantJSON(t, []byte(bodies[i]), want[i])
	}
}

// TestEditRichTextChecks proves a Markdown edit is checked against the
// field as stored: written from other text it is stale, losing what
// Markdown cannot show it is refused unless the loss is accepted, and
// in neither refusal is anything written.
func TestEditRichTextChecks(t *testing.T) {
	desc := storedDesc(layoutDoc())
	f, p := editSite(t, desc)
	stored := adf.ToMarkdown(layoutDoc())
	var writes atomic.Int32
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		writes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	edit := func(md, from string, accept bool) tracker.Result {
		return checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001", AcceptLoss: accept,
			Fields: []tracker.FieldEdit{{Field: "description", Value: tracker.MarkdownEdit(md, from)}}}))
	}
	if res := edit("mine", "what I read yesterday", false); reason(res, 0) != tracker.ReasonStale {
		t.Errorf("written from other text: %s %d", states(res), reason(res, 0))
	}
	flattened := stored
	for _, m := range []string{"<!-- layout -->", "<!-- column 50% -->", "<!-- /column -->", "<!-- column 50% -->", "<!-- /column -->", "<!-- /layout -->"} {
		flattened = strings.Replace(flattened, m+"\n\n", "", 1)
	}
	res := edit(flattened, stored, false)
	r, _ := res.Parts[0].Outcome.Refusal()
	if r.Reason != tracker.ReasonLoss || len(r.Lost) == 0 {
		t.Errorf("a loss: %s %+v", states(res), r)
	}
	if writes.Load() != 0 {
		t.Fatalf("a refused edit was written")
	}
	if res := edit(flattened, stored, true); states(res) != "edit:applied" || writes.Load() != 1 {
		t.Errorf("the loss accepted: %s, %d writes", states(res), writes.Load())
	}
	// An empty description: the edit is written from "".
	desc.Store(nil)
	if res := edit("first", "", false); states(res) != "edit:applied" {
		t.Errorf("filling an empty field: %s: %v", states(res), res.Parts[0].Outcome.Err())
	}
	if res := edit("first", "something", false); reason(res, 0) != tracker.ReasonStale {
		t.Errorf("an empty field edited from text: %s", states(res))
	}
}

// TestEmptiedTextIsAClearFromWhatItReplaced proves an emptied Markdown
// edit of a stored text is a clear checked against the text it was written
// from: stale when the field holds other text, nothing written, and sent
// as an emptied field when it holds that text.
func TestEmptiedTextIsAClearFromWhatItReplaced(t *testing.T) {
	desc := storedDesc(adf.Doc(para("old")))
	f, p := editSite(t, desc)
	var bodies []string
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		bodies = append(bodies, string(b))
		w.WriteHeader(http.StatusNoContent)
	})
	clear := func(from string) tracker.Result {
		return checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001",
			Fields: []tracker.FieldEdit{{Field: "description", Value: tracker.MarkdownEdit("", from)}}}))
	}
	if res := clear("what I read yesterday\n"); reason(res, 0) != tracker.ReasonStale || len(bodies) != 0 {
		t.Fatalf("a clear from other text: %s, %d writes", states(res), len(bodies))
	}
	if res := clear(adf.ToMarkdown(adf.Doc(para("old")))); states(res) != "edit:applied" || len(bodies) != 1 {
		t.Fatalf("a clear from the stored text: %s, %d writes", states(res), len(bodies))
	}
	jiratest.WantJSON(t, []byte(bodies[0]), `{"fields":{"description":null}}`)
}

// TestAnEmptiedLayoutNeedsItsLossAccepted proves a person's emptied
// description keeps the loss consent an edit has: a document holding what
// Markdown cannot show is refused for that loss, and sent once the loss
// is accepted.
func TestAnEmptiedLayoutNeedsItsLossAccepted(t *testing.T) {
	f, p := editSite(t, storedDesc(layoutDoc()))
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) { w.WriteHeader(http.StatusNoContent) })
	clear := func(accept bool) tracker.Result {
		return checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001", AcceptLoss: accept,
			Fields: []tracker.FieldEdit{{Field: "description", Value: tracker.MarkdownEdit("", adf.ToMarkdown(layoutDoc()))}}}))
	}
	if res := clear(false); reason(res, 0) != tracker.ReasonLoss || f.Count("PUT", issuePath) != 0 {
		t.Fatalf("an emptied layout: %s, %d writes", states(res), f.Count("PUT", issuePath))
	}
	if res := clear(true); states(res) != "edit:applied" || f.Count("PUT", issuePath) != 1 {
		t.Fatalf("an emptied layout with its loss accepted: %s", states(res))
	}
}

// An edit whose every value the issue already holds sends nothing, and
// its part is applied, unchanged and names nothing to read again.
func TestAnEditThatChangesNothingIsUnchanged(t *testing.T) {
	doc := adf.Doc(para("same"))
	f, p := editSite(t, storedDesc(doc))
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) { w.WriteHeader(http.StatusNoContent) })
	md := adf.ToMarkdown(doc)
	res := checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001",
		Fields: []tracker.FieldEdit{{Field: "description", Value: tracker.MarkdownEdit(md, md)}}}))
	if states(res) != "edit:applied" || !res.Parts[0].Unchanged || len(res.Parts[0].Changed) != 0 || f.Count("PUT", issuePath) != 0 {
		t.Fatalf("an edit to what is stored: %s, %+v, %d writes", states(res), res.Parts, f.Count("PUT", issuePath))
	}
	res = checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001",
		Fields: []tracker.FieldEdit{{Field: "description", Value: tracker.MarkdownEdit("other", md)}}}))
	if states(res) != "edit:applied" || res.Parts[0].Unchanged || f.Count("PUT", issuePath) != 1 {
		t.Fatalf("an edit that changes the text: %s, %+v", states(res), res.Parts)
	}
}

// A refusal names the field by its name quoted.
func TestAnEditRefusalQuotesTheFieldsName(t *testing.T) {
	_, p := editSite(t, storedDesc(adf.Doc(para("old"))))
	res := checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001",
		Fields: []tracker.FieldEdit{{Field: "priority", Value: tracker.MarkdownEdit("High", "")}}}))
	r, _ := res.Parts[0].Outcome.Refusal()
	if want := `"Priority" is not rich text`; !strings.Contains(r.Message, want) {
		t.Fatalf("a Markdown edit of a priority: %+v, want %s", r, want)
	}
}

// TestEditOfAReplacedPlainText proves a plain text edit written from
// texts reaches Jira's plan, which compares them with the stored text,
// and that one of any other kind is refused before anything is read.
func TestEditOfAReplacedPlainText(t *testing.T) {
	f, p := editSite(t, storedDesc(adf.Doc(para("old"))))
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) { w.WriteHeader(http.StatusNoContent) })
	edit := func(field tracker.FieldID, v tracker.Edit) tracker.Result {
		return checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: field, Value: v}}}))
	}
	if res := edit("summary", tracker.TypedValues("New").Replacing("Other")); reason(res, 0) != tracker.ReasonStale {
		t.Fatalf("written from other text: %s", states(res))
	}
	if res := edit("summary", tracker.TypedValues("New").Replacing("S")); states(res) != "edit:applied" {
		t.Fatalf("written from the stored text: %s: %v", states(res), res.Parts[0].Outcome.Err())
	}
	if res := edit("priority", tracker.TypedValues("High").Replacing("Low")); reason(res, 0) != tracker.ReasonInvalid {
		t.Fatalf("a priority written from texts: %s", states(res))
	}
	if n := f.Count("PUT", issuePath); n != 1 {
		t.Fatalf("%d writes, want the one from the stored text", n)
	}
}

// TestEditOutcomes proves an edit's answer becomes its part's outcome.
func TestEditOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
		reason tracker.Reason
	}{
		{400, "edit:refused", tracker.ReasonTracker},
		{403, "edit:refused", tracker.ReasonTracker},
		{500, "edit:unknown", 0},
	} {
		desc := storedDesc(adf.Doc(para("old")))
		f, p := editSite(t, desc)
		f.On("PUT", issuePath, jiratest.Reply(tc.status, `{"errorMessages":["no"],"errors":{"priority":"not allowed"}}`))
		res := checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: "priority", Value: tracker.PickValues("1")}}}))
		if states(res) != tc.want || reason(res, 0) != tc.reason {
			t.Errorf("%d: %s", tc.status, states(res))
		}
		if n := f.Count("PUT", issuePath); n != 1 {
			t.Errorf("%d: %d writes", tc.status, n)
		}
	}
	_, p := newSite(t)
	if res := checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "WEB-1", Fields: []tracker.FieldEdit{{Field: "priority", Value: tracker.PickValues("1")}}})); reason(res, 0) != tracker.ReasonInvalid {
		t.Errorf("an issue key for an ID: %s", states(res))
	}
}

func TestMove(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", issuePath, jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","projectTypeKey":"software"}}}`))
	f.On("GET", "/rest/api/3/issue/10001/transitions", jiratest.Reply(200, `{"transitions":[
		{"id":"31","name":"Resolve","to":{"id":"10002","name":"Done","statusCategory":{"key":"done"}},"hasScreen":true,"isAvailable":true,"fields":{
		 "resolution":{"key":"resolution","name":"Resolution","required":true,"schema":{"type":"resolution","system":"resolution"},"operations":["set"],"allowedValues":[{"id":"1","name":"Fixed"}]}}}]}`))
	f.On("POST", "/rest/api/3/issue/10001/transitions", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"transition":{"id":"31"},"fields":{"resolution":{"id":"1"}}}`)
		w.WriteHeader(http.StatusNoContent)
	})
	res := checked(t, p.Move(t.Context(), tracker.MoveInput{Issue: "10001", Move: "31", Fields: []tracker.FieldEdit{{Field: "resolution", Value: tracker.TypedValues("fixed")}}}))
	if states(res) != "move:applied" {
		t.Fatalf("got %s: %v", states(res), res.Parts[0].Outcome.Err())
	}
	res = checked(t, p.Move(t.Context(), tracker.MoveInput{Issue: "10001", Move: "31"}))
	if reason(res, 0) != tracker.ReasonMissing {
		t.Errorf("a move without its required field: %s", states(res))
	}
	if n := f.Count("POST", "/rest/api/3/issue/10001/transitions"); n != 1 {
		t.Errorf("%d moves", n)
	}
}

func TestSetEstimate(t *testing.T) {
	f, p := newSite(t)
	listsIssues(t, f, board3Issues, "10001")
	listsIssues(t, f, board4Issues, "10001")
	var bodies []string
	f.On("PUT", "/rest/agile/1.0/issue/10001/estimation", func(w http.ResponseWriter, r *http.Request, b []byte) {
		bodies = append(bodies, r.URL.Query().Get("boardId")+" "+string(b))
		field := "customfield_10016"
		if r.URL.Query().Get("boardId") == "4" {
			field = "timeoriginalestimate"
		}
		jiratest.Reply(200, `{"fieldId":"`+field+`","value":"5"}`)(w, r, b)
	})
	for _, in := range []tracker.EstimateInput{
		{Board: "3", Issue: "10001", Value: ptr(5.5)},
		{Board: "4", Issue: "10001", Value: ptr(5400.0)},
		{Board: "3", Issue: "10001"},
		{Board: "3", Issue: "10001", Value: ptr(0.0)},
		{Board: "4", Issue: "10001", Value: ptr(0.0)},
	} {
		res := checked(t, p.SetEstimate(t.Context(), in))
		if states(res) != "estimate:applied" {
			t.Fatalf("%+v: %s %v", in, states(res), res.Parts[0].Outcome.Err())
		}
		if want := []tracker.Resource{tracker.RowOf(in.Board, "10001"), tracker.CardOf("10001"), tracker.DetailOf("10001")}; !slices.Equal(res.Parts[0].Changed, want) {
			t.Errorf("changed %+v", res.Parts[0].Changed)
		}
	}
	if want := `3 {"value":"5.5"}|4 {"value":"90m"}|3 {"value":null}|3 {"value":"0"}|4 {"value":"0m"}`; strings.Join(bodies, "|") != want {
		t.Errorf("sent %s", strings.Join(bodies, "|"))
	}
	for name, in := range map[string]tracker.EstimateInput{
		"part of a minute": {Board: "4", Issue: "10001", Value: ptr(90.0)},
		"too long":         {Board: "4", Issue: "10001", Value: ptr(1e300)},
		"a board name":     {Board: "Board 3", Issue: "10001", Value: ptr(1.0)},
	} {
		if res := checked(t, p.SetEstimate(t.Context(), in)); reason(res, 0) != tracker.ReasonInvalid {
			t.Errorf("%s: %s", name, states(res))
		}
	}
	// Seconds past a Duration's range are refused before any conversion,
	// whose result Go leaves to the implementation.
	res := checked(t, p.SetEstimate(t.Context(), tracker.EstimateInput{Board: "4", Issue: "10001", Value: ptr(1e300)}))
	if !errors.Is(res.Parts[0].Outcome.Err(), tracker.ErrInvalidInput) {
		t.Errorf("too long: %v", res.Parts[0].Outcome.Err())
	}
	if len(bodies) != 5 {
		t.Errorf("%d writes", len(bodies))
	}
}

// rankEntry is one issue's entry of a 207 per-issue answer.
func rankEntry(id string, status int) string {
	return `{"issueId":` + id + `,"issueKey":"WEB-` + id[len(id)-1:] + `","status":` + decimal(int64(status)) + `,"errors":["rank refused"]}`
}

// TestRankKeepsEachIssuesOutcome proves every issue of a rank or a sprint
// move keeps the outcome Jira gave it, in a mixed answer.
func TestRankKeepsEachIssuesOutcome(t *testing.T) {
	f, p := newSite(t)
	listsIssues(t, f, board3Issues, "10001", "10002", "10009", "10010")
	f.On("PUT", "/rest/agile/1.0/issue/rank", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001","10002"],"rankAfterIssue":"10009","rankCustomFieldId":10019}`)
		jiratest.Reply(207, `{"entries":[`+rankEntry("10001", 204)+`,`+rankEntry("10002", 400)+`]}`)(w, r, b)
	})
	res := checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001", "10002"},
		Prev: "10009", Next: "10010"}))
	if states(res) != "rank:applied, rank:refused" || reason(res, 1) != tracker.ReasonTracker {
		t.Fatalf("got %s", states(res))
	}
	if r, _ := res.Parts[1].Outcome.Refusal(); !strings.Contains(r.Message, "rank refused") {
		t.Errorf("refusal %q", r.Message)
	}
	if want := []tracker.Resource{tracker.RowOf("3", "10002"), tracker.CardOf("10002")}; !slices.Equal(res.Parts[1].Changed, want) {
		t.Errorf("changed %+v", res.Parts[1].Changed)
	}
	f.On("PUT", "/rest/agile/1.0/issue/rank", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001"],"rankBeforeIssue":"10010","rankCustomFieldId":10019}`)
		jiratest.Reply(502, `{}`)(w, r, b)
	})
	res = checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Next: "10010"}))
	if states(res) != "rank:unknown" {
		t.Errorf("an unanswered rank: %s", states(res))
	}
	res = checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001", "10001"}, Next: "10010"}))
	if states(res) != "rank:refused, rank:refused" || f.Count("PUT", "/rest/agile/1.0/issue/rank") != 2 {
		t.Errorf("an issue named twice: %s", states(res))
	}
}

func TestMoveToSprint(t *testing.T) {
	f, p := newSite(t)
	sprintsOfBoard3(f)
	listsIssues(t, f, board3Issues, "10001", "10002")
	f.On("GET", "/rest/agile/1.0/sprint/7", jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3}`))
	f.On("POST", "/rest/agile/1.0/sprint/7/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001","10002"]}`)
		w.WriteHeader(http.StatusNoContent)
	})
	f.On("POST", "/rest/agile/1.0/backlog/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001","10002"]}`)
		jiratest.Reply(207, `{"entries":[`+rankEntry("10001", 204)+`,`+rankEntry("10002", 403)+`]}`)(w, r, b)
	})
	issues := []tracker.IssueID{"10001", "10002"}
	if res := checked(t, p.MoveToSprint(t.Context(), tracker.SprintMoveInput{Board: "3", Sprint: "7", Issues: issues})); states(res) != "sprint move:applied, sprint move:applied" {
		t.Errorf("into a sprint: %s", states(res))
	}
	if res := checked(t, p.MoveToSprint(t.Context(), tracker.SprintMoveInput{Board: "3", Issues: issues})); states(res) != "sprint move:applied, sprint move:refused" {
		t.Errorf("to the backlog: %s", states(res))
	}
	if res := checked(t, p.MoveToSprint(t.Context(), tracker.SprintMoveInput{Board: "3", Sprint: "Sprint 1", Issues: issues})); states(res) != "sprint move:refused, sprint move:refused" {
		t.Errorf("a sprint name: %s", states(res))
	}
}

// TestEditWithImage proves the parts of an edit whose Markdown embeds an
// image: the upload, then the edit, which is not sent when the upload
// fails.
func TestEditWithImage(t *testing.T) {
	for _, tc := range []struct {
		upload int
		want   string
	}{
		{200, "upload:applied, edit:applied"},
		{413, "upload:refused, edit:not sent"},
	} {
		desc := storedDesc(adf.Doc(para("old")))
		f, p := editSite(t, desc)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.png"), pngBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		f.On("POST", uploadPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
			if tc.upload != 200 {
				jiratest.Reply(tc.upload, `{"errorMessages":["too large"]}`)(w, r, b)
				return
			}
			jiratest.Reply(200, `[{"id":"501","filename":"a.png","mimeType":"image/png","size":30}]`)(w, r, b)
		})
		f.On("GET", "/rest/api/3/attachment/content/501", func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.Header().Set("Location", "https://api.media.example.com/file/0f1e2d3c-0000-4000-8000-000000000501/binary?token=signed")
			w.WriteHeader(http.StatusSeeOther)
		})
		f.On("PUT", issuePath, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
		res := checked(t, p.Edit(t.Context(), tracker.EditInput{Issue: "10001", ImageDir: dir, Fields: []tracker.FieldEdit{
			{Field: "description", Value: tracker.MarkdownEdit("![a](a.png)", adf.ToMarkdown(adf.Doc(para("old"))))},
		}}))
		if got := states(res); got != tc.want {
			t.Errorf("upload %d: %s, want %s", tc.upload, got, tc.want)
		}
		if n := f.Count("PUT", issuePath); (n == 1) != (tc.upload == 200) {
			t.Errorf("upload %d: %d edits", tc.upload, n)
		}
	}
}

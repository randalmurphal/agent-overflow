package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// pngBytes is enough of a PNG for content sniffing.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRfake-image-body")

const createMetaFields = `{"total":5,"fields":[
 {"fieldId":"summary","key":"summary","name":"Summary","required":true,"schema":{"type":"string","system":"summary"},"operations":["set"]},
 {"fieldId":"description","key":"description","name":"Description","required":false,"schema":{"type":"string","system":"description"},"operations":["set"]},
 {"fieldId":"priority","key":"priority","name":"Priority","required":false,"schema":{"type":"priority","system":"priority"},"operations":["set"],"allowedValues":[{"id":"1","name":"High"},{"id":"2","name":"Low"}]},
 {"fieldId":"reporter","key":"reporter","name":"Reporter","required":true,"hasDefaultValue":true,"schema":{"type":"user","system":"reporter"},"operations":["set"]},
 {"fieldId":"labels","key":"labels","name":"Labels","required":false,"schema":{"type":"array","items":"string","system":"labels"},"operations":["add","set","remove"]}
]}`

// createSite routes the create metadata of project WEB.
func createSite(t *testing.T) (*jiratest.Server, *Client) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes", jiratest.Reply(200, `{"issueTypes":[{"id":"1","name":"Task"},{"id":"2","name":"Bug"}],"total":2}`))
	f.On("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/1", jiratest.Reply(200, createMetaFields))
	return f, c
}

func TestPlanAndApplyCreate(t *testing.T) {
	f, c := createSite(t)
	plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "task", Fields: []FieldInput{
		{Field: "Summary", Value: Text("Fix the login page")},
		{Field: "description", Value: Text("Steps **here**")},
		{Field: "Priority", Value: Text("High")},
		{Field: "Labels", Value: Texts("frontend")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pv := plan.Preview()
	if pv.IssueType.Name != "Task" || len(pv.Fields) != 4 || pv.Fields[1].Display != "Steps **here**\n" || len(pv.Images) != 0 {
		t.Fatalf("plan %+v", pv)
	}
	f.On("POST", "/rest/api/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"fields":{"project":{"key":"WEB"},"issuetype":{"id":"1"},"summary":"Fix the login page",
			"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Steps "},{"type":"text","text":"here","marks":[{"type":"strong"}]}]}]},
			"priority":{"id":"1"},"labels":["frontend"]}}`)
		jiratest.Reply(201, `{"id":"10001","key":"WEB-1","self":"x"}`)(w, r, b)
	})
	// Changing the preview changes nothing the plan sends.
	pv.Fields[0].FieldID, pv.Project = "summary2", "APP"
	res, err := c.ApplyCreate(t.Context(), plan)
	if err != nil || res.Key != "WEB-1" || res.ID != "10001" {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, err := c.ApplyCreate(t.Context(), plan); !trackerapi.WriteNotApplied(err) || f.Count("POST", "/rest/api/3/issue") != 1 {
		t.Errorf("a plan applied twice: %v", err)
	}
}

// TestApplyCreateAnswerUnreadable proves a create Jira stored whose
// answer does not decode is reported as applied, not unknown.
func TestApplyCreateAnswerUnreadable(t *testing.T) {
	f, c := createSite(t)
	plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task", Fields: []FieldInput{{Field: "Summary", Value: Text("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	// A create is applied only on its documented answer, a 201 naming the
	// issue's numeric ID and key. Any other 2xx is an unknown outcome
	// that reports nothing created.
	for _, answer := range []struct {
		status int
		body   string
	}{
		{201, `<html>stored</html>`},
		{201, `{"key":"WEB-1"}`},
		{201, `{"id":"10001/x","key":"WEB-1"}`},
		{201, `{"id":"10001","key":5}`},
		{201, `{"id":10001,"key":"WEB-1","x":[}`},
		{201, `{"id":"10001","fields":"not one"}`},
		{200, `{"id":"10001","key":"WEB-1"}`},
	} {
		f.On("POST", "/rest/api/3/issue", jiratest.Reply(answer.status, answer.body))
		res, err := c.ApplyCreate(t.Context(), plan)
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepCreate || se.Applied() || !se.OutcomeUnknown() || trackerapi.WriteNotApplied(err) {
			t.Errorf("%d %s: %v", answer.status, answer.body, err)
		}
		if res.ID != "" || res.Key != "" {
			t.Errorf("%d %s: reported %+v as created", answer.status, answer.body, res)
		}
		if plan, err = c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task", Fields: []FieldInput{{Field: "Summary", Value: Text("x")}}}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPlanCopyIsTheSamePlan proves a copy of a plan, made before or
// after the first apply, is the same plan: a write whose outcome is
// unknown is not sent again through the copy, for a create and an edit.
func TestPlanCopyIsTheSamePlan(t *testing.T) {
	f, c := createSite(t)
	f.On("POST", "/rest/api/3/issue", jiratest.Reply(500, `{"errorMessages":["down"]}`))
	in := CreateInput{Project: "WEB", IssueType: "Task", Fields: []FieldInput{{Field: "Summary", Value: Text("x")}}}
	for range 2 {
		plan, err := c.PlanCreate(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		before := *plan
		if _, err := c.ApplyCreate(t.Context(), plan); trackerapi.WriteNotApplied(err) {
			t.Fatalf("the first apply: %v", err)
		}
		after := *plan
		for _, cp := range []*CreatePlan{&before, &after} {
			if _, err := c.ApplyCreate(t.Context(), cp); !trackerapi.WriteNotApplied(err) {
				t.Errorf("a copy applied again: %v", err)
			}
		}
	}
	if n := f.Count("POST", "/rest/api/3/issue"); n != 2 {
		t.Errorf("%d creates sent for two plans", n)
	}

	desc := storedDesc(t, adf.Doc(para("old")))
	ef, ec := editSite(t, desc)
	ef.On("PUT", "/rest/api/3/issue/10001", jiratest.Reply(500, `{}`))
	plan, err := ec.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "Summary", Value: Text("New")}}})
	if err != nil {
		t.Fatal(err)
	}
	before := *plan
	if _, err := ec.ApplyEdit(t.Context(), &before); trackerapi.WriteNotApplied(err) {
		t.Fatalf("the first apply, through a copy: %v", err)
	}
	after := *plan
	for _, cp := range []*EditPlan{plan, &after} {
		if _, err := ec.ApplyEdit(t.Context(), cp); !trackerapi.WriteNotApplied(err) {
			t.Errorf("an edit applied again: %v", err)
		}
	}
	if n := ef.Count("PUT", "/rest/api/3/issue/10001"); n != 1 {
		t.Errorf("%d edits sent for one plan", n)
	}
	var zero CreatePlan
	if _, err := c.ApplyCreate(t.Context(), &zero); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a plan no PlanCreate made: %v", err)
	}
}

func TestPlanCreateRefusals(t *testing.T) {
	_, c := createSite(t)
	_, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task"})
	mf, ok := errors.AsType[*MissingFieldsError](err)
	if !ok || len(mf.Fields) != 1 || mf.Fields[0].ID != "summary" || !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("missing summary: %v", err)
	}
	_, err = c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Epic"})
	if me, ok := errors.AsType[*MatchError](err); !ok || len(me.Candidates) != 2 || !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("unknown type: %v", err)
	}
	for name, fields := range map[string][]FieldInput{
		"reserved field": {{Field: "Summary", Value: Text("x")}, {Field: "project", Value: Text("APP")}},
		"set twice":      {{Field: "Summary", Value: Text("x")}, {Field: "summary", Value: Text("y")}},
		"off screen":     {{Field: "Summary", Value: Text("x")}, {Field: "Sprint", Value: Text("1")}},
		"no match":       {{Field: "Summary", Value: Text("x")}, {Field: "Priority", Value: Text("Urgent")}},
	} {
		_, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task", Fields: fields})
		if !trackerapi.WriteNotApplied(err) || !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := newTestClient(t, jiratest.NewServer(t))
	plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task", Fields: []FieldInput{{Field: "Summary", Value: Text("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ApplyCreate(t.Context(), plan); err == nil {
		t.Error("another client applied the plan")
	}
}

// TestNotSentIsNotInvalid proves a write not sent for a reason other than
// its input (an ended call, a closed client, a read it depends on that
// failed) does not match trackerapi.ErrInvalidInput, while a stale plan,
// refused at the check step, does.
func TestNotSentIsNotInvalid(t *testing.T) {
	f, c := createSite(t)
	in := CreateInput{Project: "WEB", IssueType: "Task", Fields: []FieldInput{{Field: "Summary", Value: Text("x")}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.PlanCreate(ctx, in); !trackerapi.WriteNotApplied(err) || errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("an ended call: %v", err)
	}
	f.On("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/1", jiratest.Reply(500, `{}`))
	if _, err := c.PlanCreate(t.Context(), in); err == nil || errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("a failed read: %v", err)
	}
	c.Close()
	if _, err := c.PlanCreate(t.Context(), in); !trackerapi.WriteNotApplied(err) || errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("a closed client: %v", err)
	}

	desc := storedDesc(t, adf.Doc(para("old")))
	f, c = editSite(t, desc)
	plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Text("new")}}})
	if err != nil {
		t.Fatal(err)
	}
	other := docJSON(t, adf.Doc(para("changed")))
	desc.Store(&other)
	if _, err := c.ApplyEdit(t.Context(), plan); !errors.Is(err, trackerapi.ErrInvalidInput) || f.Count("PUT", "/rest/api/3/issue/10001") != 0 {
		t.Errorf("a stale plan: %v", err)
	}

	// A read a write depends on that failed marks the write not sent,
	// not invalid.
	f, c = estimateSite(t)
	f.On("GET", boardConfigURL, jiratest.Reply(500, `{"errorMessages":["down"]}`))
	if _, _, err := c.SetEstimate(t.Context(), EstimateInput{Issue: "WEB-1", Board: 3, Value: Points(1)}); !trackerapi.WriteNotApplied(err) || errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("a failed read before the write: %v", err)
	}
}

// imageRoutes routes an upload of a.png to issue and its media redirect.
func imageRoutes(t *testing.T, f *jiratest.Server, issue string, uploads *atomic.Int32) {
	f.On("POST", "/rest/api/3/issue/"+issue+"/attachments", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.Header.Get("X-Atlassian-Token") != "no-check" {
			t.Error("an upload without the anti-XSRF header")
		}
		name, content := multipartFile(t, r, b)
		if name != "a.png" || string(content) != string(pngBytes) {
			t.Errorf("uploaded %q (%d bytes)", name, len(content))
		}
		n := uploads.Add(1)
		jiratest.Reply(200, `[{"id":"50`+string('0'+rune(n))+`","filename":"a.png","mimeType":"image/png","size":30}]`)(w, r, b)
	})
	mediaRoutes(f)
}

// mediaRoutes routes the media redirects of attachments 501 and 502.
func mediaRoutes(f *jiratest.Server) {
	for _, id := range []string{"501", "502"} {
		f.On("GET", "/rest/api/3/attachment/content/"+id, func(w http.ResponseWriter, r *http.Request, b []byte) {
			w.Header().Set("Location", "https://api.media.example.com/file/0f1e2d3c-0000-4000-8000-000000000"+id+"/binary?token=signed")
			w.WriteHeader(http.StatusSeeOther)
		})
	}
}

// multipartFile reads the one "file" part of an upload.
func multipartFile(t *testing.T, r *http.Request, body []byte) (string, []byte) {
	t.Helper()
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "file" {
		t.Fatalf("part %v %v", part, err)
	}
	content, _ := io.ReadAll(part)
	return part.FileName(), content
}

// editSite is WEB-1 (ID 10001) holding desc as its description.
func editSite(t *testing.T, desc *atomic.Pointer[string]) (*jiratest.Server, *Client) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/editmeta", jiratest.Reply(200, `{"fields":{
		"summary":{"key":"summary","name":"Summary","required":true,"schema":{"type":"string","system":"summary"},"operations":["set"]},
		"description":{"key":"description","name":"Description","required":false,"schema":{"type":"string","system":"description"},"operations":["set"]},
		"parent":{"key":"parent","name":"Parent","required":false,"schema":{"type":"issuelink","system":"parent"},"operations":["set"]}}}`))
	issue := func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"summary":"S","description":`+*desc.Load()+`}}`)(w, r, b)
	}
	f.On("GET", "/rest/api/3/issue/WEB-1", issue)
	f.On("GET", "/rest/api/3/issue/10001", issue)
	return f, c
}

func storedDesc(t *testing.T, doc *adf.Node) *atomic.Pointer[string] {
	var p atomic.Pointer[string]
	s := docJSON(t, doc)
	p.Store(&s)
	return &p
}

func TestPlanAndApplyEditKeepsWhatMarkdownCannotShow(t *testing.T) {
	desc := storedDesc(t, layoutDoc())
	f, c := editSite(t, desc)
	md := strings.Replace(adf.ToMarkdown(layoutDoc()), "left", "LEFT", 1)
	plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{
		{Field: "Description", Value: Text(md)},
		{Field: "Parent", Value: Clear()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if pv := plan.Preview(); pv.IssueID != "10001" || len(pv.Fields) != 2 {
		t.Fatalf("plan %+v", pv)
	}
	f.On("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "returnIssue", "true")
		var body struct {
			Fields map[string]json.RawMessage `json:"fields"`
			Update map[string]json.RawMessage `json:"update"`
		}
		_ = json.Unmarshal(b, &body)
		d := string(body.Fields["description"])
		if !strings.Contains(d, `"layoutSection"`) || !strings.Contains(d, `"LEFT"`) {
			t.Errorf("the layout was not kept: %s", d)
		}
		if string(body.Update["parent"]) != `[{"set":{"none":true}}]` || body.Fields["parent"] != nil {
			t.Errorf("parent: %s", b)
		}
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"description":`+d+`}}`)(w, r, b)
	})
	res, err := c.ApplyEdit(t.Context(), plan)
	if err != nil || res.Issue == nil || res.Issue.Description == nil || res.Unchanged {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestPlanEditFieldSetTwice(t *testing.T) {
	desc := storedDesc(t, adf.Doc(para("old")))
	f, c := editSite(t, desc)
	_, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{
		{Field: "Summary", Value: Text("a")}, {Field: "summary", Value: Text("b")}}})
	if !trackerapi.WriteNotApplied(err) || !strings.Contains(fmt.Sprint(err), "twice") {
		t.Errorf("got %v", err)
	}
	if n := f.Count("GET", "/rest/api/3/issue/WEB-1"); n != 0 {
		t.Errorf("read the issue for a plan already refused: %d", n)
	}
}

// TestEditPlanTargetIsFixed proves a plan applies to the issue it was
// checked for whatever a caller does with its preview.
func TestEditPlanTargetIsFixed(t *testing.T) {
	desc := storedDesc(t, adf.Doc(para("old")))
	f, c := editSite(t, desc)
	plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "Summary", Value: Text("New")}}})
	if err != nil {
		t.Fatal(err)
	}
	pv := plan.Preview()
	pv.IssueID, pv.Fields[0].FieldID = "10002", "description"
	f.On("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"fields":{"summary":"New"}}`)
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"summary":"New"}}`)(w, r, b)
	})
	if _, err := c.ApplyEdit(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestPlanEditLossCheck(t *testing.T) {
	desc := storedDesc(t, layoutDoc())
	_, c := editSite(t, desc)
	flattened := adf.ToMarkdown(layoutDoc())
	for _, m := range []string{"<!-- layout -->", "<!-- column 50% -->", "<!-- /column -->", "<!-- column 50% -->", "<!-- /column -->", "<!-- /layout -->"} {
		flattened = strings.Replace(flattened, m+"\n\n", "", 1)
	}
	in := EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "Description", Value: Text(flattened)}}}
	_, err := c.PlanEdit(t.Context(), in)
	if _, ok := errors.AsType[*adf.LossError](err); !ok || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("got %v, want a loss refusal", err)
	}
	in.AcceptLoss = true
	plan, err := c.PlanEdit(t.Context(), in)
	if err != nil || len(plan.Preview().Fields) != 1 || len(plan.Preview().Fields[0].Warnings) == 0 {
		t.Fatalf("accepted: %v", err)
	}
	// A preview's warnings are its own.
	plan.Preview().Fields[0].Warnings[0] = "changed"
	if plan.Preview().Fields[0].Warnings[0] == "changed" {
		t.Error("a preview shares its warnings with the plan")
	}
}

func TestPlanEditUnchangedSendsNothing(t *testing.T) {
	desc := storedDesc(t, layoutDoc())
	f, c := editSite(t, desc)
	plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Text(adf.ToMarkdown(layoutDoc()))}}})
	if err != nil || len(plan.Preview().Fields) != 0 || len(plan.Preview().Unchanged) != 1 {
		t.Fatalf("got %+v %v", plan, err)
	}
	before := len(f.Requests())
	if res, err := c.ApplyEdit(t.Context(), plan); err != nil || res.Issue != nil || !res.Unchanged || len(f.Requests()) != before {
		t.Errorf("an empty plan sent something: %v", err)
	}
}

func TestStalePlanIsRefused(t *testing.T) {
	desc := storedDesc(t, layoutDoc())
	f, c := editSite(t, desc)
	md := strings.Replace(adf.ToMarkdown(layoutDoc()), "intro", "Intro", 1)
	plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Text(md)}}})
	if err != nil {
		t.Fatal(err)
	}
	other := docJSON(t, adf.Doc(para("someone else's text")))
	desc.Store(&other)
	_, err = c.ApplyEdit(t.Context(), plan)
	se, ok := errors.AsType[*StepError](err)
	if _, stale := errors.AsType[*StaleError](err); !ok || se.Step != StepCheck || !stale || se.OutcomeUnknown() {
		t.Fatalf("got %v", err)
	}
	if f.Count("PUT", "/rest/api/3/issue/10001") != 0 {
		t.Error("a stale plan was written")
	}
	// The Markdown says which document it was written from.
	_, err = c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Text(md).EditedFrom(layoutDoc())}}})
	if _, stale := errors.AsType[*StaleError](err); !stale {
		t.Errorf("EditedFrom: %v", err)
	}
}

// TestStaleClearIsRefused proves clearing rich text is checked against
// the document it was planned against, as an edit is.
func TestStaleClearIsRefused(t *testing.T) {
	desc := storedDesc(t, adf.Doc(para("old")))
	f, c := editSite(t, desc)
	plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "Description", Value: Clear()}}})
	if err != nil {
		t.Fatal(err)
	}
	other := docJSON(t, adf.Doc(para("written meanwhile")))
	desc.Store(&other)
	_, err = c.ApplyEdit(t.Context(), plan)
	if _, stale := errors.AsType[*StaleError](err); !stale || f.Count("PUT", "/rest/api/3/issue/10001") != 0 {
		t.Fatalf("a clear over a concurrent change: %v", err)
	}
	// Clearing an empty field changes nothing.
	empty := "null"
	desc.Store(&empty)
	plan, err = c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "Description", Value: Clear()}}})
	if err != nil || len(plan.Preview().Unchanged) != 1 {
		t.Fatalf("an empty field cleared: %v", err)
	}
}

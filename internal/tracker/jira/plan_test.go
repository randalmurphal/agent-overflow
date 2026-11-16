package jira

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"agent-overflow/internal/tracker/adf"
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
func createSite(t *testing.T) (*fakeJira, *Client) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes", reply(200, `{"issueTypes":[{"id":"1","name":"Task"},{"id":"2","name":"Bug"}],"total":2}`))
	f.on("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/1", reply(200, createMetaFields))
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
	if plan.IssueType.Name != "Task" || len(plan.Values) != 4 || plan.Values[1].Display != "Steps **here**\n" || len(plan.Images) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	f.on("POST", "/rest/api/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"fields":{"project":{"key":"WEB"},"issuetype":{"id":"1"},"summary":"Fix the login page",
			"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Steps "},{"type":"text","text":"here","marks":[{"type":"strong"}]}]}]},
			"priority":{"id":"1"},"labels":["frontend"]}}`)
		reply(201, `{"id":"10001","key":"WEB-1","self":"x"}`)(w, r, b)
	})
	res, err := c.ApplyCreate(t.Context(), plan)
	if err != nil || res.Key != "WEB-1" || res.ID != "10001" {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, err := c.ApplyCreate(t.Context(), plan); !trackerapi.WriteNotApplied(err) || f.count("POST", "/rest/api/3/issue") != 1 {
		t.Errorf("a plan applied twice: %v", err)
	}
}

func TestPlanCreateRefusals(t *testing.T) {
	_, c := createSite(t)
	_, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task"})
	mf, ok := errors.AsType[*MissingFieldsError](err)
	if !ok || len(mf.Fields) != 1 || mf.Fields[0].ID != "summary" {
		t.Errorf("missing summary: %v", err)
	}
	_, err = c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Epic"})
	if me, ok := errors.AsType[*MatchError](err); !ok || len(me.Candidates) != 2 {
		t.Errorf("unknown type: %v", err)
	}
	for name, fields := range map[string][]FieldInput{
		"reserved field": {{Field: "Summary", Value: Text("x")}, {Field: "project", Value: Text("APP")}},
		"set twice":      {{Field: "Summary", Value: Text("x")}, {Field: "summary", Value: Text("y")}},
		"off screen":     {{Field: "Summary", Value: Text("x")}, {Field: "Sprint", Value: Text("1")}},
	} {
		_, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task", Fields: fields})
		if !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other, _ := newTestClient(t, newFake(t))
	plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task", Fields: []FieldInput{{Field: "Summary", Value: Text("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ApplyCreate(t.Context(), plan); err == nil {
		t.Error("another client applied the plan")
	}
}

// imageRoutes routes an upload of a.png to issue and its media redirect.
func imageRoutes(t *testing.T, f *fakeJira, issue string, uploads *atomic.Int32) {
	f.on("POST", "/rest/api/3/issue/"+issue+"/attachments", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.Header.Get("X-Atlassian-Token") != "no-check" {
			t.Error("an upload without the anti-XSRF header")
		}
		name, content := multipartFile(t, r, b)
		if name != "a.png" || string(content) != string(pngBytes) {
			t.Errorf("uploaded %q (%d bytes)", name, len(content))
		}
		n := uploads.Add(1)
		reply(200, `[{"id":"50`+string('0'+rune(n))+`","filename":"a.png","mimeType":"image/png","size":30}]`)(w, r, b)
	})
	for _, id := range []string{"501", "502"} {
		f.on("GET", "/rest/api/3/attachment/content/"+id, func(w http.ResponseWriter, r *http.Request, b []byte) {
			w.Header().Set("Location", "https://api.media.example.com/file/media-"+id+"/binary?token=signed")
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

func TestApplyCreateWithImage(t *testing.T) {
	f, c := createSite(t)
	images := fstest.MapFS{"shots/a.png": {Data: pngBytes}}
	plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "1", Images: images, Fields: []FieldInput{
		{Field: "Summary", Value: Text("With a picture")},
		{Field: "Description", Value: Text("See:\n\n![shot](/shots/a.png)\n\nand again ![shot](/shots/a.png)\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Images) != 1 || plan.Images[0].Size != int64(len(pngBytes)) || plan.Images[0].Name != "a.png" {
		t.Fatalf("images %+v", plan.Images)
	}
	if n := len(f.requests()); n != 2 {
		t.Fatalf("planning sent %d requests, want only the metadata reads", n)
	}
	f.on("POST", "/rest/api/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), `[image: a.png]`) || strings.Contains(string(b), pendingPrefix) {
			t.Errorf("the create must carry the stand-in, not a placeholder: %s", b)
		}
		reply(201, `{"id":"10001","key":"WEB-1"}`)(w, r, b)
	})
	var uploads atomic.Int32
	imageRoutes(t, f, "10001", &uploads)
	f.on("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var body struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		_ = json.Unmarshal(b, &body)
		if len(body.Fields) != 1 || strings.Count(string(body.Fields["description"]), `"id":"media-501"`) != 2 {
			t.Errorf("finish %s", b)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	res, err := c.ApplyCreate(t.Context(), plan)
	if err != nil || res.Key != "WEB-1" || len(res.Uploaded) != 1 || uploads.Load() != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if strings.Contains(string(plan.Values[1].Fields), "media-501") {
		t.Error("applying changed the plan")
	}
}

func TestApplyCreateImageFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failAt  string
		status  int
		step    Step
		unknown bool
	}{
		{"upload refused after the create", "POST /rest/api/3/issue/10001/attachments", 413, StepUpload, false},
		{"finish with unknown outcome", "PUT /rest/api/3/issue/10001", 500, StepFinish, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := createSite(t)
			plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task", Images: fstest.MapFS{"a.png": {Data: pngBytes}},
				Fields: []FieldInput{{Field: "Summary", Value: Text("x")}, {Field: "Description", Value: Text("![a](a.png)")}}})
			if err != nil {
				t.Fatal(err)
			}
			f.on("POST", "/rest/api/3/issue", reply(201, `{"id":"10001","key":"WEB-1"}`))
			var uploads atomic.Int32
			imageRoutes(t, f, "10001", &uploads)
			f.on("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
			method, path, _ := strings.Cut(tc.failAt, " ")
			f.on(method, path, reply(tc.status, `{"errorMessages":["no"]}`))
			res, err := c.ApplyCreate(t.Context(), plan)
			se, ok := errors.AsType[*StepError](err)
			if !ok || se.Step != tc.step || se.OutcomeUnknown() != tc.unknown {
				t.Fatalf("got %v", err)
			}
			if res.Key != "WEB-1" {
				t.Error("the result must say the issue exists")
			}
		})
	}
}

// editSite is WEB-1 (ID 10001) holding desc as its description.
func editSite(t *testing.T, desc *atomic.Pointer[string]) (*fakeJira, *Client) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1/editmeta", reply(200, `{"fields":{
		"summary":{"key":"summary","name":"Summary","required":true,"schema":{"type":"string","system":"summary"},"operations":["set"]},
		"description":{"key":"description","name":"Description","required":false,"schema":{"type":"string","system":"description"},"operations":["set"]},
		"parent":{"key":"parent","name":"Parent","required":false,"schema":{"type":"issuelink","system":"parent"},"operations":["set"]}}}`))
	issue := func(w http.ResponseWriter, r *http.Request, b []byte) {
		reply(200, `{"id":"10001","key":"WEB-1","fields":{"summary":"S","description":`+*desc.Load()+`}}`)(w, r, b)
	}
	f.on("GET", "/rest/api/3/issue/WEB-1", issue)
	f.on("GET", "/rest/api/3/issue/10001", issue)
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
	if plan.IssueID != "10001" || len(plan.Values) != 2 {
		t.Fatalf("plan %+v", plan)
	}
	f.on("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "returnIssue", "true")
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
		reply(200, `{"id":"10001","key":"WEB-1","fields":{"description":`+d+`}}`)(w, r, b)
	})
	res, err := c.ApplyEdit(t.Context(), plan)
	if err != nil || res.Issue == nil || res.Issue.Description == nil {
		t.Fatalf("got %+v %v", res, err)
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
	if plan, err := c.PlanEdit(t.Context(), in); err != nil || len(plan.Values) != 1 {
		t.Fatalf("accepted: %v", err)
	}
}

func TestPlanEditUnchangedSendsNothing(t *testing.T) {
	desc := storedDesc(t, layoutDoc())
	f, c := editSite(t, desc)
	plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Text(adf.ToMarkdown(layoutDoc()))}}})
	if err != nil || len(plan.Values) != 0 || len(plan.Unchanged) != 1 {
		t.Fatalf("got %+v %v", plan, err)
	}
	before := len(f.requests())
	if res, err := c.ApplyEdit(t.Context(), plan); err != nil || res.Issue != nil || len(f.requests()) != before {
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
	if f.count("PUT", "/rest/api/3/issue/10001") != 0 {
		t.Error("a stale plan was written")
	}
	// The Markdown says which document it was written from.
	_, err = c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Text(md).EditedFrom(layoutDoc())}}})
	if _, stale := errors.AsType[*StaleError](err); !stale {
		t.Errorf("EditedFrom: %v", err)
	}
}

func TestApplyEditImageFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upload    int // status of the second upload
		put       int
		step      Step
		uploaded  int
		unknown   bool
		wantPuts  int
		changeImg bool
	}{
		{name: "second upload fails before the write", upload: 500, put: 204, step: StepUpload, uploaded: 1, unknown: true},
		{name: "write refused after the uploads", upload: 200, put: 400, step: StepEdit, uploaded: 2, wantPuts: 1},
		{name: "write outcome unknown", upload: 200, put: 502, step: StepEdit, uploaded: 2, unknown: true, wantPuts: 1},
		{name: "image changed since the plan", changeImg: true, step: StepUpload, uploaded: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := storedDesc(t, adf.Doc(para("old")))
			f, c := editSite(t, desc)
			images := fstest.MapFS{"a.png": {Data: pngBytes}, "b.png": {Data: pngBytes}}
			plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Images: images,
				Fields: []FieldInput{{Field: "description", Value: Text("![a](a.png) ![b](b.png)")}}})
			if err != nil || len(plan.Images) != 2 {
				t.Fatalf("plan %v %v", plan, err)
			}
			if tc.changeImg {
				images["b.png"] = &fstest.MapFile{Data: append([]byte(nil), append(pngBytes, 'x')...)}
			}
			var uploads atomic.Int32
			f.on("POST", "/rest/api/3/issue/10001/attachments", func(w http.ResponseWriter, r *http.Request, b []byte) {
				n := uploads.Add(1)
				if n == 2 && tc.upload != 200 {
					reply(tc.upload, `{}`)(w, r, b)
					return
				}
				reply(200, `[{"id":"50`+string('0'+rune(n))+`","filename":"x.png"}]`)(w, r, b)
			})
			for _, id := range []string{"501", "502"} {
				f.on("GET", "/rest/api/3/attachment/content/"+id, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
					w.Header().Set("Location", "https://api.media.example.com/file/media-"+id+"/binary")
					w.WriteHeader(http.StatusFound)
				})
			}
			f.on("PUT", "/rest/api/3/issue/10001", reply(tc.put, `{"errorMessages":["x"]}`))
			res, err := c.ApplyEdit(t.Context(), plan)
			se, ok := errors.AsType[*StepError](err)
			if !ok || se.Step != tc.step || se.OutcomeUnknown() != tc.unknown || len(res.Uploaded) != tc.uploaded {
				t.Fatalf("got %+v, %v (unknown %v)", res, err, ok && se.OutcomeUnknown())
			}
			if tc.changeImg {
				if _, stale := errors.AsType[*StaleError](err); !stale || uploads.Load() != 1 {
					t.Errorf("a changed image: %v after %d uploads", err, uploads.Load())
				}
			}
			if n := f.count("PUT", "/rest/api/3/issue/10001"); n != tc.wantPuts {
				t.Errorf("%d writes, want %d", n, tc.wantPuts)
			}
		})
	}
}

// laxFS serves a PNG for any name, valid or not, and counts the opens.
type laxFS struct{ opens atomic.Int32 }

func (l *laxFS) Open(name string) (fs.File, error) {
	l.opens.Add(1)
	return fstest.MapFS{"x.png": {Data: pngBytes}}.Open("x.png")
}

func TestPlanImageChecks(t *testing.T) {
	desc := storedDesc(t, adf.Doc(para("old")))
	_, c := editSite(t, desc)
	images := fstest.MapFS{"notes.txt": {Data: []byte("plain text")}, "big.png": {Data: append(append([]byte(nil), pngBytes...), make([]byte, 8<<10)...)}}
	c.maxUpload = 8 << 10
	for name, md := range map[string]string{
		"not an image":       "![n](notes.txt)",
		"missing":            "![m](nowhere.png)",
		"outside the folder": "![o](../a.png)",
		"over the limit":     "![b](big.png)",
	} {
		_, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Images: images, Fields: []FieldInput{{Field: "description", Value: Text(md)}}})
		if !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A folder that would serve any name is still never asked for one
	// outside it.
	lax := &laxFS{}
	_, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Images: lax, Fields: []FieldInput{{Field: "description", Value: Text("![o](../a.png)")}}})
	if !trackerapi.WriteNotApplied(err) || lax.opens.Load() != 0 {
		t.Errorf("a path outside a lax folder: %v after %d opens", err, lax.opens.Load())
	}
	_, err = c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Text("![a](a.png)")}}})
	if err == nil {
		t.Error("a local image without an image folder")
	}
	_, err = c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "description", Value: Text("<!-- only a comment -->")}}})
	if !errors.Is(err, errBlank) {
		t.Errorf("blank Markdown: %v", err)
	}
}

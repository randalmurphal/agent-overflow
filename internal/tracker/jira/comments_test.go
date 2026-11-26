package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// commentSite is WEB-1 in a project of typeKey.
func commentSite(t *testing.T, typeKey string) (*jiratest.Server, *Client) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "fields", "project")
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","projectTypeKey":"`+typeKey+`"}}}`)(w, r, b)
	})
	return f, c
}

func storedCommentReply(body string) jiratest.Handler { return storedComment(201, body) }

// storedComment is the answer status with comment 900 holding body.
func storedComment(status int, body string) jiratest.Handler {
	return jiratest.Reply(status, `{"id":"900","author":`+userOneJSON+`,"body":`+body+`,"created":"2026-11-16T10:00:00.000+0000","updated":"2026-11-16T10:00:00.000+0000","jsdPublic":false}`)
}

func TestAddCommentAudience(t *testing.T) {
	for _, tc := range []struct {
		typeKey  string
		audience Audience
		props    bool
		refused  bool
	}{
		{"software", AudienceUnset, false, false},
		{"software", AudiencePublic, false, false},
		{"software", AudienceInternal, false, true},
		{"service_desk", AudienceUnset, false, true},
		{"service_desk", AudiencePublic, false, false},
		{"service_desk", AudienceInternal, true, false},
	} {
		f, c := commentSite(t, tc.typeKey)
		f.On("POST", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
			var body struct {
				Body       json.RawMessage  `json:"body"`
				Properties []map[string]any `json:"properties"`
			}
			_ = json.Unmarshal(b, &body)
			if got := len(body.Properties) > 0; got != tc.props {
				t.Errorf("%s %d: properties %s", tc.typeKey, tc.audience, b)
			}
			if tc.props {
				jiratest.WantJSON(t, b, `{"body":`+string(body.Body)+`,"properties":[{"key":"sd.public.comment","value":{"internal":true}}]}`)
			}
			storedCommentReply(string(body.Body))(w, r, b)
		})
		res, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "Looking into it", Audience: tc.audience})
		if tc.refused {
			if !trackerapi.WriteNotApplied(err) || !errors.Is(err, trackerapi.ErrInvalidInput) || f.Count("POST", "/rest/api/3/issue/WEB-1/comment") != 0 {
				t.Errorf("%s %d: want a refusal before sending, got %v", tc.typeKey, tc.audience, err)
			}
			continue
		}
		if err != nil || res.Comment == nil || res.Comment.ID != "900" || !res.Comment.Internal || adf.ToMarkdown(res.Comment.Body) != "Looking into it\n" {
			t.Errorf("%s %d: got %+v %v", tc.typeKey, tc.audience, res, err)
		}
	}
	// An issue read without its project is Jira's answer falling short,
	// not the caller's input: not sent, and not invalid.
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1", jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{}}`))
	_, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "Looking into it"})
	if !trackerapi.WriteNotApplied(err) || errors.Is(err, trackerapi.ErrInvalidInput) || f.Count("POST", "/rest/api/3/issue/WEB-1/comment") != 0 {
		t.Errorf("an issue without its project: %v", err)
	}
}

func TestAddCommentRefusesBlankAndMissesNothing(t *testing.T) {
	f, c := commentSite(t, "software")
	f.On("GET", "/rest/api/3/user/search", jiratest.Reply(200, `[]`))
	for name, md := range map[string]string{"blank": "  \n", "only a comment": "<!-- x -->", "unknown mention": "hi @[Nobody Here]"} {
		_, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: md})
		if !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAddCommentWithImageAndMention(t *testing.T) {
	f, c := commentSite(t, "software")
	f.On("GET", "/rest/api/3/user/search", userSearch(t, nil, map[string][]string{"User 2": {userTwoJSON}}))
	var uploads atomic.Int32
	imageRoutes(t, f, "WEB-1", &uploads)
	f.On("POST", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), `"id":"0f1e2d3c-0000-4000-8000-000000000501"`) || !strings.Contains(string(b), `"id":"acc-user2"`) {
			t.Errorf("comment %s", b)
		}
		var body struct {
			Body json.RawMessage `json:"body"`
		}
		_ = json.Unmarshal(b, &body)
		storedCommentReply(string(body.Body))(w, r, b)
	})
	res, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "@[User 2] see\n\n![a](a.png)\n",
		ImageDir: imageDir(t, map[string][]byte{"a.png": pngBytes})})
	if err != nil || len(res.Uploaded) != 1 || res.Comment == nil {
		t.Fatalf("got %+v %v", res, err)
	}
	order := strings.Join(f.Requests(), " | ")
	if strings.Index(order, "attachments") > strings.Index(order, "POST /rest/api/3/issue/WEB-1/comment") {
		t.Errorf("the image must be attached before the comment: %s", order)
	}
}

// TestAddCommentImageFailures proves a comment whose image upload or media
// ID read fails reports what was stored and sends no comment.
func TestAddCommentImageFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		route    func(*jiratest.Server)
		step     Step
		uploaded int
	}{
		{"the upload refused", func(f *jiratest.Server) {
			f.On("POST", "/rest/api/3/issue/WEB-1/attachments", jiratest.Reply(413, `{"errorMessages":["too large"]}`))
		}, StepUpload, 0},
		{"the media ID read failed", func(f *jiratest.Server) {
			f.On("GET", "/rest/api/3/attachment/content/501", jiratest.Reply(500, `{}`))
		}, StepMediaID, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := commentSite(t, "software")
			var uploads atomic.Int32
			imageRoutes(t, f, "WEB-1", &uploads)
			tc.route(f)
			res, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "![a](a.png)",
				ImageDir: imageDir(t, map[string][]byte{"a.png": pngBytes})})
			se, ok := errors.AsType[*StepError](err)
			if !ok || se.Step != tc.step || len(res.Uploaded) != tc.uploaded || res.Comment != nil {
				t.Fatalf("got %+v %v", res, err)
			}
			if f.Count("POST", "/rest/api/3/issue/WEB-1/comment") != 0 {
				t.Error("the comment was sent")
			}
		})
	}
}

func TestCommentsPage(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "startAt", "1")
		jiratest.WantQuery(t, r, "maxResults", "2")
		jiratest.WantQuery(t, r, "orderBy", "created")
		jiratest.Reply(200, `{"startAt":1,"total":3,"comments":[
			{"id":"901","author":`+userOneJSON+`,"body":{"type":"doc","version":1,"content":[]},"created":"2026-11-16T10:00:00.000+0000","jsdPublic":true},
			{"id":"902","body":null,"visibility":{"type":"role","value":"Developers"},"created":"2026-11-16T11:00:00.000+0000","updated":"later"}]}`)(w, r, b)
	})
	page, err := c.Comments(t.Context(), "WEB-1", 1, 2)
	if err != nil || page.Total != 3 || len(page.Comments) != 2 || page.Comments[0].Internal || page.Comments[1].Visibility == nil {
		t.Fatalf("got %+v %v", page, err)
	}
	// A timestamp that does not parse is the comment's problem; an absent
	// one is none.
	first, second := page.Comments[0], page.Comments[1]
	if len(first.Problems) != 0 || problemIn(second.Problems, "updated") == nil || len(second.Problems) != 1 || second.Created.IsZero() {
		t.Errorf("problems %+v %+v", first.Problems, second.Problems)
	}
}

// editCommentSite is comment 900 on WEB-1 holding body, restricted to a
// role.
func editCommentSite(t *testing.T, body *adf.Node) (*jiratest.Server, *Client) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/comment/900", jiratest.Reply(200, `{"id":"900","body":`+docJSON(t, body)+`,"visibility":{"type":"role","value":"Developers"}}`))
	return f, c
}

func TestEditComment(t *testing.T) {
	f, c := editCommentSite(t, layoutDoc())
	md := strings.Replace(adf.ToMarkdown(layoutDoc()), "right", "RIGHT", 1)
	f.On("PUT", "/rest/api/3/issue/WEB-1/comment/900", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var body struct {
			Body       json.RawMessage `json:"body"`
			Visibility json.RawMessage `json:"visibility"`
		}
		_ = json.Unmarshal(b, &body)
		if !strings.Contains(string(body.Body), "layoutSection") || string(body.Visibility) != `{"type":"role","value":"Developers"}` {
			t.Errorf("edit %s", b)
		}
		storedComment(200, string(body.Body))(w, r, b)
	})
	read, err := c.Comment(t.Context(), "WEB-1", "900")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: md, From: &read})
	if err != nil || res.Comment == nil || res.Unchanged {
		t.Fatalf("got %+v %v", res, err)
	}
	// The same body again sends nothing.
	res, err = c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Unchecked: true, Markdown: adf.ToMarkdown(layoutDoc())})
	if err != nil || res.Comment == nil || !res.Unchanged || f.Count("PUT", "/rest/api/3/issue/WEB-1/comment/900") != 1 {
		t.Errorf("an unchanged edit: %+v %v", res, err)
	}
}

// TestEditCommentAnswerNamesAnother proves an edit answered with another
// comment is an unknown outcome that reports no comment.
func TestEditCommentAnswerNamesAnother(t *testing.T) {
	f, c := editCommentSite(t, adf.Doc(para("old")))
	f.On("PUT", "/rest/api/3/issue/WEB-1/comment/900", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"new"}]}]},"visibility":{"type":"role","value":"Developers"}}`)
		jiratest.Reply(200, `{"id":"901","body":{"type":"doc","version":1,"content":[]}}`)(w, r, b)
	})
	res, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Unchecked: true, Markdown: "new"})
	se, ok := errors.AsType[*StepError](err)
	if !ok || se.Step != StepComment || se.Applied() || !se.OutcomeUnknown() || res.Comment != nil {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestEditCommentRefusals(t *testing.T) {
	f, c := editCommentSite(t, layoutDoc())
	flattened := strings.NewReplacer("<!-- layout -->\n\n", "", "<!-- column 50% -->\n\n", "", "<!-- /column -->\n\n", "", "<!-- /layout -->\n", "").
		Replace(adf.ToMarkdown(layoutDoc()))
	_, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Unchecked: true, Markdown: flattened})
	if _, ok := errors.AsType[*adf.LossError](err); !ok {
		t.Errorf("loss: %v", err)
	}
	// A base read with another body, or with none, is stale.
	for _, body := range []*adf.Node{adf.Doc(para("older")), nil} {
		_, err = c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: "x", From: &Comment{ID: "900", Body: body}})
		if _, ok := errors.AsType[*StaleError](err); !ok || !trackerapi.WriteNotApplied(err) {
			t.Errorf("stale from %v: %v", body, err)
		}
	}
	for name, in := range map[string]CommentEdit{
		"a bad comment ID":           {Issue: "WEB-1", ID: "../900", Unchecked: true, Markdown: "x"},
		"neither From nor Unchecked": {Issue: "WEB-1", ID: "900", Markdown: "x"},
		"both":                       {Issue: "WEB-1", ID: "900", Markdown: "x", Unchecked: true, From: &Comment{ID: "900", Body: layoutDoc()}},
		"From another comment":       {Issue: "WEB-1", ID: "900", Markdown: "x", From: &Comment{ID: "901", Body: layoutDoc()}},
	} {
		if _, err := c.EditComment(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := f.Count("PUT", "/rest/api/3/issue/WEB-1/comment/900"); n != 0 {
		t.Errorf("%d writes", n)
	}
	f.On("PUT", "/rest/api/3/issue/WEB-1/comment/900", jiratest.Reply(200, `{"id":"900","body":{"type":"doc","version":1,"content":[]}}`))
	if _, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Unchecked: true, Markdown: flattened, AcceptLoss: true}); err != nil {
		t.Errorf("accepted loss: %v", err)
	}
}

// TestEditCommentChecksLast proves the stale check is the last read
// before the write: the mention the Markdown names is read first, so a
// change made while it is read is caught, and nothing is written.
func TestEditCommentChecksLast(t *testing.T) {
	f, c := editCommentSite(t, adf.Doc(para("first")))
	read, err := c.Comment(t.Context(), "WEB-1", "900")
	if err != nil {
		t.Fatal(err)
	}
	f.On("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		f.On("GET", "/rest/api/3/issue/WEB-1/comment/900", jiratest.Reply(200, `{"id":"900","body":`+docJSON(t, adf.Doc(para("changed")))+`}`))
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	_, err = c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: "thanks @[me]", From: &read})
	if _, ok := errors.AsType[*StaleError](err); !ok || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("got %v", err)
	}
	want := []string{"GET /rest/api/3/issue/WEB-1/comment/900", "GET " + myselfURL, "GET /rest/api/3/issue/WEB-1/comment/900"}
	if got := f.Requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDeleteComment(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("DELETE", "/rest/api/3/issue/WEB-1/comment/900", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	if err := c.DeleteComment(t.Context(), "WEB-1", "900"); err != nil {
		t.Fatal(err)
	}
}

// A comment that names its issue's project (CommentInput.Project) is
// judged on that project, not on a read of the issue: its type as a bulk
// fetch named it, or read once and kept. A project read that fails, or
// that comes without the project's type, sends nothing.
func TestAddCommentOnANamedProject(t *testing.T) {
	post := func(f *jiratest.Server) {
		f.On("POST", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
			var body struct {
				Body json.RawMessage `json:"body"`
			}
			_ = json.Unmarshal(b, &body)
			storedCommentReply(string(body.Body))(w, r, b)
		})
	}
	in := CommentInput{Issue: "WEB-1", Markdown: "Looking into it", Project: "10010"}

	// Read once, then kept: the issue is never read for its project.
	f := jiratest.NewServer(t)
	c := signedIn(t, f)
	f.On("GET", "/rest/api/3/project/10010", jiratest.Reply(200, `{"id":"10010","key":"SD","name":"Service","projectTypeKey":"service_desk"}`))
	post(f)
	if _, err := c.AddComment(t.Context(), in); !trackerapi.WriteNotApplied(err) || !strings.Contains(fmt.Sprint(err), "SD is a service desk project") {
		t.Errorf("no audience on a service desk project: %v", err)
	}
	in.Audience = AudienceInternal
	if _, err := c.AddComment(t.Context(), in); err != nil {
		t.Errorf("internal: %v", err)
	}
	if n, m := f.Count("GET", "/rest/api/3/project/10010"), f.Count("GET", "/rest/api/3/issue/WEB-1"); n != 1 || m != 0 {
		t.Errorf("%d project reads and %d issue reads; want 1 and 0", n, m)
	}

	// A bulk fetch's issue names its project with the type: no read.
	f = jiratest.NewServer(t)
	c = signedIn(t, f)
	f.On("POST", "/rest/api/3/issue/bulkfetch", jiratest.Reply(200, `{"issues":[{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10010","key":"SD","projectTypeKey":"service_desk"}}}],"issueErrors":[]}`))
	post(f)
	if _, err := c.BulkFetch(t.Context(), []string{"WEB-1"}, "project"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddComment(t.Context(), in); err != nil || len(f.Requests()) != 3 {
		t.Errorf("after a bulk fetch: %v, requests %v", err, f.Requests())
	}

	// Fails closed.
	for name, h := range map[string]jiratest.Handler{
		"a failed read":    jiratest.Reply(500, `{"errorMessages":["down"]}`),
		"no type answered": jiratest.Reply(200, `{"id":"10010","key":"SD","name":"Service"}`),
	} {
		f := jiratest.NewServer(t)
		c := signedIn(t, f)
		f.On("GET", "/rest/api/3/project/10010", h)
		post(f)
		in.Audience = AudienceUnset
		if _, err := c.AddComment(t.Context(), in); !trackerapi.WriteNotApplied(err) || f.Count("POST", "/rest/api/3/issue/WEB-1/comment") != 0 {
			t.Errorf("%s: %v, %d comments sent", name, err, f.Count("POST", "/rest/api/3/issue/WEB-1/comment"))
		}
	}
}

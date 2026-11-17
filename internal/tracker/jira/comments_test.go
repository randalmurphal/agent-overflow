package jira

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/trackerapi"
)

// commentSite is WEB-1 in a project of typeKey.
func commentSite(t *testing.T, typeKey string) (*fakeJira, *Client) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "fields", "project")
		reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","projectTypeKey":"`+typeKey+`"}}}`)(w, r, b)
	})
	return f, c
}

func storedCommentReply(body string) handler {
	return reply(201, `{"id":"900","author":`+userOneJSON+`,"body":`+body+`,"created":"2026-11-16T10:00:00.000+0000","updated":"2026-11-16T10:00:00.000+0000","jsdPublic":false}`)
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
		f.on("POST", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
			var body struct {
				Body       json.RawMessage  `json:"body"`
				Properties []map[string]any `json:"properties"`
			}
			_ = json.Unmarshal(b, &body)
			if got := len(body.Properties) > 0; got != tc.props {
				t.Errorf("%s %d: properties %s", tc.typeKey, tc.audience, b)
			}
			if tc.props {
				wantJSON(t, b, `{"body":`+string(body.Body)+`,"properties":[{"key":"sd.public.comment","value":{"internal":true}}]}`)
			}
			storedCommentReply(string(body.Body))(w, r, b)
		})
		res, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "Looking into it", Audience: tc.audience})
		if tc.refused {
			if !trackerapi.WriteNotApplied(err) || f.count("POST", "/rest/api/3/issue/WEB-1/comment") != 0 {
				t.Errorf("%s %d: want a refusal before sending, got %v", tc.typeKey, tc.audience, err)
			}
			continue
		}
		if err != nil || res.Comment == nil || res.Comment.ID != "900" || !res.Comment.Internal || adf.ToMarkdown(res.Comment.Body) != "Looking into it\n" {
			t.Errorf("%s %d: got %+v %v", tc.typeKey, tc.audience, res, err)
		}
	}
}

func TestAddCommentRefusesBlankAndMissesNothing(t *testing.T) {
	f, c := commentSite(t, "software")
	f.on("GET", "/rest/api/3/user/search", reply(200, `[]`))
	for name, md := range map[string]string{"blank": "  \n", "only a comment": "<!-- x -->", "unknown mention": "hi @[Nobody Here]"} {
		_, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: md})
		if !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAddCommentWithImageAndMention(t *testing.T) {
	f, c := commentSite(t, "software")
	f.on("GET", "/rest/api/3/user/search", userSearch(t, nil, map[string][]string{"User 2": {userTwoJSON}}))
	var uploads atomic.Int32
	imageRoutes(t, f, "WEB-1", &uploads)
	f.on("POST", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), `"id":"media-501"`) || !strings.Contains(string(b), `"id":"acc-user2"`) {
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
	order := strings.Join(f.requests(), " | ")
	if strings.Index(order, "attachments") > strings.Index(order, "POST /rest/api/3/issue/WEB-1/comment") {
		t.Errorf("the image must be attached before the comment: %s", order)
	}
}

func TestCommentsPage(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "startAt", "1")
		wantQuery(t, r, "maxResults", "2")
		wantQuery(t, r, "orderBy", "created")
		reply(200, `{"startAt":1,"total":3,"comments":[
			{"id":"901","author":`+userOneJSON+`,"body":{"type":"doc","version":1,"content":[]},"created":"2026-11-16T10:00:00.000+0000","jsdPublic":true},
			{"id":"902","body":null,"visibility":{"type":"role","value":"Developers"},"created":"2026-11-16T11:00:00.000+0000"}]}`)(w, r, b)
	})
	page, err := c.Comments(t.Context(), "WEB-1", 1, 2)
	if err != nil || page.Total != 3 || len(page.Comments) != 2 || page.Comments[0].Internal || page.Comments[1].Visibility == nil {
		t.Fatalf("got %+v %v", page, err)
	}
}

// editCommentSite is comment 900 on WEB-1 holding body, restricted to a
// role.
func editCommentSite(t *testing.T, body *adf.Node) (*fakeJira, *Client) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1/comment/900", reply(200, `{"id":"900","body":`+docJSON(t, body)+`,"visibility":{"type":"role","value":"Developers"}}`))
	return f, c
}

func TestEditComment(t *testing.T) {
	f, c := editCommentSite(t, layoutDoc())
	md := strings.Replace(adf.ToMarkdown(layoutDoc()), "right", "RIGHT", 1)
	f.on("PUT", "/rest/api/3/issue/WEB-1/comment/900", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var body struct {
			Body       json.RawMessage `json:"body"`
			Visibility json.RawMessage `json:"visibility"`
		}
		_ = json.Unmarshal(b, &body)
		if !strings.Contains(string(body.Body), "layoutSection") || string(body.Visibility) != `{"type":"role","value":"Developers"}` {
			t.Errorf("edit %s", b)
		}
		storedCommentReply(string(body.Body))(w, r, b)
	})
	res, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: md, From: layoutDoc()})
	if err != nil || res.Comment == nil {
		t.Fatalf("got %+v %v", res, err)
	}
	// The same body again sends nothing.
	res, err = c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: adf.ToMarkdown(layoutDoc())})
	if err != nil || res.Comment == nil || f.count("PUT", "/rest/api/3/issue/WEB-1/comment/900") != 1 {
		t.Errorf("an unchanged edit: %v", err)
	}
}

func TestEditCommentRefusals(t *testing.T) {
	f, c := editCommentSite(t, layoutDoc())
	flattened := strings.NewReplacer("<!-- layout -->\n\n", "", "<!-- column 50% -->\n\n", "", "<!-- /column -->\n\n", "", "<!-- /layout -->\n\n", "").
		Replace(adf.ToMarkdown(layoutDoc()))
	_, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: flattened})
	if _, ok := errors.AsType[*adf.LossError](err); !ok {
		t.Errorf("loss: %v", err)
	}
	_, err = c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: "x", From: adf.Doc(para("older"))})
	if _, ok := errors.AsType[*StaleError](err); !ok {
		t.Errorf("stale: %v", err)
	}
	if _, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "../900", Markdown: "x"}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a bad comment ID: %v", err)
	}
	if n := f.count("PUT", "/rest/api/3/issue/WEB-1/comment/900"); n != 0 {
		t.Errorf("%d writes", n)
	}
	f.on("PUT", "/rest/api/3/issue/WEB-1/comment/900", reply(200, `{"id":"900","body":{"type":"doc","version":1,"content":[]}}`))
	if _, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: flattened, AcceptLoss: true}); err != nil {
		t.Errorf("accepted loss: %v", err)
	}
}

func TestDeleteComment(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("DELETE", "/rest/api/3/issue/WEB-1/comment/900", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	if err := c.DeleteComment(t.Context(), "WEB-1", "900"); err != nil {
		t.Fatal(err)
	}
}

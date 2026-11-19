package jiraprovider

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
)

func TestSprintWrites(t *testing.T) {
	f, p := newSite(t)
	start := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 14)
	f.On("POST", "/rest/agile/1.0/sprint", func(w http.ResponseWriter, r *http.Request, b []byte) {
		v := bodyOf(t, b)
		if v["name"] != "Sprint 2" || v["originBoardId"] != 3.0 || v["goal"] != "Ship" {
			t.Errorf("create %s", b)
		}
		jiratest.Reply(201, `{"id":9,"name":"Sprint 2","state":"future","originBoardId":3}`)(w, r, b)
	})
	f.On("GET", "/rest/agile/1.0/sprint/8", jiratest.Reply(200, `{"id":8,"name":"Next","state":"future","originBoardId":3}`))
	f.On("GET", "/rest/agile/1.0/sprint/7", jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3,"startDate":"2026-11-03T09:00:00.000Z","endDate":"2026-11-17T09:00:00.000Z"}`))
	f.On("POST", "/rest/agile/1.0/sprint/8", jiratest.Reply(200, `{"id":8,"name":"Next","state":"active","originBoardId":3}`))
	f.On("POST", "/rest/agile/1.0/sprint/7", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"name":"Renamed"}`)
		jiratest.Reply(200, `{"id":7,"name":"Renamed","state":"active","originBoardId":3}`)(w, r, b)
	})
	structure := []tracker.Resource{tracker.StructureOf("3")}
	for name, res := range map[string]tracker.Result{
		"create": p.CreateSprint(t.Context(), tracker.SprintInput{Board: "3", Name: "Sprint 2", Goal: "Ship", Start: start, End: end}),
		"start":  p.StartSprint(t.Context(), tracker.SprintStart{Board: "3", Sprint: "8", Start: start, End: end}),
		"edit":   p.EditSprint(t.Context(), tracker.SprintEdit{Board: "3", Sprint: "7", Name: ptr("Renamed")}),
	} {
		checked(t, res)
		if states(res) != "sprint:applied" || res.Parts[0].Issue != "" || !slices.Equal(res.Parts[0].Changed, structure) {
			t.Errorf("%s: %s %+v %v", name, states(res), res.Parts[0].Changed, res.Parts[0].Outcome.Err())
		}
	}
	for name, res := range map[string]tracker.Result{
		"starting an active sprint": p.StartSprint(t.Context(), tracker.SprintStart{Board: "3", Sprint: "7", Start: start, End: end}),
		"an end before the start":   p.CreateSprint(t.Context(), tracker.SprintInput{Board: "3", Name: "Bad", Start: end, End: start}),
		"a sprint name for an ID":   p.EditSprint(t.Context(), tracker.SprintEdit{Board: "3", Sprint: "Sprint 1", Name: ptr("x")}),
	} {
		checked(t, res)
		if states(res) != "sprint:refused" || reason(res, 0) != tracker.ReasonInvalid {
			t.Errorf("%s: %s %d", name, states(res), reason(res, 0))
		}
	}
	f.On("POST", "/rest/agile/1.0/sprint", jiratest.Reply(500, `{}`))
	if res := checked(t, p.CreateSprint(t.Context(), tracker.SprintInput{Board: "3", Name: "Lost"})); states(res) != "sprint:unknown" {
		t.Errorf("an unanswered create: %s", states(res))
	}
}

const (
	sprint7Path       = "/rest/agile/1.0/sprint/7"
	sprint7IssuesPath = "/rest/software/1.0/sprint/7/issue"
	sprint8MovePath   = "/rest/agile/1.0/sprint/8/issue"
)

// completionSite is newSite with sprint 7 active and sprint 8 future on
// board 3. Sprint 7 holds 10001 (in progress), 10002 (done) and 10003
// (to do); an issue a move took out is no longer listed, and added
// joins the listing after the first. move answers each move, 204 when
// nil; closing answers the close.
func completionSite(t *testing.T, added string, move, closing jiratest.Handler) *jiratest.Server {
	f, _ := newSite(t)
	f.On("GET", sprint7Path, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3}`))
	f.On("GET", "/rest/agile/1.0/sprint/8", jiratest.Reply(200, `{"id":8,"name":"Sprint 2","state":"future","originBoardId":3}`))
	var mu sync.Mutex
	var moved []string
	listings := 0
	f.On("GET", sprint7IssuesPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, is := range [][2]string{{"10001", "10001"}, {"10002", "10002"}, {"10003", "10000"}} {
			if !slices.Contains(moved, is[0]) {
				out = append(out, `{"id":"`+is[0]+`","key":"WEB-`+is[0][4:]+`","fields":{"status":{"id":"`+is[1]+`"}}}`)
			}
		}
		if listings > 0 && added != "" {
			out = append(out, `{"id":"`+added+`","key":"WEB-9","fields":{"status":{"id":"10000"}}}`)
		}
		listings++
		jiratest.Reply(200, `{"isLast":true,"issues":[`+strings.Join(out, ",")+`]}`)(w, r, b)
	})
	f.On("POST", sprint8MovePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001","10003"]}`)
		if move != nil {
			move(w, r, b)
			return
		}
		mu.Lock()
		moved = append(moved, "10001", "10003")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.On("POST", sprint7Path, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"state":"closed"}`)
		closing(w, r, b)
	})
	return f
}

// TestCompleteSprint proves a completion has a part per unfinished issue
// moved, by ID with its row and card, then the close's part with the
// board's structure, and that each partial case says what was applied.
func TestCompleteSprint(t *testing.T) {
	closed := jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"closed","originBoardId":3}`)
	relistFails := func(f *jiratest.Server) jiratest.Handler {
		return func(w http.ResponseWriter, r *http.Request, b []byte) {
			f.On("GET", sprint7IssuesPath, jiratest.Reply(500, `{}`))
			w.WriteHeader(http.StatusNoContent)
		}
	}
	for _, tc := range []struct {
		name    string
		added   string
		move    func(*jiratest.Server) jiratest.Handler
		closing jiratest.Handler
		want    string
		reason  tracker.Reason // of the close
	}{
		{name: "every write applied", closing: closed, want: "applied applied | applied"},
		{name: "a close answered with a malformed sprint", closing: jiratest.Reply(200, `{"id":7,"name":5}`), want: "applied applied | applied"},
		{name: "a close whose outcome is unknown", closing: jiratest.Reply(500, `{}`), want: "applied applied | unknown"},
		{name: "moves refused", move: func(*jiratest.Server) jiratest.Handler {
			return jiratest.Reply(400, `{"errorMessages":["no"]}`)
		}, closing: closed, want: "refused refused | not sent"},
		{name: "a move whose outcome is unknown", move: func(*jiratest.Server) jiratest.Handler {
			return jiratest.Reply(500, `{}`)
		}, closing: closed, want: "unknown unknown | not sent"},
		{name: "an unfinished issue joined meanwhile", added: "10009", closing: closed, want: "applied applied | refused", reason: tracker.ReasonStale},
		{name: "the listing after the moves failed", move: relistFails, closing: closed, want: "applied applied | not sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *jiratest.Server
			var move jiratest.Handler
			if tc.move != nil {
				move = func(w http.ResponseWriter, r *http.Request, b []byte) { tc.move(f)(w, r, b) }
			}
			f = completionSite(t, tc.added, move, tc.closing)
			p := newProvider(t, f, nil)
			res := checked(t, p.CompleteSprint(t.Context(), tracker.SprintCompletion{Board: "3", Sprint: "7", MoveTo: "8"}))
			if len(res.Parts) != 3 {
				t.Fatalf("parts %s", states(res))
			}
			var got []string
			for i, part := range res.Parts {
				got = append(got, part.Outcome.State().String())
				want := []tracker.Resource{tracker.StructureOf("3")}
				step := tracker.StepSprint
				if i < 2 {
					id := tracker.IssueID([]string{"10001", "10003"}[i])
					if part.Issue != id {
						t.Errorf("part %d is issue %q", i, part.Issue)
					}
					want, step = []tracker.Resource{tracker.RowOf("3", id), tracker.CardOf(id)}, tracker.StepSprintMove
				}
				if part.Step != step || !slices.Equal(part.Changed, want) {
					t.Errorf("part %d: %s %+v", i, part.Step, part.Changed)
				}
			}
			if s := got[0] + " " + got[1] + " | " + got[2]; s != tc.want {
				t.Errorf("got %s, want %s", s, tc.want)
			}
			if reason(res, 2) != tc.reason {
				t.Errorf("close refused for %d", reason(res, 2))
			}
			if !strings.HasSuffix(tc.want, "not sent") || f.Count("POST", sprint7Path) == 0 {
				return
			}
			t.Error("the close was sent")
		})
	}
}

// TestCompleteSprintRefused proves a completion refused before any
// write is the close part alone, and sends nothing.
func TestCompleteSprintRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		in     tracker.SprintCompletion
		route  func(*jiratest.Server)
		reason tracker.Reason
	}{
		"a future sprint": {tracker.SprintCompletion{Board: "3", Sprint: "7"}, func(f *jiratest.Server) {
			f.On("GET", sprint7Path, jiratest.Reply(200, `{"id":7,"state":"future","originBoardId":3}`))
		}, tracker.ReasonInvalid},
		"a sprint name for an ID":     {tracker.SprintCompletion{Board: "3", Sprint: "Sprint 1"}, nil, tracker.ReasonInvalid},
		"a board name for an ID":      {tracker.SprintCompletion{Board: "Board 3", Sprint: "7"}, nil, tracker.ReasonInvalid},
		"a sprint to move to by name": {tracker.SprintCompletion{Board: "3", Sprint: "7", MoveTo: "Sprint 2"}, nil, tracker.ReasonInvalid},
		"a listing that failed": {tracker.SprintCompletion{Board: "3", Sprint: "7"}, func(f *jiratest.Server) {
			f.On("GET", sprint7IssuesPath, jiratest.Reply(500, `{}`))
		}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := completionSite(t, "", nil, jiratest.Reply(200, `{"id":7,"state":"closed"}`))
			if tc.route != nil {
				tc.route(f)
			}
			res := checked(t, newProvider(t, f, nil).CompleteSprint(t.Context(), tc.in))
			want := "sprint:refused"
			if tc.reason == 0 {
				want = "sprint:not sent"
			}
			if states(res) != want || reason(res, 0) != tc.reason || res.Parts[0].Issue != "" {
				t.Errorf("got %s %d", states(res), reason(res, 0))
			}
			for _, r := range f.Requests() {
				if !strings.HasPrefix(r, "GET ") {
					t.Errorf("a write was sent: %s", r)
				}
			}
		})
	}
}

// storedComment answers with comment 900 holding body.
func storedComment(status int, body *adf.Node) jiratest.Handler {
	return jiratest.Reply(status, `{"id":"900","author":`+user1JSON+`,"body":`+mustJSON(body)+`,"created":"`+updatedAt+`","updated":"`+updatedAt+`","jsdPublic":true}`)
}

func TestCommentWrites(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "fields", "project")
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","projectTypeKey":"service_desk"}}}`)(w, r, b)
	})
	f.On("POST", "/rest/api/3/issue/10001/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), "sd.public.comment") || !strings.Contains(string(b), `"internal":true`) {
			t.Errorf("an internal comment %s", b)
		}
		storedComment(201, adf.Doc(para("Hello")))(w, r, b)
	})
	var stored atomic.Pointer[adf.Node]
	stored.Store(adf.Doc(para("Hello")))
	f.On("GET", "/rest/api/3/issue/10001/comment/900", func(w http.ResponseWriter, r *http.Request, b []byte) {
		storedComment(200, stored.Load())(w, r, b)
	})
	f.On("PUT", "/rest/api/3/issue/10001/comment/900", storedComment(200, adf.Doc(para("Hello there"))))
	f.On("DELETE", "/rest/api/3/issue/10001/comment/900", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
	detail := []tracker.Resource{tracker.DetailOf("10001")}

	res := checked(t, p.AddComment(t.Context(), tracker.CommentInput{Issue: "10001", Markdown: "Hello", Audience: tracker.AudienceInternal}))
	if states(res) != "comment:applied" || !slices.Equal(res.Parts[0].Changed, detail) {
		t.Errorf("add: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
	if res := checked(t, p.AddComment(t.Context(), tracker.CommentInput{Issue: "10001", Markdown: "Hello"})); reason(res, 0) != tracker.ReasonInvalid {
		t.Errorf("a service desk comment without an audience: %s", states(res))
	}
	from := adf.ToMarkdown(adf.Doc(para("Hello")))
	res = checked(t, p.EditComment(t.Context(), tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: "Hello there", From: from}))
	if states(res) != "comment:applied" {
		t.Errorf("edit: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
	stored.Store(adf.Doc(para("Changed by someone")))
	res = checked(t, p.EditComment(t.Context(), tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: "Hello again", From: from}))
	if reason(res, 0) != tracker.ReasonStale || f.Count("PUT", "/rest/api/3/issue/10001/comment/900") != 1 {
		t.Errorf("a stale edit: %s", states(res))
	}
	if res := checked(t, p.DeleteComment(t.Context(), tracker.CommentDelete{Issue: "10001", Comment: "900"})); states(res) != "comment:applied" {
		t.Errorf("delete: %s", states(res))
	}
	if res := checked(t, p.DeleteComment(t.Context(), tracker.CommentDelete{Issue: "10001", Comment: "../900"})); reason(res, 0) != tracker.ReasonInvalid {
		t.Errorf("a comment that is not an ID: %s", states(res))
	}
}

func TestLinksAndAttachments(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", "/rest/api/3/issueLinkType", jiratest.Reply(200, `{"issueLinkTypes":[{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"}]}`))
	f.On("POST", "/rest/api/3/issueLink", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"type":{"id":"10000"},"outwardIssue":{"id":"10001"},"inwardIssue":{"id":"10002"}}`)
		w.WriteHeader(http.StatusCreated)
	})
	f.On("DELETE", "/rest/api/3/issueLink/300", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
	f.On("POST", uploadPath, jiratest.Reply(200, `[{"id":"501","filename":"a.txt","mimeType":"text/plain","size":5}]`))
	both := []tracker.Resource{tracker.DetailOf("10001"), tracker.DetailOf("10002")}
	res := checked(t, p.Link(t.Context(), tracker.LinkInput{Type: "10000", Outward: "10001", Inward: "10002"}))
	if states(res) != "link:applied" || res.Parts[0].Issue != "10001" || !slices.Equal(res.Parts[0].Changed, both) {
		t.Errorf("link: %s %+v %v", states(res), res.Parts[0].Changed, res.Parts[0].Outcome.Err())
	}
	res = checked(t, p.Unlink(t.Context(), tracker.UnlinkInput{Issue: "10001", Link: "300", Other: "10002"}))
	if states(res) != "link:applied" || !slices.Equal(res.Parts[0].Changed, both) {
		t.Errorf("unlink: %s", states(res))
	}
	for name, res := range map[string]tracker.Result{
		"a link type name": p.Link(t.Context(), tracker.LinkInput{Type: "Blocks", Outward: "10001", Inward: "10002"}),
		"a link path":      p.Unlink(t.Context(), tracker.UnlinkInput{Issue: "10001", Link: "300/x", Other: "10002"}),
		"a key":            p.Unlink(t.Context(), tracker.UnlinkInput{Issue: "10001", Link: "300", Other: "WEB-2"}),
	} {
		if checked(t, res); reason(res, 0) != tracker.ReasonInvalid {
			t.Errorf("%s: %s", name, states(res))
		}
	}
	res = checked(t, p.Attach(t.Context(), tracker.AttachInput{Issue: "10001", Files: []tracker.File{{Name: "a.txt", Content: strings.NewReader("hello")}}}))
	if states(res) != "upload:applied" || !slices.Equal(res.Parts[0].Changed, []tracker.Resource{tracker.DetailOf("10001")}) {
		t.Errorf("attach: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
	f.On("POST", uploadPath, jiratest.Reply(413, `{"errorMessages":["too large"]}`))
	res = checked(t, p.Attach(t.Context(), tracker.AttachInput{Issue: "10001", Files: []tracker.File{{Name: "a.txt", Content: strings.NewReader("hello")}}}))
	if states(res) != "upload:refused" || reason(res, 0) != tracker.ReasonTracker {
		t.Errorf("a refused upload: %s", states(res))
	}
}

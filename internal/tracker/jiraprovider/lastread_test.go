package jiraprovider

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// lastRead watches one record of a fake Jira: read is the request that
// reads it and write the one that writes it, each "METHOD /path". Every
// other request that comes after the record's first read changes it, and
// a write sent while the record holds a change its last read did not
// serve fails the test: the read the write rests on was not the last
// request before it, and the write overwrote what came after.
type lastRead struct {
	read, write string
	mu          sync.Mutex
	seen        bool
	version     int // changes made so far
	served      int // the version the last read served
	writes      int
	log         []string
}

func watchRecord(t *testing.T, f *jiratest.Server, read, write string) *lastRead {
	w := &lastRead{read: read, write: write}
	f.Before(func(r *http.Request) {
		key := r.Method + " " + r.URL.Path
		w.mu.Lock()
		defer w.mu.Unlock()
		w.log = append(w.log, key)
		switch {
		case key == w.read:
			w.seen, w.served = true, w.version
		case key == w.write:
			w.writes++
			if w.version != w.served {
				t.Errorf("%s was sent over %d changes made after its last read: %v", key, w.version-w.served, w.log)
			}
		case w.seen:
			w.version++
		}
	})
	return w
}

// text is the record's text as it stands: base, marked with the change
// count once it has changed.
func (w *lastRead) text(base string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.version == 0 {
		return base
	}
	return fmt.Sprintf("%s (change %d)", base, w.version)
}

func (w *lastRead) sent() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// TestWritesRestOnTheirLastRead runs every write that passes a record it
// read to the client as read (a worklog, a comment, a rich text field, a
// sprint) on a site that changes the record during every request made
// after the read. A write that needs nothing after its read is applied
// with its read right before it; one that needs more (a mention, an
// upload) is refused as stale by the read again that the client makes
// after them, so it never overwrites the change.
func TestWritesRestOnTheirLastRead(t *testing.T) {
	old := adf.ToMarkdown(adf.Doc(para("old")))
	review := adf.ToMarkdown(adf.Doc(para("Review")))
	from := tracker.Worklog{ID: "300", Issue: "10001", Author: "acc-user1", Day: day1(), Spent: time.Hour, Description: review}
	worklog := func(t *testing.T, f *jiratest.Server) *lastRead {
		w := watchRecord(t, f, "GET "+worklogsPath+"/300", "PUT "+worklogsPath+"/300")
		f.On("GET", "/rest/api/3/user", jiratest.Reply(200, user1JSON))
		f.On("GET", worklogsPath+"/300", func(rw http.ResponseWriter, r *http.Request, b []byte) {
			jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, w.text("Review")))(rw, r, b)
		})
		f.On("PUT", worklogsPath+"/300", jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 7200, "Review")))
		return w
	}
	comment := func(t *testing.T, f *jiratest.Server) *lastRead {
		w := watchRecord(t, f, "GET "+commentsPath+"/900", "PUT "+commentsPath+"/900")
		f.On("GET", commentsPath+"/900", func(rw http.ResponseWriter, r *http.Request, b []byte) {
			storedComment(200, adf.Doc(para(w.text("old"))))(rw, r, b)
		})
		f.On("PUT", commentsPath+"/900", storedComment(200, adf.Doc(para("x"))))
		return w
	}
	description := func(t *testing.T, f *jiratest.Server, write string) *lastRead {
		w := watchRecord(t, f, "GET "+issuePath, write)
		f.On("GET", editMetaPath, jiratest.Reply(200, `{"fields":{"description":`+metaFields["description"]+`}}`))
		f.On("GET", issuePath, func(rw http.ResponseWriter, r *http.Request, b []byte) {
			jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"summary":"S","description":`+mustJSON(adf.Doc(para(w.text("old"))))+`}}`)(rw, r, b)
		})
		return w
	}
	sprint := func(t *testing.T, f *jiratest.Server, write string) *lastRead {
		w := watchRecord(t, f, "GET "+board3Sprint, write)
		sprintsOfBoard3(f)
		method, path, _ := strings.Cut(write, " ")
		f.On(method, path, func(rw http.ResponseWriter, r *http.Request, b []byte) {
			if path == "/rest/agile/1.0/sprint/8/issue" {
				rw.WriteHeader(http.StatusNoContent)
				return
			}
			jiratest.Reply(200, `{"id":7,"name":"Sprint 7","state":"active"}`)(rw, r, b)
		})
		return w
	}
	cases := []struct {
		name string
		run  func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result)
		want tracker.Reason // zero for applied
	}{
		{"worklog edit, the author's zone not yet read", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := worklog(t, f)
			return w, p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Spent: ptr(2 * time.Hour)})
		}, 0},
		{"worklog edit naming someone", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := worklog(t, f)
			return w, p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Description: ptr("thanks @[me]")})
		}, tracker.ReasonStale},
		{"comment edit", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := comment(t, f)
			return w, p.EditComment(t.Context(), tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: "new", From: old})
		}, 0},
		{"comment edit naming someone", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := comment(t, f)
			return w, p.EditComment(t.Context(), tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: "thanks @[me]", From: old})
		}, tracker.ReasonStale},
		{"comment edit with images", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := comment(t, f)
			dir := twoImages(t, f, secondStored)
			return w, p.EditComment(t.Context(), tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: twoImagesMD, From: old, ImageDir: dir})
		}, tracker.ReasonStale},
		{"description edit with images", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := description(t, f, "PUT "+issuePath)
			dir := twoImages(t, f, secondStored)
			f.On("PUT", issuePath, func(rw http.ResponseWriter, _ *http.Request, _ []byte) { rw.WriteHeader(http.StatusNoContent) })
			return w, p.Edit(t.Context(), tracker.EditInput{Issue: "10001", ImageDir: dir, Fields: []tracker.FieldEdit{
				{Field: "description", Value: tracker.MarkdownEdit(twoImagesMD, old)},
			}})
		}, tracker.ReasonStale},
		{"move with images", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := description(t, f, "POST /rest/api/3/issue/10001/transitions")
			dir := twoImages(t, f, secondStored)
			f.On("GET", "/rest/api/3/issue/10001/transitions", jiratest.Reply(200, `{"transitions":[
				{"id":"31","name":"Resolve","to":{"id":"10002","name":"Done","statusCategory":{"key":"done"}},"hasScreen":true,"isAvailable":true,"fields":{
				 "description":`+metaFields["description"]+`}}]}`))
			f.On("POST", "/rest/api/3/issue/10001/transitions", func(rw http.ResponseWriter, _ *http.Request, _ []byte) { rw.WriteHeader(http.StatusNoContent) })
			return w, p.Move(t.Context(), tracker.MoveInput{Issue: "10001", Move: "31", ImageDir: dir, Fields: []tracker.FieldEdit{
				{Field: "description", Value: tracker.MarkdownEdit(twoImagesMD, old)},
			}})
		}, tracker.ReasonStale},
		{"move to a sprint", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := sprint(t, f, "POST /rest/agile/1.0/sprint/8/issue")
			listsIssues(t, f, board3Issues, "10001")
			return w, p.MoveToSprint(t.Context(), tracker.SprintMoveInput{Board: "3", Sprint: "8", Issues: []tracker.IssueID{"10001"}})
		}, 0},
		{"sprint edit", func(t *testing.T, f *jiratest.Server, p *Provider) (*lastRead, tracker.Result) {
			w := sprint(t, f, "POST /rest/agile/1.0/sprint/7")
			return w, p.EditSprint(t.Context(), tracker.SprintEdit{Board: "3", Sprint: "7", Goal: ptr("ship")})
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newSite(t)
			w, res := tc.run(t, f, p)
			res = checked(t, res)
			i := len(res.Parts) - 1
			last := res.Parts[i].Outcome
			switch {
			case tc.want == 0 && (last.State() != tracker.StateApplied || w.sent() != 1):
				t.Errorf("%s with %d writes, want applied: %v", states(res), w.sent(), last.Err())
			case tc.want != 0 && (reason(res, i) != tc.want || w.sent() != 0):
				t.Errorf("%s with %d writes, want refused as %v: %v", states(res), w.sent(), tc.want, last.Err())
			}
		})
	}
}

// TestTempoEditRestsOnItsLastRead changes a Tempo worklog during the
// checks the update makes after the provider read it: the update reads it
// again last and is refused as stale, with nothing written.
func TestTempoEditRestsOnItsLastRead(t *testing.T) {
	_, ts, p := tempoSite(t)
	ts.On("GET", "/4/worklogs/70", nil, "", 200, tempoWL(70, 10001, "2026-11-10", "13:00:00", 3600, "Planning"))
	ts.On("GET", "/4/work-attributes", values("offset", "0", "limit", "1000"), "", 200, tempoPage())
	ts.On("GET", "/4/timesheet-approvals/user/acc-user1", values("from", "2026-11-10", "to", "2026-11-10"), "", 200,
		`{"self":"x","actions":{},"worklogs":{"self":"x"},"user":{"accountId":"acc-user1","self":"x"},"period":{"from":"2026-11-09","to":"2026-11-15"},
		"requiredSeconds":144000,"timeSpentSeconds":72000,"status":{"key":"OPEN","actor":{"accountId":"acc-user1","self":"x"},"updatedAt":"2026-11-08T10:00:00Z"}}`)
	// Changed while the checks ran.
	ts.On("GET", "/4/worklogs/70", nil, "", 200, tempoWL(70, 10001, "2026-11-10", "13:00:00", 3600, "Planning, edited"))
	from := tracker.Worklog{ID: "70", Issue: "10001", Author: "acc-user1", Day: day(t, 2026, 11, 10), Spent: time.Hour, Description: "Planning"}
	res := checked(t, p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Spent: ptr(2 * time.Hour)}))
	if reason(res, 0) != tracker.ReasonStale {
		t.Errorf("%s, want refused as stale: %v", states(res), res.Parts[0].Outcome.Err())
	}
	if reqs := ts.Requests(); slices.Contains(reqs, "PUT /4/worklogs/70") {
		t.Errorf("requests %v: the change was overwritten", reqs)
	}
}

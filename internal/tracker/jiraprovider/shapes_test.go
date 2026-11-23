package jiraprovider

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// The tests here send the input shapes each neutral Check permits that
// the tests beside each write do not.

// TestSearchShapes proves a search by text alone, and by project alone
// with only spaces for text, each send one clause, and that a zero limit
// asks for MaxSearchPage with no page token.
func TestSearchShapes(t *testing.T) {
	for _, tc := range []struct {
		q   tracker.SearchQuery
		jql string
	}{
		{tracker.SearchQuery{Text: "login"}, `text ~ "login" ORDER BY updated DESC`},
		{tracker.SearchQuery{Text: "  ", Project: "10000"}, `project = "10000" ORDER BY updated DESC`},
	} {
		f, p := newSite(t)
		f.On("POST", searchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
			v := bodyOf(t, b)
			if _, ok := v["nextPageToken"]; ok || v["jql"] != tc.jql || v["maxResults"] != float64(tracker.MaxSearchPage) {
				t.Errorf("search %s", b)
			}
			jiratest.Reply(200, page("", issue("10001", "WEB-1", updatedAt, nil)))(w, r, b)
		})
		res, err := p.Search(t.Context(), tc.q)
		if err != nil || len(res.Cards) != 1 || res.Next != "" {
			t.Errorf("%+v: %+v %v", tc.q, res, err)
		}
	}
}

// TestSprintEditShapes proves an edit of the goal alone, the start alone
// or the end alone sends that member alone.
func TestSprintEditShapes(t *testing.T) {
	start := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   tracker.SprintEdit
		keys []string
	}{
		{tracker.SprintEdit{Goal: ptr("")}, []string{"goal"}},
		{tracker.SprintEdit{Start: &start}, []string{"startDate"}},
		{tracker.SprintEdit{End: &end}, []string{"endDate"}},
	} {
		f, p := newSite(t)
		sprintsOfBoard3(f)
		f.On("GET", sprint7Path, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3,"startDate":"2026-11-03T09:00:00.000Z","endDate":"2026-11-17T09:00:00.000Z"}`))
		f.On("POST", sprint7Path, func(w http.ResponseWriter, r *http.Request, b []byte) {
			var keys []string
			for k := range bodyOf(t, b) {
				keys = append(keys, k)
			}
			if !slices.Equal(keys, tc.keys) {
				t.Errorf("sent %s, want only %v", b, tc.keys)
			}
			jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3}`)(w, r, b)
		})
		tc.in.Board, tc.in.Sprint = "3", "7"
		if res := checked(t, p.EditSprint(t.Context(), tc.in)); states(res) != "sprint:applied" {
			t.Errorf("%v: %s %v", tc.keys, states(res), res.Parts[0].Outcome.Err())
		}
	}
}

// TestCompleteSprintToBacklog proves a completion with no MoveTo moves
// the unfinished issues to the backlog, then closes the sprint.
func TestCompleteSprintToBacklog(t *testing.T) {
	f := completionSite(t, "", nil, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"closed","originBoardId":3}`))
	f.On("POST", "/rest/agile/1.0/backlog/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001","10003"]}`)
		f.On("GET", sprint7IssuesPath, jiratest.Reply(200, `{"isLast":true,"issues":[{"id":"10002","key":"WEB-2","fields":{"status":{"id":"10002"}}}]}`))
		w.WriteHeader(http.StatusNoContent)
	})
	res := checked(t, newProvider(t, f, nil).CompleteSprint(t.Context(), tracker.SprintCompletion{Board: "3", Sprint: "7"}))
	if states(res) != "sprint move:applied, sprint move:applied, sprint:applied" {
		t.Errorf("got %s", states(res))
	}
}

// TestCommentShapes proves a public comment on a service desk is sent
// without the property that would make it internal, and that a comment edit that loses content is refused
// unless the loss is accepted.
func TestCommentShapes(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", issuePath, jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","projectTypeKey":"service_desk"}}}`))
	f.On("POST", commentsPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if strings.Contains(string(b), "sd.public.comment") || strings.Contains(string(b), "properties") {
			t.Errorf("a public comment %s", b)
		}
		storedComment(201, adf.Doc(para("Hello")))(w, r, b)
	})
	if res := checked(t, p.AddComment(t.Context(), tracker.CommentInput{Issue: "10001", Markdown: "Hello", Audience: tracker.AudiencePublic})); states(res) != "comment:applied" {
		t.Errorf("a public comment: %s %v", states(res), res.Parts[0].Outcome.Err())
	}

	stored := adf.ToMarkdown(layoutDoc())
	flattened := strings.NewReplacer("<!-- layout -->\n\n", "", "<!-- column 50% -->\n\n", "", "<!-- /column -->\n\n", "", "<!-- /layout -->\n\n", "").Replace(stored)
	f.On("GET", commentsPath+"/900", storedComment(200, layoutDoc()))
	f.On("PUT", commentsPath+"/900", storedComment(200, adf.Doc(para("x"))))
	edit := tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: flattened, From: stored}
	if res := checked(t, p.EditComment(t.Context(), edit)); reason(res, 0) != tracker.ReasonLoss || f.Count("PUT", commentsPath+"/900") != 0 {
		t.Errorf("a loss: %s", states(res))
	}
	edit.AcceptLoss = true
	if res := checked(t, p.EditComment(t.Context(), edit)); states(res) != "comment:applied" {
		t.Errorf("the loss accepted: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
}

// TestMoveAcceptLoss proves a move whose rich text edit loses content is
// refused unless the loss is accepted.
func TestMoveAcceptLoss(t *testing.T) {
	f, p := editSite(t, storedDesc(layoutDoc()))
	stored := adf.ToMarkdown(layoutDoc())
	flattened := strings.NewReplacer("<!-- layout -->\n\n", "", "<!-- column 50% -->\n\n", "", "<!-- /column -->\n\n", "", "<!-- /layout -->\n\n", "").Replace(stored)
	f.On("GET", "/rest/api/3/issue/10001/transitions", jiratest.Reply(200, `{"transitions":[
		{"id":"31","name":"Resolve","to":{"id":"10002","name":"Done","statusCategory":{"key":"done"}},"hasScreen":true,"isAvailable":true,"fields":{
		 "description":`+metaFields["description"]+`}}]}`))
	f.On("POST", "/rest/api/3/issue/10001/transitions", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
	in := tracker.MoveInput{Issue: "10001", Move: "31", Fields: []tracker.FieldEdit{{Field: "description", Value: tracker.MarkdownEdit(flattened, stored)}}}
	if res := checked(t, p.Move(t.Context(), in)); reason(res, 0) != tracker.ReasonLoss {
		t.Errorf("a loss: %s", states(res))
	}
	in.AcceptLoss = true
	if res := checked(t, p.Move(t.Context(), in)); states(res) != "move:applied" {
		t.Errorf("the loss accepted: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
	if n := f.Count("POST", "/rest/api/3/issue/10001/transitions"); n != 1 {
		t.Errorf("%d moves", n)
	}
}

// TestAttachSeveralFiles proves an attach of several files is one
// request.
func TestAttachSeveralFiles(t *testing.T) {
	f, p := newSite(t)
	f.On("POST", uploadPath, jiratest.Reply(200, `[{"id":"501","filename":"a.txt","mimeType":"text/plain","size":1},{"id":"502","filename":"b.txt","mimeType":"text/plain","size":1}]`))
	res := checked(t, p.Attach(t.Context(), tracker.AttachInput{Issue: "10001", Files: []tracker.File{
		{Name: "a.txt", Content: strings.NewReader("a")}, {Name: "b.txt", Content: strings.NewReader("b")},
	}}))
	if states(res) != "upload:applied" || f.Count("POST", uploadPath) != 1 {
		t.Errorf("got %s, %d requests", states(res), f.Count("POST", uploadPath))
	}
}

// TestCreateWithoutType proves a create with no issue type, which Jira
// needs, is refused before anything is read or sent.
func TestCreateWithoutType(t *testing.T) {
	f, p := newSite(t)
	res := checked(t, p.Create(t.Context(), tracker.CreateInput{Project: "10000", Title: "New"}))
	if states(res) != "create:refused" || reason(res, 0) != tracker.ReasonInvalid || len(f.Requests()) != 0 {
		t.Errorf("got %s, %d requests", states(res), len(f.Requests()))
	}
}

// TestWorklogEditShapes proves an edit of the description alone on Jira,
// and of the time spent alone and the description alone on Tempo, are
// applied.
func TestWorklogEditShapes(t *testing.T) {
	d := day(t, 2026, 11, 10)
	f, p := worklogSite(t)
	f.On("PUT", worklogsPath+"/300", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if v := bodyOf(t, b); !strings.Contains(mustJSON(v["comment"]), "Reviewed") {
			t.Errorf("edit %s", b)
		}
		jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, "Reviewed"))(w, r, b)
	})
	from := tracker.Worklog{ID: "300", Issue: "10001", Author: "acc-user1", Day: d, Spent: time.Hour, Description: adf.ToMarkdown(adf.Doc(para("Review")))}
	if res := checked(t, p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Description: ptr("Reviewed")})); states(res) != "worklog:applied" {
		t.Errorf("a Jira description: %s %v", states(res), res.Parts[0].Outcome.Err())
	}

	_, ts, tp := tempoSite(t)
	ts.On("GET", "/4/work-attributes", values("offset", "0", "limit", "1000"), "", 200, tempoPage()).Repeat = true
	ts.On("GET", "/4/timesheet-approvals/user/acc-user1", values("from", "2026-11-10", "to", "2026-11-10"), "", 200,
		`{"self":"x","actions":{},"worklogs":{"self":"x"},"user":{"accountId":"acc-user1","self":"x"},"period":{"from":"2026-11-09","to":"2026-11-15"},
		"requiredSeconds":144000,"timeSpentSeconds":72000,"status":{"key":"OPEN","actor":{"accountId":"acc-user1","self":"x"},"updatedAt":"2026-11-08T10:00:00Z"}}`).Repeat = true
	ts.On("GET", "/4/worklogs/70", nil, "", 200, tempoWL(70, 10001, "2026-11-10", "13:00:00", 3600, "Planning")).Repeat = true
	ts.On("PUT", "/4/worklogs/70", nil,
		`{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-10","startTime":"13:00:00","timeSpentSeconds":1800,"billableSeconds":1800,"description":"Planning","attributes":[]}`,
		200, tempoWL(70, 10001, "2026-11-10", "13:00:00", 1800, "Planning"))
	ts.On("PUT", "/4/worklogs/70", nil,
		`{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-10","startTime":"13:00:00","timeSpentSeconds":3600,"billableSeconds":3600,"description":"Plans","attributes":[]}`,
		200, tempoWL(70, 10001, "2026-11-10", "13:00:00", 3600, "Plans"))
	tfrom := tracker.Worklog{ID: "70", Issue: "10001", Author: "acc-user1", Day: d, Spent: time.Hour, Description: "Planning"}
	for name, in := range map[string]tracker.WorklogEdit{
		"Tempo time spent":  {From: tfrom, Spent: ptr(30 * time.Minute)},
		"Tempo description": {From: tfrom, Description: ptr("Plans")},
	} {
		if res := checked(t, tp.Time().EditWorklog(t.Context(), in)); states(res) != "worklog:applied" {
			t.Errorf("%s: %s %v", name, states(res), res.Parts[0].Outcome.Err())
		}
	}
}

// TestHistoryWithoutBoards proves a history read naming no board reads
// the issues' changes without any board's estimate.
func TestHistoryWithoutBoards(t *testing.T) {
	f, p := newSite(t)
	f.On("POST", changelogsPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if fields := mustJSON(bodyOf(t, b)["fieldIds"]); strings.Contains(fields, "customfield_10016") {
			t.Errorf("an estimate was read with no board: %s", b)
		}
		jiratest.Reply(200, `{"issueChangeLogs":[]}`)(w, r, b)
	})
	f.On("POST", bulkFetchPath, jiratest.Reply(200, page("", issue("10001", "WEB-1", updatedAt, nil))))
	h, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{"10001"}})
	if err != nil || len(h.Issues) != 1 || h.Issues[0].Issue != "10001" {
		t.Errorf("got %+v %v", h, err)
	}
	if errors.Is(err, tracker.ErrInvalidInput) {
		t.Error("refused")
	}
}

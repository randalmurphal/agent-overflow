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
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	rankPath     = "/rest/agile/1.0/issue/rank"
	board3Sprint = "/rest/agile/1.0/board/3/sprint"
	commentsPath = "/rest/api/3/issue/10001/comment"
)

// listsIssues answers path, a board's issue list, as a board check asks
// it: the issues of the JQL's ID list that listed holds.
func listsIssues(t *testing.T, f *jiratest.Server, path string, listed ...string) {
	f.On("GET", path, func(w http.ResponseWriter, r *http.Request, b []byte) {
		m := idsJQL.FindStringSubmatch(r.URL.Query().Get("jql"))
		if m == nil {
			t.Errorf("board check JQL %q", r.URL.Query().Get("jql"))
			jiratest.Reply(400, `{}`)(w, r, b)
			return
		}
		var out []map[string]any
		for id := range strings.SplitSeq(m[1], ", ") {
			if slices.Contains(listed, id) {
				out = append(out, issue(id, "WEB-"+id[len(id)-1:], updatedAt, nil))
			}
		}
		jiratest.Reply(200, page("", out...))(w, r, b)
	})
}

// sprintsOfBoard3 answers board 3's sprint list: 6 closed, 7 active and
// 8 future, kept to the states the request names.
func sprintsOfBoard3(f *jiratest.Server) {
	f.On("GET", board3Sprint, func(w http.ResponseWriter, r *http.Request, b []byte) {
		all := map[string]string{"6": "closed", "7": "active", "8": "future"}
		want := strings.Split(r.URL.Query().Get("state"), ",")
		var out []string
		for _, id := range []string{"6", "7", "8"} {
			if r.URL.Query().Get("state") == "" || slices.Contains(want, all[id]) {
				out = append(out, `{"id":`+id+`,"name":"Sprint `+id+`","state":"`+all[id]+`"}`)
			}
		}
		jiratest.Reply(200, `{"isLast":true,"values":[`+strings.Join(out, ",")+`]}`)(w, r, b)
	})
}

// noWrites fails t when f received anything but a read.
func noWrites(t *testing.T, f *jiratest.Server, name string) {
	t.Helper()
	for _, r := range f.Requests() {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("%s: a write was sent: %s", name, r)
		}
	}
}

// TestRankNeighbors proves a rank with only Prev, only Next or both ranks
// against Prev when there is one and Next otherwise, and checks that
// neighbor against the board.
func TestRankNeighbors(t *testing.T) {
	prev := &tracker.Neighbor{Issue: "10009", Order: "0|a"}
	next := &tracker.Neighbor{Issue: "10010", Order: "0|b"}
	for _, tc := range []struct {
		name       string
		prev, next *tracker.Neighbor
		body       string
	}{
		{"prev only", prev, nil, `{"issues":["10001"],"rankAfterIssue":"10009","rankCustomFieldId":10019}`},
		{"next only", nil, next, `{"issues":["10001"],"rankBeforeIssue":"10010","rankCustomFieldId":10019}`},
		{"both", prev, next, `{"issues":["10001"],"rankAfterIssue":"10009","rankCustomFieldId":10019}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newSite(t)
			listsIssues(t, f, board3Issues, "10001", "10009", "10010")
			f.On("PUT", rankPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
				jiratest.WantJSON(t, b, tc.body)
				w.WriteHeader(http.StatusNoContent)
			})
			res := checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Prev: tc.prev, Next: tc.next}))
			if states(res) != "rank:applied" || f.Count("PUT", rankPath) != 1 {
				t.Errorf("got %s %v", states(res), res.Parts[0].Outcome.Err())
			}
		})
	}
}

// TestRankOffBoard proves an issue the board does not list is refused
// while the others are ranked, and that a neighbor off the board refuses
// the whole rank before anything is sent.
func TestRankOffBoard(t *testing.T) {
	f, p := newSite(t)
	listsIssues(t, f, board3Issues, "10001", "10009")
	f.On("PUT", rankPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001"],"rankAfterIssue":"10009","rankCustomFieldId":10019}`)
		w.WriteHeader(http.StatusNoContent)
	})
	res := checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10002", "10001"}, Prev: &tracker.Neighbor{Issue: "10009", Order: "0|a"}}))
	if states(res) != "rank:refused, rank:applied" || reason(res, 0) != tracker.ReasonInvalid || res.Parts[0].Issue != "10002" {
		t.Errorf("an issue off the board: %s", states(res))
	}
	res = checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Next: &tracker.Neighbor{Issue: "10010", Order: "0|b"}}))
	if states(res) != "rank:refused" || reason(res, 0) != tracker.ReasonInvalid {
		t.Errorf("a neighbor off the board: %s", states(res))
	}
	if n := f.Count("PUT", rankPath); n != 1 {
		t.Errorf("%d ranks sent", n)
	}
}

// TestMoveToSprintScope proves a sprint move is refused before sending
// when its sprint is not an open sprint of the board, and refuses the
// issues the board does not list while it moves the rest.
func TestMoveToSprintScope(t *testing.T) {
	f, p := newSite(t)
	sprintsOfBoard3(f)
	listsIssues(t, f, board3Issues, "10001")
	issues := []tracker.IssueID{"10001", "10002"}
	for name, sprint := range map[string]tracker.SprintID{"another board's sprint": "99", "a closed sprint": "6"} {
		res := checked(t, p.MoveToSprint(t.Context(), tracker.SprintMoveInput{Board: "3", Sprint: sprint, Issues: issues}))
		if states(res) != "sprint move:refused, sprint move:refused" || reason(res, 0) != tracker.ReasonInvalid {
			t.Errorf("%s: %s", name, states(res))
		}
	}
	noWrites(t, f, "a sprint off the board")
	f.On("GET", "/rest/agile/1.0/sprint/8", jiratest.Reply(200, `{"id":8,"name":"Sprint 8","state":"future","originBoardId":3}`))
	f.On("POST", "/rest/agile/1.0/sprint/8/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001"]}`)
		w.WriteHeader(http.StatusNoContent)
	})
	f.On("POST", "/rest/agile/1.0/backlog/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["10001"]}`)
		w.WriteHeader(http.StatusNoContent)
	})
	for name, sprint := range map[string]tracker.SprintID{"into a sprint": "8", "to the backlog": ""} {
		res := checked(t, p.MoveToSprint(t.Context(), tracker.SprintMoveInput{Board: "3", Sprint: sprint, Issues: issues}))
		if states(res) != "sprint move:applied, sprint move:refused" || reason(res, 1) != tracker.ReasonInvalid {
			t.Errorf("%s with an issue off the board: %s", name, states(res))
		}
	}
}

// TestSetEstimateOffBoard proves an estimate of an issue the board does
// not list is refused before sending.
func TestSetEstimateOffBoard(t *testing.T) {
	f, p := newSite(t)
	listsIssues(t, f, board3Issues)
	res := checked(t, p.SetEstimate(t.Context(), tracker.EstimateInput{Board: "3", Issue: "10001", Value: ptr(1.0)}))
	if states(res) != "estimate:refused" || reason(res, 0) != tracker.ReasonInvalid {
		t.Errorf("got %s", states(res))
	}
	noWrites(t, f, "an estimate off the board")
}

// TestSprintWritesScope proves a sprint started, edited or completed, or
// completed into a sprint, that is not one of the board in the state the
// write needs is refused before anything is sent.
func TestSprintWritesScope(t *testing.T) {
	f, p := newSite(t)
	sprintsOfBoard3(f)
	start := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 14)
	for name, res := range map[string]tracker.Result{
		"starting another board's sprint": p.StartSprint(t.Context(), tracker.SprintStart{Board: "3", Sprint: "99", Start: start, End: end}),
		"starting a closed sprint":        p.StartSprint(t.Context(), tracker.SprintStart{Board: "3", Sprint: "6", Start: start, End: end}),
		"editing another board's sprint":  p.EditSprint(t.Context(), tracker.SprintEdit{Board: "3", Sprint: "99", Name: ptr("x")}),
		"completing another's sprint":     p.CompleteSprint(t.Context(), tracker.SprintCompletion{Board: "3", Sprint: "99"}),
		"completing a future sprint":      p.CompleteSprint(t.Context(), tracker.SprintCompletion{Board: "3", Sprint: "8"}),
		"completing into another's":       p.CompleteSprint(t.Context(), tracker.SprintCompletion{Board: "3", Sprint: "7", MoveTo: "99"}),
		"completing into a closed sprint": p.CompleteSprint(t.Context(), tracker.SprintCompletion{Board: "3", Sprint: "7", MoveTo: "6"}),
	} {
		checked(t, res)
		if states(res) != "sprint:refused" || reason(res, 0) != tracker.ReasonInvalid {
			t.Errorf("%s: %s", name, states(res))
		}
		if r, _ := res.Parts[0].Outcome.Refusal(); !strings.Contains(r.Message, "board 3") {
			t.Errorf("%s: %q", name, r.Message)
		}
	}
	noWrites(t, f, "sprints off the board")
	f.On("GET", board3Sprint, jiratest.Reply(500, `{}`))
	if res := checked(t, p.EditSprint(t.Context(), tracker.SprintEdit{Board: "3", Sprint: "7", Name: ptr("x")})); states(res) != "sprint:not sent" {
		t.Errorf("a failed sprint listing: %s", states(res))
	}
}

// TestCommentScope proves a comment edited or deleted is read once
// through its issue's path, the edit's stale check being that read, and
// that one Jira does not find there, gone or another issue's, is refused
// before the write with the client's not found reachable.
func TestCommentScope(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", commentsPath+"/900", jiratest.Reply(404, `{"errorMessages":["Can not find a comment for the id: 900."]}`))
	for name, res := range map[string]tracker.Result{
		"an edit": p.EditComment(t.Context(), tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: "x", From: "c"}),
		"delete":  p.DeleteComment(t.Context(), tracker.CommentDelete{Issue: "10001", Comment: "900"}),
	} {
		checked(t, res)
		r, _ := res.Parts[0].Outcome.Refusal()
		if states(res) != "comment:refused" || r.Reason != tracker.ReasonInvalid || !strings.Contains(r.Message, "not on issue 10001") {
			t.Errorf("%s of another issue's comment: %s %+v", name, states(res), r)
		}
		if !errors.Is(r.Err, trackerapi.ErrNotFound) || !errors.Is(r.Err, tracker.ErrInvalidInput) {
			t.Errorf("%s: the not found is not reachable: %v", name, r.Err)
		}
	}
	noWrites(t, f, "another issue's comment")
	if n := f.Count("GET", commentsPath+"/900"); n != 2 {
		t.Errorf("%d comment reads for two writes", n)
	}
	f.On("GET", commentsPath+"/901", storedComment(200, adf.Doc(para("c"))))
	f.On("DELETE", commentsPath+"/901", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
	if res := checked(t, p.DeleteComment(t.Context(), tracker.CommentDelete{Issue: "10001", Comment: "901"})); states(res) != "comment:applied" || f.Count("GET", commentsPath+"/901") != 1 {
		t.Errorf("a delete of the issue's comment: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
	f.On("GET", commentsPath+"/901", jiratest.Reply(500, `{}`))
	if res := checked(t, p.DeleteComment(t.Context(), tracker.CommentDelete{Issue: "10001", Comment: "901"})); states(res) != "comment:not sent" {
		t.Errorf("a failed comment read: %s", states(res))
	}
	if n := f.Count("DELETE", commentsPath+"/901"); n != 1 {
		t.Errorf("%d deletes", n)
	}
}

// TestUnlinkScope proves an unlink is refused before sending unless the
// link is one of the issue's and joins it to Other.
func TestUnlinkScope(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "fields", "issuelinks")
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"issuelinks":[
			{"id":"300","type":{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"},"outwardIssue":{"id":"10003","key":"WEB-3"}}]}}`)(w, r, b)
	})
	for name, in := range map[string]tracker.UnlinkInput{
		"a link to another issue": {Issue: "10001", Link: "300", Other: "10002"},
		"a link of no issue's":    {Issue: "10001", Link: "301", Other: "10003"},
	} {
		if res := checked(t, p.Unlink(t.Context(), in)); states(res) != "link:refused" || reason(res, 0) != tracker.ReasonInvalid {
			t.Errorf("%s: %s", name, states(res))
		}
	}
	noWrites(t, f, "a link not between the issues")
}

// TestWorklogDeleteNamesWhatItRead proves a worklog delete's result names
// the author's worklogs of the day the worklog holds now, and of the day
// the caller read, from the worklog as read before the delete, when the
// caller's copy names no author.
func TestWorklogDeleteNamesWhatItRead(t *testing.T) {
	read, now := day(t, 2026, 11, 10), day(t, 2026, 11, 11)
	want := []tracker.Resource{tracker.DetailOf("10001"), tracker.WorklogsOf("acc-user1", now), tracker.WorklogsOf("acc-user1", read)}
	t.Run("jira", func(t *testing.T) {
		f, p := worklogSite(t)
		f.On("GET", worklogsPath+"/300", jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-11T09:00:00.000-0500", 3600, "Review")))
		f.On("DELETE", worklogsPath+"/300", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
		res := checked(t, p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: tracker.Worklog{ID: "300", Issue: "10001", Day: read}}))
		if states(res) != "worklog:applied" || !slices.Equal(res.Parts[0].Changed, want) {
			t.Errorf("got %s %+v", states(res), res.Parts[0].Changed)
		}
	})
	t.Run("tempo", func(t *testing.T) {
		_, ts, p := tempoSite(t)
		ts.On("GET", "/4/worklogs/70", nil, "", 200, tempoWL(70, 10001, "2026-11-11", "13:00:00", 3600, "Planning"))
		ts.On("DELETE", "/4/worklogs/70", nil, "", 204, "")
		res := checked(t, p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: tracker.Worklog{ID: "70", Issue: "10001", Day: read}}))
		if states(res) != "worklog:applied" || !slices.Equal(res.Parts[0].Changed, want) {
			t.Errorf("got %s %+v", states(res), res.Parts[0].Changed)
		}
	})
}

// TestWorklogScope proves a worklog edited or deleted whose own issue is
// not the one From names is refused before anything is sent, on Jira
// and on Tempo.
func TestWorklogScope(t *testing.T) {
	d := day(t, 2026, 11, 10)
	t.Run("jira", func(t *testing.T) {
		f, p := worklogSite(t)
		f.On("GET", worklogsPath+"/300", jiratest.Reply(200, strings.Replace(
			jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, "Review"), `"issueId":"10001"`, `"issueId":"10002"`, 1)))
		from := tracker.Worklog{ID: "300", Issue: "10001", Author: "acc-user1", Day: d, Spent: time.Hour, Description: adf.ToMarkdown(adf.Doc(para("Review")))}
		for name, res := range map[string]tracker.Result{
			"edit":   p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Spent: ptr(2 * time.Hour)}),
			"delete": p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from}),
		} {
			if checked(t, res); states(res) != "worklog:refused" || reason(res, 0) != tracker.ReasonInvalid {
				t.Errorf("%s: %s %v", name, states(res), res.Parts[0].Outcome.Err())
			}
		}
		noWrites(t, f, "another issue's worklog")
	})
	t.Run("tempo", func(t *testing.T) {
		_, ts, p := tempoSite(t)
		ts.On("GET", "/4/worklogs/71", nil, "", 200, tempoWL(71, 10002, "2026-11-10", "13:00:00", 3600, "Planning")).Repeat = true
		from := tracker.Worklog{ID: "71", Issue: "10001", Author: "acc-user1", Day: d, Spent: time.Hour, Description: "Planning"}
		for name, res := range map[string]tracker.Result{
			"edit":   p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Description: ptr("x")}),
			"delete": p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from}),
		} {
			if checked(t, res); states(res) != "worklog:refused" || reason(res, 0) != tracker.ReasonInvalid {
				t.Errorf("%s: %s %v", name, states(res), res.Parts[0].Outcome.Err())
			}
		}
	})
}

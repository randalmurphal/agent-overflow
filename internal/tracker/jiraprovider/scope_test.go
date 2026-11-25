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
// it: the issues of the JQL's ID list that listed holds, each ranked by
// its ID when the request asks for the rank field, as Jira sends only the
// fields asked for.
func listsIssues(t *testing.T, f *jiratest.Server, path string, listed ...string) {
	f.On("GET", path, func(w http.ResponseWriter, r *http.Request, b []byte) {
		m := idsJQL.FindStringSubmatch(r.URL.Query().Get("jql"))
		if m == nil {
			t.Errorf("board check JQL %q", r.URL.Query().Get("jql"))
			jiratest.Reply(400, `{}`)(w, r, b)
			return
		}
		ranked := slices.Contains(r.URL.Query()["fields"], "customfield_10019")
		var out []map[string]any
		for id := range strings.SplitSeq(m[1], ", ") {
			if slices.Contains(listed, id) {
				var rank any
				if ranked {
					rank = "0|" + id
				}
				out = append(out, issue(id, "WEB-"+id[len(id)-1:], updatedAt, map[string]any{"customfield_10019": rank}))
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
	const prev, next tracker.IssueID = "10009", "10010"
	for _, tc := range []struct {
		name       string
		prev, next tracker.IssueID
		body       string
	}{
		{"prev only", prev, "", `{"issues":["10001"],"rankAfterIssue":"10009","rankCustomFieldId":10019}`},
		{"next only", "", next, `{"issues":["10001"],"rankBeforeIssue":"10010","rankCustomFieldId":10019}`},
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

// TestRankNeighborOrder proves a rank naming both neighbors reads both
// through the board listing with the board's rank field, ranks after Prev
// when Prev still sorts before Next, and is refused before sending when
// they are swapped or Next is off the board.
func TestRankNeighborOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prev, next tracker.IssueID
		listed     []string
		sent       bool
		says       string
	}{
		{"in order", "10009", "10010", []string{"10001", "10009", "10010"}, true, ""},
		{"swapped", "10010", "10009", []string{"10001", "10009", "10010"}, false, "no longer stand in that order on board 3; read the board again"},
		{"next off the board", "10009", "10010", []string{"10001", "10009"}, false, "issue 10010, which the issues go next to, is not on board 3 now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newSite(t)
			listsIssues(t, f, board3Issues, tc.listed...)
			f.On("PUT", rankPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
				jiratest.WantJSON(t, b, `{"issues":["10001"],"rankAfterIssue":"`+string(tc.prev)+`","rankCustomFieldId":10019}`)
				w.WriteHeader(http.StatusNoContent)
			})
			res := checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Prev: tc.prev, Next: tc.next}))
			if tc.sent {
				if states(res) != "rank:applied" || f.Count("PUT", rankPath) != 1 {
					t.Errorf("got %s %v", states(res), res.Parts[0].Outcome.Err())
				}
				return
			}
			r, _ := res.Parts[0].Outcome.Refusal()
			if states(res) != "rank:refused" || r.Reason != tracker.ReasonStale || !strings.Contains(r.Message, tc.says) {
				t.Errorf("got %s %+v", states(res), r)
			}
			noWrites(t, f, tc.name)
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
	res := checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10002", "10001"}, Prev: "10009"}))
	if states(res) != "rank:refused, rank:applied" || reason(res, 0) != tracker.ReasonStale || res.Parts[0].Issue != "10002" {
		t.Errorf("an issue off the board: %s", states(res))
	}
	res = checked(t, p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Next: "10010"}))
	if states(res) != "rank:refused" || reason(res, 0) != tracker.ReasonStale {
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
		if states(res) != "sprint move:refused, sprint move:refused" || reason(res, 0) != tracker.ReasonStale {
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
		if states(res) != "sprint move:applied, sprint move:refused" || reason(res, 1) != tracker.ReasonStale {
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
	if states(res) != "estimate:refused" || reason(res, 0) != tracker.ReasonStale {
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
		if states(res) != "sprint:refused" || reason(res, 0) != tracker.ReasonStale {
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
		if states(res) != "comment:refused" || r.Reason != tracker.ReasonStale || !strings.Contains(r.Message, "not on issue 10001") {
			t.Errorf("%s of another issue's comment: %s %+v", name, states(res), r)
		}
		if !errors.Is(r.Err, trackerapi.ErrNotFound) {
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
		if res := checked(t, p.Unlink(t.Context(), in)); states(res) != "link:refused" || reason(res, 0) != tracker.ReasonStale {
			t.Errorf("%s: %s", name, states(res))
		}
	}
	noWrites(t, f, "a link not between the issues")
}

// TestWorklogWritesNeedTheRevisionAsRead proves an edit or a delete of a
// worklog whose tracker revision moved since the caller read it is refused
// as stale with nothing sent, though every field the caller shows is the
// same: a Jira worklog whose start moved within its day, a Tempo worklog
// whose billable time changed. The copy as read, its revision included,
// is applied.
func TestWorklogWritesNeedTheRevisionAsRead(t *testing.T) {
	read := day(t, 2026, 11, 10)
	hour := 2 * time.Hour
	writes := func(p *Provider, from tracker.Worklog) map[string]tracker.Result {
		return map[string]tracker.Result{
			"edit":   p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Spent: &hour}),
			"delete": p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from}),
		}
	}
	t.Run("jira", func(t *testing.T) {
		f, p := worklogSite(t)
		later := strings.Replace(jiraWorklog("300", user1JSON, "2026-11-10T11:00:00.000-0500", 3600, "Review"), updatedAt, "2026-11-10T16:00:00.000+0000", 1)
		f.On("GET", worklogsPath+"/300", jiratest.Reply(200, later))
		from := tracker.Worklog{ID: "300", Issue: "10001", Author: "acc-user1", Day: read, Spent: time.Hour,
			Description: adf.ToMarkdown(adf.Doc(para("Review"))), Updated: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
		for name, res := range writes(p, from) {
			if checked(t, res); states(res) != "worklog:refused" || reason(res, 0) != tracker.ReasonStale {
				t.Errorf("%s of a worklog moved within its day: %s %v", name, states(res), res.Parts[0].Outcome.Err())
			}
		}
		noWrites(t, f, "a worklog whose revision moved")
		f.On("GET", worklogsPath+"/300", jiratest.Reply(200, later))
		f.On("DELETE", worklogsPath+"/300", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
		from.Updated = time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
		if res := checked(t, p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from})); states(res) != "worklog:applied" {
			t.Errorf("the delete of the worklog as read: %s %v", states(res), res.Parts[0].Outcome.Err())
		}
	})
	t.Run("tempo", func(t *testing.T) {
		_, ts, p := tempoSite(t)
		later := strings.Replace(tempoWL(70, 10001, "2026-11-10", "13:00:00", 3600, "Planning"), "2026-11-03T09:30:00Z", "2026-11-10T15:00:00Z", 1)
		ts.On("GET", "/4/worklogs/70", nil, "", 200, later).Repeat = true
		from := tracker.Worklog{ID: "70", Issue: "10001", Author: "acc-user1", Day: read, Spent: time.Hour, Description: "Planning",
			Updated: time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC)}
		for name, res := range writes(p, from) {
			if checked(t, res); states(res) != "worklog:refused" || reason(res, 0) != tracker.ReasonStale {
				t.Errorf("%s of a worklog changed in Tempo: %s %v", name, states(res), res.Parts[0].Outcome.Err())
			}
		}
		for _, r := range ts.Requests() {
			if !strings.HasPrefix(r, "GET ") {
				t.Fatalf("a write was sent for a worklog whose revision moved: %s", r)
			}
		}
		ts.On("DELETE", "/4/worklogs/70", nil, "", 204, "")
		from.Updated = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
		if res := checked(t, p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from})); states(res) != "worklog:applied" {
			t.Errorf("the delete of the worklog as read: %s %v", states(res), res.Parts[0].Outcome.Err())
		}
	})
}

// TestWorklogDeleteNeedsTheWorklogAsRead proves a worklog delete is sent
// only while the worklog is the copy the caller read, on Jira and on
// Tempo: one changed since (here moved to another day, or read with no
// author) is refused as stale with nothing sent, and the delete of the
// copy as read names its issue's detail and its author's worklogs of its
// day.
func TestWorklogDeleteNeedsTheWorklogAsRead(t *testing.T) {
	read, now := day(t, 2026, 11, 10), day(t, 2026, 11, 11)
	want := []tracker.Resource{tracker.DetailOf("10001"), tracker.WorklogsOf("acc-user1", read)}
	t.Run("jira", func(t *testing.T) {
		f, p := worklogSite(t)
		f.On("GET", worklogsPath+"/300", jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-11T09:00:00.000-0500", 3600, "Review")))
		from := tracker.Worklog{ID: "300", Issue: "10001", Author: "acc-user1", Day: read, Spent: time.Hour, Description: adf.ToMarkdown(adf.Doc(para("Review"))), Updated: jiraRevision}
		noAuthor := from
		noAuthor.Author, noAuthor.Day = "", now
		for name, in := range map[string]tracker.Worklog{"moved": from, "no author": noAuthor} {
			if res := checked(t, p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: in})); states(res) != "worklog:refused" || reason(res, 0) != tracker.ReasonStale {
				t.Errorf("%s: %s %v", name, states(res), res.Parts[0].Outcome.Err())
			}
		}
		noWrites(t, f, "a worklog changed since it was read")
		f.On("GET", worklogsPath+"/300", jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, "Review")))
		f.On("DELETE", worklogsPath+"/300", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
		res := checked(t, p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from}))
		if states(res) != "worklog:applied" || !slices.Equal(res.Parts[0].Changed, want) || f.Count("DELETE", worklogsPath+"/300") != 1 {
			t.Errorf("got %s %+v", states(res), res.Parts[0].Changed)
		}
	})
	t.Run("tempo", func(t *testing.T) {
		_, ts, p := tempoSite(t)
		ts.On("GET", "/4/worklogs/70", nil, "", 200, tempoWL(70, 10001, "2026-11-11", "13:00:00", 3600, "Planning")).Repeat = true
		from := tracker.Worklog{ID: "70", Issue: "10001", Author: "acc-user1", Day: read, Spent: time.Hour, Description: "Planning", Updated: tempoRevision}
		noAuthor := from
		noAuthor.Author, noAuthor.Day = "", now
		for name, in := range map[string]tracker.Worklog{"moved": from, "no author": noAuthor} {
			if res := checked(t, p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: in})); states(res) != "worklog:refused" || reason(res, 0) != tracker.ReasonStale {
				t.Errorf("%s: %s %v", name, states(res), res.Parts[0].Outcome.Err())
			}
		}
		if reqs := ts.Requests(); slices.Contains(reqs, "DELETE /4/worklogs/70") {
			t.Fatalf("a stale copy's delete was sent: %v", reqs)
		}
		moved := from
		moved.Day = now
		ts.On("DELETE", "/4/worklogs/70", nil, "", 204, "")
		res := checked(t, p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: moved}))
		if want := []tracker.Resource{tracker.DetailOf("10001"), tracker.WorklogsOf("acc-user1", now)}; states(res) != "worklog:applied" || !slices.Equal(res.Parts[0].Changed, want) {
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
		from := tracker.Worklog{ID: "300", Issue: "10001", Author: "acc-user1", Day: d, Spent: time.Hour, Description: adf.ToMarkdown(adf.Doc(para("Review"))), Updated: jiraRevision}
		for name, res := range map[string]tracker.Result{
			"edit":   p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Spent: ptr(2 * time.Hour)}),
			"delete": p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from}),
		} {
			if checked(t, res); states(res) != "worklog:refused" || reason(res, 0) != tracker.ReasonStale {
				t.Errorf("%s: %s %v", name, states(res), res.Parts[0].Outcome.Err())
			}
		}
		noWrites(t, f, "another issue's worklog")
	})
	t.Run("tempo", func(t *testing.T) {
		_, ts, p := tempoSite(t)
		ts.On("GET", "/4/worklogs/71", nil, "", 200, tempoWL(71, 10002, "2026-11-10", "13:00:00", 3600, "Planning")).Repeat = true
		from := tracker.Worklog{ID: "71", Issue: "10001", Author: "acc-user1", Day: d, Spent: time.Hour, Description: "Planning", Updated: tempoRevision}
		for name, res := range map[string]tracker.Result{
			"edit":   p.Time().EditWorklog(t.Context(), tracker.WorklogEdit{From: from, Description: ptr("x")}),
			"delete": p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: from}),
		} {
			if checked(t, res); states(res) != "worklog:refused" || reason(res, 0) != tracker.ReasonStale {
				t.Errorf("%s: %s %v", name, states(res), res.Parts[0].Outcome.Err())
			}
		}
	})
}

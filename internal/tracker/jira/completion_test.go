package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

const (
	sprint8URL       = "/rest/agile/1.0/sprint/8"
	sprint7IssuesURL = "/rest/software/1.0/sprint/7/issue"
)

// completionSite is sprint 7, active on board 3, with 60 issues in
// progress (WEB-1 to WEB-60) and two done (WEB-61, WEB-62); sprint 8 is
// future. The sprint's issues come in pages of 40.
func completionSite(t *testing.T) (*fakeJira, *Client) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", sprint7URL, reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3}`))
	f.on("GET", sprint8URL, reply(200, `{"id":8,"name":"Sprint 2","state":"future","originBoardId":3}`))
	f.on("GET", boardConfigURL, reply(200, boardConfigBody))
	f.on("GET", sprint7IssuesURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "fields", "status")
		start := 0
		if tok := r.URL.Query().Get("nextPageToken"); tok != "" {
			if _, err := fmt.Sscanf(tok, "from-%d", &start); err != nil {
				t.Errorf("token %q: %v", tok, err)
			}
		}
		var issues []string
		for i := start + 1; i <= min(start+40, 62); i++ {
			status := "3"
			if i > 60 {
				status = "10002"
			}
			issues = append(issues, fmt.Sprintf(`{"id":"%d","key":"WEB-%d","fields":{"status":{"id":%q}}}`, 10000+i, i, status))
		}
		next, last := fmt.Sprintf(`"from-%d"`, start+40), start+40 >= 62
		if last {
			next = "null"
		}
		reply(200, fmt.Sprintf(`{"issues":[%s],"nextPageToken":%s,"isLast":%v}`, strings.Join(issues, ","), next, last))(w, r, b)
	})
	return f, c
}

// TestCompleteSprint is the full success: reads, then the moves of the
// unfinished issues in groups of 50, then the close, in that order.
func TestCompleteSprint(t *testing.T) {
	f, c := completionSite(t)
	var moved []string
	f.on("POST", sprint8URL+"/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var body batchBody
		if err := json.Unmarshal(b, &body); err != nil || body.Before != "" || body.After != "" || body.Field != 0 {
			t.Errorf("a move %s", b)
		}
		moved = append(moved, body.Issues...)
		w.WriteHeader(http.StatusNoContent)
	})
	f.on("POST", sprint7URL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"state":"closed"}`)
		reply(200, `{"id":7,"name":"Sprint 1","state":"closed","completeDate":"2026-11-16T09:00:00.000Z"}`)(w, r, b)
	})
	res, err := c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 8})
	if err != nil || !res.Closed || res.Sprint == nil || res.Sprint.State != SprintClosed || res.Sprint.Done.IsZero() {
		t.Fatalf("got %+v %v", res, err)
	}
	if strings.Join(res.Unfinished, ",") != strings.Join(webIssues(1, 60), ",") || strings.Join(moved, ",") != strings.Join(webIssues(1, 60), ",") || len(res.Moves.Done()) != 60 {
		t.Errorf("unfinished %v, moved %v", res.Unfinished, moved)
	}
	want := []string{
		"GET " + sprint7URL, "GET " + sprint8URL, "GET " + boardConfigURL,
		"GET " + sprint7IssuesURL, "GET " + sprint7IssuesURL,
		"POST " + sprint8URL + "/issue", "POST " + sprint8URL + "/issue", "POST " + sprint7URL,
	}
	if got := f.requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestCompleteSprintToTheBacklog moves the unfinished issues to the
// backlog when no sprint is named.
func TestCompleteSprintToTheBacklog(t *testing.T) {
	f, c := completionSite(t)
	f.on("POST", "/rest/agile/1.0/backlog/issue", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	f.on("POST", sprint7URL, reply(200, `{"id":7,"name":"Sprint 1","state":"closed"}`))
	if res, err := c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7}); err != nil || !res.Closed {
		t.Fatalf("got %+v %v", res, err)
	}
	if n := f.count("POST", "/rest/agile/1.0/backlog/issue"); n != 2 {
		t.Errorf("%d backlog moves", n)
	}
}

// TestCompleteSprintMoveFails proves the sprint stays open when a move
// fails partway or its outcome is unknown: nothing is closed and the
// moves say how far they got.
func TestCompleteSprintMoveFails(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		failOn  int // the move answered with status
		outcome Outcome
	}{
		"the second move refused":          {400, 1, OutcomeRefused},
		"the first move's outcome unknown": {500, 0, OutcomeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			f, c := completionSite(t)
			n := 0
			f.on("POST", sprint8URL+"/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
				if n++; n-1 == tc.failOn {
					reply(tc.status, `{"errorMessages":["no"]}`)(w, r, b)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			res, err := c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 8})
			if _, ok := errors.AsType[*BatchError](err); !ok || res.Closed || res.Sprint != nil {
				t.Fatalf("got %+v %v", res, err)
			}
			if got := res.Moves.Issues[tc.failOn*50].Outcome; got != tc.outcome {
				t.Errorf("the failed move's issues: %v", got)
			}
			if tc.failOn == 0 && res.Moves.Issues[59].Outcome != OutcomeNotSent {
				t.Errorf("the move after an unknown one: %v", res.Moves.Issues[59].Outcome)
			}
			if f.count("POST", sprint7URL) != 0 {
				t.Error("the sprint was closed")
			}
		})
	}
}

// TestCompleteSprintCloseUnreadable proves a close Jira stored whose
// answer does not decode reports the sprint closed.
func TestCompleteSprintCloseUnreadable(t *testing.T) {
	f, c := completionSite(t)
	f.on("POST", sprint8URL+"/issue", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	f.on("POST", sprint7URL, reply(200, `{"id":7,"completeDate":"later"}`))
	res, err := c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 8})
	se, ok := errors.AsType[*StepError](err)
	if !ok || !se.Applied() || !res.Closed || res.Sprint == nil || res.Sprint.ID != 7 {
		t.Fatalf("got %+v %v", res, err)
	}
}

// TestCompleteSprintRefusals proves every check runs before any write.
func TestCompleteSprintRefusals(t *testing.T) {
	cases := map[string]struct {
		in    SprintCompletion
		route func(f *fakeJira)
	}{
		"into itself":       {SprintCompletion{Sprint: 7, MoveTo: 7}, nil},
		"a negative board":  {SprintCompletion{Sprint: 7, Board: -1}, nil},
		"a future sprint":   {SprintCompletion{Sprint: 7}, func(f *fakeJira) { f.on("GET", sprint7URL, reply(200, `{"id":7,"state":"future","originBoardId":3}`)) }},
		"into a closed one": {SprintCompletion{Sprint: 7, MoveTo: 8}, func(f *fakeJira) { f.on("GET", sprint8URL, reply(200, `{"id":8,"state":"closed"}`)) }},
		"no done column": {SprintCompletion{Sprint: 7}, func(f *fakeJira) {
			f.on("GET", boardConfigURL, reply(200, `{"id":3,"columnConfig":{"columns":[{"name":"All"}]}}`))
		}},
		"a warned listing": {SprintCompletion{Sprint: 7}, func(f *fakeJira) {
			f.on("GET", sprint7IssuesURL, reply(200, `{"issues":[],"isLast":true,"warningMessages":["cut"]}`))
		}},
		"an issue no status": {SprintCompletion{Sprint: 7}, func(f *fakeJira) {
			f.on("GET", sprint7IssuesURL, reply(200, `{"issues":[{"id":"1","key":"WEB-1","fields":{}}],"isLast":true}`))
		}},
		"a listing that fails": {SprintCompletion{Sprint: 7}, func(f *fakeJira) {
			f.on("GET", sprint7IssuesURL, reply(500, `{}`))
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, c := completionSite(t)
			if tc.route != nil {
				tc.route(f)
			}
			res, err := c.CompleteSprint(t.Context(), tc.in)
			if !trackerapi.WriteNotApplied(err) || res.Closed {
				t.Fatalf("got %+v %v", res, err)
			}
			for _, r := range f.requests() {
				if !strings.HasPrefix(r, "GET ") {
					t.Errorf("a write was sent: %s", r)
				}
			}
		})
	}
}

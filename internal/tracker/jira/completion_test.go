package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	sprint8URL       = "/rest/agile/1.0/sprint/8"
	sprint7IssuesURL = "/rest/software/1.0/sprint/7/issue"
	backlogIssueURL  = "/rest/agile/1.0/backlog/issue"
)

// completionSite is sprint 7, active on board 3, with 60 issues in
// progress (WEB-1 to WEB-60, IDs 10001 to 10060) and two done (WEB-61,
// WEB-62); sprint 8 is future. The sprint's issues come in pages of 40;
// an issue a move took out, by ID, is no longer listed, and an issue in
// added is listed from the second full listing on, as one added to the
// sprint after the first, with ID 20000 and on.
type completionSite struct {
	*jiratest.Server
	t *testing.T
	c *Client

	mu       sync.Mutex
	moved    []string
	added    []string
	listings int
}

func newCompletionSite(t *testing.T) *completionSite {
	f := jiratest.NewServer(t)
	s := &completionSite{Server: f, t: t, c: newTestClient(t, f)}
	f.On("GET", sprint7URL, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3}`))
	f.On("GET", sprint8URL, jiratest.Reply(200, `{"id":8,"name":"Sprint 2","state":"future","originBoardId":3}`))
	f.On("GET", boardConfigURL, jiratest.Reply(200, boardConfigBody))
	f.On("GET", sprint7IssuesURL, s.listing)
	f.On("POST", sprint8URL+"/issue", s.move)
	f.On("POST", backlogIssueURL, s.move)
	return s
}

// listing serves one page of the sprint's issues.
func (s *completionSite) listing(w http.ResponseWriter, r *http.Request, b []byte) {
	t := s.t
	jiratest.WantQuery(t, r, "fields", "status")
	start := 0
	if tok := r.URL.Query().Get("nextPageToken"); tok != "" {
		if _, err := fmt.Sscanf(tok, "from-%d", &start); err != nil {
			t.Errorf("token %q: %v", tok, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []string
	for i := 1; i <= 62; i++ {
		key := fmt.Sprintf("WEB-%d", i)
		if slices.Contains(s.moved, fmt.Sprint(10000+i)) {
			continue
		}
		status := "3"
		if i > 60 {
			status = "10002"
		}
		all = append(all, fmt.Sprintf(`{"id":"%d","key":%q,"fields":{"status":{"id":%q}}}`, 10000+i, key, status))
	}
	if s.listings > 0 {
		for i, key := range s.added {
			all = append(all, fmt.Sprintf(`{"id":"%d","key":%q,"fields":{"status":{"id":"3"}}}`, 20000+i, key))
		}
	}
	end := min(start+40, len(all))
	next, last := fmt.Sprintf(`"from-%d"`, end), end == len(all)
	if last {
		next = "null"
		s.listings++
	}
	jiratest.Reply(200, fmt.Sprintf(`{"issues":[%s],"nextPageToken":%s,"isLast":%v}`, strings.Join(all[start:end], ","), next, last))(w, r, b)
}

// move records a move's issues, which leave the sprint, and answers 204.
func (s *completionSite) move(w http.ResponseWriter, _ *http.Request, b []byte) {
	var body batchBody
	if err := json.Unmarshal(b, &body); err != nil || len(body.Issues) == 0 || body.Before != "" || body.After != "" || body.Field != 0 {
		s.t.Errorf("a move %s", b)
	}
	s.mu.Lock()
	s.moved = append(s.moved, body.Issues...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *completionSite) movedIssues() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.moved)
}

// TestCompleteSprint is the full success: reads, then the moves of the
// unfinished issues in groups of 50, then the listing again, then the
// close, in that order.
func TestCompleteSprint(t *testing.T) {
	s := newCompletionSite(t)
	s.On("POST", sprint7URL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"state":"closed"}`)
		jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"closed","completeDate":"2026-11-16T09:00:00.000Z"}`)(w, r, b)
	})
	res, err := s.c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 8})
	if err != nil || !res.Closed || res.Sprint == nil || res.Sprint.State != SprintClosed || res.Sprint.Done.IsZero() || len(res.Remaining) != 0 {
		t.Fatalf("got %+v %v", res, err)
	}
	want60 := make([]SprintIssue, 60)
	for i := range want60 {
		want60[i] = SprintIssue{ID: fmt.Sprint(10001 + i), Key: fmt.Sprintf("WEB-%d", i+1)}
	}
	ids60 := sprintIssueIDs(want60)
	if moved := s.movedIssues(); !slices.Equal(res.Unfinished, want60) || !slices.Equal(moved, ids60) || !slices.Equal(res.Moves.Done(), ids60) {
		t.Errorf("unfinished %v, moved %v, done %v", res.Unfinished, moved, res.Moves.Done())
	}
	want := []string{
		"GET " + sprint7URL, "GET " + sprint8URL, "GET " + boardConfigURL,
		"GET " + sprint7IssuesURL, "GET " + sprint7IssuesURL,
		"POST " + sprint8URL + "/issue", "POST " + sprint8URL + "/issue",
		"GET " + sprint7IssuesURL, "POST " + sprint7URL,
	}
	if got := s.Requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestCompleteSprintToTheBacklog moves the unfinished issues to the
// backlog when no sprint is named.
func TestCompleteSprintToTheBacklog(t *testing.T) {
	s := newCompletionSite(t)
	s.On("POST", sprint7URL, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"closed"}`))
	if res, err := s.c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7}); err != nil || !res.Closed {
		t.Fatalf("got %+v %v", res, err)
	}
	if n, moved := s.Count("POST", backlogIssueURL), s.movedIssues(); n != 2 || len(moved) != 60 || moved[0] != "10001" || moved[59] != "10060" {
		t.Errorf("%d backlog moves of %v", n, moved)
	}
}

// TestCompleteSprintAddedMeanwhile proves the sprint is listed again after
// the moves: an unfinished issue that arrived after the first listing
// keeps it open and is reported, and nothing is closed.
func TestCompleteSprintAddedMeanwhile(t *testing.T) {
	s := newCompletionSite(t)
	s.added = []string{"WEB-70"}
	res, err := s.c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 8})
	se, ok := errors.AsType[*StepError](err)
	if !ok || se.Step != StepSprint || !trackerapi.WriteNotApplied(err) || res.Closed || res.Sprint != nil {
		t.Fatalf("got %+v %v", res, err)
	}
	if !slices.Equal(res.Remaining, []SprintIssue{{ID: "20000", Key: "WEB-70"}}) || len(res.Moves.Done()) != 60 {
		t.Errorf("remaining %v, moves %+v", res.Remaining, res.Moves)
	}
	if s.Count("POST", sprint7URL) != 0 {
		t.Error("the sprint was closed")
	}
	// A listing after the moves that fails keeps the sprint open too.
	s = newCompletionSite(t)
	s.On("POST", sprint8URL+"/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		s.move(w, r, b)
		s.On("GET", sprint7IssuesURL, jiratest.Reply(500, `{}`))
	})
	res, err = s.c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 8})
	if se, ok := errors.AsType[*StepError](err); !ok || se.Step != StepSprint || res.Closed || s.Count("POST", sprint7URL) != 0 {
		t.Fatalf("a failed listing after the moves: %+v %v", res, err)
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
		"the first move answered 200":      {200, 0, OutcomeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			s := newCompletionSite(t)
			n := 0
			s.On("POST", sprint8URL+"/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
				if n++; n-1 == tc.failOn {
					jiratest.Reply(tc.status, `{"errorMessages":["no"]}`)(w, r, b)
					return
				}
				s.move(w, r, b)
			})
			res, err := s.c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 8})
			if _, ok := errors.AsType[*BatchError](err); !ok || res.Closed || res.Sprint != nil {
				t.Fatalf("got %+v %v", res, err)
			}
			if got := res.Moves.Issues[tc.failOn*50].Outcome; got != tc.outcome {
				t.Errorf("the failed move's issues: %v", got)
			}
			if tc.failOn == 0 && res.Moves.Issues[59].Outcome != OutcomeNotSent {
				t.Errorf("the move after an unknown one: %v", res.Moves.Issues[59].Outcome)
			}
			if s.Count("POST", sprint7URL) != 0 {
				t.Error("the sprint was closed")
			}
		})
	}
}

// TestCompleteSprintCloseAnswers proves the close is reported only on an
// answer naming the sprint: one that names it but has a malformed date is
// closed with the date a problem, one that names it but fails to decode
// elsewhere is closed and applied, and any other is an unknown outcome
// with nothing reported closed.
func TestCompleteSprintCloseAnswers(t *testing.T) {
	for _, tc := range []struct {
		body             string
		closed, applied  bool
		err, dateProblem bool
	}{
		{`{"id":7,"state":"closed","completeDate":"later"}`, true, false, false, true},
		{`{"id":7,"name":5}`, true, true, true, false},
		{`{"completeDate":"2026-11-16T09:00:00.000Z"}`, false, false, true, false},
		{`{"id":8,"state":"closed"}`, false, false, true, false},
		{`<html>closed</html>`, false, false, true, false},
	} {
		s := newCompletionSite(t)
		s.On("POST", sprint7URL, func(w http.ResponseWriter, r *http.Request, b []byte) {
			jiratest.WantJSON(t, b, `{"state":"closed"}`)
			jiratest.Reply(200, tc.body)(w, r, b)
		})
		res, err := s.c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 8})
		if res.Closed != tc.closed || (err != nil) != tc.err || (res.Sprint != nil) != tc.closed {
			t.Errorf("%s: got %+v %v", tc.body, res, err)
			continue
		}
		if tc.closed && (res.Sprint.ID != 7 || (problemIn(res.Sprint.Problems, "completeDate") != nil) != tc.dateProblem) {
			t.Errorf("%s: the sprint %+v", tc.body, res.Sprint)
		}
		if err == nil {
			continue
		}
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepSprint || se.Applied() != tc.applied || se.OutcomeUnknown() == tc.applied {
			t.Errorf("%s: %v", tc.body, err)
		}
	}
}

// TestCompleteSprintRefusals proves every check runs before any write,
// and that only a refusal of the request is invalid input: a read of
// Jira that failed or fell short is not.
func TestCompleteSprintRefusals(t *testing.T) {
	answers := []string{"a warned listing", "an issue no status", "a listing that fails", "an issue no ID"}
	cases := map[string]struct {
		in    SprintCompletion
		route func(f *jiratest.Server)
	}{
		"into itself":      {SprintCompletion{Sprint: 7, MoveTo: 7}, nil},
		"a negative board": {SprintCompletion{Sprint: 7, Board: -1}, nil},
		"a future sprint": {SprintCompletion{Sprint: 7}, func(f *jiratest.Server) {
			f.On("GET", sprint7URL, jiratest.Reply(200, `{"id":7,"state":"future","originBoardId":3}`))
		}},
		"into a closed one": {SprintCompletion{Sprint: 7, MoveTo: 8}, func(f *jiratest.Server) { f.On("GET", sprint8URL, jiratest.Reply(200, `{"id":8,"state":"closed"}`)) }},
		"no done column": {SprintCompletion{Sprint: 7}, func(f *jiratest.Server) {
			f.On("GET", boardConfigURL, jiratest.Reply(200, `{"id":3,"columnConfig":{"columns":[{"name":"All"}]}}`))
		}},
		"a warned listing": {SprintCompletion{Sprint: 7}, func(f *jiratest.Server) {
			f.On("GET", sprint7IssuesURL, jiratest.Reply(200, `{"issues":[],"isLast":true,"warningMessages":["cut"]}`))
		}},
		"an issue no status": {SprintCompletion{Sprint: 7}, func(f *jiratest.Server) {
			f.On("GET", sprint7IssuesURL, jiratest.Reply(200, `{"issues":[{"id":"1","key":"WEB-1","fields":{}}],"isLast":true}`))
		}},
		"a listing that fails": {SprintCompletion{Sprint: 7}, func(f *jiratest.Server) {
			f.On("GET", sprint7IssuesURL, jiratest.Reply(500, `{}`))
		}},
		"an issue no ID": {SprintCompletion{Sprint: 7}, func(f *jiratest.Server) {
			f.On("GET", sprint7IssuesURL, jiratest.Reply(200, `{"issues":[{"key":"WEB-1","fields":{"status":{"id":"3"}}}],"isLast":true}`))
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newCompletionSite(t)
			if tc.route != nil {
				tc.route(s.Server)
			}
			res, err := s.c.CompleteSprint(t.Context(), tc.in)
			if !trackerapi.WriteNotApplied(err) || res.Closed {
				t.Fatalf("got %+v %v", res, err)
			}
			if invalid := errors.Is(err, trackerapi.ErrInvalidInput); invalid == slices.Contains(answers, name) {
				t.Errorf("invalid input %v: %v", invalid, err)
			}
			for _, r := range s.Requests() {
				if !strings.HasPrefix(r, "GET ") {
					t.Errorf("a write was sent: %s", r)
				}
			}
		})
	}
}

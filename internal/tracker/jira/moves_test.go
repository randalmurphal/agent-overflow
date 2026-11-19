package jira

import (
	"errors"
	"net/http"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// TestMoveToBacklog proves the plain move takes no rank fields and the
// board's backlog takes them.
func TestMoveToBacklog(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("POST", "/rest/agile/1.0/backlog/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["WEB-1","WEB-2"]}`)
		w.WriteHeader(http.StatusNoContent)
	})
	f.On("POST", "/rest/agile/1.0/backlog/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["WEB-1"],"rankBeforeIssue":"WEB-9","rankCustomFieldId":10019}`)
		w.WriteHeader(http.StatusNoContent)
	})
	if res, err := c.MoveToBacklog(t.Context(), BacklogMove{Issues: []string{"WEB-1", "WEB-2"}}); err != nil || len(res.Done()) != 2 {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, err := c.MoveToBacklog(t.Context(), BacklogMove{Board: 3, Issues: []string{"WEB-1"}, Position: RankBefore("WEB-9"), RankFieldID: 10019}); err != nil {
		t.Fatal(err)
	}
}

// TestMoveAnswers proves which answers each move takes: a sprint move and
// the plain backlog move are documented to answer only 204, so a 207 or
// a 200 leaves them unknown; the board's backlog takes a 207 too.
func TestMoveAnswers(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/agile/1.0/sprint/7", jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3}`))
	both := []string{"WEB-1", "WEB-2"}
	full := rankEntries(both, rankEntry("WEB-2", 204))
	moves := map[string]func() (BatchResult, error){
		"/rest/agile/1.0/sprint/7/issue": func() (BatchResult, error) {
			return c.MoveToSprint(t.Context(), SprintMove{Sprint: 7, Issues: both})
		},
		"/rest/agile/1.0/backlog/issue": func() (BatchResult, error) {
			return c.MoveToBacklog(t.Context(), BacklogMove{Issues: both})
		},
	}
	for _, status := range []int{207, 200} {
		for path, move := range moves {
			f.On("POST", path, func(w http.ResponseWriter, r *http.Request, b []byte) {
				jiratest.WantJSON(t, b, `{"issues":["WEB-1","WEB-2"]}`)
				jiratest.Reply(status, full)(w, r, b)
			})
			res, err := move()
			if be, ok := errors.AsType[*BatchError](err); !ok || be.Unknown != 2 || len(res.Done()) != 0 || trackerapi.WriteNotApplied(err) {
				t.Errorf("%s answered %d: %+v %v", path, status, res, err)
			}
		}
	}
	f.On("POST", "/rest/agile/1.0/backlog/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["WEB-1","WEB-2"]}`)
		jiratest.Reply(207, rankEntries(both, rankEntry("WEB-2", 400)))(w, r, b)
	})
	res, err := c.MoveToBacklog(t.Context(), BacklogMove{Board: 3, Issues: both})
	if be, ok := errors.AsType[*BatchError](err); !ok || be.Refused != 1 || be.Unknown != 0 || res.Issues[0].Outcome != OutcomeDone {
		t.Errorf("a board backlog 207: %+v %v", res, err)
	}
}

// TestMoveToSprint proves the sprint is read first, a closed one refused
// before any move, and an open one moved into.
func TestMoveToSprint(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/agile/1.0/sprint/7", jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3}`))
	f.On("GET", "/rest/agile/1.0/sprint/6", jiratest.Reply(200, `{"id":6,"name":"Sprint 0","state":"closed","originBoardId":3}`))
	f.On("POST", "/rest/agile/1.0/sprint/7/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issues":["WEB-1"],"rankAfterIssue":"WEB-9"}`)
		w.WriteHeader(http.StatusNoContent)
	})
	if res, err := c.MoveToSprint(t.Context(), SprintMove{Sprint: 7, Issues: []string{"WEB-1"}, Position: RankAfter("WEB-9")}); err != nil || len(res.Done()) != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, err := c.MoveToSprint(t.Context(), SprintMove{Sprint: 6, Issues: []string{"WEB-1"}}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a closed sprint: %v", err)
	}
	if n := f.Count("POST", "/rest/agile/1.0/sprint/6/issue"); n != 0 {
		t.Errorf("%d moves into a closed sprint", n)
	}
}

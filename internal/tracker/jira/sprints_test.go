package jira

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	sprint7URL  = "/rest/agile/1.0/sprint/7"
	sprint7JSON = `{"id":7,"name":"Sprint 1","state":"future","originBoardId":3,"goal":"Ship"}`
)

func TestSprints(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/agile/1.0/board/3/sprint", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "state", "active,future")
		jiratest.Reply(200, `{"isLast":true,"values":[
			{"id":7,"name":"Sprint 1","state":"active","originBoardId":3,"startDate":"2026-11-02T09:00:00.000+01:00","endDate":"2026-11-16T09:00:00.000+01:00"},
			{"id":8,"name":"Sprint 2","state":"future","originBoardId":3}]}`)(w, r, b)
	})
	list, err := c.Sprints(t.Context(), SprintQuery{Board: 3, States: []SprintState{SprintActive, SprintFuture}})
	if err != nil || !list.Complete || len(list.Sprints) != 2 {
		t.Fatalf("got %+v %v", list, err)
	}
	s := list.Sprints[0]
	if s.State != SprintActive || s.BoardID != 3 || !s.Start.Equal(time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)) || s.End.IsZero() {
		t.Errorf("got %+v", s)
	}
	for name, q := range map[string]SprintQuery{
		"a state":   {Board: 3, States: []SprintState{"open"}},
		"no board":  {},
		"a limit":   {Board: 3, Limit: -1},
		"a board 0": {Board: 0, States: []SprintState{SprintActive}},
	} {
		if _, err := c.Sprints(t.Context(), q); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A date that does not parse is a problem of that sprint, which stays
	// listed with its other dates.
	f.On("GET", "/rest/agile/1.0/board/4/sprint", jiratest.Reply(200, `{"isLast":true,"values":[{"id":9,"name":"x","state":"active","startDate":"soon","endDate":"2026-11-16T09:00:00.000Z","completeDate":""}]}`))
	list, err = c.Sprints(t.Context(), SprintQuery{Board: 4})
	if err != nil || len(list.Sprints) != 1 {
		t.Fatalf("a date that does not parse: %+v %v", list, err)
	}
	if s := list.Sprints[0]; s.ID != 9 || len(s.Problems) != 1 || problemIn(s.Problems, "startDate") == nil || s.End.IsZero() || !s.Start.IsZero() {
		t.Errorf("a date that does not parse: %+v", s)
	}
}

func TestCreateSprint(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	start := time.Date(2026, 11, 16, 9, 0, 0, 0, time.FixedZone("", 3600))
	f.On("POST", "/rest/agile/1.0/sprint", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"name":"Sprint 1","originBoardId":3,"goal":"Ship","startDate":"2026-11-16T09:00:00.000+01:00","endDate":"2026-11-30T09:00:00.000+01:00"}`)
		jiratest.Reply(201, sprint7JSON)(w, r, b)
	})
	s, err := c.CreateSprint(t.Context(), SprintInput{Board: 3, Name: "Sprint 1", Goal: "Ship", Start: start, End: start.Add(14 * 24 * time.Hour)})
	if err != nil || s.ID != 7 || s.State != SprintFuture {
		t.Fatalf("got %+v %v", s, err)
	}
	for name, in := range map[string]SprintInput{
		"no board":         {Name: "x"},
		"no name":          {Board: 3, Name: " "},
		"end before start": {Board: 3, Name: "x", Start: start, End: start},
	} {
		if _, err := c.CreateSprint(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := f.Count("POST", "/rest/agile/1.0/sprint"); n != 1 {
		t.Errorf("%d creates", n)
	}
}

// TestSprintWriteAnswers proves a sprint write is reported only on an
// answer naming the sprint: a malformed date is a problem of a stored
// sprint, an answer naming it that fails to decode elsewhere is applied
// with its ID, and one naming no sprint, or not JSON, or with another
// status, is an unknown outcome that reports no sprint.
func TestSprintWriteAnswers(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		id      int64
		applied bool
		err     bool
	}{
		{201, `{"id":7,"name":"Sprint 1","startDate":"soon"}`, 7, false, false},
		{201, `{"id":"7","name":5}`, 7, true, true},
		{201, `{"name":"Sprint 1"}`, 0, false, true},
		{201, `{"id":"7x","name":"Sprint 1"}`, 0, false, true},
		{201, `not json`, 0, false, true},
		{200, `{"id":7,"name":"Sprint 1"}`, 0, false, true},
	} {
		f := jiratest.NewServer(t)
		c := newTestClient(t, f)
		f.On("POST", "/rest/agile/1.0/sprint", func(w http.ResponseWriter, r *http.Request, b []byte) {
			jiratest.WantJSON(t, b, `{"name":"Sprint 1","originBoardId":3}`)
			jiratest.Reply(tc.status, tc.body)(w, r, b)
		})
		s, err := c.CreateSprint(t.Context(), SprintInput{Board: 3, Name: "Sprint 1"})
		if s.ID != tc.id || (err != nil) != tc.err {
			t.Errorf("%d %s: got %+v %v", tc.status, tc.body, s, err)
			continue
		}
		if !tc.err {
			if problemIn(s.Problems, "startDate") == nil {
				t.Errorf("%s: no problem for the date: %+v", tc.body, s)
			}
			continue
		}
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepSprint || se.Applied() != tc.applied || se.OutcomeUnknown() == tc.applied {
			t.Errorf("%d %s: %v", tc.status, tc.body, err)
		}
	}
	// An update's answer must name the sprint updated.
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", sprint7URL, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active"}`))
	f.On("POST", sprint7URL, jiratest.Reply(200, `{"id":8,"name":"Sprint A","state":"active"}`))
	name := "Sprint A"
	s, err := c.UpdateSprint(t.Context(), SprintUpdate{ID: 7, Name: &name})
	if se, ok := errors.AsType[*StepError](err); !ok || se.Applied() || !se.OutcomeUnknown() || s.ID != 0 {
		t.Errorf("an answer naming another sprint: %+v %v", s, err)
	}
}

func TestStartSprint(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	start := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	f.On("GET", sprint7URL, jiratest.Reply(200, sprint7JSON))
	f.On("POST", sprint7URL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"state":"active","startDate":"2026-11-16T09:00:00.000Z","endDate":"2026-11-30T09:00:00.000Z"}`)
		jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active"}`)(w, r, b)
	})
	if _, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("no end date: %v", err)
	}
	s, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start, End: start.Add(14 * 24 * time.Hour)})
	if err != nil || s.State != SprintActive {
		t.Fatalf("got %+v %v", s, err)
	}
	f.On("GET", sprint7URL, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active"}`))
	if _, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start, End: start.Add(time.Hour)}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("an active sprint: %v", err)
	}
	if n := f.Count("POST", sprint7URL); n != 1 {
		t.Errorf("%d writes", n)
	}
}

func TestUpdateSprint(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", sprint7URL, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"active","startDate":"2026-11-16T09:00:00.000Z","endDate":"2026-11-30T09:00:00.000Z"}`))
	f.On("POST", sprint7URL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"name":"Sprint A","endDate":"2026-12-01T09:00:00.000Z"}`)
		jiratest.Reply(200, `{"id":7,"name":"Sprint A","state":"active"}`)(w, r, b)
	})
	name := "Sprint A"
	end := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	if s, err := c.UpdateSprint(t.Context(), SprintUpdate{ID: 7, Name: &name, End: &end}); err != nil || s.Name != "Sprint A" {
		t.Fatalf("got %+v %v", s, err)
	}
	early := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	empty := " "
	for what, in := range map[string]SprintUpdate{
		"an end before the stored start": {ID: 7, End: &early},
		"nothing":                        {ID: 7},
		"an empty name":                  {ID: 7, Name: &empty},
	} {
		if _, err := c.UpdateSprint(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", what, err)
		}
	}
	f.On("GET", sprint7URL, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"closed"}`))
	if _, err := c.UpdateSprint(t.Context(), SprintUpdate{ID: 7, End: &end}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a closed sprint's dates: %v", err)
	}
	if n := f.Count("POST", sprint7URL); n != 1 {
		t.Errorf("%d writes", n)
	}
}

func TestDeleteSprint(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("DELETE", sprint7URL, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	if err := c.DeleteSprint(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSprint(t.Context(), -7); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a bad ID: %v", err)
	}
}

// sprintBoards is a site whose project WEB has scrum boards 3 and 4,
// sharing sprint 7, and whose issue WEB-1 is in WEB.
func sprintBoards(t *testing.T, f *jiratest.Server) {
	f.On("GET", boardsURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "projectKeyOrId", "WEB")
		jiratest.WantQuery(t, r, "type", "scrum")
		jiratest.Reply(200, `{"isLast":true,"values":[{"id":3,"name":"A","type":"scrum"},{"id":4,"name":"B","type":"scrum"}]}`)(w, r, b)
	})
	f.On("GET", "/rest/agile/1.0/board/3/sprint", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "state", "active,future")
		jiratest.Reply(200, `{"isLast":true,"values":[{"id":7,"name":"Sprint 1","state":"active"},{"id":8,"name":"Sprint 2","state":"future"}]}`)(w, r, b)
	})
	f.On("GET", "/rest/agile/1.0/board/4/sprint", jiratest.Reply(200, `{"isLast":true,"values":[{"id":7,"name":"Sprint 1","state":"active"},{"id":9,"name":"sprint 2","state":"future"},{"id":10,"name":"Next","state":"future"},{"id":11,"name":"Next","state":"future"}]}`))
	f.On("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "fields", "project")
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","name":"Web"}}}`)(w, r, b)
	})
}

// TestSprintByName resolves a sprint named on a value: one match (a
// sprint two boards share counts once), several, none.
func TestSprintByName(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	sprintBoards(t, f)
	v, err := c.Value(t.Context(), sprintField, Text("Sprint 1"), ValueOptions{Issue: "WEB-1"})
	if err != nil || string(v.Fields) != `7` || v.Display != "Sprint 1 (sprint 7)" {
		t.Fatalf("one match: %s %v", v.Fields, err)
	}
	// "sprint 2" spelled exactly wins over "Sprint 2".
	if v, err := c.Value(t.Context(), sprintField, Text("sprint 2"), ValueOptions{Project: "WEB"}); err != nil || string(v.Fields) != `9` {
		t.Errorf("exact spelling: %s %v", v.Fields, err)
	}
	for name, want := range map[string]int{"Next": 2, "Sprint 9": 5} {
		_, err := c.Value(t.Context(), sprintField, Text(name), ValueOptions{Project: "WEB"})
		me, ok := errors.AsType[*MatchError](err)
		if !ok || len(me.Candidates) != want || me.Ambiguous != (want == 2) || !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if v, err := c.Value(t.Context(), sprintField, Text("12"), ValueOptions{}); err != nil || string(v.Fields) != `12` {
		t.Errorf("by ID: %s %v", v.Fields, err)
	}
	if _, err := c.Value(t.Context(), sprintField, Text("Sprint 1"), ValueOptions{}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a name with no project: %v", err)
	}
}

// TestSprintTakenAsRead proves a sprint the caller read (Current) replaces
// the client's own read of it: the checks run on it, no read is sent, and
// a sprint given as read under another ID is refused before any request.
func TestSprintTakenAsRead(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("POST", sprint7URL, jiratest.Reply(200, `{"id":7,"name":"Sprint A","state":"active"}`))
	f.On("POST", sprint7URL+"/issue", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	start := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	end := start.Add(14 * 24 * time.Hour)
	future := &Sprint{ID: 7, Name: "Sprint 1", State: SprintFuture}
	closed := &Sprint{ID: 7, Name: "Sprint 1", State: SprintClosed}
	other := &Sprint{ID: 8, Name: "Sprint 2", State: SprintFuture}
	name := "Sprint A"
	if _, err := c.UpdateSprint(t.Context(), SprintUpdate{ID: 7, Name: &name, Current: future}); err != nil {
		t.Errorf("update: %v", err)
	}
	if _, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start, End: end, Current: future}); err != nil {
		t.Errorf("start: %v", err)
	}
	if _, err := c.MoveToSprint(t.Context(), SprintMove{Sprint: 7, Issues: []string{"WEB-1"}, Current: future}); err != nil {
		t.Errorf("move: %v", err)
	}
	// The checks run on the sprint given: a closed one is refused.
	if _, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start, End: end, Current: closed}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("start a closed sprint: %v", err)
	}
	if _, err := c.MoveToSprint(t.Context(), SprintMove{Sprint: 7, Issues: []string{"WEB-1"}, Current: closed}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("move into a closed sprint: %v", err)
	}
	active := &Sprint{ID: 7, State: SprintActive}
	for what, call := range map[string]func() error{
		"update": func() error {
			_, err := c.UpdateSprint(t.Context(), SprintUpdate{ID: 7, Name: &name, Current: other})
			return err
		},
		"start": func() error {
			_, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start, End: end, Current: other})
			return err
		},
		"move": func() error {
			_, err := c.MoveToSprint(t.Context(), SprintMove{Sprint: 7, Issues: []string{"WEB-1"}, Current: other})
			return err
		},
		"complete": func() error {
			_, err := c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, Current: other})
			return err
		},
		"complete to": func() error {
			_, err := c.CompleteSprint(t.Context(), SprintCompletion{Sprint: 7, MoveTo: 9, Current: active, MoveToCurrent: other})
			return err
		},
	} {
		if err := call(); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s with a sprint of another ID: %v", what, err)
		}
	}
	want := []string{"POST " + sprint7URL, "POST " + sprint7URL, "POST " + sprint7URL + "/issue"}
	if got := f.Requests(); !slices.Equal(got, want) {
		t.Errorf("requests %v, want %v", got, want)
	}
}

// TestCompleteSprintTakenAsRead proves a completion given both sprints as
// read sends neither read, its requests starting at the board's done
// column, and that a destination given as closed is refused before any
// request.
func TestCompleteSprintTakenAsRead(t *testing.T) {
	s := newCompletionSite(t)
	s.On("POST", sprint7URL, jiratest.Reply(200, `{"id":7,"name":"Sprint 1","state":"closed"}`))
	in := SprintCompletion{
		Sprint: 7, MoveTo: 8,
		Current:       &Sprint{ID: 7, State: SprintActive, BoardID: 3},
		MoveToCurrent: &Sprint{ID: 8, State: SprintFuture, BoardID: 3},
	}
	res, err := s.c.CompleteSprint(t.Context(), in)
	if err != nil || !res.Closed {
		t.Fatalf("got %+v %v", res, err)
	}
	if got := s.Requests(); got[0] != "GET "+boardConfigURL || s.Count("GET", sprint7URL) != 0 || s.Count("GET", sprint8URL) != 0 {
		t.Errorf("requests %v", got)
	}
	s = newCompletionSite(t)
	in.MoveToCurrent = &Sprint{ID: 8, State: SprintClosed, BoardID: 3}
	if _, err := s.c.CompleteSprint(t.Context(), in); !trackerapi.WriteNotApplied(err) || len(s.Requests()) != 0 {
		t.Errorf("a closed destination: %v, requests %v", err, s.Requests())
	}
}

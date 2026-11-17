package jira

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

const (
	sprint7URL  = "/rest/agile/1.0/sprint/7"
	sprint7JSON = `{"id":7,"name":"Sprint 1","state":"future","originBoardId":3,"goal":"Ship"}`
)

func TestSprints(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/agile/1.0/board/3/sprint", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "state", "active,future")
		reply(200, `{"isLast":true,"values":[
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
	f.on("GET", "/rest/agile/1.0/board/4/sprint", reply(200, `{"isLast":true,"values":[{"id":9,"name":"x","state":"active","startDate":"soon"}]}`))
	if _, err := c.Sprints(t.Context(), SprintQuery{Board: 4}); !isAnswerErr(err) {
		t.Errorf("a date that does not parse: %v", err)
	}
}

func TestCreateSprint(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	start := time.Date(2026, 11, 16, 9, 0, 0, 0, time.FixedZone("", 3600))
	f.on("POST", "/rest/agile/1.0/sprint", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"name":"Sprint 1","originBoardId":3,"goal":"Ship","startDate":"2026-11-16T09:00:00.000+01:00","endDate":"2026-11-30T09:00:00.000+01:00"}`)
		reply(201, sprint7JSON)(w, r, b)
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
	if n := f.count("POST", "/rest/agile/1.0/sprint"); n != 1 {
		t.Errorf("%d creates", n)
	}
}

// TestSprintWriteUnreadableAnswer proves a sprint written whose answer
// does not decode still names the sprint by ID, as applied.
func TestSprintWriteUnreadableAnswer(t *testing.T) {
	for body, wantID := range map[string]int64{
		`{"id":7,"name":"Sprint 1","startDate":"soon"}`: 7,
		`{"id":"7","name":5}`:                           7,
		`{"name":"Sprint 1"}`:                           0,
		`not json`:                                      0,
	} {
		f := newFake(t)
		c := newTestClient(t, f)
		f.on("POST", "/rest/agile/1.0/sprint", reply(201, body))
		s, err := c.CreateSprint(t.Context(), SprintInput{Board: 3, Name: "Sprint 1"})
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepSprint || !se.Applied() || s.ID != wantID {
			t.Errorf("%s: got %+v %v", body, s, err)
		}
	}
}

func TestStartSprint(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	start := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	f.on("GET", sprint7URL, reply(200, sprint7JSON))
	f.on("POST", sprint7URL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"state":"active","startDate":"2026-11-16T09:00:00.000Z","endDate":"2026-11-30T09:00:00.000Z"}`)
		reply(200, `{"id":7,"name":"Sprint 1","state":"active"}`)(w, r, b)
	})
	if _, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("no end date: %v", err)
	}
	s, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start, End: start.Add(14 * 24 * time.Hour)})
	if err != nil || s.State != SprintActive {
		t.Fatalf("got %+v %v", s, err)
	}
	f.on("GET", sprint7URL, reply(200, `{"id":7,"name":"Sprint 1","state":"active"}`))
	if _, err := c.StartSprint(t.Context(), SprintStart{ID: 7, Start: start, End: start.Add(time.Hour)}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("an active sprint: %v", err)
	}
	if n := f.count("POST", sprint7URL); n != 1 {
		t.Errorf("%d writes", n)
	}
}

func TestUpdateSprint(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", sprint7URL, reply(200, `{"id":7,"name":"Sprint 1","state":"active","startDate":"2026-11-16T09:00:00.000Z","endDate":"2026-11-30T09:00:00.000Z"}`))
	f.on("POST", sprint7URL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"name":"Sprint A","endDate":"2026-12-01T09:00:00.000Z"}`)
		reply(200, `{"id":7,"name":"Sprint A","state":"active"}`)(w, r, b)
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
	f.on("GET", sprint7URL, reply(200, `{"id":7,"name":"Sprint 1","state":"closed"}`))
	if _, err := c.UpdateSprint(t.Context(), SprintUpdate{ID: 7, End: &end}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a closed sprint's dates: %v", err)
	}
	if n := f.count("POST", sprint7URL); n != 1 {
		t.Errorf("%d writes", n)
	}
}

func TestDeleteSprint(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("DELETE", sprint7URL, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	if err := c.DeleteSprint(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSprint(t.Context(), -7); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a bad ID: %v", err)
	}
}

// sprintBoards is a site whose project WEB has scrum boards 3 and 4,
// sharing sprint 7, and whose issue WEB-1 is in WEB.
func sprintBoards(t *testing.T, f *fakeJira) {
	f.on("GET", boardsURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "projectKeyOrId", "WEB")
		wantQuery(t, r, "type", "scrum")
		reply(200, `{"isLast":true,"values":[{"id":3,"name":"A","type":"scrum"},{"id":4,"name":"B","type":"scrum"}]}`)(w, r, b)
	})
	f.on("GET", "/rest/agile/1.0/board/3/sprint", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "state", "active,future")
		reply(200, `{"isLast":true,"values":[{"id":7,"name":"Sprint 1","state":"active"},{"id":8,"name":"Sprint 2","state":"future"}]}`)(w, r, b)
	})
	f.on("GET", "/rest/agile/1.0/board/4/sprint", reply(200, `{"isLast":true,"values":[{"id":7,"name":"Sprint 1","state":"active"},{"id":9,"name":"sprint 2","state":"future"},{"id":10,"name":"Next","state":"future"},{"id":11,"name":"Next","state":"future"}]}`))
	f.on("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "fields", "project")
		reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","name":"Web"}}}`)(w, r, b)
	})
}

// TestSprintByName resolves a sprint named on a value: one match (a
// sprint two boards share counts once), several, none.
func TestSprintByName(t *testing.T) {
	f := newFake(t)
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

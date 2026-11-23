package jira

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	boardsURL      = "/rest/agile/1.0/board"
	boardConfigURL = "/rest/agile/1.0/board/3/configuration"
	// boardConfigJSON is a scrum board estimating in story points whose
	// last column has no status mapped: Done is the column before it.
	boardConfigBody = `{"id":3,"name":"WEB board","type":"scrum","filter":{"id":"10010"},
		"columnConfig":{"columns":[
			{"name":"To Do","statuses":[{"id":"1"}]},
			{"name":"In Progress","statuses":[{"id":"3"}],"max":5},
			{"name":"Done","statuses":[{"id":"10001"},{"id":"10002"}]},
			{"name":"Archive","statuses":[]}],
		"constraintType":"issueCount"},
		"estimation":{"type":"field","field":{"fieldId":"customfield_10016","displayName":"Story Points"}},
		"ranking":{"rankCustomFieldId":10019}}`
)

// boardPages answers the board listing with n boards, two a page.
func boardPages(t *testing.T, n int) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "maxResults", "50")
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		var values string
		for i := start; i < min(start+2, n); i++ {
			if values != "" {
				values += ","
			}
			values += fmt.Sprintf(`{"id":%d,"name":"Board %d","type":"scrum","location":{"projectId":10000,"projectKey":"WEB"}}`, i+1, i+1)
		}
		jiratest.Reply(200, fmt.Sprintf(`{"startAt":%d,"total":%d,"isLast":%v,"values":[%s]}`, start, n, start+2 >= n, values))(w, r, b)
	}
}

func TestBoards(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	pages := boardPages(t, 5)
	f.On("GET", boardsURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "projectKeyOrId", "WEB")
		jiratest.WantQuery(t, r, "type", "scrum,kanban")
		jiratest.WantQuery(t, r, "name", "Board")
		pages(w, r, b)
	})
	q := BoardQuery{Project: "WEB", Types: []BoardType{BoardScrum, BoardKanban}, Name: "Board"}
	all, err := c.Boards(t.Context(), q)
	if err != nil || !all.Complete || len(all.Boards) != 5 || all.Boards[4].ID != 5 || all.Boards[0].Location.ProjectKey != "WEB" {
		t.Fatalf("got %+v %v", all, err)
	}
	q.Limit = 3
	some, err := c.Boards(t.Context(), q)
	if err != nil || some.Complete || len(some.Boards) != 3 {
		t.Fatalf("limited: %+v %v", some, err)
	}
	before := len(f.Requests())
	for name, bad := range map[string]BoardQuery{
		"a type":     {Types: []BoardType{"Scrum"}},
		"a limit":    {Limit: -1},
		"a project":  {Project: "W/B"},
		"no project": {Project: " "},
	} {
		if _, err := c.Boards(t.Context(), bad); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.Requests()) != before {
		t.Error("a refused listing was sent")
	}
}

func TestBoardConfig(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", boardConfigURL, jiratest.Reply(200, boardConfigBody))
	cfg, err := c.BoardConfig(t.Context(), 3)
	if err != nil {
		t.Fatal(err)
	}
	want := Estimation{Type: EstimateField, FieldID: "customfield_10016", FieldName: "Story Points", Custom: true}
	if cfg.Estimation != want || cfg.RankFieldID != 10019 || cfg.FilterID != "10010" || cfg.Constraint != ConstraintIssueCount || len(cfg.Columns) != 4 || cfg.Columns[1].Max != 5 {
		t.Fatalf("got %+v", cfg)
	}
	if done := cfg.DoneStatusIDs(); !slices.Equal(done, []string{"10001", "10002"}) {
		t.Errorf("done statuses %v", done)
	}
	// The caller's copy is its own: the cache answers the next read whole.
	cfg.DoneStatusIDs()[0] = "changed"
	if cfg.Columns[2].StatusIDs[0] != "10001" {
		t.Error("DoneStatusIDs shares the column's statuses")
	}
	cfg.Columns[2].StatusIDs[0] = "changed"
	again, err := c.BoardConfig(t.Context(), 3)
	if err != nil || again.Columns[2].StatusIDs[0] != "10001" || again.DoneStatusIDs()[0] != "10001" {
		t.Fatalf("a cached configuration changed: %+v %v", again, err)
	}
	if n := f.Count("GET", boardConfigURL); n != 1 {
		t.Errorf("%d reads, want 1", n)
	}
	c.ForgetBoard(3)
	if _, err := c.BoardConfig(t.Context(), 3); err != nil || f.Count("GET", boardConfigURL) != 2 {
		t.Errorf("after ForgetBoard: %v", err)
	}
	if _, err := c.BoardConfig(t.Context(), 0); !trackerapi.WriteNotApplied(err) {
		t.Errorf("board 0: %v", err)
	}
}

// TestBoardConfigReadInFlightIsFenced proves a configuration read that
// was in flight when ForgetBoard dropped the cache is not kept.
func TestBoardConfigReadInFlightIsFenced(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", boardConfigURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if f.Count("GET", boardConfigURL) == 1 {
			c.ForgetBoard(3)
		}
		jiratest.Reply(200, boardConfigBody)(w, r, b)
	})
	for range 2 {
		if _, err := c.BoardConfig(t.Context(), 3); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.Count("GET", boardConfigURL); n != 2 {
		t.Errorf("%d reads, want 2: the read in flight was kept", n)
	}
}

func TestBoardEstimate(t *testing.T) {
	cfg := BoardConfig{BoardID: 3, Estimation: Estimation{Type: EstimateField, FieldID: "customfield_10016"}}
	is := &Issue{Key: "WEB-1", Raw: map[string]json.RawMessage{"customfield_10016": json.RawMessage(`5.5`)}}
	if v, set, err := cfg.Estimate(is); err != nil || !set || v != 5.5 {
		t.Errorf("got %v %v %v", v, set, err)
	}
	if _, set, err := cfg.Estimate(&Issue{Key: "WEB-2", Raw: map[string]json.RawMessage{"customfield_10016": json.RawMessage(`null`)}}); err != nil || set {
		t.Errorf("no estimate: %v %v", set, err)
	}
	if _, _, err := (BoardConfig{Estimation: Estimation{Type: EstimateIssueCount}}).Estimate(is); err == nil {
		t.Error("a board estimating by issue count")
	}
}

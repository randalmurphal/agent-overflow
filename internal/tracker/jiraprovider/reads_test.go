package jiraprovider

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

func TestNewChecksOptions(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newJira(t, f)
	for _, site := range []string{"", "example.atlassian.net", "ftp://example.atlassian.net", "https://example.atlassian.net/jira", "https://u@example.atlassian.net", "https://example.atlassian.net?x=1"} {
		if _, err := New(Options{Jira: c, Site: site, SprintsFrom: allSprints}); err == nil {
			t.Errorf("site %q was taken", site)
		}
	}
	if _, err := New(Options{Site: "https://example.atlassian.net"}); err == nil {
		t.Error("a Provider without a Jira client")
	}
	p, err := New(Options{Jira: c, Site: "https://example.atlassian.net/", SprintsFrom: allSprints})
	if err != nil || p.origin != "https://example.atlassian.net" {
		t.Fatalf("got %v %v", p, err)
	}
	if _, ok := p.Time().(*jiraTime); !ok {
		t.Errorf("without Tempo, Time is %T", p.Time())
	}
	if c := p.Capabilities(); !c.IssueTypes || !c.CustomFields || !c.MoveFields || !c.SprintWrites || !c.Attachments || !c.DetailMovesUpdated {
		t.Errorf("capabilities %+v", c)
	}
}

func TestDirectoryReads(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", "/rest/api/3/project/search", jiratest.Reply(200, `{"isLast":true,"values":[
		{"id":"10000","key":"WEB","name":"Web","projectTypeKey":"software"},
		{"id":"10001","key":"APP","name":"App","projectTypeKey":"service_desk"}]}`))
	f.On("GET", "/rest/api/3/issue/createmeta/10000/issuetypes", jiratest.Reply(200, `{"issueTypes":[{"id":"10002","name":"Story","hierarchyLevel":0},{"id":"10003","name":"Epic","hierarchyLevel":1}],"total":2}`))
	f.On("GET", "/rest/agile/1.0/board", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "projectKeyOrId", "10000")
		jiratest.Reply(200, `{"isLast":true,"values":[
			{"id":3,"name":"Board 3","type":"scrum","location":{"projectId":10000,"projectKey":"WEB"}},
			{"id":5,"name":"Board 5","type":"kanban","location":{"userId":"acc-user1"}}]}`)(w, r, b)
	})
	f.On("GET", "/rest/api/3/issueLinkType", jiratest.Reply(200, `{"issueLinkTypes":[{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"}]}`))

	me, err := p.Identity(t.Context())
	if err != nil || me != (tracker.User{ID: "acc-user1", Name: "User 1", Email: "user1@example.com", Active: true}) {
		t.Errorf("identity %+v %v", me, err)
	}
	projects, err := p.Projects(t.Context())
	if err != nil || len(projects) != 2 || projects[0].ChoosesAudience || !projects[1].ChoosesAudience || projects[1].ID != "10001" {
		t.Errorf("projects %+v %v", projects, err)
	}
	types, err := p.IssueTypes(t.Context(), "10000")
	if err != nil || !slices.Equal(types, []tracker.IssueType{{ID: "10002", Name: "Story"}, {ID: "10003", Name: "Epic", Level: 1}}) {
		t.Errorf("types %+v %v", types, err)
	}
	boards, err := p.Boards(t.Context(), "10000")
	if err != nil || !slices.Equal(boards, []tracker.Board{{ID: "3", Name: "Board 3", Project: "10000", Sprints: true}, {ID: "5", Name: "Board 5"}}) {
		t.Errorf("boards %+v %v", boards, err)
	}
	links, err := p.LinkTypes(t.Context())
	if err != nil || !slices.Equal(links, []tracker.LinkType{{ID: "10000", Name: "Blocks", Outward: "blocks", Inward: "is blocked by"}}) {
		t.Errorf("link types %+v %v", links, err)
	}
	for _, bad := range []tracker.ProjectID{"WEB", "", "01", "-1"} {
		if _, err := p.Boards(t.Context(), bad); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("project %q: %v", bad, err)
		}
	}
}

// structureSite is newSite with board 3's details and sprints and WEB's
// statuses, one of which is in a category Jira does not document.
func structureSite(t *testing.T) *jiratest.Server {
	f, _ := newSite(t)
	f.On("GET", board3Config, jiratest.Reply(200, boardConfigIn(3, "customfield_10016", `{"type":"project","id":"10000","key":"WEB","name":"Web","self":"x"}`)))
	f.On("GET", "/rest/agile/1.0/board/3/sprint", jiratest.Reply(200, `{"isLast":true,"values":[
		{"id":5,"name":"Old","state":"closed","originBoardId":3,"startDate":"2026-09-01T09:00:00.000Z","endDate":"2026-09-15T09:00:00.000Z","completeDate":"2026-09-15T10:00:00.000Z"},
		{"id":6,"name":"Recent","state":"closed","originBoardId":3,"startDate":"2026-10-20T09:00:00.000Z","endDate":"2026-11-03T09:00:00.000Z","completeDate":"2026-11-03T10:00:00.000Z"},
		{"id":7,"name":"Sprint 1","state":"active","originBoardId":3,"goal":"Ship","startDate":"2026-11-03T09:00:00.000Z","endDate":"2026-11-17T09:00:00.000Z"},
		{"id":8,"name":"Next","state":"future","originBoardId":9}]}`))
	f.On("GET", "/rest/api/3/project/10000/statuses", jiratest.Reply(200, `[
		{"id":"10002","name":"Story","statuses":[
			{"id":"10000","name":"To Do","statusCategory":{"key":"new"}},
			{"id":"10001","name":"In Progress","statusCategory":{"key":"indeterminate"}},
			{"id":"10002","name":"Done","statusCategory":{"key":"done"}},
			{"id":"10009","name":"Parked","statusCategory":{"key":"undefined"}}]},
		{"id":"10003","name":"Epic","statuses":[{"id":"10000","name":"To Do","statusCategory":{"key":"new"}}]}]`))
	return f
}

func TestStructure(t *testing.T) {
	f := structureSite(t)
	p := newProvider(t, f, nil)
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s, err := p.Structure(t.Context(), "3", window)
	if err != nil {
		t.Fatal(err)
	}
	if s.Board != (tracker.Board{ID: "3", Name: "Board 3", Project: "10000", Sprints: true}) || s.Estimate.Unit != tracker.UnitPoints ||
		s.Estimate.Field != "customfield_10016" {
		t.Errorf("board %+v estimate %+v", s.Board, s.Estimate)
	}
	if len(s.Columns) != 3 || s.Columns[1].Name != "In Progress" || !slices.Equal(s.Columns[1].Statuses, []tracker.StatusID{"10001"}) {
		t.Errorf("columns %+v", s.Columns)
	}
	var sprints []string
	for _, sp := range s.Sprints {
		sprints = append(sprints, string(sp.ID)+" "+[]string{"", "future", "active", "closed"}[sp.State])
	}
	if strings.Join(sprints, ", ") != "6 closed, 7 active, 8 future" {
		t.Errorf("sprints %v", sprints)
	}
	if s.Sprints[1].Goal != "Ship" || s.Sprints[1].Board != "3" || s.Sprints[0].Completed.IsZero() {
		t.Errorf("sprint %+v", s.Sprints[1])
	}
	// Sprint 8 was made on board 9 and shows on board 3: it keeps its origin.
	if s.Sprints[2].Board != "9" {
		t.Errorf("sprint 8 of board %q, want its origin 9", s.Sprints[2].Board)
	}
	groups := map[tracker.StatusID]tracker.StatusGroup{}
	for _, st := range s.Statuses {
		groups[st.ID] = st.Group
	}
	want := map[tracker.StatusID]tracker.StatusGroup{"10000": tracker.GroupToDo, "10001": tracker.GroupInProgress, "10002": tracker.GroupDone, "10009": tracker.GroupToDo}
	if len(s.Statuses) != 4 || len(groups) != 4 {
		t.Errorf("statuses %+v", s.Statuses)
	}
	for id, g := range want {
		if groups[id] != g {
			t.Errorf("status %s in group %v, want %v", id, groups[id], g)
		}
	}
	// Structure reads the configuration again each time.
	if _, err := p.Structure(t.Context(), "3", window); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("GET", board3Config); n != 2 {
		t.Errorf("%d configuration reads, want one a Structure", n)
	}
	if n := f.Count("GET", "/rest/api/3/status"); n != 0 {
		t.Errorf("%d site status reads for columns the project's statuses name", n)
	}
	if _, err := p.Structure(t.Context(), "board-3", window); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("a board that is not an ID: %v", err)
	}
}

// TestStructureReadsSprintsAndStatusesTogether proves a scrum board's
// sprint listing and its project's statuses are read at once, and that
// either failing fails the read.
func TestStructureReadsSprintsAndStatusesTogether(t *testing.T) {
	f := structureSite(t)
	p := newProvider(t, f, nil)
	held := heldTogether(2)
	f.Before(func(r *http.Request) {
		if r.URL.Path == board3Sprints || r.URL.Path == "/rest/api/3/project/10000/statuses" {
			held.held()
		}
	})
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if s, err := p.Structure(t.Context(), "3", window); err != nil || len(s.Sprints) != 3 || len(s.Statuses) != 4 {
		t.Fatalf("structure %+v, %v", s, err)
	}
	if n := held.most(); n != 2 {
		t.Errorf("%d reads in flight at most, want the sprints and the statuses together", n)
	}
	f.Before(nil)
	f.On("GET", "/rest/api/3/project/10000/statuses", jiratest.Reply(500, `{"errorMessages":["test"]}`))
	if _, err := p.Structure(t.Context(), "3", window); err == nil {
		t.Error("a failed status read gave a structure")
	}
}

// TestStructureOfBoardUnderUser proves a board located under a user,
// and so in no project, lists the statuses its columns name from the
// site's statuses, and only those; a column status the site does not
// list is left out.
func TestStructureOfBoardUnderUser(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", board4Config, jiratest.Reply(200, `{"id":4,"name":"Board 4","type":"kanban","location":{"type":"user","id":"acc-user1","name":"User 1"},
"estimation":{"type":"field","field":{"fieldId":"timeoriginalestimate"}},
"columnConfig":{"columns":[{"name":"Open","statuses":[{"id":"10000"},{"id":"10001"}]},{"name":"Gone","statuses":[{"id":"10077"}]},{"name":"Done","statuses":[{"id":"10002"}]}]}}`))
	f.On("GET", "/rest/api/3/status", jiratest.Reply(200, `[
		{"id":"10000","name":"To Do","statusCategory":{"key":"new"}},
		{"id":"10001","name":"In Progress","statusCategory":{"key":"indeterminate"}},
		{"id":"10002","name":"Done","statusCategory":{"key":"done"}},
		{"id":"10050","name":"Elsewhere","statusCategory":{"key":"done"}}]`))
	s, err := p.Structure(t.Context(), "4", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := []tracker.Status{
		{ID: "10000", Name: "To Do", Group: tracker.GroupToDo},
		{ID: "10001", Name: "In Progress", Group: tracker.GroupInProgress},
		{ID: "10002", Name: "Done", Group: tracker.GroupDone},
	}
	if s.Board != (tracker.Board{ID: "4", Name: "Board 4"}) || !slices.Equal(s.Statuses, want) {
		t.Errorf("board %+v statuses %+v", s.Board, s.Statuses)
	}
	if s.Estimate.Unit != tracker.UnitTime || s.Estimate.Field != "timeoriginalestimate" {
		t.Errorf("estimate %+v", s.Estimate)
	}
	if n := f.Count("GET", "/rest/api/3/project/10000/statuses"); n != 0 {
		t.Errorf("%d project status reads for a board in no project", n)
	}
	if len(s.Columns) != 3 || !slices.Equal(s.Columns[1].Statuses, []tracker.StatusID{"10077"}) {
		t.Errorf("columns %+v", s.Columns)
	}
	f.On("GET", "/rest/api/3/status", jiratest.Reply(500, `{"errorMessages":["down"]}`))
	if _, err := p.Structure(t.Context(), "4", time.Time{}); err == nil {
		t.Error("a structure whose statuses could not be read")
	}
}

func TestBoardIssues(t *testing.T) {
	f := structureSite(t)
	p := newProvider(t, f, nil)
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var warn bool
	f.On("GET", board3Issues, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "jql", `statusCategory != Done OR updated >= 1790812800000 OR sprint in (6, 7, 8)`)
		jiratest.WantQuery(t, r, "fields", "id")
		jiratest.WantQuery(t, r, "maxResults", "5000")
		v := map[string]any{"isLast": true, "issues": []map[string]any{{"id": "10001", "key": "WEB-1"}, {"id": "10002", "key": "WEB-2"}}}
		if warn {
			v["warningMessages"] = []string{"The value 'x' does not exist for the field 'sprint'."}
		}
		jiratest.Reply(200, mustJSON(v))(w, r, b)
	})
	got, err := p.BoardIssues(t.Context(), "3", window)
	if err != nil || !got.Complete || !slices.Equal(got.IDs, []tracker.IssueID{"10001", "10002"}) {
		t.Fatalf("got %+v %v", got, err)
	}
	warn = true
	if got, err := p.BoardIssues(t.Context(), "3", window); err != nil || got.Complete {
		t.Errorf("a listing Jira warned about: %+v %v", got, err)
	}
}

// TestBoardRowsReadThroughTheBoard proves a row is read through the
// board named: the same issue on boards estimating in different fields
// is two different rows.
func TestBoardRowsReadThroughTheBoard(t *testing.T) {
	f, p := newSite(t)
	one := issue("10001", "WEB-1", "2026-11-10T10:00:00.000+0000", nil)
	for _, path := range []string{board3Issues, board4Issues} {
		f.On("GET", path, func(w http.ResponseWriter, r *http.Request, b []byte) {
			jiratest.WantQuery(t, r, "jql", "id in (10001, 10002)")
			fields := r.URL.Query()["fields"]
			want := "customfield_10016"
			if path == board4Issues {
				want = "timeoriginalestimate"
			}
			if !slices.Contains(fields, want) || !slices.Contains(fields, "customfield_10019") || !slices.Contains(fields, "customfield_10020") {
				t.Errorf("%s: fields %v", path, fields)
			}
			jiratest.Reply(200, page("", one))(w, r, b)
		})
	}
	// The list's miss is confirmed with the board's membership query,
	// which does not match it.
	f.On("POST", matchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), `"issueIds":[10002]`) || !strings.Contains(string(b), `AND status in (10000, 10001, 10002) AND hierarchyLevel != 1"]`) {
			t.Errorf("match %s", b)
		}
		jiratest.Reply(200, `{"matches":[{"matchedIssues":[],"errors":[]}]}`)(w, r, b)
	})
	rows3, err := p.BoardRows(t.Context(), "3", []tracker.IssueID{"10001", "10002"})
	if err != nil {
		t.Fatal(err)
	}
	rows4, err := p.BoardRows(t.Context(), "4", []tracker.IssueID{"10001", "10002"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows3.Rows) != 1 || len(rows4.Rows) != 1 || !slices.Equal(rows3.Missing, []tracker.IssueID{"10002"}) {
		t.Fatalf("rows %+v / %+v", rows3, rows4)
	}
	r3, r4 := rows3.Rows[0], rows4.Rows[0]
	if r3.Estimate == nil || *r3.Estimate != 3 || r4.Estimate == nil || *r4.Estimate != 7200 {
		t.Errorf("estimates %v / %v", r3.Estimate, r4.Estimate)
	}
	if r3.Order != "0|i000WEB-1:" || r3.Card.Key != "WEB-1" || r3.Card.Status != "10001" || !slices.Equal(r3.Card.Sprints, []tracker.SprintID{"7"}) {
		t.Errorf("row %+v", r3)
	}
	if len(rows3.Users) != 1 || rows3.Users[0].ID != "acc-user1" || len(rows3.Statuses) != 1 || rows3.Statuses[0].Group != tracker.GroupInProgress {
		t.Errorf("catalog %+v", rows3.Catalog)
	}
}

// TestBoardRowsWithDeletedIssues proves an id clause Jira refuses or
// warns about, because it names an issue that no longer exists, is
// listed again without it, and the deleted issue is Missing. The warned
// listing also leaves out an issue that exists, which only the listing
// made again returns.
func TestBoardRowsWithDeletedIssues(t *testing.T) {
	for _, answer := range []string{"refused", "warned"} {
		t.Run(answer, func(t *testing.T) {
			f, p := newSite(t)
			one := issue("10001", "WEB-1", "2026-11-10T10:00:00.000+0000", nil)
			two := issue("10002", "WEB-2", "2026-11-10T10:00:00.000+0000", nil)
			f.On("GET", board3Issues, func(w http.ResponseWriter, r *http.Request, b []byte) {
				switch jql := r.URL.Query().Get("jql"); {
				case jql == "id in (10001, 10002)":
					jiratest.Reply(200, page("", one, two))(w, r, b)
				case answer == "refused":
					jiratest.Reply(400, `{"errorMessages":["An issue with key '10009' does not exist for field 'id'."]}`)(w, r, b)
				default:
					jiratest.Reply(200, mustJSON(map[string]any{"isLast": true, "issues": []any{one}, "warningMessages": []string{"An issue with key '10009' does not exist."}}))(w, r, b)
				}
			})
			f.On("POST", bulkFetchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
				jiratest.WantJSON(t, b, `{"issueIdsOrKeys":["10001","10002","10009"],"fields":["status"]}`)
				jiratest.Reply(200, page("", one, two))(w, r, b)
			})
			// The deleted issue is matched by no query, with no error
			// (measured).
			f.On("POST", matchPath, jiratest.Reply(200, `{"matches":[{"matchedIssues":[],"errors":[]}]}`))
			rows, err := p.BoardRows(t.Context(), "3", []tracker.IssueID{"10001", "10002", "10009"})
			if err != nil || len(rows.Rows) != 2 || !slices.Equal(rows.Missing, []tracker.IssueID{"10009"}) {
				t.Fatalf("got %+v %v", rows, err)
			}
		})
	}
	// When every issue exists, the failure is the listing's own.
	f, p := newSite(t)
	f.On("GET", board3Issues, jiratest.Reply(400, `{"errorMessages":["bad"]}`))
	f.On("POST", bulkFetchPath, jiratest.Reply(200, page("", issue("10001", "WEB-1", "2026-11-10T10:00:00.000+0000", nil))))
	if _, err := p.BoardRows(t.Context(), "3", []tracker.IssueID{"10001"}); err == nil {
		t.Error("a refused listing of issues that exist read as empty")
	}
}

func TestCards(t *testing.T) {
	f, p := newSite(t)
	done := issue("10002", "WEB-2", "2026-11-10T10:00:00.000+0000", map[string]any{
		"status": statusJSON("10002", "Done", "done"), "resolutiondate": "2026-11-10T10:00:00.000+0000",
		"assignee": nil, "parent": map[string]any{"id": "10003", "key": "WEB-3"}, "customfield_10020": nil,
	})
	f.On("POST", bulkFetchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		v := bodyOf(t, b)
		if ids := v["issueIdsOrKeys"].([]any); len(ids) != 3 {
			t.Errorf("asked for %v", ids)
		}
		jiratest.Reply(200, page("", issue("10001", "WEB-1", "2026-11-10T10:00:00.000+0000", nil), done))(w, r, b)
	})
	cards, err := p.Cards(t.Context(), []tracker.IssueID{"10001", "10002", "10009", "10001"})
	if err != nil || len(cards.Cards) != 2 || !slices.Equal(cards.Missing, []tracker.IssueID{"10009"}) {
		t.Fatalf("got %+v %v", cards, err)
	}
	c := cards.Cards[1]
	want := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	if !c.Resolved.Equal(want) || c.Assignee != "" || c.Parent != "10003" || c.Sprints != nil || c.Project != "10000" || c.Type.Name != "Story" {
		t.Errorf("card %+v", c)
	}
	if len(cards.Statuses) != 2 || cards.Statuses[1].Group != tracker.GroupDone {
		t.Errorf("statuses %+v", cards.Statuses)
	}
	f.On("POST", bulkFetchPath, jiratest.Reply(200, `{"issues":[],"issueErrors":[{"id":"10009","errorMessage":"Jira is busy"}]}`))
	if _, err := p.Cards(t.Context(), []tracker.IssueID{"10009"}); err == nil {
		t.Error("an issue Jira could not read was taken as missing")
	}
	if _, err := p.Cards(t.Context(), []tracker.IssueID{"WEB-1"}); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("a key for an ID: %v", err)
	}
	f.On("POST", bulkFetchPath, jiratest.Reply(200, page("", issue("10001", "WEB-1", "2026-11-10T10:00:00.000+0000", map[string]any{"status": nil}))))
	if _, err := p.Cards(t.Context(), []tracker.IssueID{"10001"}); err == nil {
		t.Error("a card without its status")
	}
}

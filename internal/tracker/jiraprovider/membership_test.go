package jiraprovider

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

func TestMembershipJQL(t *testing.T) {
	cols := []jira.BoardColumn{{Name: "To Do", StatusIDs: []string{"10000"}}, {Name: "Done", StatusIDs: []string{"10002", "10000"}}, {Name: "Empty"}}
	for name, c := range map[string]struct {
		cfg  jira.BoardConfig
		want string
		err  bool
	}{
		"scrum":              {cfg: jira.BoardConfig{BoardID: 1, Type: jira.BoardScrum, FilterID: "10010", Columns: cols}, want: "filter = 10010 AND status in (10000, 10002) AND hierarchyLevel != 1"},
		"kanban":             {cfg: jira.BoardConfig{BoardID: 1, Type: jira.BoardKanban, FilterID: "10010", Columns: cols}, want: "filter = 10010 AND status in (10000, 10002)"},
		"kanban sub-query":   {cfg: jira.BoardConfig{BoardID: 1, Type: jira.BoardKanban, FilterID: "10010", SubQuery: "fixVersion is EMPTY OR fixVersion in unreleasedVersions()", Columns: cols}, want: "filter = 10010 AND status in (10000, 10002) AND (fixVersion is EMPTY OR fixVersion in unreleasedVersions())"},
		"no mapped status":   {cfg: jira.BoardConfig{BoardID: 1, Type: jira.BoardScrum, FilterID: "10010", Columns: cols[2:]}, want: ""},
		"no filter":          {cfg: jira.BoardConfig{BoardID: 1, Type: jira.BoardScrum, Columns: cols}, err: true},
		"a filter not an ID": {cfg: jira.BoardConfig{BoardID: 1, Type: jira.BoardScrum, FilterID: "1 OR 1", Columns: cols}, err: true},
		"a status not an ID": {cfg: jira.BoardConfig{BoardID: 1, Type: jira.BoardScrum, FilterID: "10010", Columns: []jira.BoardColumn{{StatusIDs: []string{"x)"}}}}, err: true},
	} {
		got, err := membershipJQL(c.cfg)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%s: %q, %v; want %q", name, got, err, c.want)
		}
	}
}

// listedIssue and fetchedIssue are one issue as a board list and as the
// bulk fetch answer it: measured, the same fields, differing only in
// every self and the project's avatarUrls.
func listedIssue(root string) map[string]any {
	self := func(path string) string { return "https://example.atlassian.net/rest/" + root + "/" + path }
	return issue("10005", "WEB-5", "2026-11-10T10:00:00.000+0000", map[string]any{
		"status": map[string]any{"self": self("status/10001"), "id": "10001", "name": "In Progress",
			"statusCategory": map[string]any{"self": self("statuscategory/4"), "id": 4, "key": "indeterminate", "name": "In Progress"}},
		"issuetype": map[string]any{"self": self("issuetype/10002"), "id": "10002", "name": "Story", "hierarchyLevel": 0},
		"project": map[string]any{"self": self("project/10000"), "id": "10000", "key": "WEB", "name": "Web",
			"avatarUrls": map[string]string{"48x48": self("universal_avatar/view/type/project/avatar/1")}},
		"priority": map[string]any{"self": self("priority/3"), "id": "3", "name": "Medium"},
		"parent":   map[string]any{"self": self("issue/10001"), "id": "10001", "key": "WEB-1"},
		"assignee": map[string]any{"self": self("user?accountId=acc-user1"), "accountId": "acc-user1", "displayName": "User 1",
			"emailAddress": "user1@example.com", "active": true, "avatarUrls": map[string]string{"48x48": "https://avatar.example.com/1.png"}},
	})
}

// TestARowReadsNoURL: rowOf reads no self and no avatar URL, so the row a
// board's list delivers and the row NewRows reads with the bulk fetch are
// the same row, with the same catalog.
func TestARowReadsNoURL(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", board3Issues, jiratest.Reply(200, page("", listedIssue("agile/1.0"))))
	f.On("POST", matchPath, jiratest.Reply(200, `{"matches":[{"matchedIssues":[10005],"errors":[]}]}`))
	f.On("POST", bulkFetchPath, jiratest.Reply(200, page("", listedIssue("api/3"))))
	listed, err := p.BoardRows(t.Context(), "3", []tracker.IssueID{"10005"})
	if err != nil {
		t.Fatal(err)
	}
	placed, err := p.NewRows(t.Context(), "10005", []tracker.BoardID{"3"})
	if err != nil || len(placed.Rows) != 1 {
		t.Fatalf("placed %+v, %v", placed, err)
	}
	if !reflect.DeepEqual(listed.Rows[0], placed.Rows[0].Row) || !reflect.DeepEqual(listed.Catalog, placed.Catalog) {
		t.Fatalf("the list's row\n %+v %+v\nthe bulk fetch's\n %+v %+v", listed.Rows[0], listed.Catalog, placed.Rows[0].Row, placed.Catalog)
	}
}

// TestNewRows: one match request of every board's query and one bulk
// fetch of the row fields of the boards that hold the issue; a board the
// match does not place it on is Off, one whose query Jira answered with
// an error is Unknown, and a board that maps no status is Off with no
// query.
func TestNewRows(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", "/rest/agile/1.0/board/5/configuration", jiratest.Reply(200, strings.Replace(boardConfig(5, "customfield_10016"), `"filter":{"id":"15"}`, `"filter":{"id":"13"}`, 1)))
	f.On("GET", "/rest/agile/1.0/board/6/configuration", jiratest.Reply(200, strings.Replace(boardConfig(6, "customfield_10016"), `"type":"scrum"`, `"type":"kanban"`, 1)))
	f.On("GET", "/rest/agile/1.0/board/7/configuration", jiratest.Reply(200, `{"id":7,"name":"Board 7","type":"scrum","filter":{"id":"17"},"columnConfig":{"columns":[]},"ranking":{"rankCustomFieldId":10019}}`))
	f.On("POST", matchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		// Board 5 shares board 3's filter and terms: one query for both.
		jiratest.WantJSON(t, b, `{"issueIds":[10005],"jqls":[
			"filter = 13 AND status in (10000, 10001, 10002) AND hierarchyLevel != 1",
			"filter = 14 AND status in (10000, 10001, 10002) AND hierarchyLevel != 1",
			"filter = 16 AND status in (10000, 10001, 10002)"]}`)
		jiratest.Reply(200, `{"matches":[{"matchedIssues":[10005],"errors":[]},{"matchedIssues":[],"errors":[]},{"matchedIssues":[],"errors":["The value '16' does not exist for the field 'filter'."]}]}`)(w, r, b)
	})
	f.On("POST", bulkFetchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), `"customfield_10019","customfield_10016"]`) || strings.Contains(string(b), "timeoriginalestimate") {
			t.Errorf("bulk fetch %s", b)
		}
		jiratest.Reply(200, page("", listedIssue("api/3")))(w, r, b)
	})
	got, err := p.NewRows(t.Context(), "10005", []tracker.BoardID{"3", "4", "5", "6", "7"})
	if err != nil {
		t.Fatal(err)
	}
	var on []tracker.BoardID
	for _, r := range got.Rows {
		on = append(on, r.Board)
	}
	if !slices.Equal(on, []tracker.BoardID{"3", "5"}) || !slices.Equal(got.Off, []tracker.BoardID{"7", "4"}) ||
		len(got.Unknown) != 1 || got.Unknown[0].Board != "6" || len(got.Users) != 1 {
		t.Fatalf("placement %+v", got)
	}
	if got.Rows[0].Row.Order != "0|i000WEB-5:" || got.Rows[0].Row.Estimate == nil || *got.Rows[0].Row.Estimate != 3 {
		t.Errorf("row %+v", got.Rows[0].Row)
	}
	if n := f.Count("POST", matchPath) + f.Count("POST", bulkFetchPath); n != 2 {
		t.Errorf("%d match and bulk fetch requests, want 2", n)
	}
	if _, err := p.NewRows(t.Context(), "10005", nil); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("no board: %v", err)
	}
	if _, err := p.NewRows(t.Context(), "WEB-5", []tracker.BoardID{"3"}); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("a key: %v", err)
	}
}

// TestNewRowsPastTenQueries: the first ten distinct queries are sent in
// the order of the boards; a board past them is Unknown, never guessed.
func TestNewRowsPastTenQueries(t *testing.T) {
	f, p := newSite(t)
	var boards []tracker.BoardID
	for id := 20; id < 31; id++ {
		f.On("GET", fmt.Sprintf("/rest/agile/1.0/board/%d/configuration", id), jiratest.Reply(200, boardConfig(id, "customfield_10016")))
		boards = append(boards, tracker.BoardID(fmt.Sprint(id)))
	}
	f.On("POST", matchPath, jiratest.Reply(200, `{"matches":[`+strings.Repeat(`{"matchedIssues":[],"errors":[]},`, 9)+`{"matchedIssues":[],"errors":[]}]}`))
	got, err := p.NewRows(t.Context(), "10005", boards)
	if err != nil || len(got.Off) != 10 || len(got.Unknown) != 1 || got.Unknown[0].Board != "30" {
		t.Fatalf("placement %+v, %v", got, err)
	}
}

// TestNewRowsFailsWithTheMatch: a match Jira refuses or limits fails the
// placement, which judges nothing.
func TestNewRowsFailsWithTheMatch(t *testing.T) {
	f, p := newSite(t)
	f.On("POST", matchPath, jiratest.Reply(400, `{"errorMessages":["The JQL query is not valid."]}`))
	if _, err := p.NewRows(t.Context(), "10005", []tracker.BoardID{"3"}); err == nil {
		t.Fatal("a failed match placed the issue")
	} else if _, ok := errors.AsType[*trackerapi.StatusError](err); !ok {
		t.Fatalf("the match's failure is not the transport's: %v", err)
	}
}

// TestBoardRowsConfirmsAMiss: an issue the board's list leaves out (the
// index not caught up) that the match places on the board is read with
// the bulk fetch, not Missing; one the match does not place, or whose
// query Jira answers with an error, is Missing as the list says; a
// failed match fails the read.
func TestBoardRowsConfirmsAMiss(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", board3Issues, jiratest.Reply(200, page("")))
	f.On("POST", bulkFetchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), `"issueIdsOrKeys":["10005"]`) {
			t.Errorf("bulk fetch %s", b)
		}
		jiratest.Reply(200, page("", listedIssue("api/3")))(w, r, b)
	})
	answers := map[string]string{
		"placed":     `{"matches":[{"matchedIssues":[10005],"errors":[]}]}`,
		"not placed": `{"matches":[{"matchedIssues":[],"errors":[]}]}`,
		"refused":    `{"matches":[{"matchedIssues":[],"errors":["The value '13' does not exist for the field 'filter'."]}]}`,
	}
	for _, name := range []string{"placed", "not placed", "refused"} {
		f.On("POST", matchPath, jiratest.Reply(200, answers[name]))
		got, err := p.BoardRows(t.Context(), "3", []tracker.IssueID{"10005", "10006"})
		if err != nil || !slices.Contains(got.Missing, "10006") || (len(got.Rows) == 1) != (name == "placed") ||
			slices.Contains(got.Missing, "10005") == (name == "placed") {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	f.On("POST", matchPath, jiratest.Reply(400, `{"errorMessages":["bad"]}`))
	if _, err := p.BoardRows(t.Context(), "3", []tracker.IssueID{"10005"}); err == nil {
		t.Error("a failed confirmation read as Missing")
	}
}

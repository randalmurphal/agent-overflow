package trackerfake

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// membershipFixture is changeFixture with a kanban board whose sub-query
// leaves out WEB-2, an issue in a status no column maps, and the scrum
// boards' saved filter shared by the kanban board. The first board names
// its project by ID, the others by key.
func membershipFixture() Fixture {
	f := changeFixture()
	s := &f.Jira[0]
	s.Statuses = append(slices.Clone(defaultStatuses), JiraStatus{ID: "10009", Name: "Parked", Category: "new"})
	columns := s.Boards[0].Columns
	s.Boards[0].Project = s.Projects[0].ID
	s.Boards[0].FilterID, s.Boards[1].FilterID = "10010", "10010"
	s.Boards = append(s.Boards, JiraBoard{ID: 3, Name: "Flow", Type: "kanban", Project: "WEB", FilterID: "10010", SubQuery: "id != 10102", Columns: columns})
	for i := range s.Issues {
		s.Issues[i].Status = "10000"
	}
	s.Issues = append(s.Issues, JiraIssue{ID: "10104", Key: "WEB-4", Project: "WEB", Type: "10001", Summary: "Parked", Status: "10009"})
	return f
}

func listIDs(t *testing.T, w *world, path string) []string {
	t.Helper()
	var page struct{ Issues []struct{ ID string } }
	decodeReply(t, w.write(t, "GET", path, "", 200), &page)
	var out []string
	for _, is := range page.Issues {
		out = append(out, is.ID)
	}
	slices.Sort(out)
	return out
}

// TestBoardListMembership: a board's list is its filter's issues in a
// mapped status (measured), without epics on a scrum board (measured),
// within a kanban board's sub-query (unverified).
func TestBoardListMembership(t *testing.T) {
	w := newWorld(t)
	if err := w.e.Seed(membershipFixture()); err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, w, "/rest/software/1.0/board/1/issue?fields=id"); !slices.Equal(got, []string{"10101", "10102"}) {
		t.Errorf("scrum list %v", got)
	}
	if got := listIDs(t, w, "/rest/software/1.0/board/3/issue?fields=id"); !slices.Equal(got, []string{"10101", "10103"}) {
		t.Errorf("kanban list %v", got)
	}
	var cfg struct {
		Filter   struct{ ID string }
		SubQuery struct{ Query string }
	}
	decodeReply(t, w.write(t, "GET", "/rest/agile/1.0/board/3/configuration", "", 200), &cfg)
	if cfg.Filter.ID != "10010" || cfg.SubQuery.Query != "id != 10102" {
		t.Errorf("kanban configuration %+v", cfg)
	}
}

// TestMatchJQL: the match answers in the queries' order from the issues
// as they are, never the index; an ID that names nothing is matched by no
// query with no error (measured); a filter no board has is the query's
// error; a JQL the fake does not read is unhandled.
func TestMatchJQL(t *testing.T) {
	w := newWorld(t)
	f := membershipFixture()
	f.Jira[0].Issues[1].Indexed = testStart.Add(time.Hour)
	if err := w.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Matches []struct {
			MatchedIssues []int64
			Errors        []string
		}
	}
	decodeReply(t, w.write(t, "POST", "/rest/api/3/jql/match", `{"issueIds":[10101,10102,10103,10104,99999],"jqls":[
		"filter = 10010 AND status in (10000, 10001, 10002) AND hierarchyLevel != 1",
		"filter = 10010 AND status in (10000, 10001, 10002) AND (id != 10102)",
		"filter = 77"]}`, 200), &got)
	if len(got.Matches) != 3 || !slices.Equal(got.Matches[0].MatchedIssues, []int64{10101, 10102}) || len(got.Matches[0].Errors) != 0 ||
		!slices.Equal(got.Matches[1].MatchedIssues, []int64{10101, 10103}) ||
		len(got.Matches[2].MatchedIssues) != 0 || len(got.Matches[2].Errors) != 1 || !strings.Contains(got.Matches[2].Errors[0], "'77'") {
		t.Fatalf("matches %+v", got)
	}
	w.write(t, "POST", "/rest/api/3/jql/match", `{"issueIds":[],"jqls":["id = 1"]}`, 400)
	w.write(t, "POST", "/rest/api/3/jql/match", `{"issueIds":[1],"jqls":["a","b","c","d","e","f","g","h","i","j","k"]}`, 400)
}

// TestCreateIndexLag: an issue a create made is left out of lists and
// searches for the site's IndexLag, and matched at once.
func TestCreateIndexLag(t *testing.T) {
	w := newWorld(t)
	f := membershipFixture()
	f.Jira[0].IndexLag = 20 * time.Second
	if err := w.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	var created struct{ ID string }
	decodeReply(t, w.write(t, "POST", "/rest/api/3/issue", `{"fields":{"project":{"key":"WEB"},"issuetype":{"id":"10001"},"summary":"New"}}`, 201), &created)
	if got := listIDs(t, w, "/rest/software/1.0/board/1/issue?fields=id"); slices.Contains(got, created.ID) {
		t.Fatalf("the list shows the new issue before the index takes it: %v", got)
	}
	var m struct {
		Matches []struct{ MatchedIssues []int64 }
	}
	decodeReply(t, w.write(t, "POST", "/rest/api/3/jql/match", `{"issueIds":[`+created.ID+`],"jqls":["filter = 10010"]}`, 200), &m)
	if len(m.Matches[0].MatchedIssues) != 1 {
		t.Fatalf("the match missed the new issue: %+v", m)
	}
	w.clock.set(testStart.Add(20 * time.Second))
	if got := listIDs(t, w, "/rest/software/1.0/board/1/issue?fields=id"); !slices.Contains(got, created.ID) {
		t.Fatalf("the list does not show the new issue once indexed: %v", got)
	}
}

// TestListURLsDiffer: a board list's issue differs from the bulk fetch's
// only in its URLs (measured), every self and the project's avatarUrls.
func TestListURLsDiffer(t *testing.T) {
	w := newWorld(t)
	if err := w.e.Seed(membershipFixture()); err != nil {
		t.Fatal(err)
	}
	fields := "summary,status,assignee,priority,labels,issuetype,parent,created,updated,project"
	list := w.write(t, "GET", "/rest/software/1.0/board/1/issue?jql=id%20in%20(10101)&fields="+fields, "", 200).body
	bulk := w.write(t, "POST", "/rest/api/3/issue/bulkfetch", `{"issueIdsOrKeys":["10101"],"fields":["`+strings.ReplaceAll(fields, ",", `","`)+`"]}`, 200).body
	if !strings.Contains(list, "/rest/agile/1.0/status/") || strings.Contains(list, "/rest/api/3/") {
		t.Fatalf("the list's URLs are the platform's: %s", list)
	}
	if !strings.Contains(bulk, "/rest/api/3/status/") || strings.Contains(bulk, "/rest/agile/1.0/") {
		t.Fatalf("the bulk fetch's URLs are the agile root's: %s", bulk)
	}
}

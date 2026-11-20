package trackerfake

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pagingFixture holds at least three records of everything a paged route
// or connection lists.
func pagingFixture() Fixture {
	f := testFixture()
	s := &f.Jira[0]
	doc := json.RawMessage(testDoc)
	s.Users = nil
	for i := 1; i <= 4; i++ {
		n := strconv.Itoa(i)
		s.Users = append(s.Users, JiraUser{AccountID: "acc-user" + n, DisplayName: "User " + n, Email: "user" + n + "@example.com"})
	}
	screen := []JiraScreenField{{Field: "summary", Required: true}, {Field: "description"}, {Field: "labels"}}
	s.Projects = []JiraProject{
		{ID: "10000", Key: "WEB", Name: "Web", IssueTypes: []JiraIssueType{
			{ID: "10001", Name: "Task", Screen: screen}, {ID: "10002", Name: "Bug"}, {ID: "10003", Name: "Story"}, {ID: "10004", Name: "Epic", HierarchyLevel: 1}}},
		{ID: "10010", Key: "APP", Name: "App", IssueTypes: []JiraIssueType{{ID: "10001", Name: "Task"}}},
		{ID: "10020", Key: "OPS", Name: "Ops", IssueTypes: []JiraIssueType{{ID: "10001", Name: "Task"}}},
	}
	columns := []JiraColumn{{Name: "To Do", Statuses: []string{"10000"}}, {Name: "In Progress", Statuses: []string{"10001"}}, {Name: "Done", Statuses: []string{"10002"}}}
	for i := int64(1); i <= 3; i++ {
		s.Boards = append(s.Boards, JiraBoard{ID: i, Name: "Board " + strconv.FormatInt(i, 10), Type: "scrum", Project: "WEB", Columns: columns})
		s.Sprints = append(s.Sprints, JiraSprint{ID: i, Board: 1, Name: "Sprint " + strconv.FormatInt(i, 10), State: "future"})
	}
	s.Sprints[0].State, s.Sprints[0].Start, s.Sprints[0].End = "active", testStart, testStart.Add(14*24*time.Hour)
	s.Issues = nil
	for i := 1; i <= 7; i++ {
		is := JiraIssue{ID: strconv.Itoa(10100 + i), Key: "WEB-" + strconv.Itoa(i), Project: "WEB", Type: "10001", Status: "10000", Summary: "Issue " + strconv.Itoa(i)}
		switch {
		case i == 1:
			is.Type = "10004"
		case i <= 4:
			is.Parent, is.Sprints = "10101", []int64{1}
		}
		s.Issues = append(s.Issues, is)
	}
	epic := &s.Issues[0]
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		at := testStart.Add(time.Duration(i) * time.Hour)
		epic.Comments = append(epic.Comments, JiraComment{ID: "2000" + n, Author: "acc-user1", Body: doc, Created: at})
		epic.Worklogs = append(epic.Worklogs, JiraWorklog{ID: "3000" + n, Author: "acc-user1", Started: at, TimeSpent: 60 * int64(i)})
		from, to := "Old "+n, "New "+n
		epic.Changelog = append(epic.Changelog, JiraChange{ID: "4000" + n, Author: "acc-user1", Created: at, Items: []JiraChangeItem{
			{FieldID: "summary", Field: "summary", FieldType: "jira", FromString: &from, ToString: &to}}})
	}
	s.Tempo = &TempoAccount{Token: testTempo}
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		s.Tempo.Attributes = append(s.Tempo.Attributes, TempoAttribute{Key: "_attr" + n + "_", Name: "Attribute " + n, Type: "INPUT_FIELD"})
		s.Tempo.Accounts = append(s.Tempo.Accounts, TempoBilling{ID: int64(i), Key: "ACC" + n, Name: "Account " + n, Status: "OPEN"})
		s.Tempo.Worklogs = append(s.Tempo.Worklogs, TempoWorklog{ID: int64(500 + i), Issue: "10101", Author: "acc-user1", StartDate: "2026-11-1" + n, StartTime: "09:00:00", Spent: 3600})
	}
	f.Linear[0] = pagingLinear()
	return f
}

func pagingLinear() LinearWorkspace {
	w := testFixture().Linear[0]
	w.Users, w.Teams, w.States, w.Labels = nil, nil, nil, nil
	for i := 1; i <= 4; i++ {
		n := strconv.Itoa(i)
		w.Users = append(w.Users, LinearUser{ID: "u" + n, Name: "User " + n})
	}
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		w.Teams = append(w.Teams, LinearTeam{ID: "t" + n, Key: []string{"WEB", "APP", "OPS"}[i-1], Name: "Team " + n})
		w.Labels = append(w.Labels, LinearLabel{ID: "l" + n, Name: "Label " + n, Team: "t1"})
		w.Projects = append(w.Projects, LinearProject{ID: "p" + n + "-000000000000", Name: "Project " + n, Teams: []string{"t1"}})
		w.Cycles = append(w.Cycles, LinearCycle{ID: "cy" + n, Team: "t1", Number: float64(i),
			StartsAt: testStart.Add(time.Duration(i*14) * 24 * time.Hour), EndsAt: testStart.Add(time.Duration(i*14+14) * 24 * time.Hour)})
	}
	w.Teams[0].Members = []string{"u1", "u2", "u3"}
	w.States = []LinearState{
		{ID: "s1", Team: "t1", Name: "Todo", Type: "unstarted"},
		{ID: "s2", Team: "t1", Name: "Doing", Type: "started"},
		{ID: "s3", Team: "t1", Name: "Done", Type: "completed"},
		{ID: "s4", Team: "t2", Name: "Todo", Type: "unstarted"},
		{ID: "s5", Team: "t3", Name: "Todo", Type: "unstarted"},
	}
	for i := 1; i <= 4; i++ {
		n := strconv.Itoa(i)
		is := LinearIssue{ID: "i" + n, Team: "t1", Number: float64(i), Title: "Issue " + n, State: "s1", CreatedAt: testStart, UpdatedAt: testStart.Add(time.Duration(i) * time.Minute)}
		if i > 1 {
			is.Parent = "i1"
			w.Relations = append(w.Relations, LinearRelation{ID: "r" + n, Type: "related", Issue: "i1", Related: is.ID},
				LinearRelation{ID: "q" + n, Type: "related", Issue: is.ID, Related: "i1"})
			w.Comments = append(w.Comments, LinearComment{ID: "c" + n + "-comment", Issue: "i1", User: "u1", Body: "Comment " + n, CreatedAt: testStart})
			w.Attachments = append(w.Attachments, LinearAttachment{ID: "a" + n, Issue: "i1", Title: "Link " + n, URL: "https://example.com/" + n, CreatedAt: testStart})
		}
		w.Issues = append(w.Issues, is)
	}
	i1 := &w.Issues[0]
	i1.Labels = []string{"l1", "l2", "l3"}
	for i, step := range [][2]string{{"s1", "s2"}, {"s2", "s3"}, {"s3", "s1"}} {
		i1.History = append(i1.History, LinearHistory{ID: "h" + strconv.Itoa(i+1), CreatedAt: testStart.Add(time.Duration(i+1) * time.Hour),
			Actor: "u1", FromState: step[0], ToState: step[1]})
	}
	return w
}

// ids reads the IDs at path in a JSON answer: keys separated by dots, []
// for each item of a list.
func ids(t *testing.T, body, path string) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("answer %s: %v", body, err)
	}
	vals := []any{v}
	for _, step := range strings.Split(path, ".") {
		var next []any
		for _, x := range vals {
			if key, each := strings.CutSuffix(step, "[]"); each {
				if key != "" {
					x = x.(map[string]any)[key]
				}
				list, _ := x.([]any)
				next = append(next, list...)
				continue
			}
			m, _ := x.(map[string]any)
			next = append(next, m[step])
		}
		vals = next
	}
	out := make([]string, len(vals))
	for i, x := range vals {
		out[i] = fmt.Sprint(x)
	}
	return out
}

// pager reads a list one record a page, page after page to its end, and
// the whole list in one page.
type pager func(t *testing.T, g *rig) (paged, all []string)

func (g *rig) ok(t *testing.T, c call) string {
	t.Helper()
	rec := g.do(c)
	if rec.Code != 200 {
		t.Fatalf("%s %s answered %d %s", c.method, c.path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// byOffset reads pages one record long from offset 0 until one is empty.
func byOffset(t *testing.T, page func(offset int) []string) []string {
	var out []string
	for offset := 0; ; offset++ {
		got := page(offset)
		if len(got) == 0 {
			return out
		}
		if len(got) != 1 || offset > 100 {
			t.Fatalf("page at %d holds %v", offset, got)
		}
		out = append(out, got...)
	}
}

// offsetPager pages a GET by startAt and maxResults (Jira).
func offsetPager(path, list string) pager {
	return func(t *testing.T, g *rig) ([]string, []string) {
		paged := byOffset(t, func(offset int) []string {
			return ids(t, g.ok(t, jiraReq("GET", path+"startAt="+strconv.Itoa(offset)+"&maxResults=1", "")), list)
		})
		return paged, ids(t, g.ok(t, jiraReq("GET", path+"maxResults=50", "")), list)
	}
}

// tempoPager pages by offset and limit (Tempo).
func tempoPager(method, path, body, list string) pager {
	return func(t *testing.T, g *rig) ([]string, []string) {
		get := func(q string) string {
			return g.ok(t, call{origin: tempoOrigin, method: method, path: path + q, body: body})
		}
		paged := byOffset(t, func(offset int) []string { return ids(t, get("offset="+strconv.Itoa(offset)+"&limit=1"), list) })
		return paged, ids(t, get("limit=50"), list)
	}
}

// byCursor reads pages one record long, each from the cursor the one
// before handed out, until one hands out none.
func byCursor(t *testing.T, page func(cursor string) (ids []string, next string)) []string {
	var out []string
	cursor := ""
	for range 100 {
		got, next := page(cursor)
		if len(got) != 1 {
			t.Fatalf("page after %q holds %v", cursor, got)
		}
		out = append(out, got...)
		if next == "" {
			return out
		}
		cursor = next
	}
	t.Fatal("the pages never end")
	return nil
}

// tokenPager pages by nextPageToken (Jira), in the query or the body.
func tokenPager(method, path, body, list string) pager {
	page := func(t *testing.T, g *rig, size int, token string) string {
		if method == "GET" {
			q := path + "maxResults=" + strconv.Itoa(size)
			if token != "" {
				q += "&nextPageToken=" + url.QueryEscape(token)
			}
			return g.ok(t, jiraReq("GET", q, ""))
		}
		b := strings.TrimSuffix(body, "}") + `,"maxResults":` + strconv.Itoa(size)
		if token != "" {
			b += `,"nextPageToken":"` + token + `"`
		}
		return g.ok(t, jiraReq(method, path, b+"}"))
	}
	return func(t *testing.T, g *rig) ([]string, []string) {
		paged := byCursor(t, func(token string) ([]string, string) {
			body := page(t, g, 1, token)
			var next struct{ NextPageToken string }
			if err := json.Unmarshal([]byte(body), &next); err != nil {
				t.Fatal(err)
			}
			return ids(t, body, list), next.NextPageToken
		})
		return paged, ids(t, page(t, g, 50, ""), list)
	}
}

// linearPager pages a connection by first and after (Linear); doc holds
// %s where the connection's arguments go, and nodes are at list.
func linearPager(doc, list string) pager {
	return func(t *testing.T, g *rig) ([]string, []string) {
		run := func(args string) string {
			return g.ok(t, call{origin: linearOrigin, method: "POST", path: "/graphql", body: linearBody("P", "query P { "+fmt.Sprintf(doc, args)+" }", nil)})
		}
		info := strings.TrimSuffix(list, "nodes[].id") + "pageInfo."
		paged := byCursor(t, func(cursor string) ([]string, string) {
			args := "first: 1"
			if cursor != "" {
				args += `, after: "` + cursor + `"`
			}
			body := run(args)
			if ids(t, body, info+"hasNextPage")[0] != "true" {
				return ids(t, body, list), ""
			}
			return ids(t, body, list), ids(t, body, info+"endCursor")[0]
		})
		return paged, ids(t, run("first: 50"), list)
	}
}

// pagedRows are every paged route and connection; TestPaging fails for
// one with no row.
var pagedRows = map[string]pager{
	"GET rest/api/3/user/search":                                  offsetPager("/rest/api/3/user/search?query=user&", "[].accountId"),
	"GET rest/api/3/user/assignable/search":                       offsetPager("/rest/api/3/user/assignable/search?project=WEB&", "[].accountId"),
	"GET rest/api/3/issue/createmeta/{project}/issuetypes":        offsetPager("/rest/api/3/issue/createmeta/WEB/issuetypes?", "issueTypes[].id"),
	"GET rest/api/3/issue/createmeta/{project}/issuetypes/{type}": offsetPager("/rest/api/3/issue/createmeta/WEB/issuetypes/10001?", "fields[].fieldId"),
	"GET rest/api/3/project/search":                               offsetPager("/rest/api/3/project/search?", "values[].key"),
	"GET rest/api/3/priority/search":                              offsetPager("/rest/api/3/priority/search?", "values[].id"),
	"GET rest/api/3/issue/{issue}/changelog":                      offsetPager("/rest/api/3/issue/WEB-1/changelog?", "values[].id"),
	"GET rest/api/3/issue/{issue}/comment":                        offsetPager("/rest/api/3/issue/WEB-1/comment?", "comments[].id"),
	"GET rest/api/3/issue/{issue}/worklog":                        offsetPager("/rest/api/3/issue/WEB-1/worklog?", "worklogs[].id"),
	"GET rest/agile/1.0/board":                                    offsetPager("/rest/agile/1.0/board?", "values[].id"),
	"GET rest/agile/1.0/board/{board}/sprint":                     offsetPager("/rest/agile/1.0/board/1/sprint?", "values[].id"),
	"POST rest/api/3/search/jql":                                  tokenPager("POST", "/rest/api/3/search/jql", `{"jql":"project = WEB ORDER BY key ASC","fields":["summary"]}`, "issues[].key"),
	"POST rest/api/3/changelog/bulkfetch":                         tokenPager("POST", "/rest/api/3/changelog/bulkfetch", `{"issueIdsOrKeys":["WEB-1"]}`, "issueChangeLogs[].changeHistories[].id"),
	"GET rest/software/1.0/board/{board}/issue":                   tokenPager("GET", "/rest/software/1.0/board/1/issue?", "", "issues[].key"),
	"GET rest/software/1.0/board/{board}/backlog":                 tokenPager("GET", "/rest/software/1.0/board/1/backlog?", "", "issues[].key"),
	"GET rest/software/1.0/sprint/{sprint}/issue":                 tokenPager("GET", "/rest/software/1.0/sprint/1/issue?", "", "issues[].key"),
	"GET rest/software/1.0/board/{board}/sprint/{sprint}/issue":   tokenPager("GET", "/rest/software/1.0/board/1/sprint/1/issue?", "", "issues[].key"),
	"GET rest/software/1.0/epic/{epic}/issue":                     tokenPager("GET", "/rest/software/1.0/epic/WEB-1/issue?", "", "issues[].key"),
	"GET 4/worklogs/user/{account}":                               tempoPager("GET", "/4/worklogs/user/acc-user1?from=2026-11-01&to=2026-11-30&", "", "results[].tempoWorklogId"),
	"GET 4/work-attributes":                                       tempoPager("GET", "/4/work-attributes?", "", "results[].key"),
	"POST 4/accounts/search":                                      tempoPager("POST", "/4/accounts/search?", `{}`, "results[].key"),
	"Query.users":                                                 linearPager("users(%s) { nodes { id } pageInfo { hasNextPage endCursor } }", "data.users.nodes[].id"),
	"Query.teams":                                                 linearPager("teams(%s) { nodes { id } pageInfo { hasNextPage endCursor } }", "data.teams.nodes[].id"),
	"Query.workflowStates":                                        linearPager("workflowStates(%s) { nodes { id } pageInfo { hasNextPage endCursor } }", "data.workflowStates.nodes[].id"),
	"Query.cycles":                                                linearPager("cycles(%s) { nodes { id } pageInfo { hasNextPage endCursor } }", "data.cycles.nodes[].id"),
	"Query.issueLabels":                                           linearPager("issueLabels(%s) { nodes { id } pageInfo { hasNextPage endCursor } }", "data.issueLabels.nodes[].id"),
	"Query.projects":                                              linearPager("projects(%s) { nodes { id } pageInfo { hasNextPage endCursor } }", "data.projects.nodes[].id"),
	"Query.issues":                                                linearPager("issues(%s) { nodes { id } pageInfo { hasNextPage endCursor } }", "data.issues.nodes[].id"),
	"Query.searchIssues":                                          linearPager(`searchIssues(term: "issue", %s) { nodes { id } pageInfo { hasNextPage endCursor } }`, "data.searchIssues.nodes[].id"),
	"Team.states":                                                 linearPager(`team(id: "t1") { states(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.team.states.nodes[].id"),
	"Team.labels":                                                 linearPager(`team(id: "t1") { labels(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.team.labels.nodes[].id"),
	"Team.members":                                                linearPager(`team(id: "t1") { members(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.team.members.nodes[].id"),
	"Issue.labels":                                                linearPager(`issue(id: "i1") { labels(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.issue.labels.nodes[].id"),
	"Issue.children":                                              linearPager(`issue(id: "i1") { children(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.issue.children.nodes[].id"),
	"Issue.comments":                                              linearPager(`issue(id: "i1") { comments(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.issue.comments.nodes[].id"),
	"Issue.attachments":                                           linearPager(`issue(id: "i1") { attachments(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.issue.attachments.nodes[].id"),
	"Issue.relations":                                             linearPager(`issue(id: "i1") { relations(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.issue.relations.nodes[].id"),
	"Issue.inverseRelations":                                      linearPager(`issue(id: "i1") { inverseRelations(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.issue.inverseRelations.nodes[].id"),
	"Issue.history":                                               linearPager(`issue(id: "i1") { history(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.issue.history.nodes[].id"),
	"Issue.stateHistory":                                          linearPager(`issue(id: "i1") { stateHistory(%s) { nodes { id } pageInfo { hasNextPage endCursor } } }`, "data.issue.stateHistory.nodes[].id"),
}

// TestPaging reads every paged route and connection one record a page,
// past the first page to the last, and requires the whole list in order.
func TestPaging(t *testing.T) {
	s := linearSchema(t)
	var names []string
	for _, rs := range [][]string{routeNamesWith(jiraRoutes), routeNamesWith(tempoRoutes)} {
		names = append(names, rs...)
	}
	names = append(names, "POST rest/api/3/search/jql", "POST rest/api/3/changelog/bulkfetch", "POST 4/accounts/search")
	for typ, fields := range linearTypes {
		if typ == "IssueSearchResult" {
			continue // Issue's table, read by the same connection code
		}
		for _, name := range fields.names() {
			if def := s.Types[typ].Fields.ForName(name); def != nil && isConnection(def.Type.Name()) {
				names = append(names, typ+"."+name)
			}
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			p, ok := pagedRows[name]
			if !ok {
				t.Fatalf("no paging row for %s", name)
			}
			paged, all := p(t, newRig(t, pagingFixture()))
			if len(all) < 3 || !slices.Equal(paged, all) {
				t.Fatalf("one a page read %v of %v", paged, all)
			}
		})
	}
	if len(pagedRows) != len(names) {
		t.Fatalf("%d paging rows for %d paged routes and connections", len(pagedRows), len(names))
	}
}

// routeNamesWith are the routes paged by a query parameter.
func routeNamesWith[C any](routes []route[C]) []string {
	var out []string
	for _, r := range routes {
		if slices.ContainsFunc(r.query, func(q string) bool { return q == "startAt" || q == "offset" || q == "nextPageToken" }) {
			out = append(out, r.name())
		}
	}
	return out
}

package trackerfake

import (
	"encoding/json"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func tempoReq(method, path, body string) call {
	return call{origin: tempoOrigin, method: method, path: path, body: body}
}

// readRows are one answered request per route that declares it only
// reads, and per Linear query root field; TestReadRoutesChangeNothing
// fails for one with no row.
var readRows = map[string]call{
	"GET _edge/tenant_info":                                         jiraReq("GET", "/_edge/tenant_info", ""),
	"GET rest/api/3/myself":                                         jiraReq("GET", "/rest/api/3/myself", ""),
	"GET rest/api/3/user":                                           jiraReq("GET", "/rest/api/3/user?accountId=acc-user1", ""),
	"GET rest/api/3/user/search":                                    jiraReq("GET", "/rest/api/3/user/search?query=user", ""),
	"GET rest/api/3/user/assignable/search":                         jiraReq("GET", "/rest/api/3/user/assignable/search?project=WEB", ""),
	"GET rest/api/3/field":                                          jiraReq("GET", "/rest/api/3/field", ""),
	"GET rest/api/3/issue/createmeta/{project}/issuetypes":          jiraReq("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes", ""),
	"GET rest/api/3/issue/createmeta/{project}/issuetypes/{type}":   jiraReq("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/10001", ""),
	"GET rest/api/3/project/search":                                 jiraReq("GET", "/rest/api/3/project/search", ""),
	"GET rest/api/3/project/{project}":                              jiraReq("GET", "/rest/api/3/project/WEB", ""),
	"GET rest/api/3/project/{project}/statuses":                     jiraReq("GET", "/rest/api/3/project/WEB/statuses", ""),
	"GET rest/api/3/status":                                         jiraReq("GET", "/rest/api/3/status", ""),
	"GET rest/api/3/priority/search":                                jiraReq("GET", "/rest/api/3/priority/search", ""),
	"GET rest/api/3/issueLinkType":                                  jiraReq("GET", "/rest/api/3/issueLinkType", ""),
	"GET rest/api/3/issue/{issue}":                                  jiraReq("GET", "/rest/api/3/issue/WEB-1", ""),
	"GET rest/api/3/issue/{issue}/editmeta":                         jiraReq("GET", "/rest/api/3/issue/WEB-1/editmeta", ""),
	"GET rest/api/3/issue/{issue}/transitions":                      jiraReq("GET", "/rest/api/3/issue/WEB-1/transitions", ""),
	"POST rest/api/3/search/jql":                                    jiraReq("POST", "/rest/api/3/search/jql", `{"jql":"project = WEB","fields":["summary"]}`),
	"POST rest/api/3/search/approximate-count":                      jiraReq("POST", "/rest/api/3/search/approximate-count", `{"jql":"project = WEB"}`),
	"POST rest/api/3/issue/bulkfetch":                               jiraReq("POST", "/rest/api/3/issue/bulkfetch", `{"issueIdsOrKeys":["WEB-1"]}`),
	"POST rest/api/3/jql/match":                                     jiraReq("POST", "/rest/api/3/jql/match", `{"issueIds":[10001],"jqls":["project = WEB"]}`),
	"GET rest/api/3/issue/{issue}/changelog":                        jiraReq("GET", "/rest/api/3/issue/WEB-1/changelog", ""),
	"POST rest/api/3/changelog/bulkfetch":                           jiraReq("POST", "/rest/api/3/changelog/bulkfetch", `{"issueIdsOrKeys":["WEB-1"]}`),
	"GET rest/api/3/issue/{issue}/comment":                          jiraReq("GET", "/rest/api/3/issue/WEB-1/comment", ""),
	"GET rest/api/3/issue/{issue}/comment/{id}":                     jiraReq("GET", "/rest/api/3/issue/WEB-1/comment/20001", ""),
	"GET rest/api/3/issue/{issue}/remotelink":                       jiraReq("GET", "/rest/api/3/issue/WEB-1/remotelink", ""),
	"GET rest/api/3/issue/{issue}/watchers":                         jiraReq("GET", "/rest/api/3/issue/WEB-1/watchers", ""),
	"GET rest/api/3/attachment/content/{id}":                        jiraReq("GET", "/rest/api/3/attachment/content/60001?redirect=false", ""),
	"GET rest/api/3/issue/{issue}/worklog":                          jiraReq("GET", "/rest/api/3/issue/WEB-1/worklog", ""),
	"GET rest/api/3/issue/{issue}/worklog/{id}":                     jiraReq("GET", "/rest/api/3/issue/WEB-1/worklog/30001", ""),
	"GET rest/api/3/worklog/updated":                                jiraReq("GET", "/rest/api/3/worklog/updated?since=0", ""),
	"GET rest/api/3/worklog/deleted":                                jiraReq("GET", "/rest/api/3/worklog/deleted?since=0", ""),
	"POST rest/api/3/worklog/list":                                  jiraReq("POST", "/rest/api/3/worklog/list", `{"ids":[30001]}`),
	"GET rest/agile/1.0/board":                                      jiraReq("GET", "/rest/agile/1.0/board", ""),
	"GET rest/agile/1.0/board/{board}":                              jiraReq("GET", "/rest/agile/1.0/board/1", ""),
	"GET rest/agile/1.0/board/{board}/configuration":                jiraReq("GET", "/rest/agile/1.0/board/1/configuration", ""),
	"GET rest/agile/1.0/board/{board}/sprint":                       jiraReq("GET", "/rest/agile/1.0/board/1/sprint", ""),
	"GET rest/agile/1.0/sprint/{sprint}":                            jiraReq("GET", "/rest/agile/1.0/sprint/1", ""),
	"GET rest/software/1.0/board/{board}/issue":                     jiraReq("GET", "/rest/software/1.0/board/1/issue", ""),
	"GET rest/software/1.0/board/{board}/backlog":                   jiraReq("GET", "/rest/software/1.0/board/1/backlog", ""),
	"GET rest/software/1.0/sprint/{sprint}/issue":                   jiraReq("GET", "/rest/software/1.0/sprint/1/issue", ""),
	"GET rest/software/1.0/board/{board}/sprint/{sprint}/issue":     jiraReq("GET", "/rest/software/1.0/board/1/sprint/1/issue", ""),
	"GET rest/software/1.0/epic/{epic}/issue":                       jiraReq("GET", "/rest/software/1.0/epic/WEB-1/issue", ""),
	"GET rest/software/1.0/board/{board}/issue/approximate-count":   jiraReq("GET", "/rest/software/1.0/board/1/issue/approximate-count", ""),
	"GET rest/software/1.0/board/{board}/backlog/approximate-count": jiraReq("GET", "/rest/software/1.0/board/1/backlog/approximate-count", ""),
	"GET 4/worklogs/user/{account}":                                 tempoReq("GET", "/4/worklogs/user/acc-user1?from=2026-11-01&to=2026-11-30", ""),
	"GET 4/worklogs/{id}":                                           tempoReq("GET", "/4/worklogs/501", ""),
	"GET 4/work-attributes":                                         tempoReq("GET", "/4/work-attributes", ""),
	"GET 4/globalconfiguration":                                     tempoReq("GET", "/4/globalconfiguration", ""),
	"POST 4/accounts/search":                                        tempoReq("POST", "/4/accounts/search", `{}`),
	"GET 4/user-schedule/{account}":                                 tempoReq("GET", "/4/user-schedule/acc-user1?from=2026-11-16&to=2026-11-22", ""),
	"GET 4/timesheet-approvals/user/{account}":                      tempoReq("GET", "/4/timesheet-approvals/user/acc-user1?from=2026-11-16&to=2026-11-22", ""),
	"Query.viewer":                                                  linearReq("R", `query R { viewer { id } }`),
	"Query.organization":                                            linearReq("R", `query R { organization { __typename } }`),
	"Query.users":                                                   linearReq("R", `query R { users { nodes { id } } }`),
	"Query.teams":                                                   linearReq("R", `query R { teams { nodes { id } } }`),
	"Query.workflowStates":                                          linearReq("R", `query R { workflowStates { nodes { id } } }`),
	"Query.cycles":                                                  linearReq("R", `query R { cycles { nodes { id } } }`),
	"Query.issueLabels":                                             linearReq("R", `query R { issueLabels { nodes { id } } }`),
	"Query.projects":                                                linearReq("R", `query R { projects { nodes { id } } }`),
	"Query.issues":                                                  linearReq("R", `query R { issues { nodes { id } } }`),
	"Query.issue":                                                   linearReq("R", `query R { issue(id: "i1") { id } }`),
	"Query.comment":                                                 linearReq("R", `query R { comment(id: "c2-comment") { id } }`),
	"Query.issueRelation":                                           linearReq("R", `query R { issueRelation(id: "r2") { id } }`),
	"Query.cycle":                                                   linearReq("R", `query R { cycle(id: "cy1") { id } }`),
	"Query.team":                                                    linearReq("R", `query R { team(id: "t1") { id } }`),
	"Query.searchIssues":                                            linearReq("R", `query R { searchIssues(term: "issue") { nodes { id } } }`),
}

// worldState is what a request can change: the tenants, the ID counter
// and the clock.
type worldState struct {
	snapshot string
	ids      int64
	last     string
}

func (g *rig) state(t *testing.T) worldState {
	t.Helper()
	raw, err := json.Marshal(g.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	g.e.mu.Lock()
	defer g.e.mu.Unlock()
	return worldState{snapshot: string(raw), ids: g.e.ids, last: g.e.last.String()}
}

// TestReadRoutesChangeNothing drives every route that declares it only
// reads, and every Linear query root field, since the pipeline saves no
// copy of the tenant for them: each must answer and leave the tenants,
// the ID counter and the clock as they were.
func TestReadRoutesChangeNothing(t *testing.T) {
	var names []string
	for _, r := range jiraRoutes {
		if r.reads {
			names = append(names, r.name())
		}
	}
	for _, r := range tempoRoutes {
		if r.reads {
			names = append(names, r.name())
		}
	}
	for _, name := range queryFields.names() {
		names = append(names, "Query."+name)
	}
	slices.Sort(names)
	f := pagingFixture()
	f.Jira[0].Issues[0].Attachments = []JiraAttachment{{ID: "60001", Filename: "a.txt", MimeType: "text/plain", Content: []byte("a"), Author: "acc-user1"}}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			c, ok := readRows[name]
			if !ok {
				t.Fatalf("no read row for %s", name)
			}
			g := newRig(t, f)
			before := g.state(t)
			rec := g.do(c)
			// A Linear answer of 200 that carries errors did not answer.
			if rec.Code >= 300 || c.origin == linearOrigin && strings.Contains(rec.Body.String(), `"errors":[`) {
				t.Fatalf("answered %d %s", rec.Code, rec.Body)
			}
			if route := g.e.Requests(0).Requests[0].Route; c.origin != linearOrigin && route != name {
				t.Fatalf("answered by %q", route)
			}
			if after := g.state(t); after != before {
				t.Fatalf("the read changed the world:\n got %+v\nwant %+v", after, before)
			}
		})
	}
	if len(readRows) != len(names) {
		t.Fatalf("%d read rows for %d read routes and query fields", len(readRows), len(names))
	}
}

// TestReadCostDoesNotGrowWithTheTenant: a read of constant size
// allocates as much on a tenant of 5,000 issues as on one of none, on
// every tracker, since a read saves no copy of the tenant.
func TestReadCostDoesNotGrowWithTheTenant(t *testing.T) {
	reads := []call{
		jiraReq("GET", "/rest/api/3/myself", ""),
		tempoReq("GET", "/4/globalconfiguration", ""),
		linearReq("Viewer", viewerQuery),
	}
	const runs = 20
	perRead := func(issues int) []uint64 {
		f := testFixture()
		for i := range issues {
			n := strconv.Itoa(i + 2)
			f.Jira[0].Issues = append(f.Jira[0].Issues, JiraIssue{Key: "WEB-" + n, Project: "WEB", Type: "10001", Summary: "Issue " + n})
			f.Linear[0].Issues = append(f.Linear[0].Issues, LinearIssue{Team: "t1", Number: float64(i + 1), Title: "Issue " + n, State: "s1"})
		}
		g := newRig(t, f)
		out := make([]uint64, len(reads))
		for i, c := range reads {
			g.do(c)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range runs {
				if rec := g.do(c); rec.Code != 200 {
					t.Fatalf("%s %s answered %d %s", c.method, c.path, rec.Code, rec.Body)
				}
			}
			runtime.ReadMemStats(&after)
			out[i] = (after.TotalAlloc - before.TotalAlloc) / runs
		}
		return out
	}
	empty, large := perRead(0), perRead(5000)
	for i, c := range reads {
		t.Logf("%s %s: %d bytes a request with no issues, %d with 5,000", c.method, c.path, empty[i], large[i])
		if large[i] > empty[i]+16<<10 {
			t.Errorf("%s %s allocates %d bytes a request on 5,000 issues, %d on none", c.method, c.path, large[i], empty[i])
		}
	}
}

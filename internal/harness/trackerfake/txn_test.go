package trackerfake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// rig is an engine answered in process, with no listener.
type rig struct {
	e *Engine
	h map[Origin]http.Handler
}

func newRig(t *testing.T, f Fixture) *rig {
	t.Helper()
	g := &rig{e: New(Options{Now: func() time.Time { return testStart }}), h: map[Origin]http.Handler{}}
	t.Cleanup(g.e.Close)
	if err := g.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	for _, o := range []Origin{jiraOrigin, tempoOrigin, linearOrigin} {
		h, err := g.e.Handler(o)
		if err != nil {
			t.Fatal(err)
		}
		g.h[o] = h
	}
	return g
}

// call is one request to o carrying o's credential.
type call struct {
	origin             Origin
	method, path, body string
	header             map[string]string
}

func (g *rig) do(c call) *httptest.ResponseRecorder {
	req := httptest.NewRequest(c.method, "http://"+testSite+c.path, strings.NewReader(c.body))
	switch c.origin {
	case jiraOrigin:
		req.Header.Set("Authorization", basic(testJiraToken)["Authorization"])
	case tempoOrigin:
		req.Header.Set("Authorization", "Bearer "+testTempo)
	case linearOrigin:
		req.Header.Set("Authorization", testLinearKey)
	}
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.h[c.origin].ServeHTTP(rec, req)
	return rec
}

func jiraReq(method, path, body string) call {
	return call{origin: jiraOrigin, method: method, path: path, body: body}
}

func linearReq(op, query string) call {
	return call{origin: linearOrigin, method: "POST", path: "/graphql", body: linearBody(op, query, nil)}
}

// txnFixture has a record of each kind the write routes name.
func txnFixture() Fixture {
	f := testFixture()
	f.Jira[0].Issues[0].Comments = []JiraComment{{ID: "20001", Author: "acc-user1", Body: json.RawMessage(testDoc), Created: testStart}}
	f.Linear[0].Issues = []LinearIssue{
		{ID: "i1", Team: "t1", Number: 1, Title: "First", State: "s1"},
		{ID: "i2", Team: "t1", Number: 2, Title: "Second", State: "s1"},
	}
	return f
}

// probes are the writes after which twin engines must agree: each makes
// a record with a generated ID, stamped with the clock.
var probes = []call{
	jiraReq("POST", "/rest/api/3/issue/WEB-1/comment", `{"body":`+testDoc+`}`),
	linearReq("Probe", `mutation Probe { commentCreate(input: {issueId: "i1", body: "probe"}) { success } }`),
}

// sameWorld fails unless g and twin hold the same state and generate the
// same next IDs and stamps.
func sameWorld(t *testing.T, g, twin *rig) {
	t.Helper()
	for _, p := range probes {
		for _, r := range []*rig{g, twin} {
			if rec := r.do(p); rec.Code >= 300 {
				t.Fatalf("probe %s %s answered %d %s", p.method, p.path, rec.Code, rec.Body)
			}
		}
	}
	a, err := json.Marshal(g.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(twin.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("the refused request changed the world:\n got %s\nwant %s", a, b)
	}
}

// refusals is one refused request per route that may write, and per
// mutation; TestEveryRefusalChangesNothing fails for one with no row.
var refusals = map[string]call{
	"POST rest/api/3/issue":                           jiraReq("POST", "/rest/api/3/issue", `{"fields":{"project":{"key":"NOPE"},"issuetype":{"id":"10001"},"summary":"x"}}`),
	"PUT rest/api/3/issue/{issue}":                    jiraReq("PUT", "/rest/api/3/issue/WEB-1", `{"fields":{"summary":"Changed","customfield_19999":1}}`),
	"PUT rest/api/3/issue/{issue}/assignee":           jiraReq("PUT", "/rest/api/3/issue/WEB-1/assignee", `{"accountId":"acc-nobody"}`),
	"POST rest/api/3/issue/{issue}/transitions":       jiraReq("POST", "/rest/api/3/issue/WEB-1/transitions", `{"transition":{"id":"999"}}`),
	"POST rest/api/3/search/jql":                      jiraReq("POST", "/rest/api/3/search/jql", `{"jql":"id < 10"}`),
	"POST rest/api/3/search/approximate-count":        jiraReq("POST", "/rest/api/3/search/approximate-count", `{"jql":"id < 10"}`),
	"POST rest/api/3/issue/bulkfetch":                 jiraReq("POST", "/rest/api/3/issue/bulkfetch", `{"issueIdsOrKeys":[]}`),
	"POST rest/api/3/changelog/bulkfetch":             jiraReq("POST", "/rest/api/3/changelog/bulkfetch", `{"issueIdsOrKeys":[]}`),
	commentRoute:                                      jiraReq("POST", "/rest/api/3/issue/WEB-404/comment", `{"body":`+testDoc+`}`),
	"PUT rest/api/3/issue/{issue}/comment/{id}":       jiraReq("PUT", "/rest/api/3/issue/WEB-1/comment/29999", `{"body":`+testDoc+`}`),
	"DELETE rest/api/3/issue/{issue}/comment/{id}":    jiraReq("DELETE", "/rest/api/3/issue/WEB-1/comment/29999", ""),
	"POST rest/api/3/issueLink":                       jiraReq("POST", "/rest/api/3/issueLink", `{"type":{"name":"Nope"},"inwardIssue":{"key":"WEB-1"},"outwardIssue":{"key":"WEB-404"}}`),
	"DELETE rest/api/3/issueLink/{id}":                jiraReq("DELETE", "/rest/api/3/issueLink/29999", ""),
	"POST rest/api/3/issue/{issue}/remotelink":        jiraReq("POST", "/rest/api/3/issue/WEB-404/remotelink", `{"object":{"url":"https://example.com/a","title":"A"}}`),
	"DELETE rest/api/3/issue/{issue}/remotelink/{id}": jiraReq("DELETE", "/rest/api/3/issue/WEB-1/remotelink/29999", ""),
	"POST rest/api/3/issue/{issue}/watchers":          jiraReq("POST", "/rest/api/3/issue/WEB-1/watchers", `"acc-nobody"`),
	"DELETE rest/api/3/issue/{issue}/watchers":        jiraReq("DELETE", "/rest/api/3/issue/WEB-404/watchers?accountId=acc-user1", ""),
	"POST rest/api/3/issue/{issue}/attachments":       jiraReq("POST", "/rest/api/3/issue/WEB-1/attachments", ""),
	"DELETE rest/api/3/attachment/{id}":               jiraReq("DELETE", "/rest/api/3/attachment/29999", ""),
	"POST rest/api/3/issue/{issue}/worklog":           jiraReq("POST", "/rest/api/3/issue/WEB-404/worklog", `{"timeSpentSeconds":60,"started":"2026-11-19T09:00:00.000+0000"}`),
	"PUT rest/api/3/issue/{issue}/worklog/{id}":       jiraReq("PUT", "/rest/api/3/issue/WEB-1/worklog/29999", `{"timeSpentSeconds":60}`),
	"DELETE rest/api/3/issue/{issue}/worklog/{id}":    jiraReq("DELETE", "/rest/api/3/issue/WEB-1/worklog/29999", ""),
	"POST rest/api/3/worklog/list":                    jiraReq("POST", "/rest/api/3/worklog/list", `{"ids":[1],"bogus":1}`),
	"POST rest/agile/1.0/sprint":                      jiraReq("POST", "/rest/agile/1.0/sprint", `{"name":"S","originBoardId":999}`),
	"POST rest/agile/1.0/sprint/{sprint}":             jiraReq("POST", "/rest/agile/1.0/sprint/999", `{"name":"x"}`),
	"DELETE rest/agile/1.0/sprint/{sprint}":           jiraReq("DELETE", "/rest/agile/1.0/sprint/999", ""),
	"POST rest/agile/1.0/sprint/{sprint}/issue":       jiraReq("POST", "/rest/agile/1.0/sprint/999/issue", `{"issues":["WEB-1"]}`),
	"POST rest/agile/1.0/backlog/issue":               jiraReq("POST", "/rest/agile/1.0/backlog/issue", `{"issues":["WEB-1"],"bogus":1}`),
	"POST rest/agile/1.0/backlog/{board}/issue":       jiraReq("POST", "/rest/agile/1.0/backlog/999/issue", `{"issues":["WEB-1"]}`),
	"PUT rest/agile/1.0/issue/rank":                   jiraReq("PUT", "/rest/agile/1.0/issue/rank", `{"issues":["WEB-1"]}`),
	"PUT rest/agile/1.0/issue/{issue}/estimation":     jiraReq("PUT", "/rest/agile/1.0/issue/WEB-1/estimation?boardId=999", `{"value":"bad"}`),
	"POST 4/worklogs":                                 {origin: tempoOrigin, method: "POST", path: "/4/worklogs", body: `{"issueId":99999,"timeSpentSeconds":60,"startDate":"2026-11-19","authorAccountId":"acc-user1"}`},
	"PUT 4/worklogs/{id}":                             {origin: tempoOrigin, method: "PUT", path: "/4/worklogs/99999", body: `{"timeSpentSeconds":60}`},
	"DELETE 4/worklogs/{id}":                          {origin: tempoOrigin, method: "DELETE", path: "/4/worklogs/99999"},
	"POST 4/accounts/search":                          {origin: tempoOrigin, method: "POST", path: "/4/accounts/search", body: `{"bogus":1}`},
	// A Linear mutation's refusal answers 200 with an error at the field.
	"issueCreate":                  linearReq("M", `mutation M { issueCreate(input: {teamId: "t1", title: "New", stateId: "missing"}) { success } }`),
	"issueUpdate":                  linearReq("M", `mutation M { issueUpdate(id: "i1", input: {title: "Changed", labelIds: ["missing"]}) { success } }`),
	"issueBatchUpdate":             linearReq("M", `mutation M { issueBatchUpdate(ids: ["i1", "missing"], input: {title: "Changed"}) { success } }`),
	"issueArchive":                 linearReq("M", `mutation M { issueArchive(id: "missing") { success } }`),
	"issueUnarchive":               linearReq("M", `mutation M { issueUnarchive(id: "missing") { success } }`),
	"issueDelete":                  linearReq("M", `mutation M { issueDelete(id: "missing") { success } }`),
	"commentCreate":                linearReq("M", `mutation M { commentCreate(input: {issueId: "i1", body: " "}) { success } }`),
	"commentUpdate":                linearReq("M", `mutation M { commentUpdate(id: "missing", input: {body: "x"}) { success } }`),
	"commentDelete":                linearReq("M", `mutation M { commentDelete(id: "missing") { success } }`),
	"issueRelationCreate":          linearReq("M", `mutation M { issueRelationCreate(input: {issueId: "i1", relatedIssueId: "i1", type: related}) { success } }`),
	"issueRelationDelete":          linearReq("M", `mutation M { issueRelationDelete(id: "missing") { success } }`),
	"attachmentCreate":             linearReq("M", `mutation M { attachmentCreate(input: {issueId: "i1", url: "", title: "x"}) { success } }`),
	"cycleStartUpcomingCycleToday": linearReq("M", `mutation M { cycleStartUpcomingCycleToday(id: "missing") { success } }`),
}

// TestEveryRefusalChangesNothing sends each refusal to one of two twin
// engines and requires them to agree afterwards: state, IDs and stamps.
func TestEveryRefusalChangesNothing(t *testing.T) {
	var names []string
	for _, r := range jiraRoutes {
		if r.method != "GET" {
			names = append(names, r.name())
		}
	}
	for _, r := range tempoRoutes {
		if r.method != "GET" {
			names = append(names, r.name())
		}
	}
	for name := range mutations {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			c, ok := refusals[name]
			if !ok {
				t.Fatalf("no refusal row for %s", name)
			}
			g, twin := newRig(t, txnFixture()), newRig(t, txnFixture())
			rec := g.do(c)
			refused := rec.Code >= 400 || (c.origin == linearOrigin && strings.Contains(rec.Body.String(), `"errors"`))
			if !refused {
				t.Fatalf("answered %d %s, not a refusal", rec.Code, rec.Body)
			}
			sameWorld(t, g, twin)
		})
	}
	if len(refusals) != len(names) {
		t.Fatalf("%d refusal rows for %d routes and mutations", len(refusals), len(names))
	}
}

// TestRefusedSeedKeepsIDsAndClock: a fixture whose normalization takes
// IDs, reserves a seeded one and reads the clock, and is then refused,
// leaves the next IDs and stamps as an engine that never saw it makes
// them, though its clock read later than the writes that follow.
func TestRefusedSeedKeepsIDsAndClock(t *testing.T) {
	g, twin := newRig(t, txnFixture()), newRig(t, txnFixture())
	bad := testFixture()
	site := &bad.Jira[0]
	site.Site, site.Token, site.Tempo = "other.example.com", "other-token-1", nil
	site.Issues = []JiraIssue{{Key: "WEB-1", Project: "WEB", Type: "10001", Summary: "Generated ID"},
		{ID: "5000000", Key: "WEB-2", Project: "WEB", Type: "10001", Summary: "Reserved ID"}}
	bad.Linear[0].URLKey, bad.Linear[0].APIKey, bad.Linear[0].Viewer = "other", "lin_api_other01", "nobody"
	bad.Linear[0].Teams = append(bad.Linear[0].Teams, LinearTeam{Key: "APP", Name: "Generated ID"})
	g.e.opts.Now = func() time.Time { return testStart.Add(time.Hour) }
	if err := g.e.Seed(bad); err == nil || !strings.Contains(err.Error(), "viewer") {
		t.Fatalf("the late-failing fixture: %v", err)
	}
	g.e.opts.Now = func() time.Time { return testStart }
	sameWorld(t, g, twin)
}

// TestLinearMutationDocumentRollsBackWhole: a document with a refused
// mutation changes nothing, its other mutations included (measured:
// three aliased issueUpdate fields, the middle one naming a missing
// issue, answered data null and applied neither other).
func TestLinearMutationDocumentRollsBackWhole(t *testing.T) {
	g, twin := newRig(t, txnFixture()), newRig(t, txnFixture())
	rec := g.do(linearReq("M", `mutation M {
		a: issueUpdate(id: "i1", input: {title: "A"}) { success }
		b: issueUpdate(id: "i2", input: {title: "B", labelIds: ["missing"]}) { success }
	}`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"path":["b"]`) || !strings.Contains(rec.Body.String(), `"data":null`) {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
	sameWorld(t, g, twin)
}

// TestEstimationRefusalKeepsTheValue runs the estimation handler alone,
// outside the pipeline's transaction: a value it cannot read is refused
// before the estimate it would replace is touched.
func TestEstimationRefusalKeepsTheValue(t *testing.T) {
	for _, field := range []string{"customfield_10016", "timeoriginalestimate"} {
		t.Run(field, func(t *testing.T) {
			f := testFixture()
			s := &f.Jira[0]
			s.Fields = []JiraField{{ID: "customfield_10016", Name: "Story points", Schema: JiraSchema{Type: "number",
				Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float", CustomID: 10016}}}
			s.Boards = []JiraBoard{{ID: 1, Name: "Board", Type: "scrum", Project: "WEB", Estimation: field}}
			secs := int64(3600)
			s.Issues[0].Values, s.Issues[0].OriginalEstimate = map[string]json.RawMessage{"customfield_10016": json.RawMessage(`5`)}, &secs
			g := newRig(t, f)
			site := g.e.jira[testSite]
			x := &exchange{r: httptest.NewRequest("PUT", "/rest/agile/1.0/issue/WEB-1/estimation?boardId=1", nil), body: []byte(`{"value":"bad"}`)}
			c := &jiraCall{e: g.e, s: site, x: x, params: map[string]string{"issue": "WEB-1"}, query: x.r.URL.Query(), viewer: site.Account}
			if a := setEstimation(c); a.status != 400 {
				t.Fatalf("answered %d %s", a.status, a.body)
			}
			is := site.issue("WEB-1")
			if string(is.Values["customfield_10016"]) != "5" || is.OriginalEstimate == nil || *is.OriginalEstimate != 3600 {
				t.Fatalf("the refusal changed the estimate: %s, %v", is.Values["customfield_10016"], is.OriginalEstimate)
			}
		})
	}
}

// testRoute is a write that answers as its {mode} says, after changing
// the site: a comment added, an ID and a stamp taken.
var testRoute = jiraRoute{method: "POST", pattern: api + "trackerfake-test/{mode}", handle: func(c *jiraCall) answer {
	c.s.addComment(c.e, c.s.issue("WEB-1"), c.viewer, json.RawMessage(testDoc), nil, false)
	switch c.params["mode"] {
	case "refuse":
		return jiraErrors(http.StatusBadRequest, "refused")
	case "unhandled":
		return unhandledf("the test route's unhandled mode")
	case "panic":
		var none *JiraIssue
		_ = none.Key
	}
	return emptyAnswer(http.StatusCreated)
}}

func withTestRoute(t *testing.T) {
	saved := jiraRoutes
	jiraRoutes = append(slices.Clone(jiraRoutes), testRoute)
	t.Cleanup(func() { jiraRoutes = saved })
}

// TestHandlerFailuresRollBack: a handler that changed state and then
// refuses, answers unhandled or panics leaves nothing behind; the panic
// is answered 501 naming it, and the engine keeps serving.
func TestHandlerFailuresRollBack(t *testing.T) {
	withTestRoute(t)
	for _, c := range []struct {
		mode   string
		status int
		detail string
	}{
		{"refuse", 400, ""},
		{"unhandled", 501, "unhandled mode"},
		{"panic", 501, "trackerfake panicked: runtime error: invalid memory address"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			g, twin := newRig(t, txnFixture()), newRig(t, txnFixture())
			var rec *httptest.ResponseRecorder
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("the panic reached the server: %v", p)
					}
				}()
				rec = g.do(jiraReq("POST", "/rest/api/3/trackerfake-test/"+c.mode, ""))
			}()
			locked := !g.e.mu.TryLock()
			g.e.mu.Unlock() // frees a lock left held, so the cleanup's Close returns
			if locked {
				t.Fatal("the engine's lock is still held")
			}
			if rec == nil || rec.Code != c.status {
				t.Fatalf("answered %+v", rec)
			}
			if c.detail != "" {
				un := g.e.Unhandled()
				if len(un) != 1 || !strings.Contains(un[0].Detail, c.detail) {
					t.Fatalf("unhandled %+v", un)
				}
			}
			sameWorld(t, g, twin)
		})
	}
	g := newRig(t, txnFixture())
	if rec := g.do(jiraReq("POST", "/rest/api/3/trackerfake-test/ok", "")); rec.Code != 201 || len(g.e.Snapshot().Jira[0].Issues[0].Comments) != 2 {
		t.Fatalf("a successful write answered %d and was not kept", rec.Code)
	}
}

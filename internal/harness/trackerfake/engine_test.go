package trackerfake

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The engine tests talk raw HTTP to Serve's loopback servers, so each
// checks the wire a client sees and the state and log the engine keeps.

const (
	testSite      = "example.atlassian.net"
	testJiraToken = "jira-token-0001"
	testTempo     = "tempo-token-0001"
	testLinearKey = "lin_api_key0001"
	testDoc       = `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"hi"}]}]}`
	commentRoute  = "POST rest/api/3/issue/{issue}/comment"
	myselfRoute   = "GET rest/api/3/myself"
)

var (
	jiraOrigin   = Origin{Tracker: TrackerJira, Site: testSite}
	tempoOrigin  = Origin{Tracker: TrackerTempo}
	linearOrigin = Origin{Tracker: TrackerLinear}
	testStart    = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func testFixture() Fixture {
	return Fixture{
		Jira: []JiraSite{{
			Site: testSite, Account: "acc-user1", Token: testJiraToken,
			Users:    []JiraUser{{AccountID: "acc-user1", DisplayName: "User 1", Email: "user1@example.com"}},
			Projects: []JiraProject{{ID: "10000", Key: "WEB", Name: "Web"}},
			Issues:   []JiraIssue{{ID: "10101", Key: "WEB-1", Project: "WEB", Type: "10001", Summary: "First"}},
			Tempo:    &TempoAccount{Token: testTempo},
		}},
		Linear: []LinearWorkspace{{
			URLKey: "example", Name: "Example", APIKey: testLinearKey, Viewer: "u1",
			Users:  []LinearUser{{ID: "u1", Name: "User 1"}},
			Teams:  []LinearTeam{{ID: "t1", Key: "WEB", Name: "Web"}},
			States: []LinearState{{ID: "s1", Team: "t1", Name: "Todo", Type: "unstarted"}},
			Labels: []LinearLabel{{ID: "l1", Name: "Everywhere"}},
		}},
	}
}

type world struct {
	e     *Engine
	srv   *Servers
	clock *testClock
	// seen receives every request OnRequest reports.
	seen chan Request
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{clock: &testClock{now: testStart}, seen: make(chan Request, 64)}
	w.e = New(Options{Now: w.clock.Now, OnRequest: func(r Request) {
		select {
		case w.seen <- r:
		default:
		}
	}})
	t.Cleanup(w.e.Close)
	if err := w.e.Seed(testFixture()); err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(w.e, testSite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	w.srv = srv
	return w
}

// reply is an answer as the wire carried it.
type reply struct {
	status int
	header http.Header
	body   string
}

// send makes one request; err is set when the connection closed with no
// answer.
func send(ctx context.Context, method, url string, header map[string]string, body string) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return reply{}, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return reply{}, err
	}
	b, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(b)}, err
}

func basic(token string) map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("user1@example.com:"+token))}
}

func (w *world) jira(t *testing.T, method, path, body string) reply {
	t.Helper()
	r, err := send(context.Background(), method, w.srv.JiraURL(testSite)+path, withJSON(basic(testJiraToken), body), body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func withJSON(h map[string]string, body string) map[string]string {
	if body != "" {
		h["Content-Type"] = "application/json"
	}
	return h
}

func (w *world) tempo(t *testing.T, path string) reply {
	t.Helper()
	r, err := send(context.Background(), "GET", w.srv.TempoURL()+path, map[string]string{"Authorization": "Bearer " + testTempo}, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func linearBody(op, query string, vars map[string]any) string {
	b, err := json.Marshal(map[string]any{"query": query, "operationName": op, "variables": vars})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (w *world) linear(t *testing.T, key, body string) reply {
	t.Helper()
	r, err := send(context.Background(), "POST", w.srv.LinearURL()+"/graphql", map[string]string{"Authorization": key, "Content-Type": "application/json"}, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const viewerQuery = `query Viewer { viewer { id name } }`

func (w *world) comments(t *testing.T) int {
	t.Helper()
	return len(w.e.Snapshot().Jira[0].Issues[0].Comments)
}

func (w *world) lastRequest(t *testing.T) Request {
	t.Helper()
	rs := w.e.Requests(0).Requests
	if len(rs) == 0 {
		t.Fatal("no request was logged")
	}
	return rs[len(rs)-1]
}

func TestUnhandledRequestsAnswer501AndAreRecorded(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct{ name, method, path, body, want string }{
		{"route", "GET", "/rest/api/3/dashboard", "", "dashboard"},
		{"method", "PATCH", "/rest/api/3/myself", "", "myself"},
		{"query parameter", "GET", "/rest/api/3/myself?expand=groups", "", "expand"},
		{"body member", "POST", "/rest/api/3/issue/WEB-1/comment", `{"body":` + testDoc + `,"bogus":1}`, "bogus"},
		{"jql", "POST", "/rest/api/3/search/jql", `{"jql":"assignee = currentUser()"}`, "assignee"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := w.jira(t, c.method, c.path, c.body)
			if r.status != http.StatusNotImplemented || !strings.Contains(r.body, "trackerfake: unhandled") {
				t.Fatalf("answered %d %s", r.status, r.body)
			}
			if got := w.lastRequest(t); !got.Unhandled || got.Route != "" || !strings.Contains(got.Detail, c.want) {
				t.Fatalf("logged %+v", got)
			}
		})
	}
	if n := len(w.e.Unhandled()); n != 5 {
		t.Fatalf("%d unhandled requests, want 5", n)
	}
	if w.comments(t) != 0 {
		t.Fatal("an unhandled write changed state")
	}
}

func TestLinearSchemaChecks(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct{ name, body, want string }{
		{"unknown field", linearBody("Viewer", `query Viewer { viewer { id shoeSize } }`, nil), "fails Linear's schema"},
		{"operation name", linearBody("Other", viewerQuery, nil), "fails Linear's schema"},
		{"variables", linearBody("Issue", `query Issue($id: String!) { issue(id: $id) { id } }`, map[string]any{"id": 7}), "variables fail"},
		{"unimplemented argument", linearBody("Teams", `query Teams { teams(orderBy: updatedAt) { nodes { id } } }`, nil), "orderBy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := w.linear(t, testLinearKey, c.body)
			if r.status != http.StatusNotImplemented || !strings.Contains(r.body, c.want) || !strings.Contains(r.body, "TRACKERFAKE_UNHANDLED") {
				t.Fatalf("answered %d %s", r.status, r.body)
			}
		})
	}
	r := w.linear(t, testLinearKey, linearBody("Viewer", viewerQuery, nil))
	if r.status != http.StatusOK || !strings.Contains(r.body, `"name":"User 1"`) {
		t.Fatalf("viewer answered %d %s", r.status, r.body)
	}
	r = w.linear(t, testLinearKey, linearBody("Teams", `query Teams { teams(first: 251) { nodes { id } } }`, nil))
	if want := `{"data":null,"errors":[{"extensions":{"code":"INVALID_INPUT","type":"invalid input","userError":true,` +
		`"userPresentableMessage":"first must not be greater than 250"},"message":"Argument Validation Error","path":["teams"]}]}`; r.status != http.StatusOK || r.body != want {
		t.Fatalf("a page over 250 answered %d %s", r.status, r.body)
	}
	r = w.linear(t, testLinearKey, linearBody("Issue", `query Issue($id: String!) { issue(id: $id) { id } }`, map[string]any{"id": "00000000-0000-4000-8000-0000000000ff"}))
	if r.status != http.StatusOK || !strings.Contains(r.body, `"data":null`) || !strings.Contains(r.body, `"message":"Entity not found: Issue"`) ||
		!strings.Contains(r.body, `"code":"INPUT_ERROR"`) || !strings.Contains(r.body, `"path":["issue"]`) {
		t.Fatalf("a missing issue answered %d %s", r.status, r.body)
	}
	labels := func(filter string) string {
		return w.linear(t, testLinearKey, linearBody("Labels", `query Labels { issueLabels(filter: `+filter+`) { nodes { name } } }`, nil)).body
	}
	if byTeam, none := labels(`{team: {id: {eq: "t1"}}}`), labels(`{team: {null: true}}`); strings.Contains(byTeam, "Everywhere") || !strings.Contains(none, "Everywhere") {
		t.Fatalf("workspace labels: by team %s, by no team %s", byTeam, none)
	}
}

func TestCredentialRefusals(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	jiraURL := w.srv.JiraURL(testSite)
	cases := []struct {
		name   string
		url    string
		header map[string]string
		body   string
		status int
		want   string
	}{
		{"jira none", jiraURL + "/rest/api/3/myself", nil, "", 401, "Client must be authenticated"},
		{"jira wrong token", jiraURL + "/rest/api/3/myself", basic("not-the-token"), "", 401, "Client must be authenticated"},
		{"jira anonymous issue read", jiraURL + "/rest/api/3/issue/WEB-1", nil, "", 404, "Issue does not exist"},
		{"tempo wrong token", w.srv.TempoURL() + "/4/work-attributes", map[string]string{"Authorization": "Bearer not-the-token"}, "", 401, ""},
		{"linear made-up key", w.srv.LinearURL() + "/graphql", map[string]string{"Authorization": "lin_api_madeup1"}, linearBody("Viewer", viewerQuery, nil), 401, "AUTHENTICATION_ERROR"},
		{"linear bearer prefix", w.srv.LinearURL() + "/graphql", map[string]string{"Authorization": "Bearer " + testLinearKey}, linearBody("Viewer", viewerQuery, nil), 400, "Bearer prefix"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			method := "GET"
			if c.body != "" {
				method = "POST"
			}
			r, err := send(ctx, method, c.url, c.header, c.body)
			if err != nil {
				t.Fatal(err)
			}
			if r.status != c.status || !strings.Contains(r.body, c.want) {
				t.Fatalf("answered %d %s", r.status, r.body)
			}
		})
	}
	if r, _ := send(ctx, "GET", jiraURL+"/rest/api/3/myself", basic("not-the-token"), ""); r.header.Get("X-Seraph-Loginreason") != "AUTHENTICATED_FAILED" {
		t.Fatalf("a refused token's login reason is %q", r.header.Get("X-Seraph-Loginreason"))
	}
}

func TestSetTokenRotates(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct {
		tracker Tracker
		tenant  string
	}{{TrackerJira, testSite}, {TrackerTempo, testSite}, {TrackerLinear, "example"}} {
		if err := w.e.SetToken(c.tracker, c.tenant, "rotated-"+string(c.tracker)+"-1"); err != nil {
			t.Fatal(err)
		}
	}
	if r := w.jira(t, "GET", "/rest/api/3/myself", ""); r.status != 401 {
		t.Fatalf("the old Jira token answered %d", r.status)
	}
	if r, _ := send(context.Background(), "GET", w.srv.JiraURL(testSite)+"/rest/api/3/myself", basic("rotated-jira-1"), ""); r.status != 200 {
		t.Fatalf("the new Jira token answered %d", r.status)
	}
	if r := w.tempo(t, "/4/work-attributes"); r.status != 401 {
		t.Fatalf("the old Tempo token answered %d", r.status)
	}
	if r := w.linear(t, testLinearKey, linearBody("Viewer", viewerQuery, nil)); r.status != 401 {
		t.Fatalf("the old Linear key answered %d", r.status)
	}
	if r := w.linear(t, "rotated-linear-1", linearBody("Viewer", viewerQuery, nil)); r.status != 200 {
		t.Fatalf("the new Linear key answered %d", r.status)
	}
	for _, bad := range []error{
		w.e.SetToken(TrackerJira, "other.example.com", "long-enough-1"),
		w.e.SetToken(TrackerLinear, "example", "short"),
		w.e.SetToken("github", testSite, "long-enough-1"),
	} {
		if bad == nil {
			t.Fatal("SetToken took a token it should refuse")
		}
	}
}

func TestCredentialsNeverLogged(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// A request that repeats credentials in its path, query and body, and
	// one presenting a credential the engine does not know.
	_, _ = send(ctx, "POST", w.srv.JiraURL(testSite)+"/rest/api/3/issue/"+testJiraToken+"/comment?q="+testTempo,
		withJSON(basic(testJiraToken), "x"), `{"body":"`+testLinearKey+`"}`)
	_, _ = send(ctx, "GET", w.srv.JiraURL(testSite)+"/rest/api/3/myself", basic("presented-secret-1"), "")
	r := w.linear(t, testLinearKey, linearBody("Issue", `query Issue($id: String!) { issue(id: $id) { id } }`, map[string]any{"id": testLinearKey}))
	if strings.Contains(r.body, testLinearKey) {
		t.Fatalf("an answer repeats the key: %s", r.body)
	}
	// An unhandled body member's name is in the answer's reason.
	leak := `{"body":` + testDoc + `,"` + testJiraToken + `":1}`
	unhandled, err := send(ctx, "POST", w.srv.JiraURL(testSite)+"/rest/api/3/issue/WEB-1/comment", withJSON(basic(testJiraToken), leak), leak)
	if err != nil || strings.Contains(unhandled.body, testJiraToken) || unhandled.status != 501 || !strings.Contains(unhandled.body, "redacted") {
		t.Fatalf("an unhandled answer repeats the token: %d %s %v", unhandled.status, unhandled.body, err)
	}
	logged, err := json.Marshal(w.e.Requests(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testJiraToken, testTempo, testLinearKey, "presented-secret-1", "Basic "} {
		if strings.Contains(string(logged), secret) {
			t.Fatalf("the log repeats %q: %s", secret, logged)
		}
	}
	if !strings.Contains(string(logged), "redacted") {
		t.Fatal("nothing was redacted")
	}
	snap, err := json.Marshal(w.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testJiraToken, testTempo, testLinearKey} {
		if strings.Contains(string(snap), secret) {
			t.Fatalf("the snapshot holds %q", secret)
		}
	}
}

func TestClockNeverMovesBack(t *testing.T) {
	w := newWorld(t)
	w.jira(t, "POST", "/rest/api/3/issue/WEB-1/comment", `{"body":`+testDoc+`}`)
	w.clock.set(testStart.Add(-time.Hour))
	w.jira(t, "POST", "/rest/api/3/issue/WEB-1/comment", `{"body":`+testDoc+`}`)
	cs := w.e.Snapshot().Jira[0].Issues[0].Comments
	if len(cs) != 2 || cs[1].Created.Before(cs[0].Created) || !cs[1].Created.Equal(testStart) {
		t.Fatalf("comments stamped %v then %v", cs[0].Created, cs[1].Created)
	}
}

func TestSeedIsAllOrNothing(t *testing.T) {
	e := New(Options{})
	defer e.Close()
	bad := testFixture()
	bad.Linear[0].Issues = []LinearIssue{{ID: "i1", Team: "t9", Title: "Nowhere"}}
	if err := e.Seed(bad); err == nil {
		t.Fatal("a fixture naming a missing team was seeded")
	}
	if snap := e.Snapshot(); len(snap.Jira) != 0 || len(snap.Linear) != 0 {
		t.Fatalf("a refused fixture left %+v", snap)
	}
	shared := testFixture()
	other := shared.Jira[0]
	other.Site, other.Token = "other.example.com", "other-token-1"
	shared.Jira = append(shared.Jira, other)
	if err := e.Seed(shared); err == nil || !strings.Contains(err.Error(), "share a Tempo token") {
		t.Fatalf("sites sharing a Tempo token: %v", err)
	}
	if err := e.Seed(testFixture()); err != nil {
		t.Fatal(err)
	}
	e.Close()
	if err := e.Seed(testFixture()); !errors.Is(err, errClosed) {
		t.Fatalf("a closed engine seeded: %v", err)
	}
}

func TestResetClearsEverything(t *testing.T) {
	w := newWorld(t)
	if err := w.e.AddFault(Fault{Origin: jiraOrigin, Route: myselfRoute, Kind: FaultRefuse, Status: 503}); err != nil {
		t.Fatal(err)
	}
	if err := w.e.SetOffline(tempoOrigin, true); err != nil {
		t.Fatal(err)
	}
	w.jira(t, "GET", "/rest/api/3/myself", "")
	w.e.Reset()
	if snap := w.e.Snapshot(); len(snap.Jira) != 0 || len(snap.Linear) != 0 {
		t.Fatalf("reset kept %+v", snap)
	}
	if log := w.e.Requests(0); len(log.Requests) != 0 {
		t.Fatalf("reset kept %d requests", len(log.Requests))
	}
	if err := w.e.Seed(testFixture()); err != nil {
		t.Fatal(err)
	}
	if r := w.jira(t, "GET", "/rest/api/3/myself", ""); r.status != 200 {
		t.Fatalf("a fault survived reset: %d", r.status)
	}
	if r := w.tempo(t, "/4/work-attributes"); r.status != 200 {
		t.Fatalf("offline survived reset: %d", r.status)
	}
}

func TestHandlerRefusesBadOrigins(t *testing.T) {
	e := New(Options{})
	defer e.Close()
	for _, o := range []Origin{{Tracker: TrackerJira}, {Tracker: TrackerTempo, Site: testSite}, {Tracker: "github"}} {
		if _, err := e.Handler(o); err == nil {
			t.Fatalf("a handler for %+v", o)
		}
	}
}

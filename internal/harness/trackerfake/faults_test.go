package trackerfake

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (w *world) arm(t *testing.T, f Fault) {
	t.Helper()
	if err := w.e.AddFault(f); err != nil {
		t.Fatal(err)
	}
}

// await returns the next reported request that pred accepts.
func (w *world) await(t *testing.T, pred func(Request) bool) Request {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case r := <-w.seen:
			if pred(r) {
				return r
			}
		case <-timeout:
			t.Fatal("no such request was reported")
		}
	}
}

func held(r Request) bool { return r.Held }

// sent is the answer to a request made in the background.
type sent struct {
	r   reply
	err error
}

func (w *world) comment(ctx context.Context) <-chan sent {
	out := make(chan sent, 1)
	go func() {
		body := `{"body":` + testDoc + `}`
		r, err := send(ctx, "POST", w.srv.JiraURL(testSite)+"/rest/api/3/issue/WEB-1/comment", withJSON(basic(testJiraToken), body), body)
		out <- sent{r, err}
	}()
	return out
}

func TestOffline(t *testing.T) {
	w := newWorld(t)
	if err := w.e.SetOffline(jiraOrigin, true); err != nil {
		t.Fatal(err)
	}
	if got := <-w.comment(context.Background()); got.err == nil {
		t.Fatalf("an offline origin answered %d", got.r.status)
	}
	if last := w.lastRequest(t); last.Fault != FaultOffline || last.Status != 0 {
		t.Fatalf("logged %+v", last)
	}
	if w.comments(t) != 0 {
		t.Fatal("an offline origin applied a write")
	}
	if r := w.tempo(t, "/4/work-attributes"); r.status != 200 {
		t.Fatalf("another origin answered %d", r.status)
	}
	if err := w.e.SetOffline(jiraOrigin, false); err != nil {
		t.Fatal(err)
	}
	if got := <-w.comment(context.Background()); got.err != nil || got.r.status != 201 {
		t.Fatalf("back online: %+v", got)
	}
}

// A comment whose body stops before its declared length applies nothing
// and is logged as cut short, not as a request the fake lacks.
func TestABodyCutShortAppliesNothing(t *testing.T) {
	w := newWorld(t)
	body := `{"body":` + testDoc + `}`
	conn, err := net.Dial("tcp", strings.TrimPrefix(w.srv.JiraURL(testSite), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	head := "POST /rest/api/3/issue/WEB-1/comment HTTP/1.1\r\nHost: " + testSite + "\r\nAuthorization: " + basic(testJiraToken)["Authorization"] +
		"\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := conn.Write([]byte(head + body[:len(body)/2])); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	got := w.await(t, func(r Request) bool { return r.Path == "/rest/api/3/issue/WEB-1/comment" })
	if got.Route != "cut short" || got.Unhandled || got.Status != 0 {
		t.Fatalf("logged %+v", got)
	}
	if w.comments(t) != 0 {
		t.Fatal("a body cut short applied a write")
	}
}

func TestRateLimit(t *testing.T) {
	w := newWorld(t)
	w.arm(t, Fault{Origin: jiraOrigin, Kind: FaultRateLimit, RetryAfter: 7, Times: 1})
	w.arm(t, Fault{Origin: tempoOrigin, Kind: FaultRateLimit, RetryAfter: 3, Times: 1})
	w.arm(t, Fault{Origin: linearOrigin, Route: "Viewer", Kind: FaultRateLimit, RetryAfter: 5, Times: 1})
	if r := w.jira(t, "GET", "/rest/api/3/myself", ""); r.status != 429 || r.header.Get("Retry-After") != "7" || r.header.Get("X-Ratelimit-Remaining") != "0" {
		t.Fatalf("Jira answered %d %v", r.status, r.header)
	}
	if r := w.tempo(t, "/4/work-attributes"); r.status != 429 || r.body != "" {
		t.Fatalf("Tempo answered %d %v", r.status, r.header)
	}
	r := w.linear(t, testLinearKey, linearBody("Viewer", viewerQuery, nil))
	if r.status != 429 || !strings.Contains(r.body, "RATELIMITED") || r.header.Get("Retry-After") != "5" || r.header.Get("X-Ratelimit-Requests-Remaining") != "0" {
		t.Fatalf("Linear answered %d %v %s", r.status, r.header, r.body)
	}
	if r := w.jira(t, "GET", "/rest/api/3/myself", ""); r.status != 200 {
		t.Fatalf("a one-time rate limit answered again: %d", r.status)
	}
}

func TestRefuseTimes(t *testing.T) {
	w := newWorld(t)
	w.arm(t, Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultRefuse, Times: 2, Status: 503,
		Header: map[string]string{"Retry-After": "1"}, Body: []byte(`{"errorMessages":["down"]}`)})
	for i := range 2 {
		got := <-w.comment(context.Background())
		if got.err != nil || got.r.status != 503 || got.r.header.Get("Retry-After") != "1" || !strings.Contains(got.r.body, "down") {
			t.Fatalf("refusal %d: %+v", i, got)
		}
	}
	if w.comments(t) != 0 {
		t.Fatal("a refused write applied")
	}
	if r := w.jira(t, "GET", "/rest/api/3/myself", ""); r.status != 200 {
		t.Fatalf("a route fault matched another route: %d", r.status)
	}
	if got := <-w.comment(context.Background()); got.r.status != 201 || w.comments(t) != 1 {
		t.Fatalf("the third request: %+v", got)
	}
}

func TestHoldAndRelease(t *testing.T) {
	w := newWorld(t)
	w.arm(t, Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultHold})
	answered := w.comment(context.Background())
	w.await(t, held)
	// The held request blocks neither the engine nor its origin.
	if r := w.jira(t, "GET", "/rest/api/3/myself", ""); r.status != 200 {
		t.Fatalf("a request beside a held one answered %d", r.status)
	}
	if w.comments(t) != 0 {
		t.Fatal("a held write applied before release")
	}
	if err := w.e.Release(jiraOrigin, commentRoute, false); err != nil {
		t.Fatal(err)
	}
	if got := <-answered; got.err != nil || got.r.status != 201 || w.comments(t) != 1 {
		t.Fatalf("released: %+v", got)
	}
	done := w.await(t, func(r Request) bool { return r.Route == commentRoute && !r.Held })
	if done.Status != 201 || done.Fault != FaultHold {
		t.Fatalf("the held entry was completed as %+v", done)
	}
	if err := w.e.Release(jiraOrigin, commentRoute, false); err == nil {
		t.Fatal("a second release found a hold")
	}
	if got := <-w.comment(context.Background()); got.r.status != 201 || w.lastRequest(t).Fault != "" {
		t.Fatalf("a released hold still matches: %+v, logged %+v", got, w.lastRequest(t))
	}
}

// A fault with a query takes only the requests that carry it: a read of
// the issue's status is refused, one of its summary is answered.
func TestFaultMatchesQuery(t *testing.T) {
	w := newWorld(t)
	route := "GET rest/api/3/issue/{issue}"
	w.arm(t, Fault{Origin: jiraOrigin, Route: route, Query: map[string]string{"fields": "status"}, Kind: FaultRefuse, Status: 503})
	if r := w.jira(t, "GET", "/rest/api/3/issue/WEB-1?fields=summary", ""); r.status != 200 {
		t.Fatalf("a read without the fault's query answered %d", r.status)
	}
	if r := w.jira(t, "GET", "/rest/api/3/issue/WEB-1?fields=status", ""); r.status != 503 {
		t.Fatalf("a read with the fault's query answered %d", r.status)
	}
}

// A dropped hold fails its request without applying the write; cleared
// faults let it through. TestEveryWorldEndMeetsEveryRequestState covers a
// hold that a reset or close ends.
func TestHoldDropAndClear(t *testing.T) {
	for _, end := range []string{"drop", "clear"} {
		t.Run(end, func(t *testing.T) {
			w := newWorld(t)
			w.arm(t, Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultHold})
			answered := w.comment(context.Background())
			w.await(t, held)
			if end == "clear" {
				w.e.ClearFaults(jiraOrigin, commentRoute)
				if got := <-answered; got.err != nil || got.r.status != 201 {
					t.Fatalf("cleared: %+v", got)
				}
				return
			}
			if err := w.e.Release(jiraOrigin, commentRoute, true); err != nil {
				t.Fatal(err)
			}
			if got := <-answered; got.err == nil {
				t.Fatalf("a dropped hold answered %d", got.r.status)
			}
			if w.comments(t) != 0 {
				t.Fatal("a dropped hold applied its write")
			}
		})
	}
}

func TestHeldRequestCancelledByClient(t *testing.T) {
	w := newWorld(t)
	w.arm(t, Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultHold})
	ctx, cancel := context.WithCancel(context.Background())
	answered := w.comment(ctx)
	w.await(t, held)
	cancel()
	if got := <-answered; got.err == nil {
		t.Fatalf("a cancelled request answered %d", got.r.status)
	}
	done := w.await(t, func(r Request) bool { return r.Route == commentRoute && !r.Held })
	if done.Status != 0 || w.comments(t) != 0 {
		t.Fatalf("a cancelled hold ended as %+v", done)
	}
}

func TestFailAfterApply(t *testing.T) {
	w := newWorld(t)
	w.arm(t, Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultFailAfterApply, Times: 1})
	if got := <-w.comment(context.Background()); got.err == nil {
		t.Fatalf("a dropped answer arrived: %d", got.r.status)
	}
	if w.comments(t) != 1 {
		t.Fatal("the write did not land")
	}
	if last := w.lastRequest(t); last.Fault != FaultFailAfterApply || last.Status != 0 || !strings.Contains(last.Detail, "201") {
		t.Fatalf("logged %+v", last)
	}
}

func TestClosedEngineAnswersNothing(t *testing.T) {
	w := newWorld(t)
	w.e.Close()
	w.e.Close()
	if _, err := send(context.Background(), "GET", w.srv.JiraURL(testSite)+"/rest/api/3/myself", basic(testJiraToken), ""); err == nil {
		t.Fatal("a closed engine answered")
	}
}

func TestFaultChecks(t *testing.T) {
	e := New(Options{})
	defer e.Close()
	for name, f := range map[string]Fault{
		"no route":          {Origin: jiraOrigin, Route: "GET rest/api/3/dashboard", Kind: FaultHold},
		"bad origin":        {Origin: Origin{Tracker: TrackerTempo, Site: testSite}, Kind: FaultHold},
		"linear name":       {Origin: linearOrigin, Route: "Not a name", Kind: FaultHold},
		"refuse status":     {Origin: jiraOrigin, Kind: FaultRefuse, Status: 200},
		"refuse body":       {Origin: jiraOrigin, Kind: FaultRefuse, Status: 500, Body: []byte("not json")},
		"refuse type":       {Origin: jiraOrigin, Kind: FaultRefuse, Status: 500, Header: map[string]string{"content-type": "text/plain"}},
		"rate limit":        {Origin: jiraOrigin, Kind: FaultRateLimit},
		"hold with status":  {Origin: jiraOrigin, Kind: FaultHold, Status: 500},
		"hold retry after":  {Origin: jiraOrigin, Kind: FaultHold, RetryAfter: 1},
		"negative times":    {Origin: jiraOrigin, Kind: FaultHold, Times: -1},
		"unknown kind":      {Origin: jiraOrigin, Kind: "slow"},
		"offline via fault": {Origin: jiraOrigin, Kind: FaultOffline},
		"linear query":      {Origin: linearOrigin, Query: map[string]string{"a": "b"}, Kind: FaultHold},
	} {
		if err := e.AddFault(f); err == nil {
			t.Errorf("%s: armed %+v", name, f)
		}
	}
	if err := e.AddFault(Fault{Origin: tempoOrigin, Route: "GET 4/work-attributes", Kind: FaultRefuse, Status: http.StatusBadGateway}); err != nil {
		t.Fatal(err)
	}
}

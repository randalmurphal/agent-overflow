package linearprovider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	costIssues    = 500
	costComments  = 60
	costHistories = 20
	costEntries   = 100
)

// opLog counts what each GraphQL request moved. A request is recorded
// when it arrives; since waits for answers in flight to finish.
type opLog struct {
	mu       sync.Mutex
	idle     *sync.Cond
	inFlight int
	seen     []opRecord
	conns    int
}

type opRecord struct {
	op         string
	reqBytes   int
	replyBytes int
	points     int
}

func newOpLog() *opLog {
	l := &opLog{}
	l.idle = sync.NewCond(&l.mu)
	return l
}

type countingWriter struct {
	http.ResponseWriter
	n int
}

func (w *countingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.n += n
	return n, err
}

func (l *opLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var gql struct {
			OperationName string `json:"operationName"`
		}
		_ = json.Unmarshal(body, &gql)
		l.mu.Lock()
		i := len(l.seen)
		l.seen = append(l.seen, opRecord{op: gql.OperationName, reqBytes: len(body)})
		l.inFlight++
		l.mu.Unlock()
		cw := &countingWriter{ResponseWriter: w}
		defer func() {
			points, _ := strconv.Atoi(w.Header().Get("X-Complexity"))
			l.mu.Lock()
			l.seen[i].replyBytes, l.seen[i].points = cw.n, points
			l.inFlight--
			l.idle.Broadcast()
			l.mu.Unlock()
		}()
		h.ServeHTTP(cw, r)
	})
}

func (l *opLog) since(n int) []opRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.inFlight > 0 {
		l.idle.Wait()
	}
	return slices.Clone(l.seen[n:])
}

func (l *opLog) counts() (requests, conns int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen), l.conns
}

func (l *opLog) connState(_ any, state http.ConnState) {
	if state == http.StateNew {
		l.mu.Lock()
		l.conns++
		l.mu.Unlock()
	}
}

type costRun struct {
	t    *testing.T
	p    *Provider
	log  *opLog
	rows []string
	got  map[string][]string
	// conns and points are the connections each call opened and the
	// points it was charged, by label.
	conns, points map[string]int
}

var (
	costTeam    = sid(10)
	costStates  = []string{sid(20), sid(21), sid(22), sid(23)}
	costUsers   = []string{sid(1), sid(2), sid(3)}
	costLabels  = []string{sid(40), sid(41)}
	costActive  = sid(31)
	costNext    = sid(32)
	costProject = sid(50)
)

func costIssueID(i int) string { return sid(1000 + i) }

func costWorkspace(now time.Time) trackerfake.LinearWorkspace {
	at := func(d time.Duration) time.Time { return now.Add(d) }
	day := 24 * time.Hour
	w := trackerfake.LinearWorkspace{
		URLKey: "example", Name: "Example", APIKey: "lin_api_scenario1", Viewer: costUsers[0],
		Teams: []trackerfake.LinearTeam{{
			ID: costTeam, Key: "WEB", Name: "Web", Timezone: "UTC", Estimation: "fibonacci",
			CyclesEnabled: true, CycleWeeks: 2, DefaultState: costStates[1], Members: costUsers,
		}},
		States: []trackerfake.LinearState{
			{ID: costStates[0], Team: costTeam, Name: "Backlog", Type: "backlog", Position: 0},
			{ID: costStates[1], Team: costTeam, Name: "Todo", Type: "unstarted", Position: 1},
			{ID: costStates[2], Team: costTeam, Name: "In Progress", Type: "started", Position: 2},
			{ID: costStates[3], Team: costTeam, Name: "Done", Type: "completed", Position: 3},
		},
		Cycles: []trackerfake.LinearCycle{
			{ID: costActive, Team: costTeam, Number: 30, StartsAt: at(-10 * day), EndsAt: at(4 * day)},
			{ID: costNext, Team: costTeam, Number: 31, StartsAt: at(4 * day), EndsAt: at(18 * day)},
		},
		Labels:   []trackerfake.LinearLabel{{ID: costLabels[0], Name: "Bug", Team: costTeam}, {ID: costLabels[1], Name: "Feature", Team: costTeam}},
		Projects: []trackerfake.LinearProject{{ID: costProject, Name: "Launch", Teams: []string{costTeam}}},
	}
	for i, u := range costUsers {
		w.Users = append(w.Users, trackerfake.LinearUser{ID: u, Name: fmt.Sprintf("User %d", i+1), Email: fmt.Sprintf("user%d@example.com", i+1)})
	}
	for i := range 20 {
		end := at(time.Duration(-10-14*i) * day)
		w.Cycles = append(w.Cycles, trackerfake.LinearCycle{
			ID: sid(200 + i), Team: costTeam, Number: float64(29 - i), StartsAt: end.Add(-14 * day), EndsAt: end, CompletedAt: end,
		})
	}
	description := strings.Repeat("A paragraph of plain words that describes the work to be done. ", 30)
	three := 3.0
	for i := range costIssues {
		is := trackerfake.LinearIssue{
			ID: costIssueID(i), Team: costTeam, Number: float64(i + 1), Title: fmt.Sprintf("Issue %d with a title of ordinary length", i+1),
			Description: description, State: costStates[i%4], Assignee: costUsers[i%3], Creator: costUsers[0],
			Labels: []string{costLabels[i%2]}, Priority: 2, Estimate: &three, SortOrder: float64(1000 * (i + 1)),
			CreatedAt: at(-time.Duration(costIssues-i)*time.Hour - 30*day), UpdatedAt: at(-time.Duration(costIssues-i) * time.Minute),
		}
		if i%4 == 0 {
			is.Cycle = costActive
		}
		if costStates[i%4] == costStates[3] {
			is.CompletedAt = is.UpdatedAt
		}
		if costStates[i%4] == costStates[2] {
			is.StartedAt = is.UpdatedAt
		}
		w.Issues = append(w.Issues, is)
	}
	for c := range costComments {
		w.Comments = append(w.Comments, trackerfake.LinearComment{
			ID: sid(5000 + c), Issue: costIssueID(0), User: costUsers[c%3], Body: description[:600], CreatedAt: at(time.Duration(c-100) * time.Minute),
		})
	}
	for h := range costHistories {
		is := &w.Issues[h]
		t := is.CreatedAt.Add(10 * time.Minute)
		for e := range costEntries {
			t = t.Add(time.Minute)
			is.History = append(is.History, trackerfake.LinearHistory{
				ID: sid(100000 + h*1000 + e), CreatedAt: t, Actor: costUsers[e%3],
				FromState: costStates[e%4], ToState: costStates[(e+1)%4],
			})
		}
	}
	return w
}

func newCostRun(t *testing.T) (*costRun, *scenarioClock) {
	t.Helper()
	clock := &scenarioClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	if err := e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{costWorkspace(clock.Now())}}); err != nil {
		t.Fatal(err)
	}
	h, err := e.Handler(trackerfake.Origin{Tracker: trackerfake.TrackerLinear})
	if err != nil {
		t.Fatal(err)
	}
	r := &costRun{t: t, log: newOpLog(), got: map[string][]string{}, conns: map[string]int{}, points: map[string]int{}}
	srv := httptest.NewUnstartedServer(r.log.wrap(h))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) { r.log.connState(c, s) }
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := linear.New(linear.Options{
		BaseURL: srv.URL, Version: "test", Workspace: "example",
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte("lin_api_scenario1")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if r.p, err = New(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, u := range e.Unhandled() {
			t.Errorf("unhandled request %s: %s", u.Operation, u.Detail)
		}
	})
	return r, clock
}

func (r *costRun) measure(label string, call func() error) {
	r.t.Helper()
	n0, c0 := r.log.counts()
	start := time.Now()
	if err := call(); err != nil {
		r.t.Fatalf("%s: %v", label, err)
	}
	took := time.Since(start)
	recs := r.log.since(n0)
	_, c1 := r.log.counts()
	var ops, sized []string
	var in, out, points int
	for _, rec := range recs {
		ops = append(ops, rec.op)
		sized = append(sized, fmt.Sprintf("%s (%dB, %dpt)", rec.op, rec.replyBytes, rec.points))
		in += rec.reqBytes
		out += rec.replyBytes
		points += rec.points
	}
	if len(recs) > 8 {
		sized = []string{summarize(ops)}
	}
	r.got[label], r.conns[label], r.points[label] = ops, c1-c0, points
	r.rows = append(r.rows, fmt.Sprintf("%-30s req=%-3d conns=%d points=%-6d sent=%-7d read=%-8d local=%-8s %s",
		label, len(recs), c1-c0, points, in, out, took.Round(time.Microsecond), strings.Join(sized, "; ")))
}

func (r *costRun) result(label string, write func() tracker.Result) {
	r.t.Helper()
	r.measure(label, func() error {
		res := write()
		if !res.AllApplied() {
			var b strings.Builder
			for _, p := range res.Parts {
				fmt.Fprintf(&b, " [%s %s %s %v]", p.Step, p.Issue, p.Outcome.State(), p.Outcome.Err())
			}
			return fmt.Errorf("not all applied:%s", b.String())
		}
		return nil
	})
}

func summarize(ops []string) string {
	var parts []string
	for i := 0; i < len(ops); {
		j := i
		for j < len(ops) && ops[j] == ops[i] {
			j++
		}
		if j-i > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", ops[i], j-i))
		} else {
			parts = append(parts, ops[i])
		}
		i = j
	}
	return strings.Join(parts, "; ")
}

// pinned is a call's pinned cost: its requests and the points the fake
// charges them.
type pinned struct{ requests, points int }

func (r *costRun) pin(want map[string]pinned) {
	r.t.Helper()
	for _, row := range r.rows {
		r.t.Log(row)
	}
	for label, w := range want {
		got, ok := r.got[label]
		if !ok {
			r.t.Errorf("%s: not measured", label)
			continue
		}
		if len(got) != w.requests {
			r.t.Errorf("%s: %d requests, pinned %d: %s", label, len(got), w.requests, summarize(got))
		}
		if r.points[label] != w.points {
			r.t.Errorf("%s: %d points, pinned %d", label, r.points[label], w.points)
		}
	}
	for label := range r.got {
		if _, ok := want[label]; !ok {
			r.t.Errorf("%s: measured but not pinned (%d requests)", label, len(r.got[label]))
		}
	}
}

func costIDs(from, n int) []tracker.IssueID {
	out := make([]tracker.IssueID, n)
	for i := range out {
		out[i] = tracker.IssueID(costIssueID(from + i))
	}
	return out
}

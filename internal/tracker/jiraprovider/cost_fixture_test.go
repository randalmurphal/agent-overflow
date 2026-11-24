package jiraprovider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	costIssues    = 500
	costFirstID   = 20001
	costComments  = 60
	costHistories = 20
	costChanges   = 100
	costBoard     = "1"
	costOther     = "2"
)

// exchangeLog counts what each request to a served origin moved. A
// request is recorded when it arrives, so a call's requests are all
// recorded when the call returns; since waits for the answers in flight
// to finish writing before their sizes are read.
type exchangeLog struct {
	mu       sync.Mutex
	idle     *sync.Cond
	inFlight int
	seen     []exchangeRecord
}

func newExchangeLog() *exchangeLog {
	l := &exchangeLog{}
	l.idle = sync.NewCond(&l.mu)
	return l
}

type exchangeRecord struct {
	route      string // method and path, the numbers in it kept
	reqBytes   int
	replyBytes int
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

func (l *exchangeLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		l.mu.Lock()
		i := len(l.seen)
		l.seen = append(l.seen, exchangeRecord{route: r.Method + " " + r.URL.Path, reqBytes: len(body)})
		l.inFlight++
		l.mu.Unlock()
		cw := &countingWriter{ResponseWriter: w}
		defer func() {
			l.mu.Lock()
			l.seen[i].replyBytes = cw.n
			l.inFlight--
			l.idle.Broadcast()
			l.mu.Unlock()
		}()
		h.ServeHTTP(cw, r)
	})
}

// since is the records from n on, once no answer is being written.
func (l *exchangeLog) since(n int) []exchangeRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.inFlight > 0 {
		l.idle.Wait()
	}
	return slices.Clone(l.seen[n:])
}

func (l *exchangeLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

func costDoc(paragraphs int) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"type":"doc","version":1,"content":[`)
	for i := range paragraphs {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"paragraph","content":[{"type":"text","text":"Paragraph %d describes the work in plain words, with enough text to look like a real ticket body."}]}`, i)
	}
	b.WriteString(`]}`)
	return json.RawMessage(b.String())
}

func costIssueID(i int) string { return decimal(int64(costFirstID + i)) }

// costFixture is one site with a 500-issue scrum board, a second board on
// the same project, 40 closed sprints (30 before the window), and a
// month of worklogs on 20 issues in Tempo and in Jira.
func costFixture(now time.Time) trackerfake.JiraSite {
	day := now.Add(-48 * time.Hour)
	window := now.Add(-scenarioWindow)
	site := trackerfake.JiraSite{
		Site: scenarioSite, Account: "acc-user1", Token: "scenario-jira-1",
		Users: []trackerfake.JiraUser{
			{AccountID: "acc-user1", DisplayName: "User 1", Email: "user1@example.com", TimeZone: "UTC"},
			{AccountID: "acc-user2", DisplayName: "User 2", Email: "user2@example.com", TimeZone: "UTC"},
			{AccountID: "acc-user3", DisplayName: "User 3", Email: "user3@example.com", TimeZone: "UTC"},
		},
		Fields: []trackerfake.JiraField{
			{ID: "customfield_10016", Name: "Story Points", Schema: trackerfake.JiraSchema{Type: "number", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float", CustomID: 10016}},
		},
		Projects: []trackerfake.JiraProject{{ID: "10000", Key: "WEB", Name: "Web"}},
		Boards: []trackerfake.JiraBoard{
			{ID: 1, Name: "WEB board", Type: "scrum", Project: "WEB", Estimation: "customfield_10016", Columns: []trackerfake.JiraColumn{
				{Name: "To Do", Statuses: []string{"10000"}}, {Name: "In Progress", Statuses: []string{"10001"}}, {Name: "Done", Statuses: []string{"10002"}},
			}},
			{ID: 2, Name: "WEB kanban", Type: "kanban", Project: "WEB", Columns: []trackerfake.JiraColumn{
				{Name: "To Do", Statuses: []string{"10000"}}, {Name: "In Progress", Statuses: []string{"10001"}}, {Name: "Done", Statuses: []string{"10002"}},
			}},
		},
		Sprints: []trackerfake.JiraSprint{
			{ID: 1, Board: 1, Name: "Sprint active", State: "active", Start: day, End: day.Add(14 * 24 * time.Hour)},
			{ID: 2, Board: 1, Name: "Sprint future", State: "future"},
			{ID: 3, Board: 1, Name: "Sprint future 2", State: "future"},
		},
	}
	for i := range 40 {
		end := window.Add(time.Duration(i-30) * 14 * 24 * time.Hour)
		site.Sprints = append(site.Sprints, trackerfake.JiraSprint{
			ID: int64(100 + i), Board: 1, Name: fmt.Sprintf("Sprint closed %d", i), State: "closed",
			Start: end.Add(-14 * 24 * time.Hour), End: end, Complete: end,
		})
	}
	statuses := []string{"10000", "10001", "10002"}
	users := []string{"acc-user1", "acc-user2", "acc-user3"}
	for i := range costIssues {
		is := trackerfake.JiraIssue{
			ID: costIssueID(i), Key: fmt.Sprintf("WEB-%d", i+1), Project: "WEB", Type: "10002",
			Summary: fmt.Sprintf("Issue %d with a title of ordinary length", i+1), Description: costDoc(3),
			Assignee: users[i%3], Reporter: "acc-user1", Priority: "3", Status: statuses[i%3],
			Labels: []string{"frontend"}, Created: day.Add(-time.Duration(costIssues-i) * time.Hour),
			Updated: day.Add(-time.Duration(costIssues-i) * time.Minute),
			Values:  map[string]json.RawMessage{"customfield_10016": json.RawMessage(`3`)},
		}
		if is.Status == "10002" {
			is.Resolution, is.Resolved = "10000", is.Updated
		}
		switch i % 4 {
		case 0:
			is.Sprints = []int64{1}
		case 1:
			is.Sprints = []int64{2}
		}
		site.Issues = append(site.Issues, is)
	}
	first := &site.Issues[0]
	first.Description = costDoc(40)
	for c := range costComments {
		first.Comments = append(first.Comments, trackerfake.JiraComment{
			ID: decimal(int64(40001 + c)), Author: users[c%3], Body: costDoc(2), Created: day.Add(time.Duration(c) * time.Minute),
		})
	}
	for h := range costHistories {
		is := &site.Issues[h]
		at := is.Created
		for c := range costChanges {
			at = at.Add(time.Minute)
			from, to := statuses[c%3], statuses[(c+1)%3]
			is.Changelog = append(is.Changelog, trackerfake.JiraChange{Author: users[c%3], Created: at, Items: []trackerfake.JiraChangeItem{
				{FieldID: "status", Field: "status", FieldType: "jira", From: scenarioPtr(from), To: scenarioPtr(to), FromString: scenarioPtr("s" + from), ToString: scenarioPtr("s" + to)},
			}})
		}
		is.Updated = at.Add(time.Minute)
	}
	month := now.AddDate(0, 0, -30)
	site.Tempo = &trackerfake.TempoAccount{Token: "scenario-tempo-1"}
	for d := range 20 {
		start := month.AddDate(0, 0, d)
		issue := &site.Issues[d]
		site.Tempo.Worklogs = append(site.Tempo.Worklogs, trackerfake.TempoWorklog{
			ID: int64(7001 + d), Issue: issue.ID, Author: "acc-user1", StartDate: start.Format(time.DateOnly), StartTime: "09:00:00", Spent: 3600, Description: "Work",
		})
		issue.Worklogs = append(issue.Worklogs, trackerfake.JiraWorklog{
			ID: decimal(int64(30001 + d)), Author: "acc-user1", Started: start.Add(9 * time.Hour), TimeSpent: 3600,
		})
	}
	return site
}

type costRun struct {
	t     *testing.T
	e     *trackerfake.Engine
	p     *Provider
	jlog  *exchangeLog
	tlog  *exchangeLog
	clock *scenarioClock
	rows  []string
	// got is each measured call's request routes, in order, by label.
	got map[string][]string
	// bytes is each measured call's body bytes sent and read, by label.
	bytes map[string]int
	// largest is each measured call's largest answer body, by label.
	largest map[string]int
}

func newCostRun(t *testing.T, withTempo bool) *costRun {
	t.Helper()
	return newCostRunOf(t, withTempo, nil)
}

// newCostRunOf is newCostRun on costFixture changed by edit, when not
// nil.
func newCostRunOf(t *testing.T, withTempo bool, edit func(*trackerfake.JiraSite)) *costRun {
	t.Helper()
	clock := &scenarioClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	site := costFixture(clock.Now())
	if edit != nil {
		edit(&site)
	}
	if !withTempo {
		site.Tempo = nil
	}
	if err := e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
		t.Fatal(err)
	}
	r := &costRun{t: t, e: e, jlog: newExchangeLog(), tlog: newExchangeLog(), clock: clock, got: map[string][]string{}, bytes: map[string]int{}, largest: map[string]int{}}
	jh, err := e.Handler(trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite})
	if err != nil {
		t.Fatal(err)
	}
	js := httptest.NewServer(r.jlog.wrap(jh))
	t.Cleanup(js.Close)
	jc, err := jira.New(jira.Options{
		BaseURL: js.URL, Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Basic("user1@example.com", secret.New([]byte("scenario-jira-1")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jc.Close)
	opts := Options{Jira: jc, Site: "https://" + scenarioSite, SprintsFrom: func() time.Time { return clock.Now().Add(-scenarioWindow) }}
	if withTempo {
		th, err := e.Handler(trackerfake.Origin{Tracker: trackerfake.TrackerTempo})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(r.tlog.wrap(th))
		t.Cleanup(ts.Close)
		tc, err := tempo.New(tempo.Options{
			BaseURL: ts.URL, Version: "test", Issues: TempoIssues(jc),
			Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte("scenario-tempo-1")))),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tc.Close)
		opts.Tempo = tc
	}
	if r.p, err = New(opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, u := range e.Unhandled() {
			t.Errorf("unhandled request %s %s: %s", u.Method, u.Path, u.Detail)
		}
	})
	return r
}

// measure runs call and records the requests it made under label.
func (r *costRun) measure(label string, call func() error) {
	r.t.Helper()
	j0, t0 := r.jlog.len(), r.tlog.len()
	start := time.Now()
	if err := call(); err != nil {
		r.t.Fatalf("%s: %v", label, err)
	}
	took := time.Since(start)
	recs := append(r.jlog.since(j0), r.tlog.since(t0)...)
	var routes, sized []string
	var in, out int
	for _, rec := range recs {
		routes = append(routes, rec.route)
		sized = append(sized, fmt.Sprintf("%s (%dB)", rec.route, rec.replyBytes))
		in += rec.reqBytes
		out += rec.replyBytes
		r.largest[label] = max(r.largest[label], rec.replyBytes)
	}
	if len(recs) > 8 {
		sized = []string{summarize(routes)}
	}
	r.got[label] = routes
	r.bytes[label] = in + out
	r.rows = append(r.rows, fmt.Sprintf("%-34s req=%-3d sent=%-7d read=%-8d local=%-8s %s", label, len(recs), in, out, took.Round(time.Microsecond), strings.Join(sized, "; ")))
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

// summarize folds runs of one route into "route xN", digits kept.
func summarize(routes []string) string {
	var parts []string
	for i := 0; i < len(routes); {
		j := i
		for j < len(routes) && routes[j] == routes[i] {
			j++
		}
		if j-i > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", routes[i], j-i))
		} else {
			parts = append(parts, routes[i])
		}
		i = j
	}
	return strings.Join(parts, "; ")
}

func (r *costRun) log() {
	for _, row := range r.rows {
		r.t.Log(row)
	}
}

// pinBytes fails for each label whose body bytes sent and read exceed
// its bound in most.
func (r *costRun) pinBytes(most map[string]int) {
	r.t.Helper()
	for label, n := range most {
		got, ok := r.bytes[label]
		switch {
		case !ok:
			r.t.Errorf("%s: not measured", label)
		case got > n:
			r.t.Errorf("%s: %d bytes sent and read, at most %d", label, got, n)
		}
	}
}

// pin fails for each label whose request count differs from want.
func (r *costRun) pin(want map[string]int) {
	r.t.Helper()
	for label, n := range want {
		got, ok := r.got[label]
		if !ok {
			r.t.Errorf("%s: not measured", label)
			continue
		}
		if len(got) != n {
			r.t.Errorf("%s: %d requests, pinned %d: %s", label, len(got), n, summarize(got))
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

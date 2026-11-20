package jiraprovider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// The cost test runs every Provider and TimeTracker call over the real
// jira and tempo clients against the stateful fake at realistic sizes (a
// board of 500 issues, one issue with 60 comments, 20 issues with 100
// changes each, a month of worklogs) and pins the requests each call
// makes, in order, so a change to a call's request count fails here.
// The table it logs (go test -v -run TestRequestCost) also gives the
// answer bytes each call read.

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
	window := now.Add(-180 * 24 * time.Hour)
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
}

func newCostRun(t *testing.T, withTempo bool) *costRun {
	t.Helper()
	clock := &scenarioClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	site := costFixture(clock.Now())
	if !withTempo {
		site.Tempo = nil
	}
	if err := e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
		t.Fatal(err)
	}
	r := &costRun{t: t, e: e, jlog: newExchangeLog(), tlog: newExchangeLog(), clock: clock, got: map[string][]string{}}
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
	opts := Options{Jira: jc, Site: "https://" + scenarioSite}
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
	r.p.now = clock.Now
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
	}
	if len(recs) > 8 {
		sized = []string{summarize(routes)}
	}
	r.got[label] = routes
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

func TestRequestCost(t *testing.T) {
	r := newCostRun(t, true)
	p := r.p
	ctx := t.Context()
	now := r.clock.Now()
	window := now.Add(-180 * 24 * time.Hour)

	r.measure("Identity", func() error { _, err := p.Identity(ctx); return err })
	r.measure("Projects", func() error { _, err := p.Projects(ctx); return err })
	r.measure("IssueTypes", func() error { _, err := p.IssueTypes(ctx, "10000"); return err })
	r.measure("Boards", func() error { _, err := p.Boards(ctx, "10000"); return err })
	r.measure("LinkTypes", func() error { _, err := p.LinkTypes(ctx); return err })
	r.measure("Structure (cold)", func() error { _, err := p.Structure(ctx, costBoard, window); return err })
	r.measure("Structure (again)", func() error { _, err := p.Structure(ctx, costBoard, window); return err })

	// The first delta walk of the 500-issue board, then idle ticks.
	var cursor tracker.Cursor
	var walked int
	r.measure("ChangedRows walk of 500", func() error {
		for {
			page, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: costBoard, From: cursor, Start: window})
			if err != nil {
				return err
			}
			walked += len(page.Rows)
			cursor = page.Next
			if !page.More {
				return nil
			}
		}
	})
	if walked != costIssues {
		t.Fatalf("the walk delivered %d rows, want %d", walked, costIssues)
	}
	idleRows := make([]int, 3)
	for i := range idleRows {
		r.clock.advance(45 * time.Second)
		r.measure(fmt.Sprintf("ChangedRows idle tick %d", i+1), func() error {
			page, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: costBoard, From: cursor, Start: window})
			cursor = page.Next
			idleRows[i] = len(page.Rows)
			return err
		})
	}
	t.Logf("rows each idle tick delivers again: %v", idleRows)
	// The fixture's newest update is older than the overlap before the
	// fake's clock, so an idle board delivers nothing.
	if slices.ContainsFunc(idleRows, func(n int) bool { return n != 0 }) {
		t.Errorf("idle ticks delivered rows again: %v", idleRows)
	}

	r.measure("BoardIssues (500)", func() error {
		l, err := p.BoardIssues(ctx, costBoard, window)
		if err == nil && len(l.IDs) != costIssues {
			return fmt.Errorf("listed %d", len(l.IDs))
		}
		return err
	})
	r.measure("BoardRows (100)", func() error { _, err := p.BoardRows(ctx, costBoard, costIDs(0, 100)); return err })
	r.measure("BoardRows (500)", func() error { _, err := p.BoardRows(ctx, costBoard, costIDs(0, 500)); return err })
	r.measure("Cards (100)", func() error { _, err := p.Cards(ctx, costIDs(0, 100)); return err })
	r.measure("Cards (250)", func() error { _, err := p.Cards(ctx, costIDs(0, 250)); return err })
	r.measure("History (20 x 100 changes)", func() error {
		_, err := p.History(ctx, tracker.HistoryQuery{Issues: costIDs(0, costHistories), Boards: []tracker.BoardID{costBoard}})
		return err
	})
	r.measure("History (100, 80 unchanged)", func() error {
		_, err := p.History(ctx, tracker.HistoryQuery{Issues: costIDs(0, 100), Boards: []tracker.BoardID{costBoard}})
		return err
	})
	first := tracker.IssueID(costIssueID(0))
	var next tracker.PageToken
	r.measure("Detail (60 comments)", func() error {
		d, err := p.Detail(ctx, first)
		next = d.Comments.Next
		return err
	})
	r.measure("Comments (next page)", func() error { _, err := p.Comments(ctx, first, next); return err })
	var moves []tracker.Move
	r.measure("Moves", func() error { var err error; moves, err = p.Moves(ctx, first); return err })
	r.measure("CreateFields", func() error { _, err := p.CreateFields(ctx, "10000", "10002"); return err })
	r.measure("AssignableUsers", func() error { _, err := p.AssignableUsers(ctx, tracker.OnIssue(first), "User"); return err })
	r.measure("Choices (priority)", func() error {
		_, err := p.Choices(ctx, tracker.OnIssue(first), tracker.FieldPriority, "")
		return err
	})
	r.measure("Lookup (10 keys, 3 missing)", func() error {
		keys := []string{"WEB-1", "WEB-2", "WEB-3", "WEB-4", "WEB-5", "WEB-6", "WEB-7", "WEB-9001", "WEB-9002", "WEB-9003"}
		_, err := p.Lookup(ctx, keys)
		return err
	})
	r.measure("Search", func() error {
		_, err := p.Search(ctx, tracker.SearchQuery{Text: "ordinary", Project: "10000", Limit: 50})
		return err
	})

	// Structure on one board drops that board's cached configuration and
	// sprint listing only, so another board's reads keep theirs.
	r.measure("ChangedRows board 2 (cold)", func() error {
		_, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: costOther, Start: now.Add(-time.Hour)})
		return err
	})
	r.measure("Structure board 1 (warm 2)", func() error { _, err := p.Structure(ctx, costBoard, window); return err })
	r.measure("ChangedRows board 2 (after)", func() error {
		_, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: costOther, Start: now.Add(-time.Hour)})
		return err
	})

	// Writes.
	dir := t.TempDir()
	for _, name := range []string{"a.png", "b.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), pngBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.result("Create (two images)", func() tracker.Result {
		return p.Create(ctx, tracker.CreateInput{Project: "10000", Type: "10002", Title: "Created", ImageDir: dir,
			Fields: []tracker.FieldEdit{{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit("![a](a.png) ![b](b.png)", "")}}})
	})
	r.result("Edit (title, assignee)", func() tracker.Result {
		return p.Edit(ctx, tracker.EditInput{Issue: costIDs(1, 1)[0], Fields: []tracker.FieldEdit{
			{Field: tracker.FieldTitle, Value: tracker.TypedValues("Renamed")},
			{Field: tracker.FieldAssignee, Value: tracker.PickValues("acc-user2")},
		}})
	})
	done := slices.IndexFunc(moves, func(m tracker.Move) bool { return m.To == "10001" || m.To == "10002" })
	if done < 0 {
		t.Fatalf("moves %+v", moves)
	}
	r.result("Move", func() tracker.Result { return p.Move(ctx, tracker.MoveInput{Issue: first, Move: moves[done].ID}) })
	eight := 8.0
	r.result("SetEstimate", func() tracker.Result {
		return p.SetEstimate(ctx, tracker.EstimateInput{Board: costBoard, Issue: costIDs(2, 1)[0], Value: &eight})
	})
	r.result("Rank (5)", func() tracker.Result {
		return p.Rank(ctx, tracker.RankInput{Board: costBoard, Issues: costIDs(10, 5), Next: costIDs(3, 1)[0]})
	})
	r.result("MoveToSprint (30)", func() tracker.Result {
		return p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: costBoard, Sprint: "3", Issues: costIDs(100, 30)})
	})
	r.result("MoveToSprint backlog (30)", func() tracker.Result {
		return p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: costBoard, Issues: costIDs(100, 30)})
	})
	r.result("AddComment", func() tracker.Result {
		return p.AddComment(ctx, tracker.CommentInput{Issue: first, Markdown: "A new comment"})
	})
	cs, err := p.Comments(ctx, first, "")
	if err != nil {
		t.Fatal(err)
	}
	r.result("EditComment", func() tracker.Result {
		return p.EditComment(ctx, tracker.CommentEdit{Issue: first, Comment: cs.Comments[0].ID, Markdown: "Edited", From: cs.Comments[0].Body})
	})
	r.result("DeleteComment", func() tracker.Result {
		return p.DeleteComment(ctx, tracker.CommentDelete{Issue: first, Comment: cs.Comments[0].ID})
	})
	r.result("Link", func() tracker.Result {
		return p.Link(ctx, tracker.LinkInput{Type: "10000", Outward: costIDs(5, 1)[0], Inward: costIDs(6, 1)[0]})
	})
	d, err := p.Detail(ctx, costIDs(5, 1)[0])
	if err != nil || len(d.Links) != 1 {
		t.Fatalf("links %+v, %v", d.Links, err)
	}
	r.result("Unlink", func() tracker.Result {
		return p.Unlink(ctx, tracker.UnlinkInput{Issue: costIDs(5, 1)[0], Link: d.Links[0].ID, Other: costIDs(6, 1)[0]})
	})
	r.result("Attach", func() tracker.Result {
		return p.Attach(ctx, tracker.AttachInput{Issue: first, Files: []tracker.File{{Name: "notes.txt", Content: strings.NewReader("hello")}}})
	})
	d, err = p.Detail(ctx, first)
	if err != nil || len(d.Attachments) == 0 {
		t.Fatalf("attachments %+v, %v", d.Attachments, err)
	}
	r.measure("Download", func() error {
		var got bytes.Buffer
		return p.Download(ctx, tracker.DownloadInput{Issue: first, Attachment: d.Attachments[0].ID, To: &got, Limit: 1 << 20})
	})
	r.result("CreateSprint", func() tracker.Result {
		return p.CreateSprint(ctx, tracker.SprintInput{Board: costBoard, Name: "Sprint new"})
	})
	renamed := "Sprint future, renamed"
	r.result("EditSprint", func() tracker.Result {
		return p.EditSprint(ctx, tracker.SprintEdit{Board: costBoard, Sprint: "2", Name: &renamed})
	})
	r.result("CompleteSprint (125 issues)", func() tracker.Result {
		return p.CompleteSprint(ctx, tracker.SprintCompletion{Board: costBoard, Sprint: "1", MoveTo: "2"})
	})
	r.result("StartSprint", func() tracker.Result {
		return p.StartSprint(ctx, tracker.SprintStart{Board: costBoard, Sprint: "2", Start: now, End: now.Add(14 * 24 * time.Hour)})
	})

	tt := p.Time()
	from := mustDate(t, now.AddDate(0, 0, -30))
	to := mustDate(t, now)
	var wls []tracker.Worklog
	r.measure("Tempo Worklogs (month)", func() error {
		var err error
		wls, err = tt.Worklogs(ctx, tracker.WorklogQuery{Author: "acc-user1", From: from, To: to})
		if err == nil && len(wls) != 20 {
			return fmt.Errorf("%d worklogs", len(wls))
		}
		return err
	})
	r.result("Tempo AddWorklog", func() tracker.Result {
		return tt.AddWorklog(ctx, tracker.WorklogInput{Issue: first, Day: to, Spent: 30 * time.Minute, Description: "Review"})
	})
	hour := 2 * time.Hour
	r.result("Tempo EditWorklog", func() tracker.Result { return tt.EditWorklog(ctx, tracker.WorklogEdit{From: wls[0], Spent: &hour}) })
	r.result("Tempo DeleteWorklog", func() tracker.Result { return tt.DeleteWorklog(ctx, tracker.WorklogDelete{From: wls[1]}) })

	r.log()
	r.pin(map[string]int{
		"Identity": 1, "Projects": 1, "IssueTypes": 1, "Boards": 1, "LinkTypes": 1,
		"Structure (cold)": 3, "Structure (again)": 3,
		"ChangedRows walk of 500": 2, "ChangedRows idle tick 1": 1, "ChangedRows idle tick 2": 1, "ChangedRows idle tick 3": 1,
		"BoardIssues (500)": 1, "BoardRows (100)": 1, "BoardRows (500)": 1, "Cards (100)": 1, "Cards (250)": 3,
		"History (20 x 100 changes)": 2, "History (100, 80 unchanged)": 3,
		"Detail (60 comments)": 2, "Comments (next page)": 1, "Moves": 1, "CreateFields": 1,
		"AssignableUsers": 1, "Choices (priority)": 1, "Lookup (10 keys, 3 missing)": 2, "Search": 1,
		"ChangedRows board 2 (cold)": 2, "Structure board 1 (warm 2)": 3, "ChangedRows board 2 (after)": 1,
		"Create (two images)": 7, "Edit (title, assignee)": 3, "Move": 2, "SetEstimate": 2, "Rank (5)": 2,
		"MoveToSprint (30)": 3, "MoveToSprint backlog (30)": 2, "AddComment": 2, "EditComment": 2, "DeleteComment": 2,
		"Link": 1, "Unlink": 2, "Attach": 1, "Download": 2,
		"CreateSprint": 1, "EditSprint": 2, "CompleteSprint (125 issues)": 7, "StartSprint": 2,
		"Tempo Worklogs (month)": 1, "Tempo AddWorklog": 3, "Tempo EditWorklog": 3, "Tempo DeleteWorklog": 2,
	})
}

// TestRequestCostJiraWorklogs measures logged time over Jira's own
// worklogs, which a connection without Tempo uses.
func TestRequestCostJiraWorklogs(t *testing.T) {
	r := newCostRun(t, false)
	ctx := t.Context()
	now := r.clock.Now()
	tt := r.p.Time()
	from, to := mustDate(t, now.AddDate(0, 0, -30)), mustDate(t, now)
	var wls []tracker.Worklog
	r.measure("Jira Worklogs (month)", func() error {
		var err error
		wls, err = tt.Worklogs(ctx, tracker.WorklogQuery{Author: "acc-user1", From: from, To: to})
		if err == nil && len(wls) != 20 {
			return fmt.Errorf("%d worklogs", len(wls))
		}
		return err
	})
	r.result("Jira AddWorklog", func() tracker.Result {
		return tt.AddWorklog(ctx, tracker.WorklogInput{Issue: tracker.IssueID(costIssueID(0)), Day: to, Spent: 30 * time.Minute, Description: "Review"})
	})
	hour := 2 * time.Hour
	r.result("Jira EditWorklog", func() tracker.Result { return tt.EditWorklog(ctx, tracker.WorklogEdit{From: wls[0], Spent: &hour}) })
	r.result("Jira DeleteWorklog", func() tracker.Result { return tt.DeleteWorklog(ctx, tracker.WorklogDelete{From: wls[1]}) })
	r.log()
	r.pin(map[string]int{"Jira Worklogs (month)": 2, "Jira AddWorklog": 2, "Jira EditWorklog": 2, "Jira DeleteWorklog": 2})
}

func mustDate(t *testing.T, at time.Time) tracker.Date {
	t.Helper()
	d, err := tracker.DateOf(at)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

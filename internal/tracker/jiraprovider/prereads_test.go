package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/tempo/tempotest"
	"agent-overflow/internal/tracker/trackerapi"
)

// readPosts are the POSTs that read.
var readPosts = []string{searchPath, bulkFetchPath, changelogsPath}

// failedReads is a fake Jira that answers every request the test does not
// route with status, and records each such request that is a write.
type failedReads struct {
	mu   sync.Mutex
	sent []string
}

func (x *failedReads) serve(status int) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.Method != http.MethodGet && !slices.Contains(readPosts, r.URL.Path) {
			x.mu.Lock()
			x.sent = append(x.sent, r.Method+" "+r.URL.Path)
			x.mu.Unlock()
		}
		jiratest.Reply(status, `{"errorMessages":["failed"]}`)(w, r, b)
	}
}

// preRead is one write whose read before the write fails: setup routes
// the reads that come before it, and preRead is false for a write that
// reads nothing first, whose own request then fails.
type preRead struct {
	method  string // Provider's, "Time." for Jira's time, "Tempo." for Tempo's
	label   string
	setup   func(t *testing.T, f *jiratest.Server, ts *tempotest.Server, status int)
	call    func(ctx context.Context, p *Provider) tracker.Result
	preRead bool
}

// onSite routes the site's reads; sprints adds board 3's sprints.
func onSite(_ *testing.T, f *jiratest.Server, _ *tempotest.Server, _ int) { siteRoutes(f) }

func sprints(t *testing.T, f *jiratest.Server, ts *tempotest.Server, status int) {
	onSite(t, f, ts, status)
	sprintsOfBoard3(f)
}

func preReads() []preRead {
	start := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 14)
	jiraWL := tracker.Worklog{ID: "300", Issue: "10001", Author: "acc-user1"}
	tempoWL := tracker.Worklog{ID: "70", Issue: "10001", Author: "acc-user1"}
	worklog := func(_ *testing.T, f *jiratest.Server, _ *tempotest.Server, _ int) {
		f.On("GET", worklogsPath+"/300", jiratest.Reply(200, jiraWorklog("300", user1JSON, "2026-11-10T09:00:00.000-0500", 3600, "Review")))
	}
	tempoRead := func(_ *testing.T, _ *jiratest.Server, ts *tempotest.Server, status int) {
		ts.On("GET", "/4/worklogs/70", nil, "", status, `{"errors":[{"message":"failed"}]}`).Repeat = true
	}
	create := tracker.CreateInput{Project: "10000", Type: "10002", Title: "x"}
	edit := tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: "priority", Value: tracker.PickValues("1")}}}
	move := tracker.MoveInput{Issue: "10001", Move: "31"}
	estimate := tracker.EstimateInput{Board: "3", Issue: "10001"}
	rank := tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Next: "10002"}
	toSprint := tracker.SprintMoveInput{Board: "3", Sprint: "8", Issues: []tracker.IssueID{"10001"}}
	return []preRead{
		{"Create", "catalog", nil, func(ctx context.Context, p *Provider) tracker.Result { return p.Create(ctx, create) }, true},
		{"Create", "create screen", onSite, func(ctx context.Context, p *Provider) tracker.Result { return p.Create(ctx, create) }, true},
		{"Edit", "catalog", nil, func(ctx context.Context, p *Provider) tracker.Result { return p.Edit(ctx, edit) }, true},
		{"Edit", "edit screen", onSite, func(ctx context.Context, p *Provider) tracker.Result { return p.Edit(ctx, edit) }, true},
		{"Move", "catalog", nil, func(ctx context.Context, p *Provider) tracker.Result { return p.Move(ctx, move) }, true},
		{"Move", "transitions", onSite, func(ctx context.Context, p *Provider) tracker.Result { return p.Move(ctx, move) }, true},
		{"SetEstimate", "board", nil, func(ctx context.Context, p *Provider) tracker.Result { return p.SetEstimate(ctx, estimate) }, true},
		{"SetEstimate", "board check", onSite, func(ctx context.Context, p *Provider) tracker.Result { return p.SetEstimate(ctx, estimate) }, true},
		{"Rank", "board", nil, func(ctx context.Context, p *Provider) tracker.Result { return p.Rank(ctx, rank) }, true},
		{"Rank", "board check", onSite, func(ctx context.Context, p *Provider) tracker.Result { return p.Rank(ctx, rank) }, true},
		{"MoveToSprint", "board", nil, func(ctx context.Context, p *Provider) tracker.Result { return p.MoveToSprint(ctx, toSprint) }, true},
		{"MoveToSprint", "sprint check", onSite, func(ctx context.Context, p *Provider) tracker.Result { return p.MoveToSprint(ctx, toSprint) }, true},
		// The board's sprint listing holds the sprint, so the client reads
		// it no more: the board check is the last read.
		{"MoveToSprint", "board check", sprints, func(ctx context.Context, p *Provider) tracker.Result { return p.MoveToSprint(ctx, toSprint) }, true},
		{"CreateSprint", "none", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.CreateSprint(ctx, tracker.SprintInput{Board: "3", Name: "x"})
		}, false},
		{"StartSprint", "sprint check", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.StartSprint(ctx, tracker.SprintStart{Board: "3", Sprint: "8", Start: start, End: end})
		}, true},
		{"EditSprint", "sprint check", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.EditSprint(ctx, tracker.SprintEdit{Board: "3", Sprint: "7", Goal: ptr("")})
		}, true},
		{"CompleteSprint", "sprint check", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.CompleteSprint(ctx, tracker.SprintCompletion{Board: "3", Sprint: "7"})
		}, true},
		{"CompleteSprint", "done column", sprints, func(ctx context.Context, p *Provider) tracker.Result {
			return p.CompleteSprint(ctx, tracker.SprintCompletion{Board: "3", Sprint: "7"})
		}, true},
		{"AddComment", "project", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.AddComment(ctx, tracker.CommentInput{Issue: "10001", Markdown: "x"})
		}, true},
		{"EditComment", "comment", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.EditComment(ctx, tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: "x"})
		}, true},
		{"DeleteComment", "comment", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.DeleteComment(ctx, tracker.CommentDelete{Issue: "10001", Comment: "900"})
		}, true},
		// The link type is given by ID, which the client sends with no read.
		{"Link", "none", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Link(ctx, tracker.LinkInput{Type: "10000", Outward: "10001", Inward: "10002"})
		}, false},
		{"Unlink", "links", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Unlink(ctx, tracker.UnlinkInput{Issue: "10001", Link: "300", Other: "10002"})
		}, true},
		{"Attach", "none", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Attach(ctx, tracker.AttachInput{Issue: "10001", Files: []tracker.File{{Name: "a.txt", Content: strings.NewReader("a")}}})
		}, false},
		{"Time.AddWorklog", "who", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Time().AddWorklog(ctx, tracker.WorklogInput{Issue: "10001", Day: day1(), Spent: time.Hour})
		}, true},
		{"Time.EditWorklog", "worklog", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Time().EditWorklog(ctx, tracker.WorklogEdit{From: jiraWL, Spent: ptr(time.Hour)})
		}, true},
		{"Time.EditWorklog", "author", worklog, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Time().EditWorklog(ctx, tracker.WorklogEdit{From: jiraWL, Spent: ptr(time.Hour)})
		}, true},
		{"Time.DeleteWorklog", "worklog", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Time().DeleteWorklog(ctx, tracker.WorklogDelete{From: jiraWL})
		}, true},
		{"Time.DeleteWorklog", "author", worklog, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Time().DeleteWorklog(ctx, tracker.WorklogDelete{From: jiraWL})
		}, true},
		{"Tempo.AddWorklog", "who", nil, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Time().AddWorklog(ctx, tracker.WorklogInput{Issue: "10001", Day: day1(), Spent: time.Hour})
		}, true},
		{"Tempo.EditWorklog", "worklog", tempoRead, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Time().EditWorklog(ctx, tracker.WorklogEdit{From: tempoWL, Spent: ptr(time.Hour)})
		}, true},
		{"Tempo.DeleteWorklog", "worklog", tempoRead, func(ctx context.Context, p *Provider) tracker.Result {
			return p.Time().DeleteWorklog(ctx, tracker.WorklogDelete{From: tempoWL})
		}, true},
	}
}

func day1() tracker.Date {
	d, err := tracker.NewDate(2026, 11, 10)
	if err != nil {
		panic(err)
	}
	return d
}

// writeMethods are the names of iface's methods that return a
// tracker.Result, each with prefix.
func writeMethods(iface reflect.Type, prefix string) []string {
	var out []string
	for m := range iface.Methods() {
		if m.Type.NumOut() == 1 && m.Type.Out(0) == reflect.TypeFor[tracker.Result]() {
			out = append(out, prefix+m.Name)
		}
	}
	return out
}

// TestPreReadsComplete proves the table of TestFailedPreReads names every
// write of tracker.Provider, and of tracker.TimeTracker on both of Jira's
// time trackers, so a write added later fails here until it has a row.
func TestPreReadsComplete(t *testing.T) {
	named := map[string]bool{}
	for _, r := range preReads() {
		named[r.method] = true
	}
	writes := writeMethods(reflect.TypeFor[tracker.Provider](), "")
	writes = append(writes, writeMethods(reflect.TypeFor[tracker.TimeTracker](), "Time.")...)
	writes = append(writes, writeMethods(reflect.TypeFor[tracker.TimeTracker](), "Tempo.")...)
	if len(writes) < 19 {
		t.Fatalf("found only %d writes: %v", len(writes), writes)
	}
	for _, w := range writes {
		if !named[w] {
			t.Errorf("write %s has no row in preReads", w)
		}
	}
}

// TestFailedPreReads proves that when the read a write makes before it
// writes fails with a 500 or a 404, on the most minimal input Check
// permits, the result passes Result.Check, nothing is sent, and the
// tracker's answer is reachable from every part's outcome. A write that
// reads nothing first has its own request fail instead.
func TestFailedPreReads(t *testing.T) {
	for _, r := range preReads() {
		for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound} {
			t.Run(fmt.Sprintf("%s/%s/%d", r.method, r.label, status), func(t *testing.T) {
				f := jiratest.NewServer(t)
				x := &failedReads{}
				f.Otherwise(x.serve(status))
				var ts *tempotest.Server
				p := newProvider(t, f, nil)
				if strings.HasPrefix(r.method, "Tempo.") {
					ts, p = tempoProvider(t, f)
				}
				if r.setup != nil {
					r.setup(t, f, ts, status)
				}
				res := checked(t, r.call(t.Context(), p))
				if r.preRead && len(x.sent) > 0 {
					t.Errorf("sent %v", x.sent)
				}
				for i, part := range res.Parts {
					var se *trackerapi.StatusError
					if !errors.As(part.Outcome.Err(), &se) || se.Status != status {
						t.Errorf("part %d %s: %v", i, states(res), part.Outcome.Err())
					}
				}
			})
		}
	}
}

// tempoProvider is a Provider on Jira f with a Tempo of its own.
func tempoProvider(t *testing.T, f *jiratest.Server) (*tempotest.Server, *Provider) {
	ts := tempotest.NewServer(t, "fake-tempo-token")
	jc := newJira(t, f)
	tc, err := tempo.New(tempo.Options{
		BaseURL: ts.URL(), Version: "test", Issues: TempoIssues(jc),
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte("fake-tempo-token")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	p, err := New(Options{Jira: jc, Tempo: tc, Site: "https://example.atlassian.net", SprintsFrom: allSprints})
	if err != nil {
		t.Fatal(err)
	}
	return ts, p
}

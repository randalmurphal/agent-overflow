package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

const board3Sprints = "/rest/agile/1.0/board/3/sprint"

// heldSprintsSite is structureSite whose sprint listing Jira answers at
// read (no Date when zero).
func heldSprintsSite(t *testing.T, read time.Time) *jiratest.Server {
	f := structureSite(t)
	f.On("GET", board3Sprints, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if read.IsZero() {
			w.Header()["Date"] = nil
		} else {
			w.Header().Set("Date", read.Format(http.TimeFormat))
		}
		jiratest.Reply(200, `{"isLast":true,"values":[
			{"id":6,"name":"Recent","state":"closed","completeDate":"2026-11-03T10:00:00.000Z"},
			{"id":7,"name":"Sprint 1","state":"active"},{"id":8,"name":"Next","state":"future"}]}`)(w, r, b)
	})
	f.On("GET", board3Issues, jiratest.Reply(200, `{"isLast":true,"issues":[{"id":"10001","key":"WEB-1"}]}`))
	return f
}

// TestBoardIssuesUsesTheHeldSprints proves the board listing after a
// Structure takes the sprints Structure read, while Jira read them at or
// after the window; a listing read before the window, or of unknown age,
// is read again.
func TestBoardIssuesUsesTheHeldSprints(t *testing.T) {
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		read  time.Time
		reads int
	}{
		{"read after the window", window.Add(time.Hour), 1},
		{"read at the window", window, 1},
		{"read before the window", window.Add(-time.Second), 2},
		{"of unknown age", time.Time{}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := heldSprintsSite(t, tc.read)
			p := newProvider(t, f, nil)
			if _, err := p.Structure(t.Context(), "3", window); err != nil {
				t.Fatal(err)
			}
			got, err := p.BoardIssues(t.Context(), "3", window)
			if err != nil || !got.Complete || len(got.IDs) != 1 {
				t.Fatalf("got %+v %v", got, err)
			}
			if n := f.Count("GET", board3Sprints); n != tc.reads {
				t.Errorf("%d sprint listings, want %d", n, tc.reads)
			}
		})
	}
}

// TestBoardIssuesRetriesARefusedSprintList proves a listing Jira refused
// with a 400 (a held sprint deleted since) drops what the client holds of
// the board and is retried once with the sprints read again; a second
// refusal fails it.
func TestBoardIssuesRetriesARefusedSprintList(t *testing.T) {
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := heldSprintsSite(t, window.Add(time.Hour))
	p := newProvider(t, f, nil)
	if _, err := p.Structure(t.Context(), "3", window); err != nil {
		t.Fatal(err)
	}
	refusals := 1
	f.On("GET", board3Issues, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if refusals > 0 {
			refusals--
			jiratest.Reply(400, `{"errorMessages":["The value '6' does not exist for the field 'sprint'."]}`)(w, r, b)
			return
		}
		jiratest.Reply(200, `{"isLast":true,"issues":[{"id":"10001","key":"WEB-1"}]}`)(w, r, b)
	})
	if got, err := p.BoardIssues(t.Context(), "3", window); err != nil || len(got.IDs) != 1 {
		t.Fatalf("got %+v %v", got, err)
	}
	if s, c := f.Count("GET", board3Sprints), f.Count("GET", board3Config); s != 2 || c != 2 {
		t.Errorf("%d sprint listings and %d configurations, want each read again once", s, c)
	}
	refusals = 2
	if _, err := p.BoardIssues(t.Context(), "3", window); !isStatus(err, http.StatusBadRequest) {
		t.Errorf("refused twice: %v", err)
	}
	if n := f.Count("GET", board3Issues); n != 4 {
		t.Errorf("%d listings, want one retry each", n)
	}
}

// TestStructureForgetsOnlyItsBoard proves a Structure reads its own
// board's configuration and sprints again and keeps another board's.
func TestStructureForgetsOnlyItsBoard(t *testing.T) {
	f := heldSprintsSite(t, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	p := newProvider(t, f, nil)
	for range 2 {
		if _, err := p.jira.BoardConfig(t.Context(), 4); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Structure(t.Context(), "3", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.Count("GET", board4Config); n != 1 {
		t.Errorf("board 4's configuration read %d times, want 1", n)
	}
	if c, s := f.Count("GET", board3Config), f.Count("GET", board3Sprints); c != 2 || s != 2 {
		t.Errorf("board 3's configuration read %d times and sprints %d, want one each a Structure", c, s)
	}
}

// TestLookupCost pins Lookup's requests: one bulk fetch when it returns
// an issue under every key asked, and one read alone more for each key
// it leaves out, an old key or one that names nothing, once the field
// catalog is read. Each answer that leaves keys out (the bulk fetch's, a
// read alone's 404) and names no account it was made for costs a
// sign-in check after it, which shows it is the user's.
func TestLookupCost(t *testing.T) {
	f := lookupSite(t)
	p := newProvider(t, f, nil)
	current := map[string]map[string]any{
		"WEB-1": issue("10001", "WEB-1", updatedAt, nil), "APP-5": issue("10005", "APP-5", updatedAt, nil), "WEB-5": issue("10005", "APP-5", updatedAt, nil),
	}
	f.On("POST", bulkFetchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		var issues []any
		seen := map[string]bool{}
		for _, k := range bodyOf(t, b)["issueIdsOrKeys"].([]any) {
			if is, ok := current[strings.ToUpper(k.(string))]; ok && !seen[is["id"].(string)] {
				seen[is["id"].(string)] = true
				issues = append(issues, is)
			}
		}
		jiratest.Reply(200, mustJSON(map[string]any{"issues": issues, "issueErrors": []any{}}))(w, r, b)
	})
	if _, err := p.Lookup(t.Context(), []string{"WEB-1"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		keys     []string
		requests int
		found    int
	}{
		{[]string{"WEB-1", "APP-5"}, 1, 2},
		{[]string{"web-1"}, 1, 1},
		{[]string{"WEB-1", "WEB-9"}, 2, 1},
		{[]string{"WEB-5"}, 2, 1},
		{[]string{"WEB-1", "WEB-9", "WEB-5", "APP-5"}, 3, 3},
	} {
		before := len(f.Requests())
		got, err := p.Lookup(t.Context(), tc.keys)
		if err != nil || len(got.Found) != tc.found {
			t.Fatalf("%v: %+v %v", tc.keys, got, err)
		}
		if n := len(f.Requests()) - before; n != tc.requests {
			t.Errorf("%v: %d requests, want %d: %v", tc.keys, n, tc.requests, f.Requests()[before:])
		}
	}
	if n := f.Count("GET", myselfPath); n != 0 {
		t.Errorf("%d sign-in checks for answers that named their account", n)
	}
	// The most keys a lookup takes fit one bulk fetch batch.
	most := make([]string, tracker.MaxLookupKeys)
	for i := range most {
		k := fmt.Sprintf("BIG-%d", i+1)
		most[i] = k
		current[k] = issue(fmt.Sprint(20000+i), k, updatedAt, nil)
	}
	before := len(f.Requests())
	if got, err := p.Lookup(t.Context(), most); err != nil || len(got.Found) != len(most) {
		t.Fatalf("%d keys: %d found, %v", len(most), len(got.Found), err)
	}
	if n := len(f.Requests()) - before; n != 1 {
		t.Errorf("%d keys: %d requests, want 1", len(most), n)
	}
	f.SetAccount("")
	before = len(f.Requests())
	got, err := p.Lookup(t.Context(), []string{"WEB-1", "WEB-9"})
	if err != nil || !slices.Equal(got.Unmatched, []string{"WEB-9"}) {
		t.Fatalf("with no account named: %+v %v", got, err)
	}
	if n, m := len(f.Requests())-before, f.Count("GET", myselfPath); n != 4 || m != 2 {
		t.Errorf("with no account named: %d requests and %d sign-in checks, want 4 and 2: %v", n, m, f.Requests()[before:])
	}
}

// TestDetailFailures proves a failure of any of Detail's reads fails it
// and cancels the one running with it (the field catalog's runs with the
// issue's, then with the comments'), and a cancelled caller gets its
// cancellation.
func TestDetailFailures(t *testing.T) {
	for _, tc := range []struct{ failing, held string }{
		{"/rest/api/3/issue/10001/comment", fieldPath},
		{fieldPath, issuePath},
	} {
		t.Run(tc.failing, func(t *testing.T) {
			f, p := detailSite(t, 3)
			// The held read answers only once its request ends, so Detail
			// returns before the deadline only if the failure cancelled it.
			done := make(chan struct{})
			t.Cleanup(func() { close(done) })
			held := func(w http.ResponseWriter, r *http.Request, _ []byte) {
				select {
				case <-r.Context().Done():
				case <-done:
				}
			}
			f.On("GET", tc.held, held)
			f.On("GET", tc.failing, jiratest.Reply(403, `{"errorMessages":["no"]}`))
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if _, err := p.Detail(ctx, "10001"); !isStatus(err, http.StatusForbidden) {
				t.Errorf("got %v", err)
			}
			if ctx.Err() != nil {
				t.Errorf("the read of %s ran until the deadline: the failure did not cancel it", tc.held)
			}
		})
	}
	_, p := detailSite(t, 3)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Detail(ctx, "10001"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled caller: %v", err)
	}
	if _, err := p.Detail(t.Context(), "WEB 1"); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("a bad issue: %v", err)
	}
}

package jiraprovider

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// Details reads its issues detailsAtOnce at a time, keeps their order, and
// reports an issue Jira answers 404 for as Missing.
func TestDetails(t *testing.T) {
	f, p := detailSite(t, 1)
	var mu sync.Mutex
	inFlight, peak, closed := 0, 0, false
	released := make(chan struct{})
	missing := func(w http.ResponseWriter, r *http.Request, b []byte) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		if inFlight == detailsAtOnce && peak == detailsAtOnce && !closed {
			closed = true
			close(released)
		}
		mu.Unlock()
		// Held until detailsAtOnce reads are in flight together, so a
		// lower limit is seen as a lower peak, a higher one as a higher.
		select {
		case <-released:
		case <-time.After(5 * time.Second):
		}
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		jiratest.Reply(http.StatusNotFound, `{"errorMessages":["Issue does not exist or you do not have permission to see it."],"errors":{}}`)(w, r, b)
	}
	var ids []tracker.IssueID
	for i := range 2 * detailsAtOnce {
		id := fmt.Sprint(20001 + i)
		f.On("GET", "/rest/api/3/issue/"+id, missing)
		ids = append(ids, tracker.IssueID(id))
	}
	all := append(slices.Clone(ids[:3]), append([]tracker.IssueID{"10001"}, ids[3:]...)...)
	got, err := p.Details(t.Context(), all)
	if err != nil {
		t.Fatal(err)
	}
	if peak != detailsAtOnce {
		t.Errorf("%d reads in flight at most, want %d", peak, detailsAtOnce)
	}
	if len(got.Details) != 1 || got.Details[0].Card.Key != "WEB-1" || !slices.Equal(got.Missing, ids) {
		t.Errorf("details %d, missing %v", len(got.Details), got.Missing)
	}
}

// Only a 404 of an issue's own requests makes it Missing: one of its
// comments does, one of the field catalog fails the read. The catalog is
// read once for all the issues.
func TestDetailsMissingIsTheIssuesOwnNotFound(t *testing.T) {
	notFound := jiratest.Reply(http.StatusNotFound, `{"errorMessages":["test"],"errors":{}}`)
	t.Run("comments", func(t *testing.T) {
		f, p := detailSite(t, 1)
		f.On("GET", "/rest/api/3/issue/10001/comment", notFound)
		got, err := p.Details(t.Context(), []tracker.IssueID{"10001"})
		if err != nil || len(got.Details) != 0 || !slices.Equal(got.Missing, []tracker.IssueID{"10001"}) {
			t.Fatalf("details %d, missing %v, %v; want the issue missing", len(got.Details), got.Missing, err)
		}
	})
	t.Run("field catalog", func(t *testing.T) {
		f, p := detailSite(t, 1)
		f.On("GET", fieldPath, notFound)
		got, err := p.Details(t.Context(), []tracker.IssueID{"10001"})
		if err == nil || !isStatus(err, http.StatusNotFound) || len(got.Missing) != 0 {
			t.Fatalf("details with no field catalog: missing %v, %v; want the catalog's failure", got.Missing, err)
		}
	})
	t.Run("one catalog read", func(t *testing.T) {
		f, p := detailSite(t, 1)
		for _, id := range []string{"20001", "20002"} {
			f.On("GET", "/rest/api/3/issue/"+id, notFound)
		}
		p.jira.ForgetFields()
		before := len(f.Requests())
		if _, err := p.Details(t.Context(), []tracker.IssueID{"10001", "20001", "20002"}); err != nil {
			t.Fatal(err)
		}
		catalogs := 0
		for _, r := range f.Requests()[before:] {
			if r == "GET "+fieldPath {
				catalogs++
			}
		}
		if catalogs != 1 {
			t.Errorf("%d field catalog reads for three issues, want 1", catalogs)
		}
	})
}

// Any failure other than a 404 fails Details; an issue named twice or no
// issue ID is refused before any request.
func TestDetailsFailures(t *testing.T) {
	f, p := detailSite(t, 1)
	f.On("GET", "/rest/api/3/issue/20001", jiratest.Reply(http.StatusInternalServerError, `{"errorMessages":["test"]}`))
	if _, err := p.Details(t.Context(), []tracker.IssueID{"10001", "20001"}); err == nil || !isStatus(err, http.StatusInternalServerError) {
		t.Fatalf("a failed read: %v", err)
	}
	before := len(f.Requests())
	for _, ids := range [][]tracker.IssueID{{"10001", "10001"}, {"WEB-1"}} {
		if _, err := p.Details(t.Context(), ids); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("%v: %v", ids, err)
		}
	}
	if got := f.Requests()[before:]; len(got) != 0 {
		t.Errorf("refused calls sent %v", got)
	}
}

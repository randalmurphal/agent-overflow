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

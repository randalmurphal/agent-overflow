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

// inFlight counts the requests its handlers hold at once.
type inFlight struct {
	mu        sync.Mutex
	now, peak int
	closed    bool
	released  chan struct{}
	together  int
}

// heldTogether holds each request its handlers take until n are in
// flight together, or 5 seconds pass, so a lower limit is seen as a
// lower peak and a higher one as a higher.
func heldTogether(n int) *inFlight { return &inFlight{released: make(chan struct{}), together: n} }

// hold is answer, answered once the request has been held.
func (c *inFlight) hold(answer http.HandlerFunc) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		c.held()
		answer(w, r)
	}
}

// held holds one request.
func (c *inFlight) held() {
	c.mu.Lock()
	c.now++
	c.peak = max(c.peak, c.now)
	if c.now == c.together && c.peak == c.together && !c.closed {
		c.closed = true
		close(c.released)
	}
	c.mu.Unlock()
	select {
	case <-c.released:
	case <-time.After(5 * time.Second):
	}
	time.Sleep(10 * time.Millisecond)
	c.mu.Lock()
	c.now--
	c.mu.Unlock()
}

// most is the most requests that were in flight at once.
func (c *inFlight) most() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

const notFoundJSON = `{"errorMessages":["Issue does not exist or you do not have permission to see it."],"errors":{}}`

// Details reads its issues detailsAtOnce at a time, keeps their order, and
// reports an issue Jira answers 404 for as Missing.
func TestDetails(t *testing.T) {
	f, p := detailSite(t, 1)
	held := heldTogether(detailsAtOnce)
	missing := held.hold(func(w http.ResponseWriter, r *http.Request) {
		jiratest.Reply(http.StatusNotFound, notFoundJSON)(w, r, nil)
	})
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
	if n := held.most(); n != detailsAtOnce {
		t.Errorf("%d reads in flight at most, want %d", n, detailsAtOnce)
	}
	if len(got.Details) != 1 || got.Details[0].Card.Key != "WEB-1" || !slices.Equal(got.Missing, ids) {
		t.Errorf("details %d, missing %v", len(got.Details), got.Missing)
	}
}

// Only a 404 of an issue's own requests makes it Missing: one of its
// comments does, one of the field catalog fails the read. The catalog is
// read for all the issues together, as often as for one: once, and once
// more when the issues name fields it lacks (the fixture's issue does).
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
		catalogs := func(issues ...tracker.IssueID) int {
			p.jira.ForgetCredential() // drops the cached catalog
			before := len(f.Requests())
			if _, err := p.Details(t.Context(), issues); err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, r := range f.Requests()[before:] {
				if r == "GET "+fieldPath {
					n++
				}
			}
			return n
		}
		one, three := catalogs("10001"), catalogs("10001", "20001", "20002")
		if one != 2 || three != one {
			t.Errorf("%d field catalog reads for one issue and %d for three, want 2 for each", one, three)
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

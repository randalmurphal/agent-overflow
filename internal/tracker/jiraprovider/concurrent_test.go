package jiraprovider

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// TestConcurrentUse drives reads and writes of one Provider from many
// goroutines; run under -race it proves the Provider shares no state
// unguarded, and every call still gets its own answer.
func TestConcurrentUse(t *testing.T) {
	f, x := deltaSite(t)
	p := newProvider(t, f, nil)
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	for id := int64(10001); id <= 10006; id++ {
		x.set(id, at)
	}
	f.On("POST", bulkFetchPath, jiratest.Reply(200, bulk(issue("10001", "WEB-1", updatedAt, nil))))
	f.On("GET", board4Issues, jiratest.Reply(200, page("", issue("10001", "WEB-1", updatedAt, nil))))
	f.On("PUT", "/rest/agile/1.0/issue/rank", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 40 {
		wg.Go(func() {
			switch i % 4 {
			case 0:
				page, err := p.ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", Start: at})
				if err == nil && len(page.Rows) != 2 {
					err = errors.New("a delta page of the wrong size")
				}
				errs <- err
			case 1:
				cards, err := p.Cards(t.Context(), []tracker.IssueID{"10001"})
				if err == nil && len(cards.Cards) != 1 {
					err = errors.New("no card")
				}
				errs <- err
			case 2:
				rows, err := p.BoardRows(t.Context(), "4", []tracker.IssueID{"10001"}, tracker.Eligibility{})
				if err == nil && (len(rows.Rows) != 1 || *rows.Rows[0].Estimate != 7200) {
					err = errors.New("a row read through the wrong board")
				}
				errs <- err
			case 3:
				res := p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Next: "10002"})
				switch err := res.Check(); {
				case err != nil:
					errs <- err
				case !res.AllApplied():
					errs <- errors.New("rank " + states(res))
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := f.Count("GET", board3Config) + f.Count("GET", board4Config); n > 40 {
		t.Errorf("%d configuration reads; the client's cache was not used", n)
	}
}

// TestCancelledCalls proves a call whose context ended sends nothing: a
// read fails with the context's error and a write is not sent.
func TestCancelledCalls(t *testing.T) {
	f, x := deltaSite(t)
	p := newProvider(t, f, nil)
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	x.set(10001, at)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := len(f.Requests())
	if _, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: "3", Start: at}); !errors.Is(err, context.Canceled) {
		t.Errorf("delta: %v", err)
	}
	if _, err := p.Cards(ctx, []tracker.IssueID{"10001"}); !errors.Is(err, context.Canceled) {
		t.Errorf("cards: %v", err)
	}
	for name, res := range map[string]tracker.Result{
		"rank":     p.Rank(ctx, tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Next: "10002"}),
		"estimate": p.SetEstimate(ctx, tracker.EstimateInput{Board: "3", Issue: "10001", Value: ptr(1.0)}),
		"edit":     p.Edit(ctx, tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: "priority", Value: tracker.PickValues("1")}}}),
		"comment":  p.AddComment(ctx, tracker.CommentInput{Issue: "10001", Markdown: "x"}),
		"sprint":   p.CreateSprint(ctx, tracker.SprintInput{Board: "3", Name: "x"}),
		"link":     p.Link(ctx, tracker.LinkInput{Type: "10000", Outward: "10001", Inward: "10002"}),
		"worklog":  p.Time().AddWorklog(ctx, tracker.WorklogInput{Issue: "10001", Day: day(t, 2026, 11, 10), Spent: time.Hour}),
	} {
		checked(t, res)
		if res.Tally().NotSent != len(res.Parts) {
			t.Errorf("%s: %s", name, states(res))
		}
	}
	if n := len(f.Requests()); n != before {
		t.Errorf("%d requests sent after the context ended", n-before)
	}
}

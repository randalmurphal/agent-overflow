package linearprovider

import (
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// History is complete through the settling margin before Linear's own
// clock, the Date of the answer that carried it, whether that clock is
// behind this computer's or ahead of it.
func TestHistoryCompleteToByLinearClock(t *testing.T) {
	for name, offset := range map[string]time.Duration{"behind": -time.Hour, "ahead": time.Hour} {
		t.Run(name, func(t *testing.T) {
			s := newScenario(t)
			s.clock.now = time.Now().Add(offset).Truncate(time.Second)
			h, err := s.p.History(s.ctx, tracker.HistoryQuery{Issues: []tracker.IssueID{tracker.IssueID(scIssue1)}})
			if err != nil || len(h.Issues) != 1 {
				t.Fatalf("history %+v %v", h, err)
			}
			if want := s.clock.Now().Add(-settleMargin); !h.Issues[0].CompleteTo.Equal(want) {
				t.Errorf("complete to %v, want %v (Linear's clock %v)", h.Issues[0].CompleteTo, want, s.clock.Now())
			}
		})
	}
}

// An answer without a Date falls back to this computer's clock, read
// before the request.
func TestHistoryCompleteToWithoutDate(t *testing.T) {
	t.Parallel()
	ex := lineartest.Exchange{Op: "IssueHistories", Vars: historiesVars(issueID(1)), HeaderFn: noDate,
		Body: page(lineartest.IssueChangesJSON(issueID(1), web, "2026-11-01T09:00:00.000Z", nil, false, ""))}
	p, _ := newProvider(t, ex)
	local := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return local }
	h, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{tracker.IssueID(issueID(1))}})
	if err != nil || len(h.Issues) != 1 || !h.Issues[0].CompleteTo.Equal(local.Add(-settleMargin)) {
		t.Fatalf("history %+v %v", h, err)
	}
}

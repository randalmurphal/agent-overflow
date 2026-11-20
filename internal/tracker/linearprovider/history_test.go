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
				t.Errorf("complete to %v, want %v (Linear's clock)", h.Issues[0].CompleteTo, want)
			}
		})
	}
}

// An answer without a Date is final through no time: this computer's
// clock never stands in for Linear's. The margin is the one the
// provider's capabilities name.
func TestHistoryCompleteToWithoutDate(t *testing.T) {
	t.Parallel()
	ex := lineartest.Exchange{Op: "IssueHistories", Vars: historiesVars(issueID(1)), HeaderFn: noDate,
		Body: page(lineartest.IssueChangesJSON(issueID(1), web, "2026-11-01T09:00:00.000Z", nil, false, ""))}
	p, _ := newProvider(t, ex)
	h, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{tracker.IssueID(issueID(1))}})
	if err != nil || len(h.Issues) != 1 || !h.Issues[0].CompleteTo.IsZero() {
		t.Fatalf("history %+v %v", h, err)
	}
	if settle := p.Capabilities().HistorySettle; settle != settleMargin {
		t.Errorf("capabilities name a settle margin of %v, want %v", settle, settleMargin)
	}
}

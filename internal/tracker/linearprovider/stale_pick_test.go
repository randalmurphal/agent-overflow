package linearprovider

import (
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
)

// A value picked from a list that changed since names nothing now. Linear
// refuses the write naming the input member (the fake answers the
// measured refusal), and the provider's own check refuses a status the
// issue's team lacks before sending. Both are a stale write: reading the
// choices again shows what changed. Nothing of the write is applied.
func TestFakeAPickThatNamesNothingIsStale(t *testing.T) {
	s := newScenario(t)
	const gone = "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"
	before := s.detail(t, scIssue2).Card
	for _, c := range []struct {
		name, member string
		write        func() tracker.Result
	}{
		{"assignee", "assigneeId", func() tracker.Result {
			return s.p.Edit(s.ctx, tracker.EditInput{Issue: tracker.IssueID(scIssue2), Fields: []tracker.FieldEdit{
				{Field: tracker.FieldTitle, Value: tracker.TypedValues("Not applied")},
				{Field: tracker.FieldAssignee, Value: tracker.PickValues(gone)},
			}})
		}},
		{"label", "labelIds", func() tracker.Result {
			return s.p.Edit(s.ctx, tracker.EditInput{Issue: tracker.IssueID(scIssue2), Fields: []tracker.FieldEdit{
				{Field: tracker.FieldLabels, Value: tracker.PickValues(gone)},
			}})
		}},
		{"status", "status " + gone, func() tracker.Result {
			return s.p.Move(s.ctx, tracker.MoveInput{Issue: tracker.IssueID(scIssue2), Move: tracker.MoveID(gone)})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := c.write()
			if len(r.Parts) != 1 {
				t.Fatalf("parts %+v", r.Parts)
			}
			refusal, ok := r.Parts[0].Outcome.Refusal()
			if !ok || refusal.Reason != tracker.ReasonStale || !strings.Contains(refusal.Message, c.member) {
				t.Fatalf("outcome %s, refusal %+v; want stale naming %s", r.Parts[0].Outcome.State(), refusal, c.member)
			}
		})
	}
	if after := s.detail(t, scIssue2).Card; after.Title != before.Title || after.Assignee != before.Assignee || after.Status != before.Status {
		t.Fatalf("a refused write changed the issue: %+v, was %+v", after, before)
	}
}

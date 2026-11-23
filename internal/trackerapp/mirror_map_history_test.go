package trackerapp

import (
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// historyOf reads back the history mirrorHistory stored, for tests that
// hold a stored history to the one it was written from. A change of a kind
// this package does not know is an error: the store refuses to write one.
func historyOf(m store.MirrorHistory) (tracker.IssueHistory, error) {
	h := tracker.IssueHistory{Issue: tracker.IssueID(m.IssueID), CompleteFrom: timeOf(m.CompleteFrom)}
	for _, c := range m.Changes {
		ch := tracker.Change{At: timeOf(c.At), Author: tracker.UserID(c.AuthorID)}
		switch c.Kind {
		case store.MirrorChangeStatus:
			h.Status = append(h.Status, tracker.StatusChange{Change: ch, From: tracker.StatusID(c.From), To: tracker.StatusID(c.To)})
		case store.MirrorChangeSprint:
			h.Sprint = append(h.Sprint, tracker.SprintChange{Change: ch, From: sprintIDs(c.FromSprints), To: sprintIDs(c.ToSprints)})
		case store.MirrorChangeEstimate:
			h.Estimate = append(h.Estimate, tracker.EstimateChange{
				Change: ch, Board: tracker.BoardID(c.BoardID), From: copyNumber(c.FromNumber), To: copyNumber(c.ToNumber),
			})
		case store.MirrorChangeAssignee:
			h.Assignee = append(h.Assignee, tracker.AssigneeChange{Change: ch, From: tracker.UserID(c.From), To: tracker.UserID(c.To)})
		default:
			return tracker.IssueHistory{}, fmt.Errorf("trackerapp: issue %s has a change of unknown kind %q", m.IssueID, c.Kind)
		}
	}
	return h, nil
}

func sprintIDs(ids []string) []tracker.SprintID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]tracker.SprintID, len(ids))
	for i, id := range ids {
		out[i] = tracker.SprintID(id)
	}
	return out
}

package trackerapp

import (
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// mirrorHistory is h's changes as the store holds them, each kind oldest
// first. What the read was for and whether it is final are the caller's
// (run.historyOf).
func mirrorHistory(h tracker.IssueHistory) store.MirrorHistory {
	out := store.MirrorHistory{IssueID: string(h.Issue), CompleteFrom: millis(h.CompleteFrom)}
	for _, c := range h.Status {
		out.Changes = append(out.Changes, store.MirrorChange{
			Kind: store.MirrorChangeStatus, At: millis(c.At), AuthorID: string(c.Author), From: string(c.From), To: string(c.To),
		})
	}
	for _, c := range h.Sprint {
		out.Changes = append(out.Changes, store.MirrorChange{
			Kind: store.MirrorChangeSprint, At: millis(c.At), AuthorID: string(c.Author),
			FromSprints: sprintSet(c.From), ToSprints: sprintSet(c.To),
		})
	}
	for _, c := range h.Estimate {
		out.Changes = append(out.Changes, store.MirrorChange{
			Kind: store.MirrorChangeEstimate, At: millis(c.At), AuthorID: string(c.Author), BoardID: string(c.Board),
			FromNumber: copyNumber(c.From), ToNumber: copyNumber(c.To),
		})
	}
	for _, c := range h.Assignee {
		out.Changes = append(out.Changes, store.MirrorChange{
			Kind: store.MirrorChangeAssignee, At: millis(c.At), AuthorID: string(c.Author), From: string(c.From), To: string(c.To),
		})
	}
	return out
}

// sprintSet is a sprint change's side as the store holds it: never nil,
// so a change to or from no sprint is still a sprint change.
func sprintSet(ids []tracker.SprintID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

func copyNumber(n *float64) *float64 {
	if n == nil {
		return nil
	}
	v := *n
	return &v
}

// historyOf is the history m holds. A change of a kind this package does
// not know is an error: the store refuses to write one.
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

// emptyHistory is what the mirror records for an issue a history read
// asked for and the tracker did not return (deleted, or no longer
// visible): no change is known before updatedAt, the issue's update time
// in the mirror, so no value before it is inferred. The caller records it
// final for that version, so the issue is due again only when its card
// moves.
func emptyHistory(issue tracker.IssueID, updatedAt int64) store.MirrorHistory {
	return store.MirrorHistory{IssueID: string(issue), CompleteFrom: updatedAt}
}

// mirrorWorklog is w as the store holds it.
func mirrorWorklog(w tracker.Worklog) store.MirrorWorklog {
	return store.MirrorWorklog{
		ID: string(w.ID), IssueID: string(w.Issue), IssueKey: w.IssueKey, IssueTitle: w.IssueTitle, AuthorID: string(w.Author), Day: w.Day.String(),
		SpentSeconds: int64(w.Spent / time.Second), Description: w.Description, UpdatedAt: millis(w.Updated),
	}
}

// worklogOf is the worklog m holds. A day the store holds is YYYY-MM-DD,
// which it checks on write.
func worklogOf(m store.MirrorWorklog) (tracker.Worklog, error) {
	day, err := tracker.ParseDate(m.Day)
	if err != nil {
		return tracker.Worklog{}, fmt.Errorf("trackerapp: worklog %s: %w", m.ID, err)
	}
	return tracker.Worklog{
		ID: tracker.WorklogID(m.ID), Issue: tracker.IssueID(m.IssueID), IssueKey: m.IssueKey, IssueTitle: m.IssueTitle,
		Author: tracker.UserID(m.AuthorID), Day: day, Spent: time.Duration(m.SpentSeconds) * time.Second, Description: m.Description, Updated: timeOf(m.UpdatedAt),
	}, nil
}

// bodyBlob is a ticket's body: its detail without what the issue record
// holds (the card, which every read of the issue shares) and without what
// a read derives (HistoryTo and StatusChanged, which the status evidence
// took: statusEvidence). The detail is encoded as the neutral type is, so
// TestBodyBlobShape pins that type's shape: a change to it bumps
// bodyBlobVersion, which makes every stored body a miss.
type bodyBlob struct {
	V      int            `json:"v"`
	Detail tracker.Detail `json:"detail"`
}

// maxDetailEvents bounds the history a detail keeps: the newest events.
const maxDetailEvents = 500

// boundHistory keeps d's newest maxDetailEvents events, in time order,
// and moves HistoryFrom to the oldest it keeps, so the history stays
// complete from HistoryFrom: events at the time of one left out are left
// out with it.
func boundHistory(d *tracker.Detail) {
	if len(d.History) <= maxDetailEvents {
		return
	}
	slices.SortStableFunc(d.History, func(a, b tracker.Event) int { return a.At.Compare(b.At) })
	cut := len(d.History) - maxDetailEvents
	last := d.History[cut-1].At
	for cut < len(d.History) && !d.History[cut].At.After(last) {
		cut++
	}
	d.History = slices.Clone(d.History[cut:])
	if len(d.History) > 0 && d.History[0].At.After(d.HistoryFrom) {
		d.HistoryFrom = d.History[0].At
	} else if len(d.History) == 0 {
		d.HistoryFrom = last.Add(time.Nanosecond)
	}
}

// mirrorBody is d's body as the store holds it (bodyBlob). A body read
// again with nothing changed is the same bytes, whenever it was read and
// whatever its card says.
func mirrorBody(d tracker.Detail) ([]byte, error) {
	d.Card, d.HistoryTo, d.StatusChanged = tracker.Card{}, time.Time{}, tracker.StatusChange{}
	return encodeBlob("body", bodyBlob{V: bodyBlobVersion, Detail: d})
}

// bodyOf is the ticket b holds, shown with the record's card it was read
// with (ticketDoc.record); ok is false when its blob is a miss.
func bodyOf(b store.MirrorBody) (ticketDoc, bool) {
	var blob bodyBlob
	if !decodeBlob(b.Data, bodyBlobVersion, &blob, &blob.V) || b.Card.IssueID != b.IssueID {
		return ticketDoc{}, false
	}
	blob.Detail.Card, _ = cardOf(b.Card)
	return ticketDoc{d: blob.Detail, version: b.UpdatedAt, record: true, since: b.Card.StatusSince}, true
}

// statusEvidence is what d, read from a tracker whose history may lack a
// change for settle after it, tells of when its status began
// (store.MirrorStatusSince), taken before boundHistory moves HistoryFrom:
// final when its history is final through the card's update (HistoryTo,
// the tracker's clock less its settle margin, at or after Card.Updated),
// complete when it starts at the creation (HistoryFrom zero; Linear folds
// its opening interval, so it never is), with the latest status change it
// holds.
func statusEvidence(d tracker.Detail, settle time.Duration) store.MirrorStatusEvidence {
	c := d.Card
	return store.MirrorStatusEvidence{
		Status: string(c.Status), CreatedAt: millis(c.Created), UpdatedAt: millis(c.Updated),
		LatestTo: string(d.StatusChanged.To), LatestAt: millis(d.StatusChanged.At),
		Complete: d.HistoryFrom.IsZero(), Final: !d.HistoryTo.IsZero() && !d.HistoryTo.Before(c.Updated),
		Settle: settle.Milliseconds(),
	}
}

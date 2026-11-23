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
		ID: string(w.ID), IssueID: string(w.Issue), AuthorID: string(w.Author), Day: w.Day.String(),
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
		ID: tracker.WorklogID(m.ID), Issue: tracker.IssueID(m.IssueID), Author: tracker.UserID(m.AuthorID), Day: day,
		Spent: time.Duration(m.SpentSeconds) * time.Second, Description: m.Description, Updated: timeOf(m.UpdatedAt),
	}, nil
}

// detailBlob is an issue's whole detail. The detail is encoded as the
// neutral type is, so TestDetailBlobShape pins that type's shape: a
// change to it bumps detailBlobVersion, which makes every stored detail a
// miss.
type detailBlob struct {
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

// mirrorDetail is d's blob as the store holds it.
func mirrorDetail(d tracker.Detail) ([]byte, error) {
	return encodeBlob("detail", detailBlob{V: detailBlobVersion, Detail: d})
}

// detailOf is the detail m holds; ok is false when its blob is a miss.
func detailOf(m store.MirrorDetail) (tracker.Detail, bool) {
	var blob detailBlob
	if !decodeBlob(m.Data, detailBlobVersion, &blob, &blob.V) || string(blob.Detail.Card.ID) != m.IssueID {
		return tracker.Detail{}, false
	}
	return blob.Detail, true
}

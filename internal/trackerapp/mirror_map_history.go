package trackerapp

import (
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// mirrorHistory is h as the store holds it: its changes by kind, each
// kind oldest first.
func mirrorHistory(h tracker.IssueHistory) store.MirrorHistory {
	out := store.MirrorHistory{IssueID: string(h.Issue), CompleteFrom: millis(h.CompleteFrom), CompleteTo: millis(h.CompleteTo)}
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
	h := tracker.IssueHistory{Issue: tracker.IssueID(m.IssueID), CompleteFrom: timeOf(m.CompleteFrom), CompleteTo: timeOf(m.CompleteTo)}
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
// visible): no change is known before read, the time the read began in
// Unix milliseconds, so no value before it is inferred, and the issue is
// due again only when its card moves.
func emptyHistory(issue tracker.IssueID, read int64) store.MirrorHistory {
	return store.MirrorHistory{IssueID: string(issue), CompleteFrom: read, CompleteTo: read}
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

package jiraprovider

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// historySettle is how long before a history read began its changes are
// taken as final. Whether Jira records a change in its changelog at once
// is not measured; Jira documents that a search can lag a recent update,
// and the margin also covers a skew between this computer's clock and
// Jira's, since the caller compares CompleteTo with Jira's update times.
// A larger margin costs only a second read of an issue's history.
const historySettle = time.Minute

// boardEstimate is one board's estimate field, whose changes are that
// board's estimate changes.
type boardEstimate struct {
	board tracker.BoardID
	field string
}

// History reads the status, assignee, sprint and estimate changes of the
// issues q names with Jira's bulk changelog, which records every change
// from an issue's creation, so CompleteFrom is zero, and each history is
// final through historySettle before the read began. Estimate changes are
// read for each board q names through that board's estimate field; two
// boards on one field each get the field's changes.
//
// Jira's bulk changelog leaves out an issue with no change of the fields
// asked for and an issue the user cannot see alike. The issues it leaves
// out are read with a bulk fetch: those it returns have no changes and
// get an empty history, and the others are not visible and get none.
func (p *Provider) History(ctx context.Context, q tracker.HistoryQuery) (tracker.History, error) {
	if len(q.Issues) == 0 {
		return tracker.History{}, invalidf("a history read needs issues")
	}
	if err := checkIssues(q.Issues); err != nil {
		return tracker.History{}, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.History{}, err
	}
	fields := []string{"status", "assignee"}
	if s.sprint != "" {
		fields = append(fields, s.sprint)
	}
	var ests []boardEstimate
	for _, id := range q.Boards {
		b, err := p.board(ctx, id)
		if err != nil {
			return tracker.History{}, err
		}
		if b.estimated() {
			ests = append(ests, boardEstimate{board: id, field: b.cfg.Estimation.FieldID})
			if !slices.Contains(fields, b.cfg.Estimation.FieldID) {
				fields = append(fields, b.cfg.Estimation.FieldID)
			}
		}
	}
	ids := distinct(q.Issues)
	refs := make([]string, len(ids))
	for i, id := range ids {
		refs[i] = string(id)
	}
	completeTo := p.now().Add(-historySettle)
	logs, err := p.jira.BulkChangelogs(ctx, jira.ChangelogQuery{Issues: refs, Fields: fields})
	if err != nil {
		return tracker.History{}, fmt.Errorf("jiraprovider: history: %w", err)
	}
	visible, err := p.visible(ctx, ids, logs.ByIssue)
	if err != nil {
		return tracker.History{}, err
	}
	cat := newCatalog()
	var out tracker.History
	for _, id := range ids {
		if !visible[id] {
			continue
		}
		h, err := issueHistory(id, logs.ByIssue[string(id)], s.sprint, ests, cat)
		if err != nil {
			return tracker.History{}, err
		}
		h.CompleteTo = completeTo
		out.Issues = append(out.Issues, h)
	}
	out.Catalog = cat.out
	return out, nil
}

// visible reports which of ids the user can see: those with changes, and
// of the others those a bulk fetch returns.
func (p *Provider) visible(ctx context.Context, ids []tracker.IssueID, byIssue map[string][]jira.ChangeEntry) (map[tracker.IssueID]bool, error) {
	out := map[tracker.IssueID]bool{}
	var absent []string
	for _, id := range ids {
		if _, ok := byIssue[string(id)]; ok {
			out[id] = true
		} else {
			absent = append(absent, string(id))
		}
	}
	if len(absent) == 0 {
		return out, nil
	}
	res, err := p.jira.BulkFetch(ctx, absent, "status")
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: history: which issues without changes exist: %w", err)
	}
	if len(res.Failed) > 0 {
		return nil, fmt.Errorf("jiraprovider: history: Jira could not read issue %s: %s", res.Failed[0].ID, res.Failed[0].Message)
	}
	for _, is := range res.Issues {
		out[tracker.IssueID(is.ID)] = true
	}
	return out, nil
}

// issueHistory is id's changes from entries, oldest first. An entry
// whose time or sprint list did not read fails it: a change that cannot
// be placed would make every later value wrong.
func issueHistory(id tracker.IssueID, entries []jira.ChangeEntry, sprintField string, ests []boardEstimate, cat *catalog) (tracker.IssueHistory, error) {
	h := tracker.IssueHistory{Issue: id}
	for _, e := range entries {
		if len(e.Problems) > 0 || e.Created.IsZero() {
			return tracker.IssueHistory{}, fmt.Errorf("jiraprovider: history of %s: change %s has no readable time", id, e.ID)
		}
		ch := tracker.Change{At: e.Created}
		if e.Author != nil {
			ch.Author = tracker.UserID(e.Author.AccountID)
			cat.user(e.Author)
		}
		for _, it := range e.Items {
			if err := addChange(&h, ch, it, sprintField, ests); err != nil {
				return tracker.IssueHistory{}, fmt.Errorf("jiraprovider: history of %s: change %s: %w", id, e.ID, err)
			}
		}
	}
	return h, nil
}

// addChange adds the change it records to h.
func addChange(h *tracker.IssueHistory, ch tracker.Change, it jira.ChangeItem, sprintField string, ests []boardEstimate) error {
	if from, to, ok := it.StatusChange(); ok {
		h.Status = append(h.Status, tracker.StatusChange{Change: ch, From: tracker.StatusID(from), To: tracker.StatusID(to)})
	}
	if from, to, ok := it.AssigneeChange(); ok {
		h.Assignee = append(h.Assignee, tracker.AssigneeChange{Change: ch, From: tracker.UserID(from), To: tracker.UserID(to)})
	}
	if sprintField != "" && it.FieldID == sprintField {
		from, to, err := it.SprintIDs()
		if err != nil {
			return err
		}
		h.Sprint = append(h.Sprint, tracker.SprintChange{Change: ch, From: sprintSet(from), To: sprintSet(to)})
	}
	for _, e := range ests {
		if it.FieldID != e.field {
			continue
		}
		from, err := estimateNumber(it.From)
		if err != nil {
			return err
		}
		to, err := estimateNumber(it.To)
		if err != nil {
			return err
		}
		h.Estimate = append(h.Estimate, tracker.EstimateChange{Change: ch, Board: e.board, From: from, To: to})
	}
	return nil
}

// sprintSet is ids as neutral sprint IDs, sorted as text.
func sprintSet(ids []int64) []tracker.SprintID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]tracker.SprintID, len(ids))
	for i, id := range ids {
		out[i] = tracker.SprintID(decimal(id))
	}
	slices.Sort(out)
	return out
}

// estimateNumber reads an estimate field's changed value: a number, or
// none when empty.
func estimateNumber(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("estimate %q is not a number", s)
	}
	return &n, nil
}

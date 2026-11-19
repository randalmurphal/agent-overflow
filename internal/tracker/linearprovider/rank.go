package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// Bounds of the read Rank makes when its neighbors leave no room: the
// team's manual order, rankPage issues a page, at most rankPages pages
// (2,500 issues, about 25,000 points by Linear's documented rule).
const (
	rankPage  = 250
	rankPages = 10
)

// Rank places issues, in order, between two neighbors in the team's
// manual order (sortOrder, which every board column and the backlog
// share) with one update per issue. The values come from the client's
// Spread of the neighbors' keys. When no values fit between them
// (linear.ErrNoRoom), it reads the manual order again from the start,
// since Linear may have spread the values itself, and renumbers the run
// from just after Prev up to the first issue with room above Prev,
// through the client's RenumberIssues: the issues ranked first, then
// those it moved to make room, each its own part. It stops at the first
// update not applied; the rest are not sent.
func (p *Provider) Rank(ctx context.Context, in tracker.RankInput) tracker.Result {
	step := tracker.StepRank
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issues...)
	}
	ids := make([]string, len(in.Issues))
	for i, id := range in.Issues {
		if !linear.IsIssueID(string(id)) {
			return refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: fmt.Sprintf("%q is not a Linear issue ID", id)}, in.Issues...)
		}
		ids[i] = string(id)
	}
	before, err := neighborValue(in.Prev)
	if err == nil {
		var after *float64
		after, err = neighborValue(in.Next)
		if err == nil {
			_, err = linear.Spread(before, after, len(ids))
			if err == nil {
				return p.renumber(ctx, in.Board, ids, before, after)
			}
		}
	}
	if !errors.Is(err, linear.ErrNoRoom) {
		return tracker.Invalid(step, err, in.Issues...)
	}
	if in.Prev == nil {
		return refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: "no order value fits before the first issue"}, in.Issues...)
	}
	run, lo, hi, err := p.crowdedRun(ctx, in.Board, in.Prev.Issue, ids)
	if err != nil {
		return editRefusal(step, err, in.Issues...)
	}
	return p.renumber(ctx, in.Board, run, lo, hi)
}

// neighborValue is a neighbor's sortOrder from its order key; nil for no
// neighbor. A key FloatOrderKey did not make is refused, never read.
func neighborValue(n *tracker.Neighbor) (*float64, error) {
	if n == nil {
		return nil, nil
	}
	v, err := n.Order.Float()
	if err != nil {
		return nil, fmt.Errorf("%w: neighbor %s: %w", tracker.ErrInvalidInput, n.Issue, err)
	}
	return &v, nil
}

// crowdedRun reads the team's manual order and returns the run to
// renumber after prev: moving, then every following issue up to the
// first whose value leaves room for the run above prev's, with the
// values around the run (hi nil at the end of the list).
func (p *Provider) crowdedRun(ctx context.Context, board tracker.BoardID, prev tracker.IssueID, moving []string) ([]string, *float64, *float64, error) {
	q := linear.IssueQuery{Filter: linear.IssueFilter{Teams: []string{string(board)}}, Order: linear.OrderManual, Limit: rankPage}
	var lo *float64
	run := slices.Clone(moving)
	after := ""
	for range rankPages {
		page, err := p.c.ListIssues(ctx, q, after)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("linearprovider: read the manual order of %s: %w", board, err)
		}
		for _, is := range page.Items {
			switch {
			case lo == nil && is.ID == string(prev):
				v := is.SortOrder
				lo = &v
			case lo == nil || slices.Contains(moving, is.ID):
			default:
				hi := is.SortOrder
				if _, err := linear.Spread(lo, &hi, len(run)); err == nil {
					return run, lo, &hi, nil
				}
				run = append(run, is.ID)
			}
		}
		if !page.HasNextPage {
			if lo == nil {
				return nil, nil, nil, refusedf(tracker.ReasonStale, "issue %s is no longer on the board", prev)
			}
			return run, lo, nil, nil
		}
		after = page.EndCursor
	}
	return nil, nil, nil, refusedf(tracker.ReasonInvalid, "no room near issue %s within the first %d issues of the board", prev, rankPage*rankPages)
}

// renumber gives the run Spread values between lo and hi through the
// client, one part per issue in order: those stored applied, the one
// that failed by its outcome, the rest not sent.
func (p *Provider) renumber(ctx context.Context, board tracker.BoardID, run []string, lo, hi *float64) tracker.Result {
	step := tracker.StepRank
	stored, err := p.c.RenumberIssues(ctx, linear.RenumberInput{Field: linear.OrderSort, IssueIDs: run, Before: lo, After: hi})
	var out tracker.Result
	for i, id := range run {
		issue := tracker.IssueID(id)
		changed := tracker.RowOf(board, issue)
		switch {
		case i < len(stored):
			out.Parts = append(out.Parts, part(issue, step, nil, changed))
		case i == len(stored) && err != nil:
			out.Parts = append(out.Parts, part(issue, step, err, changed))
		default:
			failed := tracker.IssueID(run[len(stored)])
			out.Parts = append(out.Parts, notSentAfter(step, failed, []tracker.IssueID{issue})...)
		}
	}
	return out
}

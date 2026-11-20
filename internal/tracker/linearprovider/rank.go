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
// team's manual order by ID and order alone, rankPage issues a page, at
// most rankPages pages (2,500 issues, about 3,000 points by Linear's
// documented rule).
const (
	rankPage  = 250
	rankPages = 10
)

// Rank places issues, in order, between two neighbors in the team's
// manual order (sortOrder, which every board column and the backlog
// share) with one update per issue. It reads the issues and the
// neighbors within the board first: a neighbor not on the board refuses
// the rank, and an issue not on the board is refused while the others
// are ranked. The values come from the client's Spread of the
// neighbors' keys. When no values fit between them (linear.ErrNoRoom),
// it reads the manual order again from the start, since Linear may have
// spread the values itself, and renumbers the run from just after Prev
// up to the first issue with room above Prev, through the client's
// RenumberIssues: the issues ranked first, then those it moved to make
// room, each its own part. It stops at the first update not applied; the
// rest are not sent. The refused parts come first, as they were judged
// before anything was sent.
func (p *Provider) Rank(ctx context.Context, in tracker.RankInput) tracker.Result {
	step := tracker.StepRank
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issues...)
	}
	ids, bad := issueIDs(in.Issues)
	if len(bad) > 0 {
		return refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: fmt.Sprintf("%q is not a Linear issue ID", bad[0])}, in.Issues...)
	}
	before, err := neighborValue(in.Prev)
	if err != nil {
		return tracker.Invalid(step, err, in.Issues...)
	}
	after, err := neighborValue(in.Next)
	if err != nil {
		return tracker.Invalid(step, err, in.Issues...)
	}
	read := slices.Clone(ids)
	for _, n := range []*tracker.Neighbor{in.Prev, in.Next} {
		if n != nil {
			read = append(read, string(n.Issue))
		}
	}
	on, err := p.onBoard(ctx, in.Board, read)
	if err != nil {
		return beforeWrite(step, err, in.Issues...)
	}
	for _, n := range []*tracker.Neighbor{in.Prev, in.Next} {
		if n != nil && !on[string(n.Issue)] {
			return tracker.Refuse(step, notOnBoard(n.Issue, in.Board), in.Issues...)
		}
	}
	var out tracker.Result
	var moving []string
	for _, id := range in.Issues {
		if on[string(id)] {
			moving = append(moving, string(id))
		} else {
			out.Parts = append(out.Parts, tracker.Part{Issue: id, Step: step, Outcome: tracker.Refused(notOnBoard(id, in.Board))})
		}
	}
	if len(moving) == 0 {
		return out
	}
	ranked := p.rankOnBoard(ctx, in.Board, moving, in.Prev, before, after)
	out.Parts = append(out.Parts, ranked.Parts...)
	return out
}

// rankOnBoard ranks moving, every one on board, between before and
// after, the values of prev and the next neighbor.
func (p *Provider) rankOnBoard(ctx context.Context, board tracker.BoardID, moving []string, prev *tracker.Neighbor, before, after *float64) tracker.Result {
	step := tracker.StepRank
	issues := make([]tracker.IssueID, len(moving))
	for i, id := range moving {
		issues[i] = tracker.IssueID(id)
	}
	_, err := linear.Spread(before, after, len(moving))
	switch {
	case err == nil:
		return p.renumber(ctx, board, moving, before, after)
	case !errors.Is(err, linear.ErrNoRoom):
		return tracker.Invalid(step, err, issues...)
	case prev == nil:
		return refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: "no order value fits before the first issue"}, issues...)
	}
	run, lo, hi, err := p.crowdedRun(ctx, board, prev.Issue, moving)
	if err != nil {
		return editRefusal(step, err, issues...)
	}
	return p.renumber(ctx, board, run, lo, hi)
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
		page, err := p.c.ListIssuePositions(ctx, q, after)
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
// that failed by its outcome, the rest not sent. Each names the issue's
// row on board and its card, which every board sharing the team's order
// draws.
func (p *Provider) renumber(ctx context.Context, board tracker.BoardID, run []string, lo, hi *float64) tracker.Result {
	step := tracker.StepRank
	stored, err := p.c.RenumberIssues(ctx, linear.RenumberInput{Field: linear.OrderSort, IssueIDs: run, Before: lo, After: hi})
	var out tracker.Result
	for i, id := range run {
		issue := tracker.IssueID(id)
		switch {
		case i < len(stored):
			out.Parts = append(out.Parts, part(issue, step, nil, tracker.RowOf(board, issue), tracker.CardOf(issue)))
		case i == len(stored) && err != nil:
			out.Parts = append(out.Parts, part(issue, step, err, tracker.RowOf(board, issue), tracker.CardOf(issue)))
		default:
			failed := tracker.IssueID(run[len(stored)])
			out.Parts = append(out.Parts, notSentAfter(step, failed, []tracker.IssueID{issue})...)
		}
	}
	return out
}

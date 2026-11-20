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
// share) with one update per issue. It checks every ID, the neighbors'
// with the issues', before any request, then reads the issues and the
// neighbors within the board, with their current sortOrder: a neighbor
// not on the board refuses the rank, as does a Prev that no longer
// stands before Next, and an issue not on the board is refused while the
// others are ranked. The values come from the client's Spread of the
// neighbors' current values. When no values fit between them
// (linear.ErrNoRoom), it reads the manual order again from the start,
// since Linear may have spread the values itself, and renumbers the run
// from just after Prev up to the first issue with room above Prev,
// through the client's RenumberIssues: the issues ranked first, then
// those it moved to make room, each its own part. It stops at the first
// update not applied; the rest are not sent. The refused parts come
// first, as they were judged before anything was sent.
func (p *Provider) Rank(ctx context.Context, in tracker.RankInput) tracker.Result {
	step := tracker.StepRank
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issues...)
	}
	named := slices.Clone(in.Issues)
	for _, n := range []tracker.IssueID{in.Prev, in.Next} {
		if n != "" {
			named = append(named, n)
		}
	}
	read, bad := issueIDs(named)
	if len(bad) > 0 {
		return notIssueID(step, bad[0], in.Issues...)
	}
	on, err := p.onBoard(ctx, in.Board, read)
	if err != nil {
		return beforeWrite(step, err, in.Issues...)
	}
	var values [2]*float64
	for i, n := range []tracker.IssueID{in.Prev, in.Next} {
		if n == "" {
			continue
		}
		v, ok := on[string(n)]
		if !ok {
			return tracker.Refuse(step, notOnBoard(n, in.Board), in.Issues...)
		}
		values[i] = &v
	}
	before, after := values[0], values[1]
	if before != nil && after != nil && *before > *after {
		return refuse(step, tracker.Refusal{Reason: tracker.ReasonStale,
			Message: fmt.Sprintf("issue %s no longer stands before issue %s", in.Prev, in.Next)}, in.Issues...)
	}
	var out tracker.Result
	var moving []string
	for _, id := range in.Issues {
		if _, ok := on[string(id)]; ok {
			moving = append(moving, string(id))
		} else {
			out.Parts = append(out.Parts, tracker.Part{Issue: id, Step: step, Outcome: tracker.Refused(notOnBoard(id, in.Board))})
		}
	}
	if len(moving) == 0 {
		return out
	}
	ranked := p.rankOnBoard(ctx, in, moving, before, after)
	out.Parts = append(out.Parts, ranked.Parts...)
	return out
}

// rankOnBoard ranks moving, every one on in.Board, between before and
// after, the current values of in.Prev and in.Next.
func (p *Provider) rankOnBoard(ctx context.Context, in tracker.RankInput, moving []string, before, after *float64) tracker.Result {
	step := tracker.StepRank
	issues := make([]tracker.IssueID, len(moving))
	for i, id := range moving {
		issues[i] = tracker.IssueID(id)
	}
	_, err := linear.Spread(before, after, len(moving))
	switch {
	case err == nil:
		return p.renumber(ctx, in.Board, moving, before, after)
	case !errors.Is(err, linear.ErrNoRoom):
		return tracker.Invalid(step, err, issues...)
	case in.Prev == "":
		return refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: "no order value fits before the first issue"}, issues...)
	}
	run, lo, hi, err := p.crowdedRun(ctx, in.Board, in.Prev, in.Next, moving)
	if err != nil {
		return editRefusal(step, err, issues...)
	}
	return p.renumber(ctx, in.Board, run, lo, hi)
}

// crowdedRun reads the team's manual order and returns the run to
// renumber after prev: moving, then every following issue up to the
// first whose value leaves room for the run above prev's, with the
// values around the run (hi nil at the end of the list). A next, when
// named, listed before prev refuses the rank as stale: equal values
// leave the order to Linear's listing.
func (p *Provider) crowdedRun(ctx context.Context, board tracker.BoardID, prev, next tracker.IssueID, moving []string) ([]string, *float64, *float64, error) {
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
			case lo == nil && next != "" && is.ID == string(next):
				return nil, nil, nil, refusedf(tracker.ReasonStale, "issue %s no longer stands before issue %s", prev, next)
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

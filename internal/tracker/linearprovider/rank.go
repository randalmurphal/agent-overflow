package linearprovider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// Bounds of the read Rank makes when its neighbors leave no room: the
// team's issues by ID and order alone, rankPage issues a page, at most
// rankPages pages (10,000 issues, the listing's bound, about 4 points a
// page, measured for a page of IDs).
const (
	rankPage  = listingPage
	rankPages = listingPages
)

// Rank places issues, in order, between two neighbors in the team's
// manual order (sortOrder, which every board column and the backlog
// share) with aliased updates, 50 to a request. It checks every ID, the
// neighbors' with the issues', before any request, then reads the
// positions of the issues and the neighbors within the board: a
// neighbor not on the board refuses the rank, as does a Prev that no
// longer stands before Next (neighborValues), and an issue not on the
// board is refused while the others are ranked. The run it writes and
// its values are placeRun's, through the client's RenumberIssues: the
// issues ranked first, then those it moved to make room, each its own
// part by the client's outcome for it; the issues after a request with a
// part not applied are not sent. The refused parts come first, as they
// were judged before anything was sent.
func (p *Provider) Rank(ctx context.Context, in tracker.RankInput) tracker.Result {
	step := tracker.StepRank
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issues...)
	}
	read, bad := issueIDs(append(slices.Clone(in.Issues), neighborsOf(in)...))
	if len(bad) > 0 {
		return notIssueID(step, bad[0], in.Issues...)
	}
	on, err := p.onBoard(ctx, in.Board, read)
	if err != nil {
		return beforeWrite(step, err, in.Issues...)
	}
	before, after, err := neighborValues(in, on)
	if err != nil {
		return editRefusal(step, err, in.Issues...)
	}
	out, moving := onBoardOnly(step, in.Board, in.Issues, on)
	if len(moving) == 0 {
		return out
	}
	run, lo, hi, fail := p.placeRun(ctx, step, in, moving, before, after)
	if fail != nil {
		out.Parts = append(out.Parts, fail.Parts...)
		return out
	}
	out.Parts = append(out.Parts, p.renumber(ctx, in.Board, run, lo, hi, nil, 0).Parts...)
	return out
}

// neighborValues are the current values of in's neighbors, nil for an
// end, from on (onBoard's answer). A neighbor on refused as not on the
// board, and a Prev whose value is past Next's, refuse the place, as a
// *refusedEdit.
func neighborValues(in tracker.RankInput, on map[string]float64) (before, after *float64, err error) {
	var values [2]*float64
	for i, n := range []tracker.IssueID{in.Prev, in.Next} {
		if n == "" {
			continue
		}
		v, ok := on[string(n)]
		if !ok {
			return nil, nil, &refusedEdit{notOnBoard(n, in.Board)}
		}
		values[i] = &v
	}
	before, after = values[0], values[1]
	if before != nil && after != nil && *before > *after {
		return nil, nil, refusedf(tracker.ReasonStale, "issue %s no longer stands before issue %s", in.Prev, in.Next)
	}
	return before, after, nil
}

// onBoardOnly splits issues by on (onBoard's answer): a refused part of
// step for each one not on board, and the IDs of the others, in order.
func onBoardOnly(step tracker.Step, board tracker.BoardID, issues []tracker.IssueID, on map[string]float64) (tracker.Result, []string) {
	var out tracker.Result
	var moving []string
	for _, id := range issues {
		if _, ok := on[string(id)]; ok {
			moving = append(moving, string(id))
		} else {
			out.Parts = append(out.Parts, tracker.Part{Issue: id, Step: step, Outcome: tracker.Refused(notOnBoard(id, board))})
		}
	}
	return out, moving
}

// neighborsOf is the neighbors a rank names.
func neighborsOf(in tracker.RankInput) []tracker.IssueID {
	return slices.DeleteFunc([]tracker.IssueID{in.Prev, in.Next}, func(id tracker.IssueID) bool { return id == "" })
}

// placeRun is the run to write to place moving, every one on in.Board,
// between before and after, the current values of in.Prev and in.Next,
// with the values around it: moving alone when the client's Spread fits
// it there. When no values fit (linear.ErrNoRoom), it reads the manual
// order again (crowdedRun), since Linear may have spread the values
// itself, and the run goes from just after Prev up to the first issue
// with room above Prev. Nothing is written; a place it cannot make is
// fail, its parts of step for moving.
func (p *Provider) placeRun(ctx context.Context, step tracker.Step, in tracker.RankInput, moving []string, before, after *float64) (run []string, lo, hi *float64, fail *tracker.Result) {
	issues := make([]tracker.IssueID, len(moving))
	for i, id := range moving {
		issues[i] = tracker.IssueID(id)
	}
	failed := func(r tracker.Result) ([]string, *float64, *float64, *tracker.Result) { return nil, nil, nil, &r }
	_, err := linear.Spread(before, after, len(moving))
	switch {
	case err == nil:
		return moving, before, after, nil
	case !errors.Is(err, linear.ErrNoRoom):
		return failed(tracker.Invalid(step, err, issues...))
	case in.Prev == "":
		return failed(refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: "no order value fits before the first issue"}, issues...))
	}
	run, lo, hi, err = p.crowdedRun(ctx, in.Board, in.Prev, in.Next, moving)
	if err != nil {
		return failed(editRefusal(step, err, issues...))
	}
	return run, lo, hi, nil
}

// crowdedRun reads the team's manual order and returns the run to
// renumber after prev: moving, then every following issue up to the
// first whose value leaves room for the run above prev's, with the
// values around the run (hi nil at the end of the list). A next, when
// named, whose value is below prev's refuses the rank as stale.
//
// A rank changes the manual order, and a page cursor follows its issue
// (measured), so a walk in that order could skip or repeat issues
// ranked meanwhile. The order is therefore read by creation, which
// nothing changes, and sorted here by value, prev first among equal
// values and the rest by ID; an issue answered as another team's is not
// on the board, and a team longer than the bound refuses the rank.
func (p *Provider) crowdedRun(ctx context.Context, board tracker.BoardID, prev, next tracker.IssueID, moving []string) ([]string, *float64, *float64, error) {
	q := linear.IssueQuery{Filter: linear.IssueFilter{Teams: []string{string(board)}}, Order: linear.OrderCreatedOldest, Limit: rankPage}
	all, err := walk(ctx, func(ctx context.Context, after string) (linear.Page[linear.IssuePosition], error) {
		return p.c.ListIssuePositions(ctx, q, after)
	}, rankPages)
	switch {
	case errors.Is(err, errTooLong):
		return nil, nil, nil, refusedf(tracker.ReasonInvalid, "no room near issue %s, and board %s holds more than %d issues to renumber among", prev, board, rankPage*rankPages)
	case err != nil:
		return nil, nil, nil, fmt.Errorf("linearprovider: read the manual order of %s: %w", board, err)
	}
	all = slices.DeleteFunc(all, func(is linear.IssuePosition) bool { return is.TeamID != string(board) })
	at := slices.IndexFunc(all, func(is linear.IssuePosition) bool { return is.ID == string(prev) })
	if at < 0 {
		return nil, nil, nil, refusedf(tracker.ReasonStale, "issue %s is no longer on the board", prev)
	}
	lo := all[at].SortOrder
	notPrev := func(is linear.IssuePosition) int {
		if is.ID == string(prev) {
			return 0
		}
		return 1
	}
	slices.SortFunc(all, func(a, b linear.IssuePosition) int {
		return cmp.Or(cmp.Compare(a.SortOrder, b.SortOrder), cmp.Compare(notPrev(a), notPrev(b)), cmp.Compare(a.ID, b.ID))
	})
	run := slices.Clone(moving)
	seenPrev := false
	for _, is := range all {
		switch {
		case is.ID == string(prev):
			seenPrev = true
		case !seenPrev:
			if is.ID == string(next) {
				return nil, nil, nil, refusedf(tracker.ReasonStale, "issue %s no longer stands before issue %s", prev, next)
			}
		case slices.Contains(moving, is.ID):
		default:
			hi := is.SortOrder
			if _, err := linear.Spread(&lo, &hi, len(run)); err == nil {
				return run, &lo, &hi, nil
			}
			run = append(run, is.ID)
		}
	}
	return run, &lo, nil, nil
}

// renumber gives the run Spread values between lo and hi through the
// client, one part per issue in order, each by the client's outcome for
// it. Each names the issue's row on board and its card, which every
// board sharing the team's order draws. With into, the run's first moved
// issues also move into that cycle ("" for none) in the same update, and
// each of those is a sprint move part that names its detail too.
func (p *Provider) renumber(ctx context.Context, board tracker.BoardID, run []string, lo, hi *float64, into *string, moved int) tracker.Result {
	issues := make([]tracker.IssueID, len(run))
	for i, id := range run {
		issues[i] = tracker.IssueID(id)
	}
	stepOf := func(i int) tracker.Step {
		if i < moved {
			return tracker.StepSprintMove
		}
		return tracker.StepRank
	}
	written, err := p.c.RenumberIssues(ctx, linear.RenumberInput{Field: linear.OrderSort, IssueIDs: run, Before: lo, After: hi, Into: into, Moved: moved})
	var out tracker.Result
	if err != nil {
		for i, issue := range issues {
			out.Parts = append(out.Parts, tracker.Invalid(stepOf(i), err, issue).Parts...)
		}
		return out
	}
	for i, w := range written {
		issue := tracker.IssueID(w.IssueID)
		changed := []tracker.Resource{tracker.RowOf(board, issue), tracker.CardOf(issue)}
		if i < moved {
			changed = append(changed, tracker.DetailOf(issue))
		}
		out.Parts = append(out.Parts, part(issue, stepOf(i), w.Err, changed...))
	}
	return out
}

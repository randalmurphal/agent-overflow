package linear

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// A walk in update order never pages by Linear's cursor. The cursor
// follows its issue: it means after where that issue is now (measured),
// so a walk in an order an update changes skips every issue between a
// page's last issue and its new place when it is updated between pages.
// Such a walk pages by Position instead: a bound, the update time of the
// last issue delivered, and how far the issues updated at exactly it
// (the tie) were delivered. A tie that fits a cohort is left out by ID:
// each page asks again for the issues at or past the bound, less the
// cohort's still updated exactly at it (an or of updatedAt past the
// bound and id nin). Linear accepts that filter and sorts by it
// correctly under an oldest-update-first sort and, with lte and lt,
// under a newest-first one (both measured). A larger tie is drained: the
// issues updated at exactly the bound (updatedAt eq, measured to select
// them exactly), less the cohort, in creation order by Linear's cursor,
// which an issue leaving the filter or being trashed between pages does
// not disturb (measured); then the walk goes on strictly past the bound.
// An issue updated during a walk moves in the order and is delivered
// again where it now stands, or not at all when it moved behind the
// walk; no issue the update did not touch is skipped or repeated, and no
// tie is too large for a walk to pass.

// maxCohort bounds a cohort, the issues of the tie that every later
// request names until the bound moves, an idle poll's included: about 39
// bytes each, so at most about 4 KB a request. A larger tie costs one
// drain request a page of it. Linear stores update times to the
// millisecond and gave the issues of one batch update distinct times
// (measured), so a tie past the cohort is rare.
const maxCohort = 100

// Position is a walk's place in update order, one no update can move.
// It selects every issue past Bound and, of those updated at exactly
// Bound: none when Past is set; with Drain set, those after *Drain in
// creation order less At (a drain in progress, *Drain empty before its
// first page); otherwise those not in At. The zero value is the start of
// the order. A caller stores the Position a page returns and passes it
// back unchanged, in the same process or a later one.
type Position struct {
	// Bound is the update time of the last issue delivered.
	Bound time.Time
	// At are IDs delivered updated at exactly Bound, at most maxCohort.
	At []string
	// Drain is Linear's cursor in the drain of the tie at Bound.
	Drain *string
	// Past reports that every issue updated at Bound was delivered.
	Past bool
}

// check refuses a Position no walk returns.
func (p Position) check() error {
	switch {
	case p.Bound.IsZero() && (len(p.At) > 0 || p.Drain != nil || p.Past):
		return errors.New("a position at the start of the order holds nothing else")
	case p.Past && (len(p.At) > 0 || p.Drain != nil):
		return errors.New("a position past its bound holds no tie")
	case len(p.At) > maxCohort:
		return fmt.Errorf("a position leaves out at most %d issues by ID, not %d", maxCohort, len(p.At))
	}
	return checkIDs("the position's cohort", p.At)
}

// walkOrder is a walk's direction: newest update first, or oldest.
type walkOrder bool

const (
	oldestFirst walkOrder = false
	newestFirst walkOrder = true
)

// filter is f narrowed to the issues p has not delivered, in order o.
func (p Position) filter(o walkOrder, f *issueFilterInput) *issueFilterInput {
	if p.Bound.IsZero() {
		return f
	}
	out := &issueFilterInput{}
	if f != nil {
		out.And = slices.Clone(f.And)
	}
	b := formatTime(p.Bound)
	atOrPast, past := &dateComparator{Gte: b}, &dateComparator{Gt: b}
	if o == newestFirst {
		atOrPast, past = &dateComparator{Lte: b}, &dateComparator{Lt: b}
	}
	nin := issueFilterInput{ID: &idComparator{Nin: p.At}}
	switch {
	case p.Past:
		out.And = append(out.And, issueFilterInput{UpdatedAt: past})
	case p.Drain != nil:
		out.And = append(out.And, issueFilterInput{UpdatedAt: &dateComparator{Eq: b}})
		if len(p.At) > 0 {
			out.And = append(out.And, nin)
		}
	default:
		out.And = append(out.And, issueFilterInput{UpdatedAt: atOrPast})
		if len(p.At) > 0 {
			// An ID alone would also leave out a cohort issue updated again
			// during the walk, until a newer issue moved the bound.
			out.And = append(out.And, issueFilterInput{Or: []issueFilterInput{{UpdatedAt: past}, nin}})
		}
	}
	return out
}

// readPosition is the page of the issues q selects past p in order o,
// and the Position after it. The page holds what it delivers: when a tie
// outgrows the cohort it ends before the tie's next issue, which the
// drain then delivers, and HasNextPage reports whether the walk goes on,
// true after a drain ends. A read that delivers nothing while the walk
// goes on (the tie's first issue past a full cohort, or a drain whose
// issues all moved) is followed by the next, at most three in all, so a
// page that says more follow holds an issue unless every issue it would
// have held was updated away twice over. A page that breaks the order,
// delivers an issue p leaves out, or holds none while saying more follow
// fails.
func readPosition(ctx context.Context, c *Client, o walkOrder, p Position, q IssueQuery) (Page[Issue], Position, error) {
	var pg Page[Issue]
	var err error
	for range 3 {
		if pg, p, err = readOnce(ctx, c, o, p, q); err != nil || len(pg.Items) > 0 || !pg.HasNextPage {
			break
		}
	}
	return pg, p, err
}

// readOnce is one request of readPosition.
func readOnce(ctx context.Context, c *Client, o walkOrder, p Position, q IssueQuery) (Page[Issue], Position, error) {
	if err := p.check(); err != nil {
		return Page[Issue]{}, Position{}, invalid(opIssues.name, "%w", err)
	}
	// Linear's times are milliseconds and formatTime sends no finer, so
	// the bound the page answers is the bound truncated.
	p.Bound = p.Bound.Truncate(time.Millisecond)
	after := ""
	switch {
	case p.Drain != nil:
		q.Order, after = OrderCreatedOldest, *p.Drain
	case o == newestFirst:
		q.Order = orderUpdatedNewest
	default:
		q.Order = orderUpdatedOldest
	}
	vars, err := issueVars(q, after)
	if err != nil {
		return Page[Issue]{}, Position{}, invalid(opIssues.name, "%w", err)
	}
	vars.Filter = p.filter(o, vars.Filter)
	pg, err := fetchPage(ctx, c, opIssues, vars, wireIssue.typed)
	if err != nil {
		return Page[Issue]{}, Position{}, err
	}
	if pg.HasNextPage && len(pg.Items) == 0 {
		// The position would not move, so the next page would ask again.
		return Page[Issue]{}, Position{}, fmt.Errorf("linear %s: a page with no issues says more follow", opIssues.name)
	}
	var next Position
	if p.Drain != nil {
		next, err = p.drained(pg)
	} else {
		var n int
		next, n, err = p.advance(o, pg.Items)
		pg.Items = pg.Items[:n]
	}
	if err != nil {
		return Page[Issue]{}, Position{}, err
	}
	// A drain that starts, goes on or ends leaves the walk to go on; in
	// the update order, whether more follow is the page's to say.
	pg.EndCursor = ""
	if p.Drain != nil || next.Drain != nil {
		pg.HasNextPage = true
	}
	return pg, next, nil
}

// drained is the Position after pg, a page of p's drain.
func (p Position) drained(pg Page[Issue]) (Position, error) {
	for _, is := range pg.Items {
		switch {
		case !is.UpdatedAt.Equal(p.Bound):
			return Position{}, fmt.Errorf("linear %s: issue %s, updated %s, is outside the tie at %s it drains",
				opIssues.name, is.ID, formatTime(is.UpdatedAt), formatTime(p.Bound))
		case slices.Contains(p.At, is.ID):
			return Position{}, fmt.Errorf("linear %s: issue %s was left out by ID and came back", opIssues.name, is.ID)
		}
	}
	if !pg.HasNextPage {
		return Position{Bound: p.Bound, Past: true}, nil
	}
	cursor := pg.EndCursor
	return Position{Bound: p.Bound, At: p.At, Drain: &cursor}, nil
}

// advance is the Position after items, a page in order o read from p,
// and how many of items it delivers: all, unless the tie at the bound
// outgrows the cohort, when it stops before the issue that would not fit
// and starts the drain. p and its cohort are left as they are.
func (p Position) advance(o walkOrder, items []Issue) (Position, int, error) {
	next := Position{Bound: p.Bound, Past: p.Past}
	at := make([]string, len(p.At), max(len(p.At), min(len(p.At)+len(items), maxCohort)))
	copy(at, p.At)
	seen := make(map[string]struct{}, cap(at))
	for _, id := range at {
		seen[id] = struct{}{}
	}
	for i, is := range items {
		order := is.UpdatedAt.Compare(next.Bound)
		if o == newestFirst {
			order = -order
		}
		if next.Bound.IsZero() {
			order = 1
		}
		switch {
		case order < 0 || order == 0 && next.Past:
			return Position{}, 0, fmt.Errorf("linear %s: issue %s, updated %s, comes after an issue updated %s",
				opIssues.name, is.ID, formatTime(is.UpdatedAt), formatTime(next.Bound))
		case order > 0:
			next.Bound, next.Past, at = is.UpdatedAt, false, at[:0]
			clear(seen)
		default:
			if _, dup := seen[is.ID]; dup {
				return Position{}, 0, fmt.Errorf("linear %s: issue %s was left out by ID and came back", opIssues.name, is.ID)
			}
			if len(at) == maxCohort {
				start := ""
				next.At, next.Drain = at, &start
				return next, i, nil
			}
		}
		at = append(at, is.ID)
		seen[is.ID] = struct{}{}
	}
	if len(at) > 0 {
		next.At = at
	}
	return next, len(items), nil
}

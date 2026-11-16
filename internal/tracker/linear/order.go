package linear

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// OrderField is one of an issue's manual orders, each lowest first.
type OrderField int

const (
	// OrderSort is sortOrder, the order of every manually sorted list:
	// board columns and the backlog alike.
	OrderSort OrderField = iota
	// OrderPrioritySort is prioritySortOrder, the order within one
	// priority when a list is ordered by priority.
	OrderPrioritySort
	// OrderSubIssueSort is subIssueSortOrder, the order of a parent's
	// sub-issues.
	OrderSubIssueSort
)

// ErrNoRoom is an order with no value strictly between two neighbors:
// they are equal, adjacent floating-point values, or out of order. The
// caller reads the list again, since Linear may have spread the values
// itself, and when there is still no room gives the run of crowded
// issues new values with RenumberIssues.
var ErrNoRoom = errors.New("linear: no order value fits between the neighbors")

// endStep is the distance from the end neighbor a value placed at either
// end of a list takes.
const endStep = 1

// Between returns an order value strictly between the neighbors' values,
// before and after; nil is that end of the list, and both nil is an
// empty list. It fails with ErrNoRoom when no such value exists.
func Between(before, after *float64) (float64, error) {
	if err := checkFinite(before, after); err != nil {
		return 0, err
	}
	switch {
	case before == nil && after == nil:
		return 0, nil
	case before == nil:
		return beyond(*after, -1)
	case after == nil:
		return beyond(*before, 1)
	}
	lo, hi := *before, *after
	mid := interpolate(lo, hi, 0.5)
	if !(lo < mid && mid < hi) {
		return 0, ErrNoRoom
	}
	return mid, nil
}

// beyond is the end-of-list rule both Between and Spread use: a value
// past v in direction dir (-1 or 1), endStep away, or the nearest
// representable value where v is too large for endStep to show. It fails
// with ErrNoRoom past the largest finite value.
func beyond(v float64, dir float64) (float64, error) {
	out := v + dir*endStep
	if out == v {
		out = math.Nextafter(v, math.Inf(int(dir)))
	}
	if math.IsInf(out, 0) {
		return 0, ErrNoRoom
	}
	return out, nil
}

// interpolate is the value a fraction f of the way from lo to hi, without
// overflow when the span exceeds the largest finite value.
func interpolate(lo, hi, f float64) float64 {
	if span := hi - lo; !math.IsInf(span, 0) {
		return lo + span*f
	}
	return lo*(1-f) + hi*f
}

// Spread returns n increasing order values strictly between before and
// after (nil is that end of the list), evenly spaced between two
// neighbors and spaced by the end-of-list rule at an end, for renumbering
// a run of n issues. It fails with ErrNoRoom when the span cannot hold n
// distinct values.
func Spread(before, after *float64, n int) ([]float64, error) {
	if n < 0 {
		return nil, errors.New("linear: Spread of a negative count")
	}
	if err := checkFinite(before, after); err != nil {
		return nil, err
	}
	out := make([]float64, n)
	var err error
	switch {
	case before == nil && after == nil:
		for i := range out {
			if i > 0 {
				out[i], err = beyond(out[i-1], 1)
			}
		}
	case before == nil:
		v := *after
		for i := n - 1; i >= 0 && err == nil; i-- {
			v, err = beyond(v, -1)
			out[i] = v
		}
	case after == nil:
		v := *before
		for i := 0; i < n && err == nil; i++ {
			v, err = beyond(v, 1)
			out[i] = v
		}
	default:
		for i := range out {
			out[i] = interpolate(*before, *after, float64(i+1)/float64(n+1))
		}
	}
	if err != nil {
		return nil, err
	}
	prev, last := math.Inf(-1), math.Inf(1)
	if before != nil {
		prev = *before
	}
	if after != nil {
		last = *after
	}
	for _, v := range out {
		if !(prev < v) || math.IsInf(v, 0) {
			return nil, ErrNoRoom
		}
		prev = v
	}
	if n > 0 && !(prev < last) {
		return nil, ErrNoRoom
	}
	return out, nil
}

func checkFinite(values ...*float64) error {
	for _, v := range values {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return errors.New("linear: a neighbor's order value is not a finite number")
		}
	}
	return nil
}

func (f OrderField) set(w *issueInput, v float64) error {
	switch f {
	case OrderSort:
		w.SortOrder = Set(v)
	case OrderPrioritySort:
		w.PrioritySortOrder = Set(v)
	case OrderSubIssueSort:
		w.SubIssueSortOrder = Set(v)
	default:
		return fmt.Errorf("unknown OrderField %d", f)
	}
	return nil
}

// MoveInput places an issue between two neighbors of one list by Field.
// Before and After are the neighbors' current values of that field; nil
// is that end of the list.
type MoveInput struct {
	IssueID string
	Field   OrderField
	Before  *float64
	After   *float64
}

// MoveIssue sets the issue's Field to a value Between its neighbors with
// one issue update, and returns the issue as stored. ErrNoRoom is
// returned before anything is sent.
func (c *Client) MoveIssue(ctx context.Context, in MoveInput) (*Issue, error) {
	if in.IssueID == "" {
		return nil, invalid("MoveIssue", "an issue ID is required")
	}
	v, err := Between(in.Before, in.After)
	if err != nil {
		return nil, invalid("MoveIssue", "%w", err)
	}
	var w issueInput
	if err := in.Field.set(&w, v); err != nil {
		return nil, invalid("MoveIssue", "%w", err)
	}
	return c.updateIssue(ctx, in.IssueID, w)
}

// RenumberInput gives a run of issues new values of Field, in the order
// listed, between the neighbors of the run: Before and After are the
// values of the issues just outside it (nil is that end of the list).
type RenumberInput struct {
	Field    OrderField
	IssueIDs []string
	Before   *float64
	After    *float64
}

// RenumberIssues writes Spread values to the run, one issue update each,
// in order. An ID listed twice is refused before any request; the same
// issue named once by ID and once by identifier cannot be told apart.
// It returns the issues as stored; on failure they are the ones updated
// before it, and the failed update's own outcome is the error's
// (WriteNotApplied). Linear has no update that sets distinct values on
// several issues at once.
func (c *Client) RenumberIssues(ctx context.Context, in RenumberInput) ([]Issue, error) {
	seen := make(map[string]bool, len(in.IssueIDs))
	for _, id := range in.IssueIDs {
		switch {
		case id == "":
			return nil, invalid("RenumberIssues", "IssueIDs holds an empty ID")
		case seen[id]:
			return nil, invalid("RenumberIssues", "IssueIDs names %s twice", id)
		}
		seen[id] = true
	}
	if err := in.Field.set(&issueInput{}, 0); err != nil {
		return nil, invalid("RenumberIssues", "%w", err)
	}
	values, err := Spread(in.Before, in.After, len(in.IssueIDs))
	if err != nil {
		return nil, invalid("RenumberIssues", "%w", err)
	}
	out := make([]Issue, 0, len(in.IssueIDs))
	for i, id := range in.IssueIDs {
		var w issueInput
		_ = in.Field.set(&w, values[i]) // checked above
		issue, err := c.updateIssue(ctx, id, w)
		if err != nil {
			return out, fmt.Errorf("linear RenumberIssues: %d of %d issues renumbered: %w", i, len(in.IssueIDs), err)
		}
		out = append(out, *issue)
	}
	return out, nil
}

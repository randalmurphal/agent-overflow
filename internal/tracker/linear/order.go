package linear

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
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
// empty list. It is Spread of one value, and fails with ErrNoRoom when
// no representable value lies strictly between the neighbors.
func Between(before, after *float64) (float64, error) {
	values, err := Spread(before, after, 1)
	if err != nil {
		return 0, err
	}
	return values[0], nil
}

// beyond is the end-of-list rule: a value past v in direction dir (-1 or
// 1), endStep away, or the nearest representable value where v is too
// large for endStep to show. It fails with ErrNoRoom past the largest
// finite value.
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
// after (nil is that end of the list), for renumbering a run of n
// issues. Between two neighbors they are evenly spaced, or stepped
// through the representable values when even spacing would make two of
// them equal; at an end they follow the end-of-list rule. It fails with
// ErrNoRoom only when fewer than n representable values lie strictly
// between the neighbors (or past the end one).
func Spread(before, after *float64, n int) ([]float64, error) {
	if n < 0 {
		return nil, errors.New("linear: Spread of a negative count")
	}
	if err := checkFinite(before, after); err != nil {
		return nil, err
	}
	out := make([]float64, n)
	if n == 0 {
		return out, nil
	}
	var err error
	switch {
	case before != nil && after != nil:
		return spreadBetween(*before, *after, n)
	case before == nil && after == nil:
		for i := 1; i < n && err == nil; i++ {
			out[i], err = beyond(out[i-1], 1)
		}
	case before == nil:
		v := *after
		for i := n - 1; i >= 0 && err == nil; i-- {
			v, err = beyond(v, -1)
			out[i] = v
		}
	default:
		v := *before
		for i := 0; i < n && err == nil; i++ {
			v, err = beyond(v, 1)
			out[i] = v
		}
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// spreadBetween is Spread between two finite neighbors. Room is counted
// in representable values: ordinal numbers them in order, so the values
// strictly between lo and hi are the ordinals strictly between theirs.
func spreadBetween(lo, hi float64, n int) ([]float64, error) {
	kl, kh := ordinal(lo), ordinal(hi)
	if kh <= kl {
		return nil, ErrNoRoom
	}
	// The distance fits in a uint64 even when the int64 difference
	// overflows; the wrapped subtraction gives it exactly.
	d := uint64(kh) - uint64(kl)
	if d-1 < uint64(n) {
		return nil, ErrNoRoom
	}
	out := make([]float64, n)
	prev := lo
	for i := range out {
		v := interpolate(lo, hi, float64(i+1)/float64(n+1))
		// Rounding can make v equal prev. It reaches hi only for n
		// near 2^52, beyond any slice; the check keeps the result
		// strictly inside without relying on that bound.
		if !(prev < v && v < hi) {
			return stepBetween(kl, d, n), nil
		}
		out[i], prev = v, v
	}
	return out, nil
}

// stepBetween is n values whose ordinals divide the d steps from kl
// evenly. With at least n values strictly inside (d > n), consecutive
// positions differ by at least one and stay strictly inside.
func stepBetween(kl int64, d uint64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		// (i+1)*d/(n+1) without overflow; the quotient is below d.
		hi, lo := bits.Mul64(uint64(i+1), d)
		q, _ := bits.Div64(hi, lo, uint64(n+1))
		out[i] = fromOrdinal(int64(uint64(kl) + q))
	}
	return out
}

// ordinal numbers the finite float64 values in increasing order, with
// -0 and +0 one value: consecutive representable values have consecutive
// ordinals.
func ordinal(f float64) int64 {
	b := math.Float64bits(f)
	if b>>63 == 0 {
		return int64(b)
	}
	return -int64(b &^ (1 << 63))
}

// fromOrdinal is the value with ordinal k; 0 is +0.
func fromOrdinal(k int64) float64 {
	if k >= 0 {
		return math.Float64frombits(uint64(k))
	}
	return math.Float64frombits(uint64(-k) | 1<<63)
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

// of is the issue's value of f; f is one set accepts.
func (f OrderField) of(is Issue) float64 {
	switch f {
	case OrderPrioritySort:
		return is.PrioritySortOrder
	case OrderSubIssueSort:
		if is.SubIssueSortOrder == nil {
			return math.NaN()
		}
		return *is.SubIssueSortOrder
	case OrderSort:
	}
	return is.SortOrder
}

// IsIssueID reports a UUID in Linear's spelling of its IDs: 36
// characters, lowercase hexadecimal digits in groups of 8-4-4-4-12. Each
// issue has one such ID; its identifier (WEB-1) is not one.
func IsIssueID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		switch ch := s[i]; i {
		case 8, 13, 18, 23:
			if ch != '-' {
				return false
			}
		default:
			if !isLowerHex(ch) {
				return false
			}
		}
	}
	return true
}

func isLowerHex(ch byte) bool { return '0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f' }

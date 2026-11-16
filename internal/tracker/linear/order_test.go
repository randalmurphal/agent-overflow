package linear

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func TestBetween(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		before, after *float64
		want          float64
		err           error
	}{
		{"empty list", nil, nil, 0, nil},
		{"top", nil, f64(10), 9, nil},
		{"bottom", f64(10), nil, 11, nil},
		{"middle", f64(1), f64(2), 1.5, nil},
		{"negative", f64(-3), f64(-1), -2, nil},
		{"top of a huge value", nil, f64(1e300), math.Nextafter(1e300, math.Inf(-1)), nil},
		{"bottom of a huge value", f64(1e300), nil, math.Nextafter(1e300, math.Inf(1)), nil},
		{"top at the float limit", nil, f64(-math.MaxFloat64), 0, ErrNoRoom},
		{"bottom at the float limit", f64(math.MaxFloat64), nil, 0, ErrNoRoom},
		{"equal neighbors", f64(5), f64(5), 0, ErrNoRoom},
		{"adjacent floats", f64(1), f64(math.Nextafter(1, 2)), 0, ErrNoRoom},
		{"out of order", f64(2), f64(1), 0, ErrNoRoom},
	} {
		got, err := Between(tc.before, tc.after)
		if !errors.Is(err, tc.err) || (tc.err == nil && got != tc.want) {
			t.Errorf("%s: %v %v, want %v %v", tc.name, got, err, tc.want, tc.err)
		}
		if err == nil && ((tc.before != nil && !(*tc.before < got)) || (tc.after != nil && !(got < *tc.after))) {
			t.Errorf("%s: %v is not strictly between", tc.name, got)
		}
	}
	if _, err := Between(f64(math.NaN()), nil); err == nil || errors.Is(err, ErrNoRoom) {
		t.Errorf("NaN neighbor: %v", err)
	}
	if _, err := Between(nil, f64(math.Inf(1))); err == nil {
		t.Error("infinite neighbor accepted")
	}
}

// Repeated insertion at the same place exhausts the space between two
// values; ErrNoRoom then tells the caller to renumber.
func TestBetweenExhaustion(t *testing.T) {
	t.Parallel()
	lo, hi := 0.0, 1.0
	for i := 0; ; i++ {
		v, err := Between(&lo, &hi)
		if errors.Is(err, ErrNoRoom) {
			if i < 50 {
				t.Fatalf("no room after only %d insertions", i)
			}
			return
		}
		if err != nil || i > 2000 {
			t.Fatalf("insertion %d: %v", i, err)
		}
		hi = v
	}
}

func TestSpread(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		before, after *float64
		n             int
		want          []float64
		err           error
	}{
		{"empty list", nil, nil, 3, []float64{0, 1, 2}, nil},
		{"top", nil, f64(10), 2, []float64{8, 9}, nil},
		{"bottom", f64(10), nil, 2, []float64{11, 12}, nil},
		{"between", f64(0), f64(4), 3, []float64{1, 2, 3}, nil},
		{"none", f64(0), f64(4), 0, []float64{}, nil},
		{"crowded", f64(1), f64(math.Nextafter(1, 2)), 2, nil, ErrNoRoom},
		{"collision inside the span", f64(1), f64(1 + 3*(math.Nextafter(1, 2)-1)), 4, nil, ErrNoRoom},
		{"out of order", f64(4), f64(0), 1, nil, ErrNoRoom},
	} {
		got, err := Spread(tc.before, tc.after, tc.n)
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: error %v, want %v", tc.name, err, tc.err)
			continue
		}
		if tc.err == nil && !equalFloats(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := Spread(nil, nil, -1); err == nil {
		t.Error("negative count accepted")
	}
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMoveIssue(t *testing.T) {
	t.Parallel()
	issue := issueResult(issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1.5))
	f := newFake(t,
		exchange{op: "UpdateIssue", vars: `{"id":"WEB-1","input":{"sortOrder":1.5}}`, body: issue},
		exchange{op: "UpdateIssue", vars: `{"id":"WEB-1","input":{"prioritySortOrder":-11.5}}`, body: issue},
		exchange{op: "UpdateIssue", vars: `{"id":"WEB-1","input":{"subIssueSortOrder":0}}`, body: issue},
	)
	c := f.client(t)
	ctx := context.Background()
	got, err := c.MoveIssue(ctx, MoveInput{IssueID: "WEB-1", Field: OrderSort, Before: f64(1), After: f64(2)})
	if err != nil || got.SortOrder != 1.5 {
		t.Fatalf("move: %+v %v", got, err)
	}
	if _, err := c.MoveIssue(ctx, MoveInput{IssueID: "WEB-1", Field: OrderPrioritySort, After: f64(-10.5)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MoveIssue(ctx, MoveInput{IssueID: "WEB-1", Field: OrderSubIssueSort}); err != nil {
		t.Fatal(err)
	}
	_, err = c.MoveIssue(ctx, MoveInput{IssueID: "WEB-1", Before: f64(3), After: f64(3)})
	if !errors.Is(err, ErrNoRoom) || !WriteNotApplied(err) {
		t.Errorf("equal neighbors: %v", err)
	}
	if _, err := c.MoveIssue(ctx, MoveInput{IssueID: "WEB-1", Field: OrderField(9)}); err == nil {
		t.Error("unknown field accepted")
	}
}

// A renumbering that fails part way returns the issues it stored and
// says how far it got.
func TestRenumberIssuesPartialFailure(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		exchange{op: "UpdateIssue", vars: `{"id":"WEB-1","input":{"sortOrder":1}}`, body: issueResult(issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1))},
		exchange{op: "UpdateIssue", vars: `{"id":"WEB-2","input":{"sortOrder":2}}`, status: 500, body: `{"errors":[{"message":"boom"}]}`},
	)
	got, err := f.client(t).RenumberIssues(context.Background(), RenumberInput{
		Field: OrderSort, IssueIDs: []string{"WEB-1", "WEB-2", "WEB-3"}, Before: f64(0), After: f64(4),
	})
	if err == nil || len(got) != 1 || got[0].Identifier != "WEB-1" {
		t.Fatalf("renumber: %+v %v", got, err)
	}
	if !strings.Contains(err.Error(), "1 of 3 issues renumbered") || WriteNotApplied(err) {
		t.Errorf("error %v", err)
	}
}

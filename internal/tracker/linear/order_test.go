package linear

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
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
		{"span past the float limit", f64(-math.MaxFloat64), f64(math.MaxFloat64), 0, nil},
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
		{"span past the float limit", f64(-math.MaxFloat64), f64(math.MaxFloat64), 1, []float64{0}, nil},
		// The end rule is Between's: endStep, or the next value when
		// endStep does not show.
		{"bottom of a huge value", f64(1e300), nil, 1, []float64{math.Nextafter(1e300, math.Inf(1))}, nil},
		{"top of a huge value", nil, f64(-1e300), 1, []float64{math.Nextafter(-1e300, math.Inf(-1))}, nil},
		{"bottom at the float limit", f64(math.MaxFloat64), nil, 1, nil, ErrNoRoom},
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
	// A span wider than the largest finite value is still spaced evenly
	// by value, not by representable value.
	if got, err := Spread(f64(-math.MaxFloat64), f64(math.MaxFloat64), 3); err != nil || len(got) != 3 ||
		!(got[0] < -1e307 && got[1] == 0 && got[2] > 1e307) {
		t.Errorf("wide span: %v %v", got, err)
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, pair := range [][2]*float64{{f64(bad), nil}, {nil, f64(bad)}, {f64(0), f64(bad)}} {
			if _, err := Spread(pair[0], pair[1], 1); err == nil || errors.Is(err, ErrNoRoom) {
				t.Errorf("neighbor %v: %v, want a refusal of the value", bad, err)
			}
		}
	}
}

// Room is the count of representable values strictly between the
// neighbors: a span holding exactly n values gives all n, even where
// even spacing rounds two of them together, and one more is ErrNoRoom.
// Between is the n = 1 case.
func TestSpreadCountsRepresentableValues(t *testing.T) {
	t.Parallel()
	up := func(v float64, k int) float64 {
		for range k {
			v = math.Nextafter(v, math.Inf(1))
		}
		return v
	}
	tiny := math.SmallestNonzeroFloat64
	for _, tc := range []struct {
		name   string
		lo, hi float64
		inside []float64 // every representable value strictly between
	}{
		// Even spacing rounds the last two values both to 1.
		{"across a binade", 0x1.ffffffffffffdp-1, 0x1.0000000000001p+0, []float64{0x1.ffffffffffffep-1, 0x1.fffffffffffffp-1, 1}},
		// Even spacing rounds the first two values both to 1.
		{"collision below the middle", 0x1.ffffffffffffep-1, 0x1.0000000000002p+0, []float64{0x1.fffffffffffffp-1, 1, 0x1.0000000000001p+0}},
		{"within a binade", 1, up(1, 4), []float64{up(1, 1), up(1, 2), up(1, 3)}},
		{"around zero", -2 * tiny, 2 * tiny, []float64{-tiny, 0, tiny}},
		{"subnormals", 0, 4 * tiny, []float64{tiny, 2 * tiny, 3 * tiny}},
		{"subnormal to normal", math.Nextafter(0x1p-1022, 0), up(0x1p-1022, 1), []float64{0x1p-1022}},
		{"negative across a binade", -0x1.0000000000001p+0, -0x1.ffffffffffffdp-1, []float64{-1, -0x1.fffffffffffffp-1, -0x1.ffffffffffffep-1}},
	} {
		lo, hi := tc.lo, tc.hi
		n := len(tc.inside)
		got, err := Spread(&lo, &hi, n)
		if err != nil || !equalFloats(got, tc.inside) {
			t.Errorf("%s: Spread of %d: %v %v, want %v", tc.name, n, got, err, tc.inside)
		}
		if _, err := Spread(&lo, &hi, n+1); !errors.Is(err, ErrNoRoom) {
			t.Errorf("%s: Spread of %d: %v, want ErrNoRoom", tc.name, n+1, err)
		}
		for k := 1; k < n; k++ {
			got, err := Spread(&lo, &hi, k)
			if err != nil || !increasingWithin(got, lo, hi) || len(got) != k {
				t.Errorf("%s: Spread of %d: %v %v", tc.name, k, got, err)
			}
		}
		if v, err := Between(&lo, &hi); err != nil || !(lo < v && v < hi) {
			t.Errorf("%s: Between: %v %v", tc.name, v, err)
		}
		// With no value inside, Between has no room.
		edge := tc.inside[0]
		if _, err := Between(&lo, &edge); !errors.Is(err, ErrNoRoom) {
			t.Errorf("%s: Between adjacent values: %v", tc.name, err)
		}
	}
}

func increasingWithin(values []float64, lo, hi float64) bool {
	prev := lo
	for _, v := range values {
		if !(prev < v) {
			return false
		}
		prev = v
	}
	return prev < hi
}

// Invented issue IDs in Linear's spelling.
const (
	uuid1 = "6f1c2a3b-0000-4000-8000-000000000001"
	uuid2 = "6f1c2a3b-0000-4000-8000-000000000002"
	uuid3 = "6f1c2a3b-0000-4000-8000-000000000003"
)

// RenumberIssues takes issue IDs only, so one issue has one spelling: an
// identifier, any other spelling of a UUID, or an ID listed twice is
// refused before any request.
func TestRenumberIssuesRefusesAliases(t *testing.T) {
	t.Parallel()
	c := newFake(t).client(t)
	for name, tc := range map[string]struct {
		ids  []string
		want string
	}{
		"identifier":           {[]string{uuid1, "WEB-1"}, `"WEB-1" is not an issue ID`},
		"uppercase":            {[]string{strings.ToUpper(uuid1)}, "not an issue ID"},
		"braces":               {[]string{"{" + uuid1 + "}"}, "not an issue ID"},
		"no hyphens":           {[]string{strings.ReplaceAll(uuid1, "-", "")}, "not an issue ID"},
		"hyphen misplaced":     {[]string{"6f1c2a3b0-000-4000-8000-000000000001"}, "not an issue ID"},
		"hex where hyphens go": {[]string{strings.ReplaceAll(uuid1, "-", "0")}, "not an issue ID"},
		"too long":             {[]string{uuid1 + "0"}, "not an issue ID"},
		"empty":                {[]string{""}, "not an issue ID"},
		"listed twice":         {[]string{uuid1, uuid2, uuid1}, uuid1 + " is named twice"},
		"non-hex lowercase g":  {[]string{"6f1c2a3g-0000-4000-8000-000000000001"}, "not an issue ID"},
	} {
		_, err := c.RenumberIssues(context.Background(), RenumberInput{IssueIDs: tc.ids})
		if err == nil || !WriteNotApplied(err) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
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
	issue := issueResult(lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1.5))
	f := newFake(t,
		lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"WEB-1","input":{"sortOrder":1.5}}`, Body: issue},
		lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"WEB-1","input":{"prioritySortOrder":-11.5}}`, Body: issue},
		lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"WEB-1","input":{"subIssueSortOrder":0}}`, Body: issue},
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

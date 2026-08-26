package main

import (
	"strings"
	"testing"
)

// The listing pickThread selects against, in the order `threads` prints
// it — the index column is a promise about THIS order.
func selectorRows() []threadRow {
	return []threadRow{
		{ID: "11111111-1111-1111-1111-111111111111", Title: "Fix the scroll spring", UpdatedAt: 100},
		{ID: "22222222-2222-2222-2222-222222222222", Title: "Fix the tail jump", UpdatedAt: 300},
		{ID: "33333333-3333-3333-3333-333333333333", Title: "Bench the renderer", UpdatedAt: 200},
	}
}

func TestPickThreadAcceptsEverySpelling(t *testing.T) {
	rows := selectorRows()
	for _, tc := range []struct {
		selector string
		wantID   string
	}{
		{"22222222-2222-2222-2222-222222222222", rows[1].ID},
		{"22222222-2222-2222-2222-222222222222", rows[1].ID},
		{"#1", rows[0].ID},
		{"#3", rows[2].ID},
		{"last", rows[1].ID},  // highest updatedAt, not last in the listing
		{"LAST", rows[1].ID},  // the spelling is not case-sensitive
		{"bench", rows[2].ID}, // a unique title prefix, lower-cased
		{"Bench the", rows[2].ID},
		{"fix the scroll", rows[0].ID},
	} {
		got, err := pickThread(rows, tc.selector)
		if err != nil {
			t.Errorf("pickThread(%q): %v", tc.selector, err)
			continue
		}
		if got.ID != tc.wantID {
			t.Errorf("pickThread(%q) = %s, want %s", tc.selector, got.ID, tc.wantID)
		}
	}
}

// A prefix matching two rows must never pick one. The candidate list
// carries the index column precisely so the caller can retype `#2`.
func TestPickThreadRefusesAnAmbiguousTitlePrefix(t *testing.T) {
	_, err := pickThread(selectorRows(), "fix")
	if err == nil {
		t.Fatal("an ambiguous prefix resolved")
	}
	if code := exitCodeOf(t, err); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	for _, want := range []string{"#1", "#2", "matches 2 threads"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("candidate list omits %q: %v", want, err)
		}
	}
}

// `items --thread garbage` used to print "no items" and exit 0, which
// reads as "that thread is empty" — the wrong finding entirely, and the
// one a caller is least likely to double-check.
func TestPickThreadRefusesAMissAndNamesTheListing(t *testing.T) {
	_, err := pickThread(selectorRows(), "garbage")
	if err == nil {
		t.Fatal("a selector matching nothing resolved")
	}
	if !strings.Contains(err.Error(), `no thread "garbage"`) || !strings.Contains(err.Error(), "ao-harness threads") {
		t.Fatalf("error = %v", err)
	}
}

func TestPickThreadRefusesAnOutOfRangeIndex(t *testing.T) {
	for _, selector := range []string{"#0", "#4", "#x", "#-1"} {
		_, err := pickThread(selectorRows(), selector)
		if err == nil {
			t.Errorf("pickThread(%q) resolved", selector)
			continue
		}
		if !strings.Contains(err.Error(), "#1..#3") {
			t.Errorf("pickThread(%q) does not name the range: %v", selector, err)
		}
	}
}

func TestPickThreadOnAnEmptyInstanceSaysSo(t *testing.T) {
	_, err := pickThread(nil, "last")
	if err == nil {
		t.Fatal("a selector resolved against no rows")
	}
	if !strings.Contains(err.Error(), "no threads") {
		t.Fatalf("error = %v", err)
	}
}

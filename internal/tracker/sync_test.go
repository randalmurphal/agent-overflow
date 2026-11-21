package tracker

import (
	"errors"
	"testing"
	"time"
)

func TestCheckNewRows(t *testing.T) {
	if err := CheckNewRows("10001", []BoardID{"1", "2"}); err != nil {
		t.Fatalf("good input refused: %v", err)
	}
	bad := map[string]struct {
		issue  IssueID
		boards []BoardID
	}{
		"no issue":    {"", []BoardID{"1"}},
		"no boards":   {"10001", nil},
		"empty board": {"10001", []BoardID{"1", ""}},
		"board twice": {"10001", []BoardID{"1", "2", "1"}},
	}
	for name, in := range bad {
		if err := CheckNewRows(in.issue, in.boards); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: CheckNewRows = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestDeltaResume(t *testing.T) {
	newest := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		walkStart   time.Time
		wantRestart time.Time
		wantExact   bool
	}{
		{"no tracker clock steps back from the newest row", time.Time{}, newest.Add(-DeltaOverlap), false},
		{"newest row older than the overlap resumes exactly", newest.Add(DeltaOverlap + time.Second), newest, true},
		{"newest row exactly at the overlap resumes exactly", newest.Add(DeltaOverlap), newest, true},
		{"newest row inside the overlap restarts at the overlap", newest.Add(DeltaOverlap - time.Millisecond), newest.Add(-time.Millisecond), false},
		{"newest row at the walk's start restarts at the overlap", newest, newest.Add(-DeltaOverlap), false},
		{"newest row after the walk's start restarts at the overlap", newest.Add(-time.Hour), newest.Add(-time.Hour - DeltaOverlap), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restart, exact := DeltaResume(newest, tt.walkStart)
			if !restart.Equal(tt.wantRestart) || exact != tt.wantExact {
				t.Fatalf("DeltaResume = %v, %v; want %v, %v", restart, exact, tt.wantRestart, tt.wantExact)
			}
		})
	}
}

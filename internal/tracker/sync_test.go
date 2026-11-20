package tracker

import (
	"testing"
	"time"
)

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

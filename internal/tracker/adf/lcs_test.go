package adf

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestLinearAlignmentPinned(t *testing.T) {
	a, b := []string{"A", "B", "A"}, []string{"B", "A", "B"}
	got, err := lcs(t.Context(), a, b, 0)
	if err != nil || !slices.Equal(got, []int{-1, 0, 1}) {
		t.Fatalf("%v, %v", got, err)
	}
}

func TestLinearAlignmentMaximumReuse(t *testing.T) {
	var sequences [][]string
	for size, count := 0, 1; size <= 5; size, count = size+1, count*2 {
		for mask := range count {
			a := make([]string, size)
			for i := range a {
				a[i] = string(rune('A' + (mask>>i)&1))
			}
			sequences = append(sequences, a)
		}
	}
	for _, a := range sequences {
		for _, b := range sequences {
			dense, err := lcs(t.Context(), a, b, 1000)
			if err != nil {
				t.Fatal(err)
			}
			linear, err := lcs(t.Context(), a, b, 0)
			if err != nil {
				t.Fatal(err)
			}
			count, want, last := 0, 0, -1
			for i, j := range linear {
				if dense[i] >= 0 {
					want++
				}
				if j < 0 {
					continue
				}
				if j <= last || a[i] != b[j] {
					t.Fatalf("invalid match %v, %v: %v", a, b, linear)
				}
				count++
				last = j
			}
			if count != want {
				t.Fatalf("%v, %v: linear %v, dense %v", a, b, linear, dense)
			}
		}
	}
}

func TestAlignmentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, cells := range []int{0, alignmentCells} {
		if _, err := lcs(ctx, []string{"a"}, []string{"b"}, cells); !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	}
}

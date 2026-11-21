package adf

import (
	"context"
	"errors"
	"math/rand/v2"
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

// lcs, with its common ends taken first, matches as many keys as an
// exhaustive search does, on the table and on the linear-space path.
func TestAlignmentOptimal(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	seq := func(letters int) []string {
		s := make([]string, rng.IntN(10))
		for i := range s {
			s[i] = string(rune('a' + rng.IntN(letters)))
		}
		return s
	}
	var brute func(a, b []string) int
	brute = func(a, b []string) int {
		if len(a) == 0 || len(b) == 0 {
			return 0
		}
		out := max(brute(a[1:], b), brute(a, b[1:]))
		if a[0] == b[0] {
			out = max(out, 1+brute(a[1:], b[1:]))
		}
		return out
	}
	for range 20000 {
		letters := 1 + rng.IntN(3)
		a, b := seq(letters), seq(letters)
		want := brute(a, b)
		for _, cells := range []int{1000, 0} {
			match, err := lcs(t.Context(), a, b, cells)
			if err != nil {
				t.Fatal(err)
			}
			got, last := 0, -1
			for i, j := range match {
				if j < 0 {
					continue
				}
				if j <= last || a[i] != b[j] {
					t.Fatalf("cells %d: %q, %q: invalid %v", cells, a, b, match)
				}
				got, last = got+1, j
			}
			if got != want {
				t.Fatalf("cells %d: %q, %q: %v matches %d, want %d", cells, a, b, match, got, want)
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

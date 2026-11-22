package adf

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// Deleting one of two images with the same URL keeps the other one whole:
// the run matcher keeps the original that matches more of the edit.
func TestEditDeletesTwinImageKeepsCaption(t *testing.T) {
	image := func(width float64, caption string) *Node {
		n := &Node{Type: "mediaSingle", Attrs: map[string]any{"layout": "center", "width": width},
			Content: []*Node{{Type: "media", Attrs: map[string]any{
				"type": "external", "url": "https://example.com/a.png"}}}}
		if caption != "" {
			n.Content = append(n.Content, &Node{Type: "caption",
				Content: []*Node{{Type: "text", Text: caption}}})
		}
		return n
	}
	orig := &Node{Type: "doc", Version: 1, Content: []*Node{image(40, ""), image(80, "Figure 1")}}
	md := ToMarkdown(Doc(orig.Content[1]))
	if !strings.Contains(ToMarkdown(orig), md) {
		t.Fatalf("second image's Markdown %q not in %q", md, ToMarkdown(orig))
	}
	out, ws, err := Edit(t.Context(), orig, md, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLoss(ws, false); err != nil {
		t.Fatalf("deleting the first image loses nothing: %v", err)
	}
	if got, want := mustJSON(out), mustJSON(&Node{Type: "doc", Version: 1,
		Content: []*Node{image(80, "Figure 1")}}); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// The run matcher matches as many keys as an exhaustive search does, on
// both paths: the table (cells 0) and the linear-space one (cells 1).
func TestMatchRunsOptimal(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	word := func(letters int) string { return string(rune('a' + rng.IntN(letters))) }
	for range 30000 {
		letters := 1 + rng.IntN(3)
		runs := make([][]string, rng.IntN(7))
		for i := range runs {
			for range rng.IntN(4) {
				runs[i] = append(runs[i], word(letters))
			}
		}
		b := make([]string, rng.IntN(9))
		for i := range b {
			b[i] = word(letters)
		}
		want := bruteRuns(runs, b)
		for _, cells := range []int{0, 1} {
			start, err := matchRuns(t.Context(), runs, b, cells)
			if err != nil {
				t.Fatal(err)
			}
			if got := runsScore(t, runs, b, start); got != want {
				t.Fatalf("cells %d: runs %q against %q: %v matches %d keys, want %d",
					cells, runs, b, start, got, want)
			}
		}
	}
}

// runsScore checks that start places runs in order, without overlap, each
// on equal keys, and returns how many keys it matches.
func runsScore(t *testing.T, runs [][]string, b []string, start []int) int {
	t.Helper()
	score, next := 0, 0
	for i, s := range start {
		if s < 0 {
			continue
		}
		r := runs[i]
		if len(r) == 0 || s < next || s+len(r) > len(b) || !slices.Equal(r, b[s:s+len(r)]) {
			t.Fatalf("runs %q against %q: invalid %v", runs, b, start)
		}
		score, next = score+len(r), s+len(r)
	}
	return score
}

// bruteRuns tries every placement of every run.
func bruteRuns(runs [][]string, b []string) int {
	var best func(i, from int) int
	best = func(i, from int) int {
		if i == len(runs) {
			return 0
		}
		out := best(i+1, from)
		if r := runs[i]; len(r) > 0 {
			for s := from; s+len(r) <= len(b); s++ {
				if slices.Equal(r, b[s:s+len(r)]) {
					out = max(out, len(r)+best(i+1, s+len(r)))
				}
			}
		}
		return out
	}
	return best(0, 0)
}

func TestMatchRunsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runs, b := [][]string{{"a"}, {"b"}}, []string{"b", "a"}
	for _, cells := range []int{0, 1} {
		if _, err := matchRuns(ctx, runs, b, cells); !errors.Is(err, context.Canceled) {
			t.Fatalf("cells %d: %v", cells, err)
		}
	}
}

// benchRuns is n runs, every fifth an image and its caption and the rest
// one block, and b their keys with the runs changed picks replaced.
func benchRuns(n int, changed func(i int) bool) ([][]string, []string) {
	runs := make([][]string, n)
	var b []string
	for i := range runs {
		k := fmt.Sprintf("block %d", i)
		runs[i] = []string{k}
		if i%5 == 0 {
			runs[i] = append(runs[i], k+" caption")
		}
		if changed(i) {
			b = append(b, fmt.Sprintf("new %d", i))
			continue
		}
		b = append(b, runs[i]...)
	}
	return runs, b
}

func BenchmarkMatchRuns(b *testing.B) {
	for _, c := range []struct {
		name    string
		n       int
		changed func(i int) bool
	}{
		{"one-edit-9000", 9000, func(i int) bool { return i == 4500 }},
		{"rewrite-half-800", 800, func(i int) bool { return i%2 == 0 }},
		{"rewrite-half-3000", 3000, func(i int) bool { return i%2 == 0 }},
	} {
		runs, keys := benchRuns(c.n, c.changed)
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := matchRuns(b.Context(), runs, keys, alignmentCells); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

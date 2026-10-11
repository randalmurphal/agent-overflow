package cilog

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func numberedLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d %s", i, strings.Repeat("x", i%37))
	}
	return lines
}

func TestWindowReadsAtTheirBoundaries(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := gitlabKey("1")
	want := numberedLines(1000)
	fetchText(t, s, key, strings.Join(want, "\n")+"\n", `"a"`)
	for _, tc := range []struct{ from, count int }{
		{0, 1}, {0, 300}, {999, 1}, {999, 10}, {255, 2}, {256, 1}, {257, 3}, {511, 513}, {1000, 5}, {1500, 1},
	} {
		window, err := s.Lines(key, tc.from, tc.count, 1<<20)
		if err != nil {
			t.Fatalf("Lines(%d, %d): %v", tc.from, tc.count, err)
		}
		expect := want[min(tc.from, len(want)):min(tc.from+tc.count, len(want))]
		if !slices.Equal(window.Lines, expect) || window.From != tc.from || window.TotalLines != 1000 || window.Clipped != nil {
			t.Fatalf("Lines(%d, %d) = %d lines from %d, want %d", tc.from, tc.count, len(window.Lines), window.From, len(expect))
		}
		if window.Lines == nil {
			t.Fatalf("Lines(%d, %d) answered nil, want an empty list", tc.from, tc.count)
		}
	}
}

func TestAnEmptyLogHasNoLines(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	fetchText(t, s, key, "", `"empty"`)
	index, err := s.Index(key, Job{Name: "j", Status: "success"})
	if err != nil {
		t.Fatal(err)
	}
	if index.TotalLines != 0 || index.TotalBytes != 0 || len(index.Sections) != 0 {
		t.Fatalf("empty log index = %+v", index)
	}
	window, err := s.Lines(key, 0, 10, 1<<20)
	if err != nil || len(window.Lines) != 0 {
		t.Fatalf("empty log lines = %+v, %v", window, err)
	}
}

// A line longer than the scan block is skipped through and returned whole
// when the budget allows, clipped at the budget on a rune boundary when it
// is the first line of a read, and left for the next read otherwise.
func TestALineLongerThanTheScanBlock(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	long := strings.Repeat("é", (3*readBlock+5)/2)
	lines := append(numberedLines(300), long)
	lines = append(lines, numberedLines(10)...)
	fetchText(t, s, key, strings.Join(lines, "\n")+"\n", `"long"`)

	window, err := s.Lines(key, 299, 3, 1<<22)
	if err != nil || !slices.Equal(window.Lines, lines[299:302]) {
		t.Fatalf("whole long line read failed: %v", err)
	}
	// Skipping the long line to reach the one after it.
	window, err = s.Lines(key, 301, 2, 1<<20)
	if err != nil || !slices.Equal(window.Lines, lines[301:303]) {
		t.Fatalf("read after the long line = %q, %v", window.Lines, err)
	}
	// Over the budget as the first line: clipped, valid UTF-8, marked.
	window, err = s.Lines(key, 300, 5, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Lines) != 1 || !slices.Equal(window.Clipped, []int{300}) || len(window.Lines[0]) != 1000 || !utf8.ValidString(window.Lines[0]) {
		t.Fatalf("clipped read = %d lines, clipped %v, first %d bytes", len(window.Lines), window.Clipped, len(window.Lines[0]))
	}
	// Over the budget after other lines: the read stops before it.
	window, err = s.Lines(key, 298, 5, 1<<16)
	if err != nil || !slices.Equal(window.Lines, lines[298:300]) || window.Clipped != nil {
		t.Fatalf("read up to the long line = %d lines, clipped %v, %v", len(window.Lines), window.Clipped, err)
	}
}

func TestLinesOfALogTheStoreDoesNotHold(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	if _, err := s.Lines(githubKey("404"), 0, 1, 100); err != ErrNoLog {
		t.Fatalf("Lines of an unknown log = %v, want ErrNoLog", err)
	}
}

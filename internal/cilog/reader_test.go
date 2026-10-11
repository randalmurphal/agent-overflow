package cilog

import (
	"bytes"
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
		window, err := s.Lines(key, tc.from, tc.count, 1<<20, 1<<30)
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
	window, err := s.Lines(key, 0, 10, 1<<20, 1<<30)
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

	window, err := s.Lines(key, 299, 3, 1<<22, 1<<30)
	if err != nil || !slices.Equal(window.Lines, lines[299:302]) {
		t.Fatalf("whole long line read failed: %v", err)
	}
	// Skipping the long line to reach the one after it.
	window, err = s.Lines(key, 301, 2, 1<<20, 1<<30)
	if err != nil || !slices.Equal(window.Lines, lines[301:303]) {
		t.Fatalf("read after the long line = %q, %v", window.Lines, err)
	}
	// Over the budget as the first line: clipped, valid UTF-8, marked.
	window, err = s.Lines(key, 300, 5, 1001, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Lines) != 1 || !slices.Equal(window.Clipped, []int{300}) || len(window.Lines[0]) != 1000 || !utf8.ValidString(window.Lines[0]) {
		t.Fatalf("clipped read = %d lines, clipped %v, first %d bytes", len(window.Lines), window.Clipped, len(window.Lines[0]))
	}
	// Over the budget after other lines: the read stops before it.
	window, err = s.Lines(key, 298, 5, 1<<16, 1<<30)
	if err != nil || !slices.Equal(window.Lines, lines[298:300]) || window.Clipped != nil {
		t.Fatalf("read up to the long line = %d lines, clipped %v, %v", len(window.Lines), window.Clipped, err)
	}
}

// A line longer than the read's line bound is clipped to it on a rune
// boundary and listed, and the read goes on past it; only the clipped text
// counts against the budget.
func TestLinesAreClippedAtTheLineBound(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	long := strings.Repeat("é", 3*readBlock)
	lines := []string{"a", long, "b", long, "c"}
	fetchText(t, s, key, strings.Join(lines, "\n")+"\n", `"long"`)
	window, err := s.Lines(key, 0, 5, 3000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	clippedLong := strings.Repeat("é", 500)
	if !slices.Equal(window.Lines, []string{"a", clippedLong, "b", clippedLong, "c"}) || !slices.Equal(window.Clipped, []int{1, 3}) {
		t.Fatalf("clipped read = %d lines, clipped %v", len(window.Lines), window.Clipped)
	}
	// The budget stops the read before a clipped line it cannot hold.
	window, err = s.Lines(key, 0, 5, 1500, 1001)
	if err != nil || !slices.Equal(window.Lines, []string{"a", clippedLong, "b"}) {
		t.Fatalf("budgeted read = %d lines, %v", len(window.Lines), err)
	}
}

func TestLinesOfALogTheStoreDoesNotHold(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	if _, err := s.Lines(githubKey("404"), 0, 1, 100, 1<<30); err != ErrNoLog {
		t.Fatalf("Lines of an unknown log = %v, want ErrNoLog", err)
	}
}

// CopyLog writes the whole display log; CopySection writes one row's
// lines as the last Index computed them, and refuses a row it did not.
func TestCopiesWriteTheLogAndASection(t *testing.T) {
	t.Parallel()
	s, key, index := gitlabIndex(t, testTrace, Job{Name: "unit", Status: "failed"})
	var whole bytes.Buffer
	n, err := s.CopyLog(key, &whole)
	if err != nil || n != index.TotalBytes {
		t.Fatalf("CopyLog = %d bytes, %v; want %d", n, err, index.TotalBytes)
	}
	if got := strings.Split(strings.TrimSuffix(whole.String(), "\n"), "\n"); !slices.Equal(got, allLines(t, s, key)) {
		t.Fatalf("copied log %q", got)
	}
	script := index.Sections[2]
	var part bytes.Buffer
	row, err := s.CopySection(key, script.Key, &part)
	if err != nil || row != script {
		t.Fatalf("CopySection = %+v, %v", row, err)
	}
	if want := strings.Join(sectionText(t, s, key, script), "\n") + "\n"; part.String() != want || int64(part.Len()) != script.Bytes {
		t.Fatalf("section copy %q, want %q", part.String(), want)
	}
	if _, err := s.CopySection(key, "nope", &part); err == nil {
		t.Fatal("a row the index does not have was copied")
	}
	// A newer version whose rows were not computed yet is not copied by
	// the rows of the one before.
	fetchText(t, s, key, testTrace+"more\n", `"t2"`)
	if _, err := s.CopySection(key, script.Key, &part); err == nil {
		t.Fatal("a section was copied by the rows of a replaced version")
	}
	if _, err := s.CopyLog(githubKey("404"), &part); err != ErrNoLog {
		t.Fatalf("CopyLog of an unknown log = %v", err)
	}
}

// Before a GitHub log is published its steps are still rows, without
// lines; a GitLab job has none until its trace names them.
func TestSectionsWithoutALog(t *testing.T) {
	t.Parallel()
	job := Job{Name: "unit", Status: statusRunning, Steps: []Step{
		{Number: 1, Name: "Set up job", Status: "success", StartedAt: "2026-10-11T00:00:00Z", CompletedAt: "2026-10-11T00:00:02Z"},
		{Number: 2, Name: "Run tests", Status: statusRunning, StartedAt: "2026-10-11T00:00:02Z"},
		{Number: 3, Name: "Skipped", Status: statusSkipped},
	}}
	rows := SectionsWithoutLog(githubKey("1"), job)
	if len(rows) != 2 || rows[0].Key != "step:1" || rows[0].DurationSeconds != 2 || rows[1].Status != statusRunning || rows[0].Lines != 0 || rows[1].Lines != 0 {
		t.Fatalf("GitHub rows without a log = %+v", rows)
	}
	if rows := SectionsWithoutLog(gitlabKey("1"), job); rows != nil {
		t.Fatalf("GitLab rows without a log = %+v", rows)
	}
}

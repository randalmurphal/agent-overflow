package unidiff

import (
	"strings"
	"testing"
)

func TestParseHunkHeader(t *testing.T) {
	cases := []struct {
		line string
		want Hunk
		ok   bool
	}{
		{"@@ -1,3 +1,4 @@", Hunk{1, 3, 1, 4}, true},
		{"@@ -22,7 +33,9 @@ def context():", Hunk{22, 7, 33, 9}, true},
		{"@@ garbage @@", Hunk{}, false},
		{"@@ -7 +8 @@ func main() {", Hunk{7, 1, 8, 1}, true},
		{"@@ -0,0 +1,2 @@", Hunk{0, 0, 1, 2}, true},
		{"@@ -5,2 +4,0 @@", Hunk{5, 2, 4, 0}, true},
		{"@@@ -1,2 -1,2 +1,3 @@@", Hunk{}, false},
		{"@@ -a,1 +1 @@", Hunk{}, false},
		{"@@ -1,2 @@", Hunk{}, false},
		{"@@ -1,2 +1,2", Hunk{}, false},
		{"--- a/x", Hunk{}, false},
	}
	for _, tc := range cases {
		got, ok := ParseHunkHeader(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseHunkHeader(%q) = %+v, %v; want %+v, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func kinds(patch string) []LineKind {
	var body Body
	var out []LineKind
	for _, line := range strings.Split(patch, "\n") {
		out = append(out, body.Next(line))
	}
	return out
}

func TestBodyReadsHeaderLookalikesInsideAHunkAsBodyLines(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/q.sql b/q.sql",
		"--- a/q.sql",
		"+++ b/q.sql",
		"@@ -1,3 +1,3 @@",
		"--- removed sql comment",
		"+++ added counter",
		" context",
		"-last",
		"\\ No newline at end of file",
		"+last",
		"diff --git a/r.txt b/r.txt",
		"--- a/r.txt",
		"+++ b/r.txt",
		"@@ -1 +1 @@",
		"-x",
		"+y",
	}, "\n")
	want := []LineKind{
		Outside, Outside, Outside, HunkHeader,
		Removed, Added, Context, Removed, NoNewline, Added,
		Outside, Outside, Outside, HunkHeader, Removed, Added,
	}
	got := kinds(patch)
	if len(got) != len(want) {
		t.Fatalf("got %d kinds, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d %q: kind %d, want %d", i, strings.Split(patch, "\n")[i], got[i], want[i])
		}
	}
}

func TestBodyEndsWhereItsCountsEndWithoutADiffLine(t *testing.T) {
	// A plain multi-file unified diff has no `diff --git` lines: the next
	// file's headers follow the last body line directly.
	patch := strings.Join([]string{
		"--- a/one",
		"+++ b/one",
		"@@ -1,2 +1,2 @@",
		" same",
		"-old",
		"+new",
		"--- a/two",
		"+++ b/two",
		"@@ -1 +1,2 @@",
		"",
		"+added",
	}, "\n")
	want := []LineKind{
		Outside, Outside, HunkHeader, Context, Removed, Added,
		Outside, Outside, HunkHeader, Context, Added,
	}
	got := kinds(patch)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: kind %d, want %d", i, got[i], want[i])
		}
	}
}

func TestBodyEndsAtALineItCannotHold(t *testing.T) {
	// Counts larger than the lines that follow: a line that fits neither
	// side ends the body, and what follows is read by its prefix.
	patch := strings.Join([]string{
		"@@ -1,5 +1,5 @@",
		"-a",
		"diff --git a/b b/b",
		"--- a/b",
		"@@ -1 +0,0 @@",
		"+no room on the new side",
		"-b",
		"-past the count",
		"@@@ -1 -1 +1 @@@",
		"+outside a malformed header",
	}, "\n")
	want := []LineKind{
		HunkHeader, Removed, Outside, Outside,
		HunkHeader, Outside, Outside, Outside,
		HunkHeader, Outside,
	}
	got := kinds(patch)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: kind %d, want %d", i, got[i], want[i])
		}
	}
}

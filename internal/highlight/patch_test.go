package highlight

import (
	"strings"
	"testing"
)

const samplePatch = `diff --git a/route.py b/route.py
index 1234567..89abcde 100644
--- a/route.py
+++ b/route.py
@@ -22,7 +33,9 @@ def other():
 def route_template_path(request):
-    """Old docstring line.
+    """New docstring line with and, or keywords.
+    Second added line.
     """
     return request.scope
@@ -50,3 +62,3 @@ class Foo:
     def bar(self):
-        return 1
+        return 2
`

func TestParsePatchStructure(t *testing.T) {
	p := parsePatch(samplePatch)

	// The trailing empty segment from the final newline is not a diff
	// line — the frontend skips it, so parsePatch must too.
	wantLineCount := len(strings.Split(samplePatch, "\n")) - 1
	if p.lineCount != wantLineCount {
		t.Fatalf("lineCount = %d, want %d", p.lineCount, wantLineCount)
	}
	if len(p.hunks) != 2 {
		t.Fatalf("hunks = %d, want 2", len(p.hunks))
	}

	h := p.hunks[0]
	if h.oldStart != 22 || h.newStart != 33 {
		t.Errorf("hunk 0 starts = (%d,%d), want (22,33)", h.oldStart, h.newStart)
	}
	wantOld := "def route_template_path(request):\n" +
		`    """Old docstring line.` + "\n" +
		`    """` + "\n" +
		"    return request.scope"
	if string(h.oldDoc) != wantOld {
		t.Errorf("hunk 0 oldDoc:\n%q\nwant:\n%q", h.oldDoc, wantOld)
	}
	wantNew := "def route_template_path(request):\n" +
		`    """New docstring line with and, or keywords.` + "\n" +
		"    Second added line.\n" +
		`    """` + "\n" +
		"    return request.scope"
	if string(h.newDoc) != wantNew {
		t.Errorf("hunk 0 newDoc:\n%q\nwant:\n%q", h.newDoc, wantNew)
	}

	// Line refs: context lines land in both docs with a 1-byte pad;
	// add/del in one doc with no pad.
	var sides []hunkSide
	var pads []int
	for _, ref := range h.lines {
		sides = append(sides, ref.side)
		pads = append(pads, ref.outPad)
	}
	wantSides := []hunkSide{sideBoth, sideOld, sideNew, sideNew, sideBoth, sideBoth}
	if len(sides) != len(wantSides) {
		t.Fatalf("hunk 0 line refs = %d, want %d", len(sides), len(wantSides))
	}
	for i := range wantSides {
		if sides[i] != wantSides[i] {
			t.Errorf("line ref %d side = %d, want %d", i, sides[i], wantSides[i])
		}
	}
	for i, side := range wantSides {
		wantPad := 0
		if side == sideBoth {
			wantPad = 1
		}
		if pads[i] != wantPad {
			t.Errorf("line ref %d pad = %d, want %d", i, pads[i], wantPad)
		}
	}

	// New-side file line numbers advance from newStart across
	// add/context lines only.
	ref := h.lines[0] // context "def route_template_path"
	if ref.newFileLine != 33 {
		t.Errorf("first context newFileLine = %d, want 33", ref.newFileLine)
	}
	if h.lines[2].newFileLine != 34 || h.lines[3].newFileLine != 35 {
		t.Errorf("added lines newFileLine = (%d,%d), want (34,35)",
			h.lines[2].newFileLine, h.lines[3].newFileLine)
	}
	if h.lines[1].newFileLine != -1 {
		t.Errorf("del line newFileLine = %d, want -1", h.lines[1].newFileLine)
	}
}

func TestParsePatchAddedFile(t *testing.T) {
	patch := "diff --git a/new.py b/new.py\n" +
		"new file mode 100644\n" +
		"--- /dev/null\n" +
		"+++ b/new.py\n" +
		"@@ -0,0 +1,2 @@\n" +
		"+x = 1\n" +
		"+y = 2\n"
	p := parsePatch(patch)
	if len(p.hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(p.hunks))
	}
	h := p.hunks[0]
	if h.oldStart != 0 || h.newStart != 1 {
		t.Errorf("starts = (%d,%d), want (0,1)", h.oldStart, h.newStart)
	}
	if len(h.oldDoc) != 0 {
		t.Errorf("oldDoc = %q, want empty", h.oldDoc)
	}
	if string(h.newDoc) != "x = 1\ny = 2" {
		t.Errorf("newDoc = %q", h.newDoc)
	}
}

func TestParsePatchNoNewlineMarker(t *testing.T) {
	patch := "@@ -1,1 +1,1 @@\n-old\n+new\n\\ No newline at end of file\n"
	p := parsePatch(patch)
	if len(p.hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(p.hunks))
	}
	h := p.hunks[0]
	if len(h.lines) != 2 {
		t.Fatalf("line refs = %d, want 2 (marker excluded)", len(h.lines))
	}
	if string(h.newDoc) != "new" || string(h.oldDoc) != "old" {
		t.Errorf("docs = (%q, %q)", h.oldDoc, h.newDoc)
	}
}

func TestParsePatchEmptyContextLine(t *testing.T) {
	// Some tools emit a fully empty line for empty context (no leading
	// space). Both forms must land in both docs; only the spaced form
	// gets an output pad.
	patch := "@@ -1,3 +1,3 @@\n a\n\n-b\n+c\n"
	p := parsePatch(patch)
	h := p.hunks[0]
	if string(h.newDoc) != "a\n\nc" {
		t.Errorf("newDoc = %q, want %q", h.newDoc, "a\n\nc")
	}
	if h.lines[0].outPad != 1 || h.lines[1].outPad != 0 {
		t.Errorf("pads = (%d,%d), want (1,0)", h.lines[0].outPad, h.lines[1].outPad)
	}
}

func TestParseHunkHeaderForms(t *testing.T) {
	cases := []struct {
		line       string
		oldS, newS int
		ok         bool
	}{
		{"@@ -22,7 +33,9 @@ def context():", 22, 33, true},
		{"@@ -1 +1 @@", 1, 1, true},
		{"@@ -0,0 +1,5 @@", 0, 1, true},
		{"@@ garbage @@", 0, 0, false},
		{"@@ -x,1 +1,1 @@", 0, 0, false},
	}
	for _, c := range cases {
		o, n, ok := parseHunkHeader(c.line)
		if ok != c.ok || o != c.oldS || n != c.newS {
			t.Errorf("parseHunkHeader(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.line, o, n, ok, c.oldS, c.newS, c.ok)
		}
	}
}

func TestEncodeLine(t *testing.T) {
	// abcdef: a,b keyword; c plain; d,e,f string
	classes := []uint16{ClassKeyword, ClassKeyword, ClassNone, ClassString, ClassString, ClassString}
	line := encodeLine(classes)
	want := []uint16{2, ClassKeyword, 1, ClassNone, 3, ClassString}
	if len(line.Runs) != len(want) {
		t.Fatalf("runs = %v, want %v", line.Runs, want)
	}
	for i := range want {
		if line.Runs[i] != want[i] {
			t.Fatalf("runs = %v, want %v", line.Runs, want)
		}
	}

	if encodeLine([]uint16{ClassNone, ClassNone}).Runs != nil {
		t.Error("all-plain line should encode nil runs")
	}
	if encodeLine(nil).Runs != nil {
		t.Error("empty line should encode nil runs")
	}
	long := make([]uint16, maxLineBytes+1)
	long[0] = ClassKeyword
	if encodeLine(long).Runs != nil {
		t.Error("over-cap line should encode plain")
	}
}

func TestEncodeLinesSplitsAndCounts(t *testing.T) {
	src := []byte("ab\nc\n")
	classes := []uint16{ClassKeyword, ClassKeyword, 0, ClassString, 0}
	lines := encodeLines(src, classes)
	// "ab", "c", "" — trailing newline yields a final empty line,
	// matching the frontend's split('\n').
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	if lines[0].Runs == nil || lines[1].Runs == nil || lines[2].Runs != nil {
		t.Errorf("unexpected run presence: %+v", lines)
	}
	if countLines(src) != 3 || countLines(nil) != 1 {
		t.Errorf("countLines mismatch: %d, %d", countLines(src), countLines(nil))
	}
}

func TestPadRuns(t *testing.T) {
	line := EncodedLine{Runs: []uint16{3, ClassKeyword}}
	padded := padRuns(line, 1)
	want := []uint16{1, ClassNone, 3, ClassKeyword}
	for i := range want {
		if padded.Runs[i] != want[i] {
			t.Fatalf("padded = %v, want %v", padded.Runs, want)
		}
	}
	if padRuns(EncodedLine{}, 1).Runs != nil {
		t.Error("padding a plain line stays plain")
	}
	if got := padRuns(line, 0); &got.Runs[0] != &line.Runs[0] {
		t.Error("zero pad should return the input unchanged")
	}
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The health rollup's whole point is that a second run reports what
// happened SINCE the first. These tests drive the cursor and the scanners
// over canned files; nothing here needs an instance.

func writeLines(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanNewLinesAdvancesCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	writeLines(t, path, "one\ntwo\n")

	lines, cursor, rotated, err := scanNewLines(path, healthFileCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if rotated {
		t.Error("a first read is not a rotation")
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want 2", lines)
	}

	// Nothing appended: the same cursor must report nothing.
	lines, cursor, _, err = scanNewLines(path, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("a second read with no writes returned %v", lines)
	}

	writeLines(t, path, "one\ntwo\nthree\n")
	lines, _, _, err = scanNewLines(path, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "three" {
		t.Errorf("lines = %v, want [three]", lines)
	}
}

func TestScanNewLinesLeavesPartialLine(t *testing.T) {
	// uitrace appends; a reader that consumed a half-written record would
	// report a torn line once and never see the whole one.
	path := filepath.Join(t.TempDir(), "log.jsonl")
	writeLines(t, path, "whole\npart")

	lines, cursor, _, err := scanNewLines(path, healthFileCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "whole" {
		t.Fatalf("lines = %v, want [whole]", lines)
	}

	writeLines(t, path, "whole\npartial\n")
	lines, _, _, err = scanNewLines(path, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "partial" {
		t.Errorf("lines = %v, want [partial] read whole on the second pass", lines)
	}
}

func TestScanNewLinesDetectsRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	writeLines(t, path, "a\nb\nc\nd\ne\n")
	_, cursor, _, err := scanNewLines(path, healthFileCursor{})
	if err != nil {
		t.Fatal(err)
	}

	// uitrace rotates at a size cap; the new file is smaller than the old
	// offset, so a cursor-keeping reader would seek past its end.
	writeLines(t, path, "fresh\n")
	lines, next, rotated, err := scanNewLines(path, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Error("a shrunk file is a rotation")
	}
	if len(lines) != 1 || lines[0] != "fresh" {
		t.Errorf("lines = %v, want [fresh]", lines)
	}
	if next.Offset != int64(len("fresh\n")) {
		t.Errorf("offset = %d, want %d", next.Offset, len("fresh\n"))
	}
}

// The rotation the size heuristic cannot see: the old file is moved
// aside, a NEW one takes its name, and by the next check it has already
// grown past the offset the last one stopped at. Every byte the reader
// then skips is a line it never reported, with nothing marking the gap.
func TestScanNewLinesDetectsARotatedAndRegrownFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")
	writeLines(t, path, "a\nb\nc\nd\ne\n")
	_, cursor, _, err := scanNewLines(path, healthFileCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if cursor.Ident == "" {
		t.Skip("this filesystem exposes no file identity; the size heuristic is the whole answer here")
	}

	// Rename-and-recreate, the shape logrotate uses, then regrow past the
	// old offset before the next check looks.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	writeLines(t, path, "one\ntwo\nthree\nfour\n")

	lines, next, rotated, err := scanNewLines(path, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Fatal("a replaced file that regrew past the old offset is still a rotation")
	}
	if len(lines) != 4 || lines[0] != "one" || lines[3] != "four" {
		t.Errorf("lines = %v, want the new file read from its start", lines)
	}
	if next.Ident == cursor.Ident {
		t.Error("the cursor kept the dead file's identity")
	}
}

func TestScanNewLinesAbsentFileIsNotAnError(t *testing.T) {
	// ui-trace only exists once the frontend traced something, which is the
	// normal case for a fresh instance.
	lines, cursor, rotated, err := scanNewLines(filepath.Join(t.TempDir(), "nope.jsonl"), healthFileCursor{Offset: 40})
	if err != nil {
		t.Fatalf("absent file returned %v", err)
	}
	if len(lines) != 0 || rotated || cursor.Offset != 0 {
		t.Errorf("lines=%v rotated=%t cursor=%+v", lines, rotated, cursor)
	}
}

func TestHealthCursorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := loadHealthCursor(dir); got.Instance != "" {
		t.Errorf("an absent cursor should load empty, got %+v", got)
	}
	want := healthCursor{
		Instance:       "abcd1234",
		CheckedAt:      "2026-08-26T10:00:00Z",
		FrontendErrors: healthFileCursor{Offset: 12, Size: 12},
		UITrace:        healthFileCursor{Offset: 40, Size: 44},
	}
	if err := saveHealthCursor(dir, want); err != nil {
		t.Fatal(err)
	}
	got := loadHealthCursor(dir)
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	// A corrupt cursor must not stop a check: the cost is one over-report.
	writeLines(t, healthCursorPath(dir), "{not json")
	if got := loadHealthCursor(dir); got.Instance != "" {
		t.Errorf("a corrupt cursor should load empty, got %+v", got)
	}
}

func TestCountOracleTriggers(t *testing.T) {
	lines := []string{
		`{"label":"timeline.margin.diverge","delta":9}`,
		`{"label":"timeline.row.resize","rows":3}`,
		`{"label":"timeline.margin.diverge","delta":11}`,
		`{"label":"timeline.reasoning.tailJump"}`,
		`not json at all`,
		`{"label":"something.else"}`,
	}
	counts := countOracleTriggers(lines)
	if counts["timeline.margin.diverge"] != 2 {
		t.Errorf("diverge = %d, want 2", counts["timeline.margin.diverge"])
	}
	if counts["timeline.reasoning.tailJump"] != 1 {
		t.Errorf("tailJump = %d, want 1", counts["timeline.reasoning.tailJump"])
	}
	// The row-resize tracker emits continuously in an oracle build and says
	// nothing about correctness; counting it would bury the two that do.
	if _, ok := counts["timeline.row.resize"]; ok {
		t.Error("timeline.row.resize is a tracker, not an oracle")
	}
	if got := formatOracleCounts(counts); got != "timeline.margin.diverge x2, timeline.reasoning.tailJump x1" {
		t.Errorf("formatOracleCounts = %q", got)
	}
}

func TestScanBackendLogSeverityOrder(t *testing.T) {
	scan := scanBackendLog([]string{
		"2026/08/26 10:00:00 starting up",
		"2026/08/26 10:00:01 warn: mock provider is pinned",
		"2026/08/26 10:00:02 error: could not read settings",
		"panic: runtime error: invalid memory address",
		"goroutine 1 [running]:",
	})
	if scan.Total != 5 {
		t.Errorf("Total = %d, want 5", scan.Total)
	}
	if scan.Fatal != 1 || scan.Errors != 1 || scan.Warns != 1 {
		t.Errorf("fatal/errors/warns = %d/%d/%d, want 1/1/1", scan.Fatal, scan.Errors, scan.Warns)
	}
	if len(scan.Sample) != 3 {
		t.Errorf("sample = %v, want the three classified lines", scan.Sample)
	}
}

func TestScanBackendLogPanicWinsOverError(t *testing.T) {
	// One line that says both is one panic, not a panic plus an error.
	scan := scanBackendLog([]string{"panic: error decoding frame"})
	if scan.Fatal != 1 || scan.Errors != 0 {
		t.Errorf("fatal/errors = %d/%d, want 1/0", scan.Fatal, scan.Errors)
	}
}

func TestScanBackendLogSampleIsCapped(t *testing.T) {
	lines := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		lines = append(lines, "error: something went wrong")
	}
	scan := scanBackendLog(lines)
	if scan.Errors != 20 {
		t.Errorf("Errors = %d, want 20 (the count is always exact)", scan.Errors)
	}
	if len(scan.Sample) != healthLogSampleCap {
		t.Errorf("sample = %d lines, want %d", len(scan.Sample), healthLogSampleCap)
	}
}

func TestHealthExitCodeRules(t *testing.T) {
	cases := []struct {
		name     string
		sections []healthSection
		want     int
		worst    healthStatus
	}{
		{
			name:     "all ok",
			sections: []healthSection{{Name: "process", Status: healthOK}, {Name: "mocks", Status: healthOK}},
			want:     exitOK, worst: healthOK,
		},
		{
			// A rollup that failed on every stderr warning would be ignored
			// within a day, so a warn is reported and exits 0.
			name:     "warn does not fail",
			sections: []healthSection{{Name: "process", Status: healthOK}, {Name: "ui-oracles", Status: healthWarn}},
			want:     exitOK, worst: healthWarn,
		},
		{
			name:     "one red is red",
			sections: []healthSection{{Name: "process", Status: healthOK}, {Name: "frontend-errors", Status: healthRed}},
			want:     exitBadNews, worst: healthRed,
		},
		{
			name:     "red beats a later warn",
			sections: []healthSection{{Name: "process", Status: healthRed}, {Name: "mocks", Status: healthWarn}},
			want:     exitBadNews, worst: healthRed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := healthReport{At: "2026-08-26T10:00:00Z", Instance: "abcd1234", Sections: tc.sections}
			if got := report.Worst(); got != tc.worst {
				t.Errorf("Worst = %q, want %q", got, tc.worst)
			}
			if got := healthExitCode(report); got != tc.want {
				t.Errorf("exit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRenderHealthWatchLinesAreGreppable(t *testing.T) {
	report := healthReport{
		At:       "2026-08-26T10:00:00Z",
		Instance: "abcd1234",
		Sections: []healthSection{
			{Name: "process", Status: healthOK, Detail: "pid 42 harness on port 1234, up 5m0s"},
			{Name: "frontend-errors", Status: healthRed, Detail: "2 new since the last check", Lines: []string{"boom"}},
		},
	}
	out := renderHealthWatchLines(report)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("watch printed %d lines, want one per section:\n%s", len(lines), out)
	}
	for _, line := range lines {
		if line[:len(report.At)] != report.At {
			t.Errorf("every watch line must start with the timestamp: %q", line)
		}
	}
	// No clear-screen or cursor movement: the output has to survive a pipe.
	if strings.ContainsAny(out, "\x1b\r") {
		t.Errorf("watch output carries terminal control codes: %q", out)
	}
}

func TestRenderHealthReportShowsEvidenceLines(t *testing.T) {
	report := healthReport{
		At:       "2026-08-26T10:00:00Z",
		Instance: "abcd1234",
		Sections: []healthSection{
			{Name: "backend-stderr", Status: healthWarn, Detail: "3 new line(s)", Lines: []string{"error: nope"}},
		},
	}
	out := renderHealthReport(report)
	if !strings.Contains(out, "health warn") {
		t.Errorf("the header must carry the overall verdict: %q", out)
	}
	if !strings.Contains(out, "error: nope") {
		t.Errorf("a section's evidence lines must print: %q", out)
	}
}

func TestScanFrontendErrorsSplitsNoticesFromFaults(t *testing.T) {
	scan := scanFrontendErrors([]string{
		`{"kind":"error","message":"ResizeObserver loop completed with undelivered notifications.","stack":""}`,
		`{"kind":"error","message":"TypeError: undefined is not a function","stack":"at render"}`,
		`{"kind":"error","message":"ResizeObserver loop limit exceeded","stack":""}`,
	})
	if scan.Faults != 1 {
		t.Errorf("Faults = %d, want 1", scan.Faults)
	}
	// The notice is still counted: it means layout work outran a frame,
	// which this timeline cares about. It just is not a thrown error.
	if scan.Notices != 2 {
		t.Errorf("Notices = %d, want 2", scan.Notices)
	}
	if len(scan.Sample) != 1 || !strings.Contains(scan.Sample[0], "TypeError") {
		t.Errorf("sample = %v, want only the fault", scan.Sample)
	}
}

// "Nothing was ever started here" is its own answer, and it is not a red
// rollup. With no registry row AND no instance file, the target was
// invented by the default-data-root fallback — health reported a dead
// process for an instance that never existed, at exitBadNews, which reads
// as "your harness crashed".
func TestHealthOnANeverBootedRootIsItsOwnVerdict(t *testing.T) {
	e, _, _ := testEnv(t.TempDir())
	e.instance = t.TempDir()

	_, err := e.collectHealth(context.Background())
	if err == nil {
		t.Fatal("health reported on a root nothing ever claimed")
	}
	if code := exitCodeOf(t, err); code != exitError {
		t.Fatalf("exit code = %d, want %d (a refusal, not bad news about a run)", code, exitError)
	}
	if !strings.Contains(err.Error(), "ao-harness up") {
		t.Fatalf("the verdict does not name how to start one: %v", err)
	}
}

// n/a is "this concern could not be evaluated", distinct from ok, which
// claims it WAS evaluated and came back clean. It must never worsen the
// rollup, and it must never be mistaken for a pass.
func TestNotApplicableSectionsNeverWorsenTheVerdict(t *testing.T) {
	report := healthReport{Sections: []healthSection{
		{Name: "process", Status: healthOK},
		{Name: "ui-oracles", Status: healthNA},
		{Name: "soak-autopilot", Status: healthNA},
	}}
	if worst := report.Worst(); worst != healthOK {
		t.Fatalf("worst = %q, want %q", worst, healthOK)
	}
	if healthExitCode(report) != exitOK {
		t.Fatalf("an n/a section failed the rollup")
	}
	if healthNA == healthOK {
		t.Fatal("n/a and ok must stay distinguishable in the rendered report")
	}
}

// A ui-oracle section over ZERO trace records is the case that forced the
// third status: reading "ok" there means "the oracles did not fire", and
// the truth is "no oracle ran" — which is what a caller relying on the
// rollup as a gate most needs to be told.
func TestUIOraclesWithNoTraceRecordsIsNotApplicable(t *testing.T) {
	dataDir := t.TempDir()
	e, _, _ := testEnv(t.TempDir())

	section, _ := e.oracleSection(dataDir, healthFileCursor{})
	if section.Status != healthNA {
		t.Fatalf("status = %q, want %q for a build with no trace at all", section.Status, healthNA)
	}
	if !strings.Contains(section.Detail, "UI_TRACE=1") {
		t.Fatalf("the detail does not name the build flag that arms it: %q", section.Detail)
	}
}

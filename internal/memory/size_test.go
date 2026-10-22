package memory

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// fill writes n facts of kind to dir, each description width characters of
// letter, named prefix-0000.md onward.
func fill(t *testing.T, dir, prefix, kind string, n, width int, letter string) {
	t.Helper()
	for i := range n {
		extra := []string{}
		if kind == "context" {
			extra = append(extra, "expires: 2026-12-31")
		}
		write(t, dir, fmt.Sprintf("%s-%04d.md", prefix, i), factText(kind, strings.Repeat(letter, width), extra...))
	}
}

// Memory over Limit is delivered whole: every fact and the whole
// instructions.md, opened by a notice of its size, and the assistant is told
// to shorten it. The notice names the size the delivery has without it.
func TestNothingIsCut(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	fill(t, s.project, "p", "ruling", 40, 280, "p")
	fill(t, s.plan.User, "u", "preference", 20, 280, "u")
	fill(t, HomeDir(s.plan.Home), "c", "context", 10, 280, "c")
	instructions := strings.Repeat("Role line.\n", 400)
	write(t, s.plan.Home, "instructions.md", instructions)
	for name, tc := range map[string]struct {
		plan      Plan
		facts     int
		assistant bool
	}{
		"assistant": {s.plan, 70, true},
		"thread":    {Plan{User: s.plan.User, Projects: s.plan.Projects}, 60, false},
	} {
		text := build(t, tc.plan)
		if got := strings.Count(text, strings.Repeat("p", 280)) + strings.Count(text, strings.Repeat("u", 280)) + strings.Count(text, strings.Repeat("c", 280)); got != tc.facts {
			t.Errorf("%s: %d facts delivered, want %d", name, got, tc.facts)
		}
		if got := strings.Contains(text, strings.TrimRight(instructions, "\n")); got != tc.assistant {
			t.Errorf("%s: instructions delivered = %v", name, got)
		}
		// The notice states the size of the stamped delivery, itself
		// included, which is the size the app reports.
		d := readDelivery(tc.plan, day)
		stamped := len(Stamp(text, day))
		size, over := Oversize(text)
		if !over || !d.over || d.size != stamped || size != stamped {
			t.Fatalf("%s: over %v/%v, size %d, notice size %d, stamped %d", name, over, d.over, size, d.size, stamped)
		}
		notice := Heading + "\n\n" + fmt.Sprintf("[This memory is %d bytes, more than the %d a hook's output reaches you whole. If you see only its start, read all of it from the file the provider saved it to before you act.]\n\n", stamped, Limit)
		if !strings.HasPrefix(text, notice) {
			t.Errorf("%s: no notice at the top:\n%s", name, text[:300])
		}
		if got := strings.Contains(text, "- Your memory is more than the 10000 bytes a hook's output reaches the model whole"); got != tc.assistant {
			t.Errorf("%s: maintenance warning = %v", name, got)
		}
	}
}

// Under Limit a delivery carries no notice and no size warning, and a
// delivery is over only past Limit.
func TestTheLimitIsTheStampedSize(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	text := build(t, s.plan)
	if _, over := Oversize(text); over || strings.Contains(text, "[This memory is ") || strings.Contains(text, "Your memory is more than") {
		t.Fatalf("small memory reads as over:\n%s", text)
	}
	if _, over := Oversize(strings.Repeat("x", Limit-stampSize)); over {
		t.Fatal("a delivery of exactly Limit is over")
	}
	if _, over := Oversize(strings.Repeat("x", Limit-stampSize+1)); !over {
		t.Fatal("a delivery past Limit is not over")
	}
}

// An instructions.md past what a delivery reads is named with its path and
// size, and the assistant is told to shorten it.
func TestLongInstructionsAreNamed(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	write(t, s.plan.Home, "instructions.md", strings.Repeat("x", instructionsKeep+1)+"\n")
	text := build(t, s.plan)
	path := filepath.Join(s.plan.Home, "instructions.md")
	if !strings.Contains(text, fmt.Sprintf("[instructions.md is %d bytes, more than a delivery carries. Read it from %s before you act.]", instructionsKeep+2, path)) ||
		!strings.Contains(text, fmt.Sprintf("- %s (%d bytes) is more than the %d a delivery carries, so it is named instead of shown. Shorten it.", path, instructionsKeep+2, instructionsKeep)) {
		t.Fatalf("delivered:\n%s", text)
	}
}

// Warnings about single files are bounded; the rest are counted.
func TestWarningsAreBounded(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	for i := range maxListedProblems + 7 {
		write(t, s.project, fmt.Sprintf("bad-%02d.md", i), "malformed\n")
	}
	warnings := block(t, build(t, s.plan), "### Memory maintenance")
	if got := strings.Count(warnings, "\n- "); got != maxListedProblems+1 || !strings.HasSuffix(warnings, "\n- And 7 more like these.") {
		t.Fatalf("%d lines:\n%s", got, warnings)
	}
}

// A project whose folder cannot be read is listed with why, not as empty.
func TestAProjectListNamesAnUnreadableFolder(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	broken := Project{Name: "broken", Path: "/Users/u/repos/broken", Dir: filepath.Join(filepath.Dir(s.project), "repos--broken")}
	write(t, filepath.Dir(s.project), "repos--broken", "not a folder\n")
	plan := Plan{User: s.plan.User, Home: s.plan.Home, Projects: append([]Project{broken}, s.plan.Projects...)}
	text, problems := Build(plan, day)
	if len(problems) != 1 || !strings.Contains(text, "- broken (/Users/u/repos/broken): ["+folderShown(broken.Dir)+" could not be read: ") || strings.Contains(text, "broken (/Users/u/repos/broken): "+folderShown(broken.Dir)+", 0 facts") {
		t.Fatalf("problems %v, delivered:\n%s", problems, text)
	}
}

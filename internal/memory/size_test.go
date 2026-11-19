package memory

import (
	"fmt"
	"path/filepath"
	"slices"
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

// Memory over Limit is delivered whole: each section that can grow is a
// hook output of its own, so only a section itself over Limit is over, and
// its part opens with a notice of its stamped size. The assistant's warnings
// name each such section and how to fix it; a thread gets none.
func TestNothingIsCut(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	fill(t, s.project, "p", "ruling", 40, 280, "p")
	fill(t, s.plan.User, "u", "preference", 20, 280, "u")
	fill(t, HomeDir(s.plan.Home), "c", "context", 10, 280, "c")
	instructions := strings.Repeat("Role line.\n", 1000)
	write(t, s.plan.Home, "instructions.md", instructions)
	over := func(what, fix string) string {
		return fmt.Sprintf("- %s is more than the %d bytes a hook's output reaches every provider's model whole, so a session may see only its start until it reads the file the provider saved it to. %s", what, Limit, fix)
	}
	for name, tc := range map[string]struct {
		plan     Plan
		facts    int
		over     []Part
		warnings []string
	}{
		"assistant": {s.plan, 70, []Part{PartInstructions, PartProject}, []string{
			over("Your "+filepath.Join(s.plan.Home, "instructions.md"), fixInstructions),
			over("The index of "+folderShown(s.project), fixIndex),
		}},
		"thread": {Plan{User: s.plan.User, Projects: s.plan.Projects}, 60, []Part{PartProject}, nil},
	} {
		outputs, problems := Build(tc.plan, day)
		if len(problems) > 0 {
			t.Fatalf("%s: problems %v", name, problems)
		}
		text := joined(outputs)
		if got := strings.Count(text, strings.Repeat("p", 280)) + strings.Count(text, strings.Repeat("u", 280)) + strings.Count(text, strings.Repeat("c", 280)); got != tc.facts {
			t.Errorf("%s: %d facts delivered, want %d", name, got, tc.facts)
		}
		if got := strings.Contains(text, strings.TrimRight(instructions, "\n")); got != (tc.plan.Home != "") {
			t.Errorf("%s: instructions delivered = %v", name, got)
		}
		// A part's size is its stamped size, the notice included, which is
		// what the provider receives and the app reports.
		var gotOver []Part
		for _, output := range outputs {
			if stamped := len(Stamp(output.Text, day)); output.Size != stamped {
				t.Errorf("%s: %s is %d bytes stamped, its size says %d", name, output.Part, stamped, output.Size)
			}
			if !output.Over() {
				if strings.Contains(output.Text, "[This part is ") {
					t.Errorf("%s: %s under Limit carries a notice", name, output.Part)
				}
				continue
			}
			gotOver = append(gotOver, output.Part)
			notice := fmt.Sprintf("[This part is %d bytes, more than the %d a hook's output reaches you whole. If you see only its start, read all of it from the file the provider saved it to before you act.]\n\n", output.Size, Limit)
			if !strings.HasPrefix(output.Text, tc.plan.HeadingOf(output.Part)+"\n\n"+notice) {
				t.Errorf("%s: %s has no notice at its top:\n%s", name, output.Part, output.Text[:300])
			}
		}
		if !slices.Equal(gotOver, tc.over) {
			t.Errorf("%s: parts over Limit %q, want %q", name, gotOver, tc.over)
		}
		if tc.warnings == nil {
			if strings.Contains(text, warningsHeading) {
				t.Errorf("%s: warned:\n%s", name, text)
			}
			continue
		}
		warnings := block(t, text, warningsHeading)
		for _, line := range tc.warnings {
			if !strings.Contains(warnings+"\n", line+"\n") {
				t.Errorf("%s: no warning %q in:\n%s", name, line, warnings)
			}
		}
		if got := strings.Count(warnings, "\n- "); got != len(tc.warnings) {
			t.Errorf("%s: %d warnings:\n%s", name, got, warnings)
		}
	}
}

// An instructions.md past what a delivery reads is named with its path and
// size, and the assistant is told how to fix it.
func TestLongInstructionsAreNamed(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	write(t, s.plan.Home, "instructions.md", strings.Repeat("x", instructionsKeep+1)+"\n")
	text := build(t, s.plan)
	path := filepath.Join(s.plan.Home, "instructions.md")
	if !strings.Contains(text, fmt.Sprintf("[instructions.md is %d bytes, more than a delivery carries. Read it from %s before you act.]", instructionsKeep+2, path)) ||
		!strings.Contains(text, fmt.Sprintf("- %s (%d bytes) is more than the %d a delivery carries, so it is named instead of shown. %s", path, instructionsKeep+2, instructionsKeep, fixInstructions)) {
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
	outputs, problems := Build(plan, day)
	text := joined(outputs)
	if len(problems) != 1 || !strings.Contains(text, "- broken (/Users/u/repos/broken): ["+folderShown(broken.Dir)+" could not be read: ") || strings.Contains(text, "broken (/Users/u/repos/broken): "+folderShown(broken.Dir)+", 0 facts") {
		t.Fatalf("problems %v, delivered:\n%s", problems, text)
	}
}

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

// stampedSize is a delivery's size as the provider counts it.
func stampedSize(text string) int { return utf16Len(Stamp(text, day)) }

// The size the cut works from is the size of what it renders, for every
// cut, in UTF-16 units: a character outside the Basic Multilingual Plane
// counts twice.
func TestTheCutMeasuresWhatItRenders(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	fill(t, s.project, "p", "ruling", 12, 40, "é")
	fill(t, s.project, "c", "context", 2, 10, "x")
	fill(t, s.plan.User, "u", "preference", 5, 30, "\U0001F600")
	fill(t, HomeDir(s.plan.Home), "h", "convention", 7, 20, "h")
	write(t, s.project, "bad.md", "malformed\n")
	write(t, s.plan.Home, "instructions.md", strings.Repeat("Role line.\n", 30))
	for _, plan := range []Plan{s.plan, {User: s.plan.User, Projects: s.plan.Projects}} {
		d := readDelivery(plan, day)
		for keepUser := range len(d.user.facts) + 1 {
			for _, keepProject := range []int{d.project.floor, d.project.floor + 3, len(d.project.facts)} {
				for _, out := range []bool{false, true} {
					d.user.keep, d.project.keep = keepUser, keepProject
					if d.instructions != nil {
						d.instructions.out = out
					}
					if d.home != nil {
						d.home.keep = len(d.home.facts) - keepUser%3
					}
					if got, want := d.size(), stampedSize(d.render()); got != want {
						t.Fatalf("user %d, project %d, out %v: size %d, rendered %d", keepUser, keepProject, out, got, want)
					}
				}
			}
		}
	}
}

// Over the budget, the longest index loses facts from its end, never its
// context facts, and says how many it left out; the assistant is told.
func TestTheLongestIndexIsCutFromItsEnd(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	fill(t, s.project, "a", "context", 3, 60, "c")
	fill(t, s.project, "b", "ruling", 60, 120, "r")
	fill(t, s.project, "z", "reference", 40, 120, "f")
	fill(t, s.plan.User, "u", "preference", 4, 40, "u")
	text := build(t, s.plan)
	if size := stampedSize(text); size > Budget {
		t.Fatalf("delivery is %d units", size)
	}
	index := block(t, text, "### Project memory")
	if strings.Contains(index, "References:") || strings.Count(index, strings.Repeat("c", 60)) != 3 {
		t.Fatalf("cut the wrong end:\n%s", index)
	}
	shown := strings.Count(index, strings.Repeat("r", 120))
	cut := 60 + 40 - shown
	if shown == 0 || !strings.HasSuffix(index, fmt.Sprintf("\n[%d more facts in %s]", cut, folderShown(s.project))) {
		t.Fatalf("index:\n%s", index)
	}
	if user := block(t, text, "### User memory"); strings.Count(user, strings.Repeat("u", 40)) != 4 {
		t.Fatalf("the shorter index was cut:\n%s", user)
	}
	if !strings.Contains(text, fmt.Sprintf("- The index of %s was cut to fit this delivery: %d facts are not shown.", folderShown(s.project), cut)) {
		t.Fatalf("no cut warning:\n%s", text)
	}
	// One fact more would not have fit.
	d := readDelivery(s.plan, day)
	d.fit()
	d.project.keep++
	if d.size() <= Budget {
		t.Fatalf("cut one fact more than it had to: %d", d.size())
	}
	// A thread in the project gets the same cut and no warning.
	thread := build(t, Plan{User: s.plan.User, Projects: s.plan.Projects})
	if stampedSize(thread) > Budget || strings.Contains(thread, "Memory maintenance") || !strings.Contains(thread, "more facts in "+folderShown(s.project)+"]") {
		t.Fatalf("thread:\n%s", thread)
	}
}

// Two long indexes are cut longest first, so they end about even.
func TestTheCutEvensOutLongIndexes(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	fill(t, s.project, "p", "ruling", 80, 100, "p")
	fill(t, HomeDir(s.plan.Home), "h", "ruling", 40, 100, "h")
	d := readDelivery(s.plan, day)
	d.fit()
	if d.size() > Budget {
		t.Fatalf("size %d", d.size())
	}
	if d.project.keep == len(d.project.facts) || d.home.keep == len(d.home.facts) {
		t.Fatalf("kept %d of %d and %d of %d", d.project.keep, len(d.project.facts), d.home.keep, len(d.home.facts))
	}
	// They differ by no more than one fact line.
	if diff := d.project.size(d.project.keep) - d.home.size(d.home.keep); diff > 120 || diff < -120 {
		t.Fatalf("project and home differ by %d units", diff)
	}
}

// instructions.md gives way to its path only when it would not fit with
// every index cut to its context facts.
func TestInstructionsGiveWayOnlyWhenNothingElseCan(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	path := filepath.Join(s.plan.Home, "instructions.md")
	fill(t, s.project, "p", "ruling", 40, 100, "p")
	write(t, s.plan.Home, "instructions.md", strings.Repeat("i", 5000)+"\n")
	text := build(t, s.plan)
	if !strings.Contains(text, strings.Repeat("i", 5000)) || stampedSize(text) > Budget {
		t.Fatalf("instructions gave way while facts could:\n%s", text)
	}
	write(t, s.plan.Home, "instructions.md", strings.Repeat("i", 8500)+"\n")
	text = build(t, s.plan)
	if strings.Contains(text, strings.Repeat("i", 100)) || stampedSize(text) > Budget {
		t.Fatalf("instructions kept:\n%s", text)
	}
	if !strings.Contains(text, "[instructions.md is 8501 bytes, more than fits beside your memory. Read it from "+path+" before you act.]") ||
		!strings.Contains(text, "- "+path+" (8501 bytes) did not fit in this delivery beside your memory and was left out. Shorten it.") {
		t.Fatalf("no path line or warning:\n%s", text)
	}
	// With instructions.md out, every fact fits again.
	if strings.Contains(text, "more facts in") {
		t.Fatalf("facts cut beside a path line:\n%s", text)
	}
	// Past the read bound it is never read whole.
	write(t, s.plan.Home, "instructions.md", strings.Repeat("i", instructionsKeep+1))
	if text := build(t, s.plan); !strings.Contains(text, fmt.Sprintf("[instructions.md is %d bytes", instructionsKeep+1)) {
		t.Fatalf("a long file:\n%s", text)
	}
}

// Context facts are never cut: a delivery they alone push over the budget
// is delivered whole and the assistant is told.
func TestContextFactsAreNeverCut(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	fill(t, s.project, "c", "context", 40, 280, "c")
	text := build(t, s.plan)
	if got := strings.Count(text, strings.Repeat("c", 280)); got != 40 {
		t.Fatalf("%d context facts delivered", got)
	}
	if !strings.Contains(text, "- This delivery is still about ") || !strings.Contains(text, " after every cut, over the 9000 it is held to.") {
		t.Fatalf("no warning:\n%s", text)
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

// An assistant's project list that would not fit with every index at its
// context facts loses projects from its end, says how many and where their
// folders are, and the assistant is told.
func TestTheProjectListIsCutFromItsEnd(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	plan := Plan{User: s.plan.User, Home: s.plan.Home}
	for i := range 80 {
		path := fmt.Sprintf("/Users/u/repos/%s-%02d", strings.Repeat("p", 60), i)
		dir, err := ProjectDir(filepath.Dir(filepath.Dir(s.plan.Home)), "/Users/u", path)
		if err != nil {
			t.Fatal(err)
		}
		plan.Projects = append(plan.Projects, Project{Name: fmt.Sprintf("project %02d", i), Path: path, Dir: dir})
	}
	fill(t, HomeDir(s.plan.Home), "h", "ruling", 10, 100, "h")
	text := build(t, plan)
	if size := stampedSize(text); size > Budget {
		t.Fatalf("delivery is %d units", size)
	}
	list := block(t, text, "### Your projects")
	shown := strings.Count(list, "\n- ")
	folders := folderShown(filepath.Dir(plan.Projects[0].Dir))
	if shown == 0 || shown == 80 || !strings.Contains(list, "\n- project 00 (") ||
		!strings.HasSuffix(list, fmt.Sprintf("\n[%d more projects; their memory folders are in %s]", 80-shown, folders)) {
		t.Fatalf("list:\n%s", list)
	}
	if !strings.Contains(text, fmt.Sprintf("- The list of your projects was cut to fit this delivery: %d projects are not listed.", 80-shown)) {
		t.Fatalf("no cut warning:\n%s", text)
	}
	// The list gives way to the home's facts only as far as their context
	// floor needs: the home keeps its rulings only where room is left.
	d := readDelivery(plan, day)
	d.fit()
	d.projects.keep++
	if d.size() <= Budget && d.home.keep == len(d.home.facts) {
		t.Fatalf("cut one project more than it had to: %d", d.size())
	}
	for keep := range []int{0, 1, len(d.projects.lines) - 1, len(d.projects.lines)} {
		d.projects.keep = keep
		if got, want := d.size(), stampedSize(d.render()); got != want {
			t.Fatalf("keep %d: size %d, rendered %d", keep, got, want)
		}
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

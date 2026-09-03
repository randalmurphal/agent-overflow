package main

import (
	"os"
	"strings"
	"testing"
)

// The `nogui` tag selects a whole alternative file set — this package's
// main_nogui.go instead of main_desktop.go, internal/app's windowless startup
// branch, and the absence of every !nogui desktop file. Two SHIPPING artifacts
// are built with it: the Windows launcher's WSL payload
// (build/windows/Taskfile.yml) and the headless Linux binary serve mode runs
// on (scripts/build-release.sh).
//
// Nothing else in the default gates compiles that half. Without the pass this
// test guards, a !nogui file gaining a symbol the nogui half does not define
// compiles clean all the way to release day, and breaks the two artifacts
// nobody builds until then.
//
// This asserts the RECIPE, not the outcome — a test cannot usefully shell out
// to a second full build of the tree. Removing the pass for speed is the
// plausible regression, and it fails here.
func TestGoBuildCompilesTheNoguiHalf(t *testing.T) {
	recipe := makefileRecipe(t, "go-build")
	if !strings.Contains(recipe, "-tags nogui") {
		t.Fatalf("the go-build recipe no longer compiles the nogui half:\n%s", recipe)
	}
	if !strings.Contains(recipe, "go list -tags nogui") {
		t.Errorf("the nogui pass must resolve its own package list: a package can exist "+
			"under one tag set and not the other.\n%s", recipe)
	}
}

// The headless artifact is the reason serve mode installs on a machine with no
// desktop session, and the reason the ops doc can promise one. It is built with
// the same tag set as the WSL payload deliberately: that binary IS this binary.
func TestReleaseScriptBuildsTheHeadlessArtifact(t *testing.T) {
	body, err := os.ReadFile("scripts/build-release.sh")
	if err != nil {
		t.Fatalf("read scripts/build-release.sh: %v", err)
	}
	script := string(body)

	for _, want := range []string{
		"agent-overflow-headless-linux-amd64", // the published artifact name
		"-tags production,nogui",              // the same tag set as the WSL payload
		"build_headless_linux",                // built here, not copied out of the WSL leg
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scripts/build-release.sh no longer contains %q", want)
		}
	}

	// It must be validated like every other Linux artifact: an unbuilt or
	// placeholder file is an ELF check away from being caught, and shipping
	// one is worse than shipping nothing.
	if !strings.Contains(script, `validate_elf "$ROOT_DIR/bin/agent-overflow-headless"`) {
		t.Error("the headless artifact is not run through validate_elf")
	}
}

// makefileRecipe returns the body of one Makefile target: every line after
// `<target>:` up to the next line that starts in column zero.
func makefileRecipe(t *testing.T, target string) string {
	t.Helper()
	body, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	lines := strings.Split(string(body), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, target+":") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("Makefile has no %q target", target)
	}
	var recipe []string
	for _, line := range lines[start:] {
		if line != "" && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			break
		}
		recipe = append(recipe, line)
	}
	if len(recipe) == 0 {
		t.Fatalf("the %q target has an empty recipe", target)
	}
	return strings.Join(recipe, "\n")
}

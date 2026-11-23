package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/bundle"
)

func TestTheEmbeddedDeclarationParses(t *testing.T) {
	in, err := parseRunInputs(runInputsMk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(in.binaries(), " "), "bin/ao-store-test") {
		t.Fatalf("binaries %v lack the store test binary", in.binaries())
	}
}

func TestBrowsersFollowTheRunsConfig(t *testing.T) {
	in := runInputs{
		"E2E_BROWSERS": {"chromium", "webkit"}, "E2E_BROWSERS_MANUAL": {"shell"},
		"E2E_BROWSERS_FLOW": {"flow"}, "E2E_BROWSERS_ANDROID": {"android"},
	}
	for _, tc := range []struct {
		mode runMode
		args []string
		want []string
	}{
		{modeTests, []string{"--shard=1/8"}, []string{"chromium", "webkit"}},
		{modeTests, []string{"--config=playwright.manual.config.ts", "tests/x.spec.ts"}, []string{"shell"}},
		{modeTests, []string{"-c", "playwright.android.config.ts"}, []string{"android"}},
		{modeFreeze, nil, []string{"shell"}},
		{modeFlow, []string{"--headed"}, []string{"flow"}},
	} {
		got, err := in.browsersFor(tc.mode, tc.args)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("browsersFor(%v, %v) = %v, %v; want %v", tc.mode, tc.args, got, err, tc.want)
		}
	}
	if _, err := in.browsersFor(modeTests, []string{"--config=other.config.ts"}); err == nil {
		t.Fatal("a config the declaration does not name got browsers")
	}
}

func TestParseDryRunReadsEachInstallLocation(t *testing.T) {
	out := "Chrome for Testing 156 (playwright chromium v1248)\n  Install location:    /pw/chromium-1248\n  Download url:        https://x\n\n" +
		"WebKit 27.2 (playwright webkit v2370)\n  Install location:    /pw/webkit-2370\n"
	got, err := parseDryRun([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []browserInstall{
		{"Chrome for Testing 156 (playwright chromium v1248)", "/pw/chromium-1248"},
		{"WebKit 27.2 (playwright webkit v2370)", "/pw/webkit-2370"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDryRun = %v, want %v", got, want)
	}
	if _, err := parseDryRun([]byte("nothing\n")); err == nil {
		t.Fatal("output without an install location parsed")
	}
}

// harnessRoot lays out what harness-build leaves: the declared binaries,
// frontend/dist and the record of its id.
func harnessRoot(t *testing.T, in runInputs) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range in.binaries() {
		writeFile(t, filepath.Join(root, rel), "#!/bin/sh\n", 0o755)
	}
	dist := filepath.Join(root, "frontend", "dist")
	writeFile(t, filepath.Join(dist, "index.html"), "<html></html>", 0o644)
	manifest, err := bundle.New(os.DirFS(dist), "").Manifest()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, bundleRecord), manifest.ID+"\n", 0o644)
	return root
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightNamesEachMissingInput(t *testing.T) {
	in, err := parseRunInputs(runInputsMk)
	if err != nil {
		t.Fatal(err)
	}
	browsers := t.TempDir()
	installed := filepath.Join(browsers, "chromium-1")
	writeFile(t, filepath.Join(installed, "INSTALLATION_COMPLETE"), "", 0o644)
	probe := func(_ string, names []string) ([]browserInstall, error) {
		return []browserInstall{{"Chromium", installed}, {"Chrome for Testing (playwright chromium)", filepath.Join(browsers, "chrome-2")}}, nil
	}
	opts := launchOptions{mode: modeTests}

	root := harnessRoot(t, in)
	err = preflight(root, filepath.Join(root, "e2e"), opts, in, probe)
	if err == nil || !strings.Contains(err.Error(), "Chrome for Testing (playwright chromium): not installed") {
		t.Fatalf("preflight with a browser missing = %v", err)
	}
	if strings.Contains(err.Error(), "frontend/dist") || strings.Contains(err.Error(), "bin/") {
		t.Fatalf("preflight named an input that is there: %v", err)
	}

	allThere := func(string, []string) ([]browserInstall, error) {
		return []browserInstall{{"Chromium", installed}}, nil
	}
	if err := preflight(root, filepath.Join(root, "e2e"), opts, in, allThere); err != nil {
		t.Fatalf("preflight with every input = %v", err)
	}

	// A shard that unpacked bin/ alone, and a dist rebuilt after the binaries.
	if err := os.Remove(filepath.Join(root, "bin", "ao-mockforge")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "frontend", "dist", "index.html"), "<html>rebuilt</html>", 0o644)
	err = preflight(root, filepath.Join(root, "e2e"), opts, in, allThere)
	if err == nil || !strings.Contains(err.Error(), "bin/ao-mockforge: not an executable file") || !strings.Contains(err.Error(), "but the binaries embed") {
		t.Fatalf("preflight = %v, want the missing binary and the mismatched bundle named", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "frontend", "dist")); err != nil {
		t.Fatal(err)
	}
	if err := preflight(root, filepath.Join(root, "e2e"), opts, in, allThere); err == nil || !strings.Contains(err.Error(), "frontend/dist:") {
		t.Fatalf("preflight without frontend/dist = %v", err)
	}
}

func TestTheTypecheckDigestFollowsTheSuite(t *testing.T) {
	root := t.TempDir()
	git := exec.Command("git", "init", "-q", root)
	git.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(root, ".gitignore"), "node_modules/\ntest-results/\n", 0o644)
	writeFile(t, filepath.Join(root, "e2e", "tests", "a.spec.ts"), "a", 0o644)
	writeFile(t, filepath.Join(root, "frontend", "src", "shared.ts"), "shared", 0o644)
	listed := []string{"frontend/src/shared.ts"}
	digest := func() string {
		t.Helper()
		d, err := suiteDigest(root, listed)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	base := digest()

	// Output under ignored directories does not count.
	writeFile(t, filepath.Join(root, "e2e", "test-results", "trace.zip"), "x", 0o644)
	if digest() != base {
		t.Fatal("a file under an ignored directory changed the digest")
	}
	for name, change := range map[string]func(){
		"an edited spec":            func() { writeFile(t, filepath.Join(root, "e2e", "tests", "a.spec.ts"), "b", 0o644) },
		"a new spec, not yet added": func() { writeFile(t, filepath.Join(root, "e2e", "tests", "c.spec.ts"), "c", 0o644) },
		"an edited file tsc read":   func() { writeFile(t, filepath.Join(root, "frontend", "src", "shared.ts"), "edited", 0o644) },
		"a removed file tsc read":   func() { os.Remove(filepath.Join(root, "frontend", "src", "shared.ts")) },
	} {
		before := digest()
		change()
		if digest() == before {
			t.Fatalf("%s left the digest unchanged", name)
		}
	}
}

func TestARecordedCheckIsSkippedOnlyWhileTheSuiteIsUnchanged(t *testing.T) {
	root := t.TempDir()
	git := exec.Command("git", "init", "-q", root)
	git.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	testDir := filepath.Join(root, "e2e")
	writeFile(t, filepath.Join(testDir, "tests", "a.spec.ts"), "a", 0o644)
	digest, err := suiteDigest(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, typecheckRecord), `{"digest":"`+digest+`","files":[]}`, 0o644)
	var out strings.Builder
	// No tsc can run in this directory: a skipped check is the only pass.
	if err := ensureTypechecked(root, testDir, &out); err != nil || !strings.Contains(out.String(), "still holds") {
		t.Fatalf("ensureTypechecked over the recorded suite = %v, %q", err, out.String())
	}
	writeFile(t, filepath.Join(testDir, "tests", "a.spec.ts"), "edited", 0o644)
	if err := ensureTypechecked(root, testDir, &out); err == nil {
		t.Fatal("an edited suite was not checked again")
	}
}

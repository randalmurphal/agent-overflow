package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agent-overflow/internal/bundle"
)

// runInputsMk declares what a run reads besides the suite: run-inputs.mk,
// which the Makefile, the CI image and CI's packing read too.
//
//go:embed run-inputs.mk
var runInputsMk string

// bundleRecord is where harness-build records the id of the frontend/dist
// its binaries embed. Specs serve frontend/dist itself too, so a run needs
// that very tree beside the binaries.
const bundleRecord = "bin/e2e-bundle-id"

// runInputs is the parsed declaration: each NAME := words line.
type runInputs map[string][]string

func parseRunInputs(text string) (runInputs, error) {
	vars := runInputs{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":=")
		if !ok {
			return nil, fmt.Errorf("run-inputs.mk:%d: %q is not a NAME := words line", i+1, line)
		}
		vars[strings.TrimSpace(name)] = strings.Fields(value)
	}
	for _, name := range []string{"E2E_COMMANDS", "E2E_TEST_PACKAGES", "E2E_BROWSERS", "E2E_BROWSERS_MANUAL", "E2E_BROWSERS_FLOW", "E2E_BROWSERS_ANDROID"} {
		if len(vars[name]) == 0 {
			return nil, fmt.Errorf("run-inputs.mk sets no %s", name)
		}
	}
	return vars, nil
}

// binaries are the files harness-build puts in bin/ that a run starts.
func (in runInputs) binaries() []string {
	var paths []string
	for _, name := range in["E2E_COMMANDS"] {
		paths = append(paths, "bin/"+name)
	}
	for _, pkg := range in["E2E_TEST_PACKAGES"] {
		paths = append(paths, "bin/ao-"+filepath.Base(pkg)+"-test")
	}
	return paths
}

// browsersFor names the Playwright browsers the run's config launches.
func (in runInputs) browsersFor(mode runMode, args []string) ([]string, error) {
	switch mode {
	case modeFlow:
		return in["E2E_BROWSERS_FLOW"], nil
	case modeFreeze:
		return in["E2E_BROWSERS_MANUAL"], nil
	}
	config := "playwright.config.ts"
	for i, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--config="):
			config = strings.TrimPrefix(arg, "--config=")
		case (arg == "--config" || arg == "-c") && i+1 < len(args):
			config = args[i+1]
		}
	}
	switch filepath.Base(config) {
	case "playwright.config.ts":
		return in["E2E_BROWSERS"], nil
	case "playwright.manual.config.ts":
		return in["E2E_BROWSERS_MANUAL"], nil
	case "playwright.android.config.ts":
		return in["E2E_BROWSERS_ANDROID"], nil
	}
	return nil, fmt.Errorf("cmd/ao-harness-e2e/run-inputs.mk declares no browsers for %s; add them there", config)
}

// browserInstall is one install Playwright would make for a browser name.
type browserInstall struct {
	title, dir string
}

// probeBrowsers asks Playwright where each named browser lives, without
// installing anything.
type probeBrowsers func(testDir string, names []string) ([]browserInstall, error)

func playwrightInstalls(testDir string, names []string) ([]browserInstall, error) {
	cmd := exec.Command("pnpm", append([]string{"exec", "playwright", "install", "--dry-run"}, names...)...)
	cmd.Dir = testDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ask Playwright where %s are installed (pnpm exec playwright install --dry-run): %v\n%s", strings.Join(names, " "), err, stderr.String())
	}
	return parseDryRun(out)
}

// parseDryRun reads `playwright install --dry-run`: a title line per
// install, then an indented "Install location:" line.
func parseDryRun(out []byte) ([]browserInstall, error) {
	var installs []browserInstall
	title := ""
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" && !strings.HasPrefix(line, " ") {
			title = line
			continue
		}
		if dir, ok := strings.CutPrefix(strings.TrimSpace(line), "Install location:"); ok {
			installs = append(installs, browserInstall{title: title, dir: strings.TrimSpace(dir)})
		}
	}
	if len(installs) == 0 {
		return nil, fmt.Errorf("playwright install --dry-run named no install location:\n%s", out)
	}
	return installs, nil
}

// preflight fails, naming each one, while an input the run needs is
// missing: a binary, the frontend/dist those binaries embed, or a browser
// the config launches. Without it the omission surfaces only as spec
// failures, minutes in.
func preflight(root, testDir string, opts launchOptions, in runInputs, probe probeBrowsers) error {
	var missing []string
	for _, rel := range in.binaries() {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			missing = append(missing, fmt.Sprintf("%s: not an executable file (make harness-build builds it)", rel))
		}
	}
	if problem := checkBundle(root); problem != "" {
		missing = append(missing, problem)
	}
	names, err := in.browsersFor(opts.mode, opts.args)
	if err != nil {
		return err
	}
	installs, err := probe(testDir, names)
	if err != nil {
		return err
	}
	for _, install := range installs {
		if install.dir == "<system>" {
			continue
		}
		if _, err := os.Stat(filepath.Join(install.dir, "INSTALLATION_COMPLETE")); err != nil {
			missing = append(missing, fmt.Sprintf("%s: not installed in %s (cd e2e && pnpm exec playwright install %s)", install.title, install.dir, strings.Join(names, " ")))
		}
	}
	if len(missing) > 0 {
		return errors.New("this run lacks inputs that cmd/ao-harness-e2e/run-inputs.mk declares:\n  " + strings.Join(missing, "\n  "))
	}
	return nil
}

// checkBundle answers what is wrong with frontend/dist, or "" when it is
// the tree harness-build recorded.
func checkBundle(root string) string {
	recorded, err := os.ReadFile(filepath.Join(root, bundleRecord))
	if err != nil {
		return fmt.Sprintf("%s: %v (make harness-build records it)", bundleRecord, err)
	}
	dist := filepath.Join(root, "frontend", "dist")
	manifest, err := bundle.New(os.DirFS(dist), "").Manifest()
	if err != nil {
		return fmt.Sprintf("frontend/dist: %v (make harness-build builds it with the binaries)", err)
	}
	if want := strings.TrimSpace(string(recorded)); manifest.ID != want {
		return fmt.Sprintf("frontend/dist: bundle %s, but the binaries embed %s; rebuild both with make harness-build", manifest.ID, want)
	}
	return ""
}

package browser

import (
	"fmt"
	"strings"
)

// Finding the headless engine's Chromium.
//
// **No engine is ever downloaded** (spec docs/specs/embedded-browser.md §9,
// and the rule this package's AGENTS.md carries): the serve deployment uses
// a Chromium the operator installed, or it has no browser tools at all. So
// discovery is a lookup and nothing else, and every failure it can produce
// names the one setting that fixes it.
//
// Pure and seam-driven — the PATH lookup and the platform are parameters —
// because the whole point of this file is that `go test` proves the table
// on every platform without a browser anywhere on the machine.

// chromiumSettingKey is the host-tier setting an operator names their own
// Chromium with. It is spelled once, and every error below carries it,
// because a message that says "not found" without saying where to say
// otherwise leaves the reader nowhere to go.
const chromiumSettingKey = "browserChromiumPath"

// chromiumProgramNames are the PATH names a system Chromium ships under,
// most-specific first: the Chromium builds before the Chrome ones, since an
// operator who installed both on a serve host installed Chromium ON PURPOSE.
var chromiumProgramNames = []string{
	"chromium",
	"chromium-browser",
	"google-chrome",
	"google-chrome-stable",
	"chrome",
}

// darwinChromiumBundles are the app-bundle executables a Mac keeps its
// browsers in. macOS installs put nothing on PATH, so a Mac with Chrome in
// /Applications would otherwise report no browser at all.
var darwinChromiumBundles = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
}

// findChromium resolves the browser the headless engine runs, or says why it
// cannot.
//
// The override is tried FIRST and, when it fails, is the whole answer: an
// operator who named a binary and got a different one silently would be
// debugging the wrong browser. Falling back there is the bug, not the
// robustness.
//
// lookPath is exec.LookPath in production. It answers both questions at
// once — does the file exist, and can this process execute it — for a bare
// program name and for a path alike, which is why the override and the
// bundles go through it too.
//
// The sentences are unprefixed on purpose: the caller frames them (the boot
// log says "browser tools off: ...", a tool error says "browser: ..."), and
// a doubled prefix reads like two failures.
func findChromium(override string, lookPath func(string) (string, error), goos string) (string, error) {
	if named := strings.TrimSpace(override); named != "" {
		resolved, err := lookPath(named)
		if err != nil {
			return "", fmt.Errorf("%s names %q, which this machine cannot execute: %w", chromiumSettingKey, named, err)
		}
		return resolved, nil
	}
	for _, name := range chromiumProgramNames {
		if resolved, err := lookPath(name); err == nil {
			return resolved, nil
		}
	}
	if goos == "darwin" {
		for _, bundle := range darwinChromiumBundles {
			if resolved, err := lookPath(bundle); err == nil {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("no Chromium found on this machine (set %s or install chromium)", chromiumSettingKey)
}

// validateChromiumBinary re-checks at engine start what discovery resolved at
// selection.
//
// Not redundant: selection runs once at boot and a serve host stays up for
// weeks, so the browser can be upgraded out from under it or the override
// edited to a path that does not exist. A tool error naming the file and the
// setting beats chromedp's own "chrome failed to start" naming neither.
func validateChromiumBinary(path string, lookPath func(string) (string, error)) error {
	if _, err := lookPath(path); err != nil {
		return fmt.Errorf("Chromium is not executable at %q; set %s to a Chromium binary or install one: %w", path, chromiumSettingKey, err)
	}
	return nil
}

// tailOf keeps the LAST limit bytes of a message.
//
// A failed launch's diagnosis is its final lines — the sandbox refusal, the
// missing library, the abort — while everything before them is startup
// chatter the process chose the length of. Bounding from the front would
// keep exactly the part nobody needs and let a noisy binary decide how much
// memory an error costs.
func tailOf(message string, limit int) string {
	if len(message) <= limit {
		return message
	}
	return "..." + message[len(message)-limit:]
}

package webview2host

import (
	"os"
	"sort"
	"strings"
)

// EnvOverrideNames are the process environment variables WebView2 lets
// override the corresponding CreateCoreWebView2EnvironmentWithOptions
// arguments.
//
// This list is load-bearing, and the failure it prevents is silent. A
// variable that is SET BUT EMPTY still counts as an override: with
// WEBVIEW2_USER_DATA_FOLDER="" exported, EVERY environment in the process
// collapses onto the default <exe>.WebView2\EBWebView folder, one browser
// process, one shared profile — the pane reads the SPA's cookies and
// localStorage, CreateCoreWebView2EnvironmentWithOptions still returns
// S_OK, and the completion handler still reports hr=0. There is no error
// anywhere. (Measured on this developer's machine during the 2026-08-31
// spike, which exports all four empty or zero; it cost most of that
// spike. Evidence: /tmp/spike-webview2-dual/VERDICTS.md item 2.)
//
// The launcher inherits whatever the user's shell exports, so the scrub
// runs in the launcher process before ANY environment is created — the
// SPA's included, since a collapsed folder is equally wrong from that
// side.
var EnvOverrideNames = []string{
	"WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS",
	"WEBVIEW2_BROWSER_EXECUTABLE_FOLDER",
	"WEBVIEW2_RELEASE_CHANNEL_PREFERENCE",
	"WEBVIEW2_USER_DATA_FOLDER",
}

// ScrubEnvOverrides unsets every name in EnvOverrideNames and returns the
// ones that were present, sorted, for logging. Presence is decided with
// LookupEnv, not by comparing to "": an empty value is exactly the case
// that overrides silently.
func ScrubEnvOverrides() []string {
	var removed []string
	for _, name := range EnvOverrideNames {
		value, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		// The value goes into launcher.log, and it came from the user's
		// shell: strip control characters so it cannot forge a log line.
		removed = append(removed, name+"="+strings.Map(printableOnly, value))
		_ = os.Unsetenv(name)
	}
	sort.Strings(removed)
	return removed
}

// FormatScrub renders ScrubEnvOverrides' result for launcher.log.
func FormatScrub(removed []string) string {
	if len(removed) == 0 {
		return "none"
	}
	return strings.Join(removed, " ")
}

func printableOnly(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return '?'
	}
	return r
}

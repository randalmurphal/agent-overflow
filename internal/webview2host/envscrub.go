package webview2host

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// EnvOverrideNames are the process environment variables WebView2 lets
// override the corresponding CreateCoreWebView2EnvironmentWithOptions
// arguments.
//
// The failure this list prevents is silent. A variable that is set but
// empty still counts as an override: with WEBVIEW2_USER_DATA_FOLDER=""
// exported, every environment in the process collapses onto the default
// <exe>.WebView2\EBWebView folder, one browser process and one shared
// profile, so the pane reads the SPA's cookies and localStorage, while
// CreateCoreWebView2EnvironmentWithOptions returns S_OK and the completion
// handler reports hr=0.
//
// The launcher inherits whatever the user's shell exports, so the scrub
// runs in the launcher process before any environment is created, the
// SPA's included, since a collapsed folder is equally wrong from that
// side.
var EnvOverrideNames = []string{
	"WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS",
	"WEBVIEW2_BROWSER_EXECUTABLE_FOLDER",
	"WEBVIEW2_RELEASE_CHANNEL_PREFERENCE",
	"WEBVIEW2_USER_DATA_FOLDER",
}

// ScrubEnvOverrides unsets every name in EnvOverrideNames and returns the
// ones that were present and removed, sorted, for logging, and an error
// naming each one it could not unset. Presence is decided with LookupEnv,
// not by comparing to "": an empty value is exactly the case that
// overrides silently.
func ScrubEnvOverrides() ([]string, error) {
	var removed []string
	var failed []error
	for _, name := range EnvOverrideNames {
		value, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		// The value goes into launcher.log, and it came from the user's
		// shell: strip control characters so it cannot forge a log line.
		if err := os.Unsetenv(name); err != nil {
			failed = append(failed, fmt.Errorf("unset %s: %w", name, err))
			continue
		}
		removed = append(removed, name+"="+strings.Map(printableOnly, value))
	}
	sort.Strings(removed)
	return removed, errors.Join(failed...)
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

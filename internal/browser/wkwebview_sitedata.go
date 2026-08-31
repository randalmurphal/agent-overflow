package browser

import (
	"fmt"
	"strings"
)

// The WKWebView engine's site-data clear, tag-free half — same reason
// wkwebview_identity.go is tag-free: the darwin file that produces this input
// compiles nowhere but a Mac, and the failure mode here is a silent lie about
// whether the user's cookies are actually gone. WebKit answers the removal once
// per data store, so folding those answers into the ONE sentence a Settings
// error shows is a rule worth compiling and testing on every platform.

// wkClearFailureLimit bounds how many distinct reasons one clear failure names.
// A container WebKit cannot write answers the same sentence once per workspace
// the user has ever opened, and a toast is one line a human reads, not a
// transcript.
const wkClearFailureLimit = 3

// wkClearSiteDataFailure folds the per-store removal failures the Objective-C
// half joined with newlines into one error, or nil when nothing failed.
//
// Blank is SUCCESS, and that covers the two cases that are not failures at all:
// every store removed, and WebKit reporting no data stores to remove (a macOS
// 11-13 Mac, which only ever had non-persistent stores, or one that has not
// persisted any site data yet).
func wkClearSiteDataFailure(reported string) error {
	if strings.TrimSpace(reported) == "" {
		return nil
	}
	var reasons []string
	seen := make(map[string]struct{})
	extra := 0
	for _, line := range strings.Split(reported, "\n") {
		reason := strings.TrimSpace(line)
		if reason == "" {
			continue
		}
		if _, repeated := seen[reason]; repeated {
			continue
		}
		seen[reason] = struct{}{}
		if len(reasons) == wkClearFailureLimit {
			extra++
			continue
		}
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 {
		return nil
	}
	message := strings.Join(reasons, "; ")
	if extra > 0 {
		message = fmt.Sprintf("%s (and %d more)", message, extra)
	}
	return fmt.Errorf("browser: clear macOS site data: %s", message)
}

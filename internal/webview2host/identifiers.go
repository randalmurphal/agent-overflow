package webview2host

import "fmt"

const (
	// maxPageIDLen bounds a page handle. Page ids are opaque to the
	// launcher: it uses them as map keys and echoes them back in reports.
	maxPageIDLen = 128
	// maxProfileIDLen is WebView2's own limit on a CoreWebView2Profile
	// name (64 characters excluding the terminator).
	maxProfileIDLen = 64
)

// ValidatePageID accepts only [A-Za-z0-9_-]. The character class is
// deliberately narrower than anything a page id needs to express, because
// the id reaches log lines and the report RPC and there is no reason for
// it to be able to carry a newline, a quote or a path separator.
func ValidatePageID(id string) error {
	return validateToken("page id", id, maxPageIDLen)
}

// ValidateProfileID accepts the same class, bounded at WebView2's own
// profile-name limit.
//
// WebView2 additionally allows '#', '@', '$', '(', ')', '+', '~', '.' and
// space in a CoreWebView2Profile name, and maps the name onto a real
// directory under the user-data folder case-insensitively. Restricting to
// this subset makes every accepted id a legal profile name AND a legal
// directory component on any filesystem, with no trailing-dot or
// trailing-space rule to remember and no case-folding collision to
// reason about. The backend hands out workspace hashes, which already
// fit.
func ValidateProfileID(id string) error {
	return validateToken("profile id", id, maxProfileIDLen)
}

func validateToken(what, id string, maxLen int) error {
	if id == "" {
		return fmt.Errorf("%s is required", what)
	}
	if len(id) > maxLen {
		return fmt.Errorf("%s is %d bytes, over the %d limit", what, len(id), maxLen)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return fmt.Errorf("%s %q has a disallowed byte at offset %d", what, id, i)
		}
	}
	return nil
}

// The pane environment's user-data folder is named by
// appidentity.BrowserProfilesDir and created beside the SPA webview's own
// directory under the launcher's data root. ONE folder for the whole pane
// environment: per-workspace isolation is a named CoreWebView2Profile
// inside it, not a folder per workspace, so every pane shares one browser
// process and one debugging port. Host.Config.UserDataDir carries the
// resolved path; this package never derives it, so there is exactly one
// place that decides which instance writes where.

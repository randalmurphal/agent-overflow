package externalurl

import "strings"

// deniedSchemes are the URL schemes an opener never hands to the OS: script
// and document URLs the host browser would evaluate, and Windows protocol
// handlers with remote-code-execution history. Every other registered scheme
// (mailto, tel, vscode, obsidian, slack, ...) is the user's to open.
//
// The frontend renders links from the same list,
// frontend/src/lib/markdown/render/elements/urlSchemes.ts, and
// TestDeniedSchemesMatchFrontend keeps the two identical.
var deniedSchemes = map[string]struct{}{
	"javascript":   {},
	"vbscript":     {},
	"data":         {},
	"blob":         {},
	"about":        {},
	"jar":          {},
	"ms-msdt":      {},
	"search-ms":    {},
	"ms-officecmd": {},
	"ms-cxh":       {},
	"ms-cxh-full":  {},
	// The app's own scheme: real path links never leave the webview.
	"agent-overflow": {},
}

// fileScheme is refused by the opener for its own reason: on Windows the
// shell opener executes a file URL's target. Files open through the editor
// gate (internal/editor.ResolvePath), never through here.
const fileScheme = "file"

// SchemeOpenable reports whether a URL with this scheme may be handed to
// the OS opener. A one-letter scheme is a Windows drive path, not a URL.
func SchemeOpenable(scheme string) bool {
	scheme = strings.ToLower(scheme)
	if len(scheme) <= 1 || scheme == fileScheme {
		return false
	}
	_, denied := deniedSchemes[scheme]
	return !denied
}

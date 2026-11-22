// Package weborigin normalizes the origin of an HTTP(S) URL, which the
// desktop windows and the Windows launcher compare to decide whether a web
// message came from the app's own document. Each caller keeps its own rule
// for which origins it accepts.
package weborigin

import (
	"net/url"
	"strings"
)

// Of answers raw's origin, scheme://host[:port] in lower case, and false
// for a URL that does not parse, has no host, or is not http or https.
func Of(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host), true
}

package forgeapi

import (
	"net/url"
	"strings"
)

// RedactURL renders u for an error or log line: no user info, and no query
// unless u is on one of forgeHosts (a forge API query is paging and
// filters; any other host's query, such as a signed blob URL's, is a
// credential in all but name). The path is kept.
func RedactURL(u *url.URL, forgeHosts ...string) string {
	if u == nil {
		return ""
	}
	out := *u
	out.User = nil
	out.Fragment = ""
	out.RawFragment = ""
	keep := false
	for _, host := range forgeHosts {
		if host != "" && strings.EqualFold(host, u.Host) {
			keep = true
			break
		}
	}
	if !keep {
		out.RawQuery = ""
		out.ForceQuery = false
	}
	return out.String()
}

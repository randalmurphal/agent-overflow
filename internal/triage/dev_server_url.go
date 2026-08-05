package triage

import (
	"net/netip"
	"strings"
)

// Dev-server detection: find the loopback URL a dev server prints on
// startup ("Local: http://localhost:5173") so a command row can offer an
// "open in browser" affordance without the user expanding the output.
//
// This scan is a CANDIDATE generator, not proof a server exists —
// output that merely mentions a loopback URL (a `tail` of a file
// containing one) matches identically. The frontend gates the chip on
// ProbeDevServerURL (internal/devserverprobe), which confirms a
// listener on the port; textual filters here only need to pick the
// right candidate, never to prove liveness.
//
// This runs inside ExtractCommandOutputMeta, which already makes one full
// pass over the same bytes (strings.Count for lineCount). The scan below
// is a second single pass anchored on strings.Index(…, "://") — no regex,
// no allocation until a match is found, and it stops at the FIRST
// qualifying URL, so the common case (a banner in the first few hundred
// bytes) exits almost immediately. Non-matching output costs one
// substring search over the window, which is cheaper than the line count
// already being taken.

// maxDevServerURLBytes caps the URL we are willing to hand to the browser.
// Anything longer is a data blob that happened to contain "http://", not a
// dev-server banner.
const maxDevServerURLBytes = 2048

// devServerURLEmbedPrefixes are the bytes that, immediately before the
// scheme, mean the URL is a *reference* rather than an announcement: a
// markdown/parenthesised link, a JS stack frame, a quoted string in JSON
// or an error message, or an env-var assignment. A dev server's own
// startup banner never puts one of these directly against the scheme.
const devServerURLEmbedPrefixes = "([{<\"'`="

// DetectDevServerURL returns the first loopback HTTP(S) URL in command
// output that looks like a running local server, normalized for the
// browser (0.0.0.0 / :: rewritten to localhost, IPv6 bracketed). It
// returns "" when the output announces no such server.
func DetectDevServerURL(output string) string {
	for offset := 0; offset+3 <= len(output); {
		rel := strings.Index(output[offset:], "://")
		if rel < 0 {
			return ""
		}
		sep := offset + rel
		schemeStart, scheme, ok := devServerScheme(output, sep)
		if !ok {
			offset = sep + 3
			continue
		}
		url, end := parseDevServerURL(output, schemeStart, scheme, sep+3)
		if url != "" {
			return url
		}
		if end <= sep+3 {
			end = sep + 3
		}
		offset = end
	}
	return ""
}

// devServerScheme walks backwards from the "://" separator to confirm an
// http/https scheme that is not embedded in a larger token or wrapped in
// one of the reference delimiters above.
func devServerScheme(s string, sep int) (start int, scheme string, ok bool) {
	switch {
	case sep >= 5 && strings.EqualFold(s[sep-5:sep], "https"):
		start, scheme = sep-5, "https"
	case sep >= 4 && strings.EqualFold(s[sep-4:sep], "http"):
		start, scheme = sep-4, "http"
	default:
		return 0, "", false
	}
	if start == 0 {
		return start, scheme, true
	}
	prev := s[start-1]
	if isSchemeByte(prev) || strings.IndexByte(devServerURLEmbedPrefixes, prev) >= 0 {
		return 0, "", false
	}
	return start, scheme, true
}

// parseDevServerURL reads the authority and path that follow "://" and
// returns the normalized URL plus the offset the caller should resume
// scanning from. An empty URL means this candidate did not qualify.
func parseDevServerURL(s string, schemeStart int, scheme string, authorityStart int) (string, int) {
	authorityEnd := authorityStart
	for authorityEnd < len(s) && isAuthorityByte(s[authorityEnd]) {
		authorityEnd++
	}
	if authorityEnd == authorityStart {
		return "", authorityEnd
	}

	pathEnd := authorityEnd
	for pathEnd < len(s) && isURLPathByte(s[pathEnd]) {
		pathEnd++
	}
	path := trimURLTrailers(s[authorityEnd:pathEnd])

	if pathEnd-schemeStart > maxDevServerURLBytes {
		return "", pathEnd
	}
	host, port, ok := splitAuthority(s[authorityStart:authorityEnd])
	if !ok {
		return "", pathEnd
	}
	normalizedHost, ok := normalizeLoopbackHost(host)
	if !ok {
		return "", pathEnd
	}
	// A trailing ":line:column" makes this a source location inside a
	// stack trace or build log, not a server address.
	if hasSourceLocationSuffix(path) {
		return "", pathEnd
	}

	var b strings.Builder
	b.Grow(pathEnd - schemeStart + 8)
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(normalizedHost)
	if port != "" {
		b.WriteByte(':')
		b.WriteString(port)
	}
	b.WriteString(path)
	return b.String(), pathEnd
}

// splitAuthority separates host from port. Bracketed IPv6 hosts keep
// their brackets; a bare host may not contain a colon once the port is
// removed (that would be an unbracketed IPv6 literal, which is not a
// legal URL authority).
func splitAuthority(authority string) (host, port string, ok bool) {
	if strings.HasPrefix(authority, "[") {
		closer := strings.IndexByte(authority, ']')
		if closer < 0 {
			return "", "", false
		}
		host = authority[:closer+1]
		rest := authority[closer+1:]
		if rest == "" {
			return host, "", true
		}
		if rest[0] != ':' {
			return "", "", false
		}
		port = rest[1:]
	} else if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
		host, port = authority[:colon], authority[colon+1:]
		if strings.IndexByte(host, ':') >= 0 {
			return "", "", false
		}
	} else {
		return authority, "", true
	}
	if !isPortNumber(port) {
		return "", "", false
	}
	return host, port, true
}

// normalizeLoopbackHost accepts only hosts a dev server can be reached on
// from this machine. The wildcard bind addresses (0.0.0.0, ::) are what
// servers advertise but not what a browser can navigate to, so they are
// rewritten to localhost.
func normalizeLoopbackHost(host string) (string, bool) {
	lowered := strings.ToLower(host)
	if lowered == "localhost" {
		return "localhost", true
	}
	literal := lowered
	bracketed := strings.HasPrefix(literal, "[") && strings.HasSuffix(literal, "]")
	if bracketed {
		literal = literal[1 : len(literal)-1]
	}
	addr, err := netip.ParseAddr(literal)
	if err != nil {
		return "", false
	}
	if addr.IsUnspecified() {
		return "localhost", true
	}
	if !addr.IsLoopback() {
		return "", false
	}
	if addr.Is4() {
		return addr.String(), true
	}
	return "[" + addr.String() + "]", true
}

// hasSourceLocationSuffix reports whether a URL path ends in ":line:col".
func hasSourceLocationSuffix(path string) bool {
	end := len(path)
	for range 2 {
		digits := end
		for digits > 0 && isDigit(path[digits-1]) {
			digits--
		}
		if digits == end || digits == 0 || path[digits-1] != ':' {
			return false
		}
		end = digits - 1
	}
	return true
}

// trimURLTrailers drops sentence punctuation and unbalanced closing
// brackets that terminal output puts against a URL.
func trimURLTrailers(path string) string {
	for len(path) > 0 {
		last := path[len(path)-1]
		switch last {
		case '.', ',', ';', ':', '!', '?':
			path = path[:len(path)-1]
		case ')', ']':
			opener := "("
			if last == ']' {
				opener = "["
			}
			if strings.Count(path, opener) >= strings.Count(path, string(last)) {
				return path
			}
			path = path[:len(path)-1]
		default:
			return path
		}
	}
	return path
}

func isPortNumber(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	value := 0
	for i := 0; i < len(port); i++ {
		if !isDigit(port[i]) {
			return false
		}
		value = value*10 + int(port[i]-'0')
	}
	return value > 0 && value <= 65535
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isSchemeByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || isDigit(b) ||
		b == '+' || b == '-' || b == '.'
}

func isAuthorityByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || isDigit(b) ||
		b == '.' || b == '-' || b == ':' || b == '[' || b == ']'
}

// isURLPathByte accepts the printable bytes a path/query/fragment may use.
// The excluded punctuation is what surrounds a URL in prose, markdown, and
// quoted log lines rather than what appears inside one.
func isURLPathByte(b byte) bool {
	if b <= 0x20 || b >= 0x7f {
		return false
	}
	switch b {
	case '"', '\'', '`', '<', '>', '\\', '|', '^', '{', '}':
		return false
	}
	return true
}

// Package repoidentity parses transient Git origins, compares verified forge
// IDs, and removes repository URLs from diagnostics. It performs no I/O.
package repoidentity

import (
	"net/url"
	"regexp"
	"strings"
)

var scpRemote = regexp.MustCompile(`^(?:[^@/\\]+@)?([^:/\\]{2,}):([^\\]+)$`)

// SafeRemote removes authentication and parameters for local origin parsing.
// Its result is transient and must not enter project persistence or transport.
func SafeRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.ContainsAny(s, "\r\n\x00") {
		return ""
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Opaque != "" {
			return ""
		}
		u.User = nil
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		u.RawFragment = ""
		return u.String()
	}
	if m := scpRemote.FindStringSubmatch(s); m != nil {
		// SSH usernames are authentication details, not repository identity.
		prefix := ""
		if strings.HasPrefix(s, "git@") {
			prefix = "git@"
		}
		return prefix + m[1] + ":" + strings.SplitN(strings.SplitN(m[2], "?", 2)[0], "#", 2)[0]
	}
	return s
}

// Locator normalizes supported network remote spellings. GitHub's SSH-over-443
// endpoint and path case are equivalent to github.com. Other hosts retain case.
func Locator(raw string) string {
	s := SafeRemote(raw)
	host, path := "", ""
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "file" {
			return ""
		}
		host, path = u.Hostname(), u.EscapedPath()
	} else if m := scpRemote.FindStringSubmatch(s); m != nil {
		host, path = m[1], m[2]
	} else {
		return ""
	}
	host = strings.ToLower(host)
	path = strings.Trim(path, "/")
	if strings.HasSuffix(strings.ToLower(path), ".git") {
		path = path[:len(path)-4]
	}
	if host == "ssh.github.com" {
		host = "github.com"
	}
	if host == "github.com" {
		path = strings.ToLower(path)
	}
	if host == "" || path == "" {
		return ""
	}
	return host + "/" + path
}

// Matches accepts only a forge-verified repository identity.
func Matches(a, b string) bool { return a != "" && a == b }

var scpInText = regexp.MustCompile(`(?i)(?:[^\s<>"@/]+@)?[a-z0-9][a-z0-9._-]+:[^\s<>"]+`)

var remoteInText = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s<>"]+`)

// RedactText handles remote URLs embedded in Git errors before persistence.
func RedactText(text string) string {
	redact := func(raw string) string {
		// Apostrophes are legal in userinfo. Keep them inside the match so a
		// quoted password cannot leave its suffix outside the redacted URL.
		coordinate := strings.TrimRight(raw, "'")
		return "[remote]" + raw[len(coordinate):]
	}
	return scpInText.ReplaceAllStringFunc(remoteInText.ReplaceAllStringFunc(text, redact), redact)
}

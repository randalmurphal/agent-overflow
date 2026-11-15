package providerupdate

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// Versions are the providers' bare tokens ("2.1.325", "0.171.0-alpha.5"),
// read as SemVer: a prerelease sorts before its release, a +build suffix is
// ignored, and "1.2" equals "1.2.0".

// canonical is token as x/mod/semver spells it, "" when it is not a version.
func canonical(token string) string {
	v := "v" + strings.TrimSpace(token)
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// newer reports whether candidate is a later version than installed. Either
// side unparseable is an error the caller reports, never "not newer".
func newer(candidate, installed string) (bool, error) {
	c, i := canonical(candidate), canonical(installed)
	if c == "" {
		return false, errUnparseable(candidate)
	}
	if i == "" {
		return false, errUnparseable(installed)
	}
	return semver.Compare(c, i) > 0, nil
}

// requireVersion returns raw trimmed when it is a version, else an error
// naming source.
func requireVersion(raw, source string) (string, error) {
	token := strings.TrimSpace(raw)
	if canonical(token) == "" {
		return "", fmt.Errorf("%s answered %q, which is not a version", source, truncate(token, 64))
	}
	return token, nil
}

func errUnparseable(raw string) error {
	return fmt.Errorf("%q is not a version this app can compare", truncate(raw, 64))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// sharedComponents is the leading numeric components a and b have in
// common, each followed by ".": "2.1." for 2.1.3 and 2.1.30, "" when the
// first differs. It narrows a tag listing to the refs that can fall
// between them; a character prefix would not, since 2.1.3 and 2.1.30
// share "2.1.3", which excludes 2.1.4.
func sharedComponents(a, b string) string {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	var prefix strings.Builder
	for i := 0; i < min(len(as), len(bs)); i++ {
		if as[i] != bs[i] || !isNumeric(as[i]) {
			break
		}
		prefix.WriteString(as[i] + ".")
	}
	return prefix.String()
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

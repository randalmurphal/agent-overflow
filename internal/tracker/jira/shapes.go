package jira

import "strings"

// The shapes of identifiers and words the client checks, written out
// rather than as package-level regular expressions, which cost every
// process that links the package their compilation at start. Each
// matches exactly what the expression in its comment matches
// (TestShapesMatchTheirExpressions).

// isNumeric reports a numeric ID: ^[0-9]+$.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isKeyName reports a project key or a field ID: ^[A-Za-z][A-Za-z0-9_]*$.
func isKeyName(s string) bool {
	if s == "" || !isLetter(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isLetter(c) && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// isIssueKey reports an issue key: ^[A-Za-z][A-Za-z0-9_]*-[0-9]+$.
func isIssueKey(s string) bool {
	i := strings.IndexByte(s, '-')
	return i > 0 && isKeyName(s[:i]) && isNumeric(s[i+1:])
}

// isMediaID reports a canonical UUID, hex in either case:
// ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$.
func isMediaID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// isWordByte is a byte of \w in Go's regular expressions: an ASCII
// letter, digit or underscore. A byte of a multi-byte rune is not.
func isWordByte(c byte) bool { return isLetter(c) || c >= '0' && c <= '9' || c == '_' }

// lowerTextOperators is s with each whole word AND, OR and NOT in lower
// case: \b(AND|OR|NOT)\b, each match lowered. s itself when it holds
// none, with no allocation.
func lowerTextOperators(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		n := 0
		switch {
		case i > 0 && isWordByte(s[i-1]):
		case strings.HasPrefix(s[i:], "AND"), strings.HasPrefix(s[i:], "NOT"):
			n = 3
		case strings.HasPrefix(s[i:], "OR"):
			n = 2
		}
		if n == 0 || i+n < len(s) && isWordByte(s[i+n]) {
			continue
		}
		if b == nil {
			b = []byte(s)
		}
		for j := i; j < i+n; j++ {
			b[j] += 'a' - 'A'
		}
		i += n - 1
	}
	if b == nil {
		return s
	}
	return string(b)
}

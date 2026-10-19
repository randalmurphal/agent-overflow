package stringsx

import "strings"

// ShellQuote quotes s as one word for a POSIX shell: inside single quotes,
// with each single quote in s closed, escaped and reopened.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

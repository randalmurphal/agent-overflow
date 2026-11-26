package trackerapp

import (
	"slices"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/linear"
)

// IsTicketKey reports whether s is one ticket key as a tracker of kind
// writes it, by the provider's own parser: a Jira issue key such as WEB-12,
// or a Linear identifier such as R-12, whose team key may be one letter.
// Case does not matter.
func IsTicketKey(kind, s string) bool {
	switch kind {
	case store.TrackerKindJira:
		return jira.IsIssueKey(strings.ToUpper(s))
	case store.TrackerKindLinear:
		_, _, ok := linear.ParseIdentifier(s)
		return ok
	}
	return false
}

// TicketKeys is the one rule for which ticket keys a text names (a pull
// request's title, a branch's name), in the order they first appear, each
// once and in capitals: a project key of a letter and one or more letters,
// digits or underscores, a dash and a number with no leading zero, bounded
// on both sides by the text's end or a character that is not an ASCII
// letter or digit. Case does not matter (branches are usually lower case),
// so "web-1-fix" names WEB-1 and "WEB-12" does not name WEB-1. Whether a
// key is a ticket of this computer's trackers is the store's question
// (store.TicketsByKey).
func TicketKeys(text string) []string {
	var keys []string
	for i := 0; i < len(text); i++ {
		if !isASCIILetter(text[i]) || (i > 0 && isASCIIAlnum(text[i-1])) {
			continue
		}
		end, ok := keyEnd(text, i)
		if !ok {
			continue
		}
		key := strings.ToUpper(text[i:end])
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
		i = end - 1
	}
	return keys
}

// keyEnd is the end of the key that starts at text[start], a letter that
// follows a boundary.
func keyEnd(text string, start int) (int, bool) {
	i := start + 1
	for i < len(text) && (isASCIIAlnum(text[i]) || text[i] == '_') {
		i++
	}
	if i-start < 2 || i >= len(text) || text[i] != '-' {
		return 0, false
	}
	i++
	if i >= len(text) || text[i] < '1' || text[i] > '9' {
		return 0, false
	}
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i < len(text) && isASCIIAlnum(text[i]) {
		return 0, false
	}
	return i, true
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isASCIIAlnum(c byte) bool { return isASCIILetter(c) || c >= '0' && c <= '9' }

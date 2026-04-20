package triage

import "strings"

// deleteByPrefix removes every entry from `m` whose key has the given
// prefix. Used across the turn-lifecycle and cleanup paths to drop all
// thread-scoped (prefix "<threadID>|" or "<threadID>:") or turn-scoped
// (prefix "<threadID>|<turnIndex>|") correlation state in one pass.
// Safe to call on a nil map (it iterates zero times).
func deleteByPrefix[V any](m map[string]V, prefix string) {
	for key := range m {
		if strings.HasPrefix(key, prefix) {
			delete(m, key)
		}
	}
}

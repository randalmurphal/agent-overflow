package codex

import "sort"

// SelfQueuedClaimIDsForTest reports the `clientUserMessageId`s this session
// currently claims in the provider's queue.
//
// The ledger is unexported session state, but WHO owns a queued row is an
// app-layer decision (`rearmCodexProviderQueueClaims` picks AO's ids out of a
// queue that can also hold `codex queue --thread` rows), so the app-layer test
// for that decision needs to read the result back.
//
// Scoped out of `_test.go` so sibling packages can import it, same as
// SetActiveTurnIDForTest next door.
func SelfQueuedClaimIDsForTest(s *Session) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.selfQueuedSubmissions))
	for clientID := range s.selfQueuedSubmissions {
		out = append(out, clientID)
	}
	sort.Strings(out)
	return out
}

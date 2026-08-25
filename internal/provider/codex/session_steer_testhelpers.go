package codex

import "time"

// SetActiveTurnIDForTest pre-seeds the session's activeTurnID so Steer
// doesn't trip its "no active turn" guard. In production the field is
// populated by the turn/start response or a turn/started notification
// (see Send and the dispatch loop's EventTurnStart case). App-layer
// tests in the root `main` package need a way to set it without
// spinning up a real wire round-trip — same motivation as the
// NewProbeOnlyTestSession helper next door.
//
// Scoped out of `_test.go` so sibling packages can import it; the
// codex package's own tests poke `s.turn.activeTurnID` directly because
// they live in the same package.
func SetActiveTurnIDForTest(s *Session, turnID string) {
	s.mu.Lock()
	s.turn.activeTurnID = turnID
	s.mu.Unlock()
}

// SetRequestTimeoutForTest replaces the JSON-RPC response timeout. App-layer
// tests use this to exercise timeout paths without waiting for the production
// default.
func SetRequestTimeoutForTest(s *Session, timeout time.Duration) {
	s.mu.Lock()
	s.requestTimeoutOverride = timeout
	s.mu.Unlock()
}

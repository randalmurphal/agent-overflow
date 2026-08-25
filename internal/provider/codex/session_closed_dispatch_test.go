package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agent-overflow/internal/provider"
)

// TestClosedSessionDispatchRefusesAsErrSessionClosed pins the regression
// class ErrSessionClosed exists for: Close zeroes s.turn and s.review whole,
// so any method that reads them AFTER Close sees an idle session and makes
// the wrong decision — Steer answered ErrNoActiveTurn (which the app layer
// retries as a fresh Send at the next turn index, the 3fa6ce74 flush
// regression), CompactThread and Revert read "no turn in flight" and
// proceeded onto the dead pipe, and reserveReview WROTE a fresh reviewRun
// onto the closed session. Each entry point must refuse with ErrSessionClosed
// before reading the zeroed state.
//
// Interrupt deliberately has no guard: its post-Close behavior (send turnId
// "" and fail on the dead pipe) matches the pre-campaign behavior and no
// caller classifies its error.
func TestClosedSessionDispatchRefusesAsErrSessionClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// `cat` is a stand-in process, never a provider CLI: Close needs a real
	// *provider.Process to close and this test must not spawn codex.
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		cancel:   cancel,
	}
	// Satisfy Revert's preflights (thread id, app-server version, paginated
	// history) so the test proves the closing guard refuses, not an earlier
	// unrelated check.
	codexThread := "codex-thread-1"
	s.codexThreadID.Store(&codexThread)
	version := threadRevertMinimumCodexVersion
	s.appServerVersion.Store(&version)
	mode := paginatedThreadHistoryMode
	s.threadHistoryMode.Store(&mode)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := s.Steer(ctx, "hello", provider.SendOptions{}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Steer after Close = %v, want ErrSessionClosed", err)
	} else if IsNoActiveTurnRace(err) {
		// The whole point of the sentinel: a closed session must never
		// classify as the live turn-just-ended race, which carries retry
		// semantics (re-Send at the next turn index).
		t.Fatalf("Steer's post-Close error %v classifies as the live no-active-turn race", err)
	}
	if err := s.CompactThread(ctx); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("CompactThread after Close = %v, want ErrSessionClosed", err)
	}
	if err := s.reserveReview(1, ReviewTarget{}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("reserveReview after Close = %v, want ErrSessionClosed", err)
	}
	if s.review != nil {
		t.Fatalf("reserveReview after Close wrote a reviewRun onto the closed session")
	}
	if _, err := s.Revert(ctx, "turn-1"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Revert after Close = %v, want ErrSessionClosed", err)
	}
}

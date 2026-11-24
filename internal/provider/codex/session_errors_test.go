package codex

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsNoActiveTurnRace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"sentinel", ErrNoActiveTurn, true},
		{"wrapped sentinel", fmt.Errorf("steer: %w", ErrNoActiveTurn), true},
		// Mirror the upstream JSON-RPC error text; the substring is the
		// authoritative classifier per codex-rs/core/src/session/mod.rs.
		{"wire substring", errors.New("codex: turn/steer rpc: InvalidRequest: NoActiveTurn"), true},
		{"unrelated error", errors.New("network: connection reset"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNoActiveTurnRace(tc.err); got != tc.want {
				t.Fatalf("IsNoActiveTurnRace(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsAmbiguousSteerTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"turn/steer timeout", &RequestTimeoutError{Method: "turn/steer"}, true},
		{"other method timeout", &RequestTimeoutError{Method: "message/send"}, false},
		{"non-timeout error", errors.New("some other error"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAmbiguousSteerTimeout(tc.err); got != tc.want {
				t.Fatalf("IsAmbiguousSteerTimeout(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

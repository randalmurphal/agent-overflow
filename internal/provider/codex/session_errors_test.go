package codex

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsNoActiveTurnRaceSentinel(t *testing.T) {
	if !IsNoActiveTurnRace(ErrNoActiveTurn) {
		t.Fatal("IsNoActiveTurnRace(ErrNoActiveTurn) = false, want true")
	}
}

func TestIsNoActiveTurnRaceWrappedSentinel(t *testing.T) {
	wrapped := fmt.Errorf("steer: %w", ErrNoActiveTurn)
	if !IsNoActiveTurnRace(wrapped) {
		t.Fatalf("IsNoActiveTurnRace(wrapped sentinel) = false, want true; err=%v", wrapped)
	}
}

func TestIsNoActiveTurnRaceWireSubstring(t *testing.T) {
	// Mirror the upstream JSON-RPC error text; the substring is the
	// authoritative classifier per codex-rs/core/src/session/mod.rs.
	wireErr := errors.New("codex: turn/steer rpc: InvalidRequest: NoActiveTurn")
	if !IsNoActiveTurnRace(wireErr) {
		t.Fatalf("IsNoActiveTurnRace(wire substring) = false, want true; err=%v", wireErr)
	}
}

func TestIsNoActiveTurnRaceUnrelatedError(t *testing.T) {
	if IsNoActiveTurnRace(errors.New("network: connection reset")) {
		t.Fatal("IsNoActiveTurnRace(unrelated) = true, want false")
	}
}

func TestIsNoActiveTurnRaceNil(t *testing.T) {
	if IsNoActiveTurnRace(nil) {
		t.Fatal("IsNoActiveTurnRace(nil) = true, want false")
	}
}

func TestIsAmbiguousSteerTimeoutMatchesTurnSteer(t *testing.T) {
	timeoutErr := &RequestTimeoutError{Method: "turn/steer"}
	if !IsAmbiguousSteerTimeout(timeoutErr) {
		t.Fatalf("IsAmbiguousSteerTimeout(turn/steer timeout) = false, want true; err=%v", timeoutErr)
	}
}

func TestIsAmbiguousSteerTimeoutIgnoresOtherMethods(t *testing.T) {
	timeoutErr := &RequestTimeoutError{Method: "message/send"}
	if IsAmbiguousSteerTimeout(timeoutErr) {
		t.Fatalf("IsAmbiguousSteerTimeout(message/send timeout) = true, want false; err=%v", timeoutErr)
	}
}

func TestIsAmbiguousSteerTimeoutIgnoresNonTimeout(t *testing.T) {
	if IsAmbiguousSteerTimeout(errors.New("some other error")) {
		t.Fatal("IsAmbiguousSteerTimeout(non-timeout error) = true, want false")
	}
	if IsAmbiguousSteerTimeout(nil) {
		t.Fatal("IsAmbiguousSteerTimeout(nil) = true, want false")
	}
}

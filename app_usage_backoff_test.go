package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

func newBackoffLedgerForTest(now *time.Time) *usageBackoffLedger {
	return &usageBackoffLedger{now: func() time.Time { return *now }}
}

// A 429 holds exactly the account that earned it, for the server's
// Retry-After: the throttle is per-bearer, so other accounts — and the other
// provider — must stay clear.
func TestUsageBackoffLedgerScopesA429ToItsAccount(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ledger := newBackoffLedgerForTest(&now)

	ledger.Note(
		string(provider.Claude),
		"selected",
		fmt.Errorf("probe: %w", &claude.RateLimitedError{RetryAfter: 45 * time.Second}),
	)

	if got := ledger.Remaining(string(provider.Claude), "selected"); got != 45*time.Second {
		t.Fatalf("Remaining(selected) = %v, want 45s", got)
	}
	if got := ledger.Remaining(string(provider.Claude), "other"); got != 0 {
		t.Fatalf("Remaining(other) = %v, want 0 — the throttle is per-account", got)
	}
	if got := ledger.Remaining(string(provider.Codex), "selected"); got != 0 {
		t.Fatalf("Remaining(codex/selected) = %v, want 0 — same ID, different provider", got)
	}

	now = now.Add(45 * time.Second)
	if got := ledger.Remaining(string(provider.Claude), "selected"); got != 0 {
		t.Fatalf("Remaining after expiry = %v, want 0", got)
	}
}

// A 429 without a usable Retry-After starts the escalating default: the
// observed server window is ~1h, so consecutive headerless 429s double the
// hold (10m → 20m → 40m → 1h cap) instead of retrying straight back into the
// active window. A 429 that DOES name its window, or a success, resets the
// escalation.
func TestUsageBackoffLedgerEscalatesWithoutRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ledger := newBackoffLedgerForTest(&now)
	key := string(provider.Claude)

	wantHolds := []time.Duration{
		initialUsageProbeBackoff,
		2 * initialUsageProbeBackoff,
		4 * initialUsageProbeBackoff,
		maxUsageProbeBackoff,
		maxUsageProbeBackoff,
	}
	for i, want := range wantHolds {
		ledger.Note(key, "selected", &claude.RateLimitedError{})
		if got := ledger.Remaining(key, "selected"); got != want {
			t.Fatalf("Remaining after headerless 429 #%d = %v, want %v", i+1, got, want)
		}
		now = now.Add(want)
	}

	// The escalation is per account: a first headerless 429 elsewhere starts
	// at the initial hold.
	ledger.Note(key, "other", &claude.RateLimitedError{})
	if got := ledger.Remaining(key, "other"); got != initialUsageProbeBackoff {
		t.Fatalf("Remaining(other) = %v, want %v", got, initialUsageProbeBackoff)
	}

	// A server-named window replaces the guesswork and resets the strikes...
	ledger.Note(key, "selected", &claude.RateLimitedError{RetryAfter: 45 * time.Second})
	if got := ledger.Remaining(key, "selected"); got != 45*time.Second {
		t.Fatalf("Remaining after Retry-After 429 = %v, want 45s", got)
	}
	now = now.Add(45 * time.Second)
	ledger.Note(key, "selected", &claude.RateLimitedError{})
	if got := ledger.Remaining(key, "selected"); got != initialUsageProbeBackoff {
		t.Fatalf("Remaining after reset-then-headerless = %v, want %v", got, initialUsageProbeBackoff)
	}
	now = now.Add(initialUsageProbeBackoff)

	// ...and so does a success.
	ledger.Note(key, "selected", nil)
	ledger.Note(key, "selected", &claude.RateLimitedError{})
	if got := ledger.Remaining(key, "selected"); got != initialUsageProbeBackoff {
		t.Fatalf("Remaining after success-then-headerless = %v, want %v", got, initialUsageProbeBackoff)
	}
}

// A successful probe proves the throttle lifted and clears the hold; other
// errors say nothing about the throttle and must leave it in place.
func TestUsageBackoffLedgerOutcomeTransitions(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ledger := newBackoffLedgerForTest(&now)
	key := string(provider.Claude)

	ledger.Note(key, "a", nil)
	ledger.Note(key, "a", errors.New("connection reset"))
	if got := ledger.Remaining(key, "a"); got != 0 {
		t.Fatalf("Remaining after non-429 outcomes = %v, want 0", got)
	}

	ledger.Note(key, "a", &claude.RateLimitedError{RetryAfter: 90 * time.Second})
	ledger.Note(key, "a", errors.New("connection reset"))
	if got := ledger.Remaining(key, "a"); got != 90*time.Second {
		t.Fatalf("Remaining after unrelated error = %v, want the 90s hold kept", got)
	}

	ledger.Note(key, "a", nil)
	if got := ledger.Remaining(key, "a"); got != 0 {
		t.Fatalf("Remaining after success = %v, want the hold cleared", got)
	}
}

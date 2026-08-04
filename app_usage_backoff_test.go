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

// A 429 without a usable Retry-After falls back to the default backoff.
func TestUsageBackoffLedgerDefaultsWithoutRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ledger := newBackoffLedgerForTest(&now)

	ledger.Note(string(provider.Claude), "selected", &claude.RateLimitedError{})

	if got := ledger.Remaining(string(provider.Claude), "selected"); got != defaultUsageProbeBackoff {
		t.Fatalf("Remaining = %v, want the %v default", got, defaultUsageProbeBackoff)
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

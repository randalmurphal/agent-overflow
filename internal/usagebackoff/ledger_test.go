package usagebackoff

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// syncLogBuffer is a mutex-guarded sink for log.SetOutput. The ledger logs from
// the caller's goroutine, but -race still wants the test's read of the captured
// text guarded against the logger's writes.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogOutput routes the standard logger into a race-safe buffer for the
// duration of the test.
func captureLogOutput(t *testing.T) *syncLogBuffer {
	t.Helper()
	logs := &syncLogBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	return logs
}

func newBackoffLedgerForTest(now *time.Time) *Ledger {
	return &Ledger{now: func() time.Time { return *now }}
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
		initialProbeBackoff,
		2 * initialProbeBackoff,
		4 * initialProbeBackoff,
		maxProbeBackoff,
		maxProbeBackoff,
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
	if got := ledger.Remaining(key, "other"); got != initialProbeBackoff {
		t.Fatalf("Remaining(other) = %v, want %v", got, initialProbeBackoff)
	}

	// A server-named window replaces the guesswork and resets the strikes...
	ledger.Note(key, "selected", &claude.RateLimitedError{RetryAfter: 45 * time.Second})
	if got := ledger.Remaining(key, "selected"); got != 45*time.Second {
		t.Fatalf("Remaining after Retry-After 429 = %v, want 45s", got)
	}
	now = now.Add(45 * time.Second)
	ledger.Note(key, "selected", &claude.RateLimitedError{})
	if got := ledger.Remaining(key, "selected"); got != initialProbeBackoff {
		t.Fatalf("Remaining after reset-then-headerless = %v, want %v", got, initialProbeBackoff)
	}
	now = now.Add(initialProbeBackoff)

	// ...and so does a success.
	ledger.Note(key, "selected", nil)
	ledger.Note(key, "selected", &claude.RateLimitedError{})
	if got := ledger.Remaining(key, "selected"); got != initialProbeBackoff {
		t.Fatalf("Remaining after success-then-headerless = %v, want %v", got, initialProbeBackoff)
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

// A server throttle runs for about an hour; app restarts are far more frequent
// than that. A hold that does not survive one hands the next boot a clean
// slate and walks straight back into the live window.
func TestUsageBackoffLedgerHoldsSurviveARestart(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage-backoff.json")

	ledger := newBackoffLedgerForTest(&now)
	ledger.Load(path)
	ledger.Note(
		string(provider.Claude),
		"selected",
		&claude.RateLimitedError{RetryAfter: time.Hour},
	)

	reloadedNow := now.Add(10 * time.Minute)
	reloaded := newBackoffLedgerForTest(&reloadedNow)
	reloaded.Load(path)
	if got := reloaded.Remaining(string(provider.Claude), "selected"); got != 50*time.Minute {
		t.Fatalf("Remaining after restart = %v, want the 50m left of the hold", got)
	}
	if got := reloaded.Remaining(string(provider.Claude), "other"); got != 0 {
		t.Fatalf("Remaining(other) = %v, want the per-account scope preserved", got)
	}

	// A success clears the hold, and that clearing persists too.
	reloaded.Note(string(provider.Claude), "selected", nil)
	clearedNow := reloadedNow
	cleared := newBackoffLedgerForTest(&clearedNow)
	cleared.Load(path)
	if got := cleared.Remaining(string(provider.Claude), "selected"); got != 0 {
		t.Fatalf("Remaining after a successful probe = %v, want 0", got)
	}
}

// The headerless escalation is what keeps a 429 with no Retry-After from
// retrying into the same window, so the strike count has to outlive a restart
// even once its own hold has expired.
func TestUsageBackoffLedgerHeadlessStrikesSurviveARestart(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage-backoff.json")

	ledger := newBackoffLedgerForTest(&now)
	ledger.Load(path)
	ledger.Note(string(provider.Claude), "selected", &claude.RateLimitedError{})
	if got := ledger.Remaining(string(provider.Claude), "selected"); got != initialProbeBackoff {
		t.Fatalf("first headerless hold = %v, want %v", got, initialProbeBackoff)
	}

	// Restart well past the first hold: it is gone, the strike is not.
	restartNow := now.Add(2 * initialProbeBackoff)
	restarted := newBackoffLedgerForTest(&restartNow)
	restarted.Load(path)
	if got := restarted.Remaining(string(provider.Claude), "selected"); got != 0 {
		t.Fatalf("expired hold = %v, want it pruned on load", got)
	}
	restarted.Note(string(provider.Claude), "selected", &claude.RateLimitedError{})
	if got := restarted.Remaining(string(provider.Claude), "selected"); got != 2*initialProbeBackoff {
		t.Fatalf("second headerless hold = %v, want the escalation preserved", got)
	}
}

// A strike count whose own hold has already expired is the only surviving
// record of how deep the escalation had gone, and every Note rewrites the
// WHOLE file — including for other accounts. A save that walked only the live
// holds would drop the strike-only account on the next unrelated write, so its
// following headerless 429 would restart at 10m instead of the 20m it earned
// and retry back into the same server window.
func TestUsageBackoffLedgerKeepsStrikeOnlyEntriesThroughAnotherAccountsWrite(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage-backoff.json")
	key := string(provider.Claude)

	now := base
	first := newBackoffLedgerForTest(&now)
	first.Load(path)
	first.Note(key, "a", &claude.RateLimitedError{})

	// Past a's hold: it loads as a strike with no live hold, which is the
	// state only another account's write can now carry forward.
	later := base.Add(2 * initialProbeBackoff)
	second := newBackoffLedgerForTest(&later)
	second.Load(path)
	if got := second.Remaining(key, "a"); got != 0 {
		t.Fatalf("Remaining(a) after its hold expired = %v, want 0", got)
	}
	second.Note(key, "b", &claude.RateLimitedError{})

	third := newBackoffLedgerForTest(&later)
	third.Load(path)
	third.Note(key, "a", &claude.RateLimitedError{})
	if got := third.Remaining(key, "a"); got != 2*initialProbeBackoff {
		t.Fatalf(
			"Remaining(a) = %v, want %v — a's escalation survived b's write",
			got,
			2*initialProbeBackoff,
		)
	}
	if got := third.Remaining(key, "b"); got != initialProbeBackoff {
		t.Fatalf("Remaining(b) = %v, want %v", got, initialProbeBackoff)
	}
}

// The holds are worth persisting only because they outlive the process, so a
// write that cannot land silently demotes the ledger to memory-only — exactly
// the "rate limits never update anymore" state this file exists to prevent,
// minus any way to diagnose it. It must be announced.
func TestUsageBackoffLedgerAnnouncesAFailedWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("regular file"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	ledger := newBackoffLedgerForTest(&now)
	// Load first, and only capture afterwards: an unreadable path announces
	// itself too, so the assertion below can only be satisfied by the write.
	ledger.Load(filepath.Join(blocker, "usage-backoff.json"))
	logs := captureLogOutput(t)

	ledger.Note(string(provider.Claude), "selected", &claude.RateLimitedError{RetryAfter: time.Hour})

	if !strings.Contains(logs.String(), "usage backoff") {
		t.Fatal("a backoff write that could not land was swallowed")
	}
	// The hold still applies for this process; only its durability was lost.
	if got := ledger.Remaining(string(provider.Claude), "selected"); got != time.Hour {
		t.Fatalf("Remaining after a failed write = %v, want the hold still enforced", got)
	}
}

// A corrupt file must cost the holds, never the boot.
func TestUsageBackoffLedgerToleratesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-backoff.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureLogOutput(t)

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	ledger := newBackoffLedgerForTest(&now)
	ledger.Load(path)

	if got := ledger.Remaining(string(provider.Claude), "selected"); got != 0 {
		t.Fatalf("Remaining after a corrupt load = %v, want 0", got)
	}
	if !strings.Contains(logs.String(), "usage backoff") {
		t.Fatal("a corrupt backoff file was discarded silently")
	}
	// The ledger still works, and repairs the file on its next write.
	ledger.Note(string(provider.Claude), "selected", &claude.RateLimitedError{RetryAfter: time.Hour})
	repaired := newBackoffLedgerForTest(&now)
	repaired.Load(path)
	if got := repaired.Remaining(string(provider.Claude), "selected"); got != time.Hour {
		t.Fatalf("Remaining after repair = %v, want the new hold", got)
	}
}

package main

import (
	"errors"
	"sync"
	"time"

	"agent-overflow/internal/provider/claude"
)

// initialUsageProbeBackoff applies after the FIRST 429 whose Retry-After
// header was absent or unusable; each consecutive headerless 429 doubles it up
// to maxUsageProbeBackoff. The observed throttle window on Claude's usage
// endpoint is about an hour, so a short fixed default (this was once 1 minute)
// had the app retrying straight back into the active window — every attempt
// re-earning the throttle, which reads as "rate limits never update anymore".
const initialUsageProbeBackoff = 10 * time.Minute

// maxUsageProbeBackoff caps the headerless-429 escalation at the observed
// throttle window.
const maxUsageProbeBackoff = time.Hour

// usageBackoffLedger scopes server-imposed usage-endpoint backoffs (429) to
// the account that earned them. The throttle is per-bearer: one account being
// rate limited says nothing about the others (observed 2026-08-03 — an
// inactive account's probe succeeded mid-throttle while every request for the
// selected account served 429). A provider-wide hold would bury every other
// card's refresh behind the selected account's penalty, which is exactly what
// made throttled-but-alive accounts indistinguishable from dead ones.
//
// The refresh path consults Remaining before touching the endpoint and feeds
// every outcome back through Note, so a held account costs zero requests until
// its backoff expires. The zero value is ready to use. accountID "" keys the
// unmanaged canonical-credential probe that runs before any account is saved.
type usageBackoffLedger struct {
	mu sync.Mutex
	// now is a test seam; nil means the wall clock.
	now   func() time.Time
	until map[usageBackoffKey]time.Time
	// headerlessStrikes counts consecutive 429s that carried no usable
	// Retry-After, per account, driving the exponential default backoff.
	// Cleared by a success or by a 429 that does name its window.
	headerlessStrikes map[usageBackoffKey]int
}

type usageBackoffKey struct {
	provider  string
	accountID string
}

// Remaining reports how much of the account's backoff still holds. Zero means
// requests are allowed.
func (l *usageBackoffLedger) Remaining(providerName, accountID string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.until[usageBackoffKey{providerName, accountID}]
	if !ok {
		return 0
	}
	if remaining := until.Sub(l.clock()); remaining > 0 {
		return remaining
	}
	return 0
}

// Note records one probe outcome for the account. A 429 — a
// claude.RateLimitedError anywhere in the chain — starts a backoff honoring
// the server's Retry-After, or the escalating headerless default when the
// server named no window. A successful probe clears any leftover hold and the
// strike count, because it proves the throttle lifted. Other errors change
// nothing: a 401 or a transport failure says nothing about the throttle
// either way.
func (l *usageBackoffLedger) Note(providerName, accountID string, err error) {
	key := usageBackoffKey{providerName, accountID}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		delete(l.until, key)
		delete(l.headerlessStrikes, key)
		return
	}
	var limited *claude.RateLimitedError
	if !errors.As(err, &limited) {
		return
	}
	retry := limited.RetryAfter
	if retry > 0 {
		// The server named its window; the guesswork counter resets with it.
		delete(l.headerlessStrikes, key)
	} else {
		if l.headerlessStrikes == nil {
			l.headerlessStrikes = make(map[usageBackoffKey]int)
		}
		l.headerlessStrikes[key]++
		retry = maxUsageProbeBackoff
		// Bound the shift, not just the product — a runaway strike count
		// would overflow the Duration before min() could cap it.
		if doublings := l.headerlessStrikes[key] - 1; doublings < 3 {
			retry = min(initialUsageProbeBackoff<<doublings, maxUsageProbeBackoff)
		}
	}
	if l.until == nil {
		l.until = make(map[usageBackoffKey]time.Time)
	}
	l.until[key] = l.clock().Add(retry)
}

func (l *usageBackoffLedger) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

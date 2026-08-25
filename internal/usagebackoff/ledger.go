// Package usagebackoff scopes server-imposed usage-endpoint backoffs (429) to
// the account that earned them, durably.
package usagebackoff

import (
	"errors"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/provider/claude"
)

// initialProbeBackoff applies after the FIRST 429 whose Retry-After header was
// absent or unusable; each consecutive headerless 429 doubles it up to
// maxProbeBackoff. The observed throttle window on Claude's usage
// endpoint is about an hour, so a short fixed default (this was once 1 minute)
// had the app retrying straight back into the active window — every attempt
// re-earning the throttle, which reads as "rate limits never update anymore".
const initialProbeBackoff = 10 * time.Minute

// maxProbeBackoff caps the headerless-429 escalation at the observed
// throttle window.
const maxProbeBackoff = time.Hour

// Ledger scopes server-imposed usage-endpoint backoffs (429) to the account
// that earned them. The throttle is per-bearer: one account being
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
//
// The holds outlive the process. A server throttle runs for about an hour and
// app restarts are frequent, so an in-memory-only ledger handed every restart
// a clean slate and walked the user straight back into the live window —
// re-earning the throttle each time, which is the "rate limits never update
// anymore" symptom. Load binds the ledger to a file; every state change writes
// it back.
type Ledger struct {
	mu sync.Mutex
	// now is a test seam; nil means the wall clock.
	now func() time.Time
	// path is the durable copy. Empty means memory-only — the zero value, and
	// what unit tests that never call Load get.
	path  string
	until map[ledgerKey]time.Time
	// headerlessStrikes counts consecutive 429s that carried no usable
	// Retry-After, per account, driving the exponential default backoff.
	// Cleared by a success or by a 429 that does name its window.
	headerlessStrikes map[ledgerKey]int
}

type ledgerKey struct {
	provider  string
	accountID string
}

// Remaining reports how much of the account's backoff still holds. Zero means
// requests are allowed.
func (l *Ledger) Remaining(providerName, accountID string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.until[ledgerKey{providerName, accountID}]
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
func (l *Ledger) Note(providerName, accountID string, err error) {
	key := ledgerKey{providerName, accountID}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		if _, held := l.until[key]; !held {
			if _, struck := l.headerlessStrikes[key]; !struck {
				return
			}
		}
		delete(l.until, key)
		delete(l.headerlessStrikes, key)
		l.saveLocked()
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
			l.headerlessStrikes = make(map[ledgerKey]int)
		}
		l.headerlessStrikes[key]++
		retry = maxProbeBackoff
		// Bound the shift, not just the product — a runaway strike count
		// would overflow the Duration before min() could cap it.
		if doublings := l.headerlessStrikes[key] - 1; doublings < 3 {
			retry = min(initialProbeBackoff<<doublings, maxProbeBackoff)
		}
	}
	if l.until == nil {
		l.until = make(map[ledgerKey]time.Time)
	}
	l.until[key] = l.clock().Add(retry)
	l.saveLocked()
}

// fileEntry is one persisted account hold. The wire shape is the ledger's own
// file format; nothing else reads it.
type fileEntry struct {
	Provider          string    `json:"provider"`
	AccountID         string    `json:"accountId"`
	Until             time.Time `json:"until"`
	HeaderlessStrikes int       `json:"headerlessStrikes,omitempty"`
}

type ledgerFile struct {
	Entries []fileEntry `json:"entries"`
}

// Load binds the ledger to path and adopts whatever holds are still running.
// Expired entries that carry no strike count are dropped on the way in — a
// hold that has served its time is not state worth keeping.
//
// A file that cannot be read is not fatal: the ledger starts empty and says
// so. The cost of losing the holds is one throttled request per account; the
// cost of refusing to boot is the whole app.
func (l *Ledger) Load(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.path = path
	var file ledgerFile
	found, err := atomicfile.ReadJSON(path, &file)
	if err != nil {
		log.Printf("usage backoff: read %s: %v; starting with no holds", path, err)
		return
	}
	if !found {
		return
	}
	now := l.clock()
	for _, entry := range file.Entries {
		key := ledgerKey{entry.Provider, entry.AccountID}
		if entry.HeaderlessStrikes > 0 {
			if l.headerlessStrikes == nil {
				l.headerlessStrikes = make(map[ledgerKey]int)
			}
			l.headerlessStrikes[key] = entry.HeaderlessStrikes
		}
		if !entry.Until.After(now) {
			continue
		}
		if l.until == nil {
			l.until = make(map[ledgerKey]time.Time)
		}
		l.until[key] = entry.Until
	}
}

// saveLocked writes the current holds back to the bound file. Called from
// every mutation so a crash costs at most the change in flight.
//
// A failed write is announced, not swallowed: it means the next restart walks
// back into a live throttle, which is precisely the symptom this file exists
// to prevent, and a silent version of it would be undiagnosable.
func (l *Ledger) saveLocked() {
	if l.path == "" {
		return
	}
	now := l.clock()
	file := ledgerFile{Entries: make([]fileEntry, 0, len(l.until))}
	seen := make(map[ledgerKey]struct{}, len(l.until)+len(l.headerlessStrikes))
	for key, until := range l.until {
		strikes := l.headerlessStrikes[key]
		if !until.After(now) && strikes == 0 {
			continue
		}
		seen[key] = struct{}{}
		file.Entries = append(file.Entries, fileEntry{
			Provider:          key.provider,
			AccountID:         key.accountID,
			Until:             until,
			HeaderlessStrikes: strikes,
		})
	}
	// A strike count with no live hold still drives the next escalation, so it
	// has to persist on its own.
	for key, strikes := range l.headerlessStrikes {
		if _, ok := seen[key]; ok || strikes == 0 {
			continue
		}
		file.Entries = append(file.Entries, fileEntry{
			Provider:          key.provider,
			AccountID:         key.accountID,
			HeaderlessStrikes: strikes,
		})
	}
	if err := atomicfile.WriteJSON(l.path, file); err != nil {
		log.Printf("usage backoff: write %s: %v; holds will not survive a restart", l.path, err)
	}
}

func (l *Ledger) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

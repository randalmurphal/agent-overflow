package transport

import (
	"sync"
	"time"
)

// A ticket is this package's one single-use-token mechanism, and both
// users of it share this file.
//
// The mechanic is the same in both cases: mint a CSPRNG token, hand it to
// a client over a channel that is already authenticated, and let the
// FIRST presentation spend it. What a spent ticket buys differs, and that
// is the only thing that differs:
//
//   - the PAGE ticket (Credential.MintPageTicket) buys the HttpOnly page
//     cookie at /bootstrap.json. Its subject is empty — a launch has one
//     page credential and the ticket only decides who receives it — and
//     it carries no deadline, because the URL it rides is produced for a
//     person to open and a launcher's fixed URL must still work an hour
//     later.
//   - the WS ticket (Server.handleAuthTicket) names a SESSION and is
//     presented on the upgrade, so the session credential never rides a
//     WebSocket URL (docs/specs/remote-access.md §4). It carries a
//     deadline measured in seconds, because a client mints one
//     immediately before the connection it is for.
//
// Those two differences — a subject, and a deadline — are parameters
// rather than a second implementation. Building the WS half separately
// would have meant a second constant-time compare, a second eviction
// rule, and a second place for "single use" to be got subtly wrong.

// ticketEntry is one outstanding ticket.
type ticketEntry struct {
	// token is the minted secret. Compared in constant time, never
	// logged.
	token string
	// subject is what the ticket admits the holder to, or empty when the
	// book's tickets admit the one thing the book is for. Returned by
	// consume so a caller reads what it bought rather than tracking it
	// alongside.
	subject string
	// expiresAtNanos is when the ticket stops being spendable, or 0 in a
	// book with no deadline.
	expiresAtNanos int64
}

// ticketBook holds the minted-but-unspent tickets of one kind, oldest
// first.
//
// Bounded twice over: by max, so a producer that mints without ever
// spending cannot grow it, and (when ttl is set) by the deadline, so an
// unspent ticket stops occupying a slot on its own. Eviction keeps the
// NEWEST — the ticket a caller just minted is the one about to be
// presented.
//
// The zero value is not usable; construct with newTicketBook. A slice
// rather than a map because max is small and the lookup must be a
// constant-time compare against every candidate anyway: hashing the
// presented token to find its bucket would leak, through the miss, that
// no ticket with that prefix exists.
type ticketBook struct {
	max int
	ttl time.Duration
	// now is injectable so tests move time instead of sleeping.
	now func() time.Time

	mu      sync.Mutex
	entries []ticketEntry
}

func newTicketBook(max int, ttl time.Duration) *ticketBook {
	return &ticketBook{max: max, ttl: ttl, now: time.Now, entries: make([]ticketEntry, 0, 4)}
}

// mint returns a fresh ticket for subject and records it as outstanding.
func (b *ticketBook) mint(subject string) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	now := b.now()
	expires := int64(0)
	if b.ttl > 0 {
		expires = now.Add(b.ttl).UnixNano()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropLapsed(now.UnixNano())
	if len(b.entries) >= b.max {
		// Drop the oldest. Copying forward rather than reslicing keeps
		// the backing array at its cap instead of walking off the end.
		copy(b.entries, b.entries[len(b.entries)-b.max+1:])
		b.entries = b.entries[:b.max-1]
	}
	b.entries = append(b.entries, ticketEntry{token: token, subject: subject, expiresAtNanos: expires})
	return token, nil
}

// consume spends a ticket, reporting its subject and whether it was there
// at all. A ticket answers exactly one call: a URL that already bought its
// cookie cannot buy a second one, and an upgrade that already spent its
// ticket cannot be replayed.
func (b *ticketBook) consume(token string) (subject string, ok bool) {
	if token == "" {
		return "", false
	}
	now := b.now().UnixNano()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropLapsed(now)
	for i, entry := range b.entries {
		if ConstantTimeEqual(entry.token, token) != nil {
			continue
		}
		b.entries = append(b.entries[:i], b.entries[i+1:]...)
		return entry.subject, true
	}
	return "", false
}

// dropLapsed removes expired entries on access rather than owning a timer.
// Caller holds b.mu.
func (b *ticketBook) dropLapsed(nowNanos int64) {
	if b.ttl <= 0 {
		return
	}
	kept := b.entries[:0]
	for _, entry := range b.entries {
		if entry.expiresAtNanos > nowNanos {
			kept = append(kept, entry)
		}
	}
	b.entries = kept
}

// hasLive reports whether an unspent ticket can still be redeemed.
func (b *ticketBook) hasLive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropLapsed(b.now().UnixNano())
	return len(b.entries) != 0
}

// outstanding reports how many tickets are minted but unspent. For tests,
// and for the bound's own assertions.
func (b *ticketBook) outstanding() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

package trackerapp

import (
	"time"

	"agent-overflow/internal/store"
)

// The sync cadence, named in one place. A board polls its delta while a
// client shows it and not at all otherwise; its reconcile and structure
// ride the delta tick when their time comes; its history runs when the
// mirror holds issues due one. A connection's open tickets are read
// together by its round while a client shows one of them, and a ticket
// reads at once when another read stores a changed card of it. Linked
// issues poll while a connection is on and a client is connected. A failed
// poll retries on pump.Retry's doubling schedule from RetryBase up to the
// poll's interval; a rate limit waits for its release.
//
// A board's delta and a connection's ticket round poll every JiraPollEvery
// on Jira and every LinearPollEvery on Linear: Jira's token traffic has no
// hourly quota, and Linear allows 2,500 requests per user an hour.
const (
	JiraPollEvery   = 10 * time.Second
	LinearPollEvery = 45 * time.Second
	ReconcileEvery  = 10 * time.Minute
	StructureEvery  = 30 * time.Minute
	LinkedEvery     = 5 * time.Minute
	RetryBase       = 5 * time.Second
)

// pollEvery is the board delta and open ticket interval of a connection of
// kind.
func pollEvery(kind string) time.Duration {
	if kind == store.TrackerKindJira {
		return JiraPollEvery
	}
	return LinearPollEvery
}

// Cadence is the service's sync intervals. A zero Delta or Ticket is the
// poll interval of the connection's kind (pollEvery), and a zero Poker is
// PokerEvery (poker.go); any other zero field is the constant of the same
// name. Tests shorten them.
type Cadence struct {
	Delta, Reconcile, Structure, Ticket, Linked, RetryBase, Poker time.Duration
}

// withDefaults is c with each zero field that has one constant set to it.
func (c Cadence) withDefaults() Cadence {
	or := func(d, def time.Duration) time.Duration {
		if d > 0 {
			return d
		}
		return def
	}
	c.Reconcile, c.Structure = or(c.Reconcile, ReconcileEvery), or(c.Structure, StructureEvery)
	c.Linked, c.RetryBase = or(c.Linked, LinkedEvery), or(c.RetryBase, RetryBase)
	return c
}

// delta is a board's delta interval on a connection of kind.
func (c Cadence) delta(kind string) time.Duration {
	if c.Delta > 0 {
		return c.Delta
	}
	return pollEvery(kind)
}

// ticket is an open ticket's interval on a connection of kind.
func (c Cadence) ticket(kind string) time.Duration {
	if c.Ticket > 0 {
		return c.Ticket
	}
	return pollEvery(kind)
}

// poker is a followed ticket's planning poker interval.
func (c Cadence) poker() time.Duration {
	if c.Poker > 0 {
		return c.Poker
	}
	return PokerEvery
}

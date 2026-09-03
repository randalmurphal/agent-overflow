package identity

import "sync"

// The device-proof replay guard.
//
// A proof is single-use, and this is what makes that true: an in-memory
// set of the `jti` values already spent, bounded by the freshness window
// (docs/specs/remote-access.md §14 — "the DPoP replay guard is an
// in-memory TTL map bounded by the window with a hard size cap, not a file
// per proof").
//
// Why memory and not the database. A file or a row per proof would burn an
// inode and a syscall per REQUEST and would need its own collection pass,
// on a path whose entire budget is tens of microseconds. The cost of
// memory is that a restart clears the set, so a proof minted just before
// one could be presented twice across it. The spec accepts that and says
// why: the window is two minutes, a restart is not a routine event, and a
// replay inside it still needs a proof produced by the device's own
// private key.
//
// Why two maps rather than one map of expiry times. A per-entry timestamp
// costs a scan to collect and a comparison to read; rotating two sets
// costs one comparison per admit and frees a whole generation at once with
// no scan at all. An entry therefore lives between one and two windows —
// always at least the window, which is the only property correctness
// needs.

// proofReplayWindowMillis is how long a spent `jti` is remembered.
//
// It must be at least as long as a proof can stay ACCEPTABLE, which is
// wider than the freshness window itself: a proof stamped `iatMs = T` is
// admitted for now anywhere in [T-freshness, T+freshness], so one proof is
// live across two freshness windows. Remembering for less would let the
// same proof be spent twice inside its own lifetime, which is precisely
// what this exists to stop.
var proofReplayWindowMillis = 2 * deviceProofFreshness.Milliseconds()

// maxTrackedProofs is the hard size cap the spec calls for. Reaching it
// rotates the generation early rather than refusing anything.
//
// The alternative — refusing a fresh proof because the set is full — would
// turn a burst into a sign-out for every real device on the machine, and
// it would do so at exactly the moment the guard is least useful. Early
// rotation instead shortens the remembered window, and what that costs is
// bounded by who can cause it: every entry here is a proof that ALREADY
// verified under a device's private key, so producing enough of them to
// reach this cap inside one window means already holding the key the guard
// is protecting.
//
// 8192 against a two-minute window is roughly 68 verified proofs per
// second, sustained. A paired device mints one per credential request —
// single digits per minute — so no set of devices a person owns approaches
// it.
const maxTrackedProofs = 8192

// proofReplay is the spent-proof set. The zero value is not usable; use
// newProofReplay.
type proofReplay struct {
	mu sync.Mutex
	// rotateAt is when current becomes previous, in Unix milliseconds.
	rotateAt int64
	// current and previous are the two generations. A lookup consults
	// both; a rotation drops previous entirely, which is the collection.
	current  map[string]struct{}
	previous map[string]struct{}
}

func newProofReplay() *proofReplay {
	return &proofReplay{current: make(map[string]struct{})}
}

// admit spends a proof identifier, reporting whether it had not been seen.
//
// False means replayed. The identifier is recorded only on a true answer,
// but every caller reaches this after the proof has fully verified — see
// verifyDeviceProof, which runs this last on purpose. Recording earlier
// would let an unverifiable presentation consume the identifier of a proof
// the real device is about to use.
func (g *proofReplay) admit(jti string, now int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if now >= g.rotateAt || len(g.current) >= maxTrackedProofs {
		g.previous = g.current
		g.current = make(map[string]struct{})
		g.rotateAt = now + proofReplayWindowMillis
	}
	if _, seen := g.current[jti]; seen {
		return false
	}
	if _, seen := g.previous[jti]; seen {
		return false
	}
	g.current[jti] = struct{}{}
	return true
}

// tracked reports how many identifiers are held, for tests and for the
// bound this file argues. Not exported: the count is an implementation
// property, not a fact any caller should branch on.
func (g *proofReplay) tracked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.current) + len(g.previous)
}

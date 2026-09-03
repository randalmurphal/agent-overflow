package transport

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A per-peer request budget for the credential surfaces.
//
// What this bounds is WORK, not guessing: the launch token is 256 random
// bits and the scoped tokens are minted per provider process, so no
// achievable request rate makes guessing one plausible. What a request
// rate does reach is the backend's own cost — a ticket exchange writes a
// cookie and consumes an entry from a bounded table, a `/pageurl` call
// mints a ticket and evicts an older one, a scoped call runs a real
// method. A peer that repeats any of those without limit spends the
// backend's budget, and on the `/pageurl` route specifically it evicts
// tickets other pages are about to present.
//
// Deliberately NOT applied to `/healthz` or the SPA assets. A readiness
// probe answers before the app exists and is polled by design, and a page
// load is dozens of asset requests in one burst — limiting either would
// break ordinary use to bound work that is already trivial.
//
// Loopback peers are limited on the same terms as everyone else. Exempting
// them would leave the whole path unexercised in development and in the
// e2e suite, so its first real run would be its first LAN bind. The
// budgets are set so ordinary local use — a reconnect ladder refetching
// its manifest, a Playwright worker opening pages, a scripted CLI — never
// reaches them.

// rateLimit is one budget: a burst that may be spent at once, and the
// sustained rate it refills at.
type rateLimit struct {
	// burst is the maximum tokens a peer can hold. One request costs one.
	burst float64
	// perSecond is the refill rate.
	perSecond float64
}

// The credential surfaces. Separate budgets, and separate tables, so a
// burst on one cannot spend another's: a reconnect ladder hammering
// `/bootstrap.json` must not lock the same user's CLI out of `/rpc`.
var (
	// bootstrapRateLimit covers the ticket exchange. One page load costs
	// one, and a reconnect refetches the manifest, so the burst is sized
	// for a flapping connection rather than for a single page.
	bootstrapRateLimit = rateLimit{burst: 120, perSecond: 4}
	// pageURLRateLimit covers ticket minting. Every caller is a
	// navigation somebody asked for: a reload keybinding, `ao-harness
	// open`, an e2e worker opening a page.
	pageURLRateLimit = rateLimit{burst: 120, perSecond: 4}
	// scopedRPCRateLimit covers the CLI route, whose legitimate callers
	// are scripts. The budget is the loosest of the three because the
	// surface is loopback-only and its methods are already narrowed to
	// the scoped table.
	scopedRPCRateLimit = rateLimit{burst: 300, perSecond: 30}
	// authRateLimit covers the three device-facing credential routes
	// (/auth/pair, /auth/token, /auth/ticket) TOGETHER, on one table:
	// they are alternative ways for the same peer to ask this backend for
	// a credential, so a peer that has spent its budget on one must not
	// simply move to the next. The tightest of the four budgets, because
	// legitimate use is rare — a pairing happens when a person adds a
	// device, a rotation once per access window, and a ticket once per
	// connect — while this is the one surface reachable without a
	// credential at all.
	authRateLimit = rateLimit{burst: 30, perSecond: 1}
)

// maxTrackedPeers bounds the table. Reaching it takes that many distinct
// source addresses all spending at once, since an idle peer is dropped as
// soon as its budget refills — see allow.
const maxTrackedPeers = 4096

// peerBucket is one peer's remaining budget. A value, not a pointer: the
// happy path rewrites it in place under the lock, which allocates nothing,
// and 24 bytes inline beats a pointer chase plus a heap object per peer.
type peerBucket struct {
	// tokens remaining, refilled lazily on access. There is no timer and
	// no sweep goroutine; a bucket nobody touches costs nothing until the
	// next insert notices it is full.
	tokens float64
	// last is when tokens was computed, in Unix nanoseconds.
	last int64
	// noted records that this peer's exhaustion has already been logged.
	// Cleared when the budget refills, so the log carries one line per dry
	// spell rather than one per refused request — a flood must not be able
	// to flood the log with its own evidence.
	noted bool
}

// full reports whether this bucket is indistinguishable from a peer that
// was never seen, which is what makes it safe to drop.
func (b peerBucket) full(limit rateLimit) bool { return b.tokens >= limit.burst }

// refill credits the time since this bucket was last read. Lazy on
// purpose: there is no timer and no sweep goroutine, so a peer nobody is
// asking about costs nothing at all.
func (b *peerBucket) refill(limit rateLimit, now int64) {
	elapsed := now - b.last
	if elapsed <= 0 {
		return
	}
	b.tokens += float64(elapsed) / float64(time.Second) * limit.perSecond
	if b.tokens > limit.burst {
		b.tokens = limit.burst
	}
	b.last = now
}

// rateLimiter is a per-peer token bucket over one route.
//
// The zero value is not usable; construct with newRateLimiter. Not safe to
// copy after first use.
type rateLimiter struct {
	// name identifies the surface in the log line. Fixed at construction.
	name  string
	limit rateLimit
	// now is injectable so tests can move time without sleeping. Reading
	// the clock is the only work done outside the lock.
	now func() time.Time

	mu    sync.Mutex
	peers map[string]peerBucket
	// fullNotedAt is when the table-full refusal was last logged, in Unix
	// nanoseconds. That path has no bucket to mark, so it is throttled by
	// time instead — a flood must not be able to flood the log with its
	// own evidence.
	fullNotedAt int64
}

// tableFullLogEvery throttles the one refusal that is about the limiter's
// own capacity rather than about the peer being refused.
const tableFullLogEvery = 10 * time.Second

func newRateLimiter(name string, limit rateLimit) *rateLimiter {
	return &rateLimiter{
		name:  name,
		limit: limit,
		now:   time.Now,
		peers: make(map[string]peerBucket),
	}
}

// allow spends one token for peer and reports whether the request may
// proceed. Also returns how long the caller should wait before retrying,
// which is zero when allowed.
//
// The admitted path is: one clock read, one map lookup, some float
// arithmetic, one map store on an existing key. No allocation.
func (l *rateLimiter) allow(peer string) (bool, time.Duration) {
	now := l.now().UnixNano()

	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, tracked := l.peers[peer]
	if !tracked {
		if len(l.peers) >= maxTrackedPeers && !l.dropIdleLocked(now) {
			// Every tracked peer is actively spending and the table is
			// full. Admitting an untracked one would mean not limiting it
			// at all, which is the opposite of what this is for. Reaching
			// here needs thousands of distinct addresses spending within
			// seconds of each other.
			l.noteTableFullLocked(now, peer)
			return false, l.retryAfter()
		}
		bucket = peerBucket{tokens: l.limit.burst, last: now}
	} else {
		bucket.refill(l.limit, now)
		// Back at full means this peer has been quiet for a whole burst
		// window. That ends the dry spell the log already recorded, so the
		// next exhaustion is worth a line again. Clearing it any sooner —
		// on any successful request — would put one line per refill period
		// in the log for a peer parked at the edge of its budget.
		if bucket.full(l.limit) {
			bucket.noted = false
		}
	}

	if bucket.tokens < 1 {
		if !bucket.noted {
			bucket.noted = true
			log.Printf("transport: rate limit reached on %s for peer %s", l.name, peer)
		}
		// Stamp the bucket even though nothing was spent, so the refill
		// clock keeps running while a peer is being refused; otherwise a
		// peer that never stops asking would never accumulate credit.
		l.peers[peer] = bucket
		return false, l.retryAfter()
	}

	bucket.tokens--
	l.peers[peer] = bucket
	return true, 0
}

// noteTableFullLocked logs the capacity refusal, throttled by time because
// it has no bucket to mark. Names the peer that was turned away, since
// that is the only attribution available. Called with l.mu held.
func (l *rateLimiter) noteTableFullLocked(now int64, peer string) {
	if l.fullNotedAt != 0 && now-l.fullNotedAt < int64(tableFullLogEvery) {
		return
	}
	l.fullNotedAt = now
	log.Printf("transport: rate limit peer table full on %s (%d peers); refused %s",
		l.name, len(l.peers), peer)
}

// dropIdleLocked removes every peer whose budget has fully refilled, which
// carries no information a fresh entry would not. Reports whether it freed
// anything. Called with l.mu held, and only from the insert path at
// capacity, so its O(n) pass is amortized across maxTrackedPeers inserts.
func (l *rateLimiter) dropIdleLocked(now int64) bool {
	freed := false
	for peer, bucket := range l.peers {
		bucket.refill(l.limit, now)
		if bucket.full(l.limit) {
			delete(l.peers, peer)
			freed = true
			continue
		}
		// Keep the credit this pass computed. Discarding it would make a
		// sweep cost the peer the time that elapsed during it.
		l.peers[peer] = bucket
	}
	return freed
}

// retryAfter is how long one token takes to refill. Reported to the client
// so a well-behaved caller backs off by the right amount instead of
// guessing, and so a retry that is guaranteed to be refused again is not
// attempted.
func (l *rateLimiter) retryAfter() time.Duration {
	if l.limit.perSecond <= 0 {
		return time.Second
	}
	wait := time.Duration(float64(time.Second) / l.limit.perSecond)
	if wait < time.Millisecond {
		return time.Millisecond
	}
	return wait
}

// tracked reports how many peers hold state. For tests, and for the bound
// this type promises.
func (l *rateLimiter) tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.peers)
}

// peerKey reduces a RemoteAddr to the host that identifies the peer. The
// port changes per connection, so keying on the whole address would give
// every request its own bucket and limit nothing at all.
//
// Hand-parsed rather than net.SplitHostPort, which allocates an *AddrError
// on any input it cannot read. The key is derived on every request, so
// that allocation would be in the budget — and an unreadable address is
// exactly the input that must not cost more than a readable one.
//
// Every return is a sub-slice of the input, so this allocates nothing for
// any input at all. Total by construction: an address it cannot split is
// its own key, because one bucket for the whole string is a limit and no
// bucket is not. Pinned against net.SplitHostPort for well-formed input by
// TestPeerKeyMatchesSplitHostPort.
func peerKey(remoteAddr string) string {
	if len(remoteAddr) > 0 && remoteAddr[0] == '[' {
		if end := strings.IndexByte(remoteAddr, ']'); end > 0 {
			return remoteAddr[1:end]
		}
		return remoteAddr
	}
	colon := strings.IndexByte(remoteAddr, ':')
	// No colon means no port. A SECOND colon means an unbracketed IPv6
	// literal, where cutting at the last one would collapse every address
	// in a subnet onto the same key.
	if colon < 0 || strings.IndexByte(remoteAddr[colon+1:], ':') >= 0 {
		return remoteAddr
	}
	return remoteAddr[:colon]
}

// rateLimited wraps a handler with a per-peer budget.
//
// The refusal is 429 with Retry-After, NOT the 404 the credential channel
// uses. That difference is deliberate and load-bearing: the SPA latches
// terminal `unauthorized` state on a 401/403/404 from the manifest and
// stops its reconnect ladder (§ Credentials and refusal shapes). A client
// that merely burst would then be told its credential is dead and would
// stop trying — the same class of mistake as answering a readiness probe
// with a 404. A rate-limit refusal is transient and has to look like it.
func rateLimited(limiter *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	if limiter == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, retryAfter := limiter.allow(peerKey(r.RemoteAddr)); !ok {
			seconds := int(retryAfter.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

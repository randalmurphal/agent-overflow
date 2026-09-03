package transport

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClock is a hand-advanced clock, so the refill behavior is tested
// without a test that sleeps.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Unix(1_700_000_000, 0)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func newTestLimiter(limit rateLimit) (*rateLimiter, *testClock) {
	clock := newTestClock()
	limiter := newRateLimiter("/test", limit)
	limiter.now = clock.now
	return limiter, clock
}

func TestRateLimiterSpendsTheBurstThenRefuses(t *testing.T) {
	limiter, _ := newTestLimiter(rateLimit{burst: 3, perSecond: 1})
	for i := range 3 {
		if ok, _ := limiter.allow("10.0.0.1"); !ok {
			t.Fatalf("request %d was refused inside the burst", i+1)
		}
	}
	ok, retryAfter := limiter.allow("10.0.0.1")
	if ok {
		t.Fatal("a fourth request was admitted past a burst of 3")
	}
	if retryAfter != time.Second {
		t.Fatalf("Retry-After is %s, want the time one token takes to refill", retryAfter)
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	limiter, clock := newTestLimiter(rateLimit{burst: 2, perSecond: 4})
	limiter.allow("10.0.0.1")
	limiter.allow("10.0.0.1")
	if ok, _ := limiter.allow("10.0.0.1"); ok {
		t.Fatal("the budget was not spent")
	}
	// A quarter second is exactly one token at 4/s.
	clock.advance(250 * time.Millisecond)
	if ok, _ := limiter.allow("10.0.0.1"); !ok {
		t.Fatal("a refilled token was not honored")
	}
	if ok, _ := limiter.allow("10.0.0.1"); ok {
		t.Fatal("more than the refilled token was granted")
	}
	// Refill never exceeds the burst, however long the peer waits.
	clock.advance(time.Hour)
	for i := range 2 {
		if ok, _ := limiter.allow("10.0.0.1"); !ok {
			t.Fatalf("request %d after a long idle was refused", i+1)
		}
	}
	if ok, _ := limiter.allow("10.0.0.1"); ok {
		t.Fatal("an idle peer accumulated more than the burst")
	}
}

// TestRateLimiterKeepsRefillingWhileRefusing — a peer that never stops
// asking must still accumulate credit, or one that bursts once would be
// refused forever.
func TestRateLimiterKeepsRefillingWhileRefusing(t *testing.T) {
	limiter, clock := newTestLimiter(rateLimit{burst: 1, perSecond: 1})
	limiter.allow("10.0.0.1")
	for range 20 {
		clock.advance(20 * time.Millisecond)
		limiter.allow("10.0.0.1")
	}
	clock.advance(time.Second)
	if ok, _ := limiter.allow("10.0.0.1"); !ok {
		t.Fatal("a peer that kept asking never regained its budget")
	}
}

func TestRateLimiterBudgetsAreIndependentPerPeer(t *testing.T) {
	limiter, _ := newTestLimiter(rateLimit{burst: 1, perSecond: 1})
	if ok, _ := limiter.allow("10.0.0.1"); !ok {
		t.Fatal("first peer refused")
	}
	if ok, _ := limiter.allow("10.0.0.1"); ok {
		t.Fatal("first peer got a second token")
	}
	if ok, _ := limiter.allow("10.0.0.2"); !ok {
		t.Fatal("one peer's burst refused another peer")
	}
}

// TestRateLimiterDropsIdlePeers — the table is bounded by peers that are
// actively spending, not by every address ever seen. A peer whose budget
// refilled carries nothing a fresh entry would not.
func TestRateLimiterDropsIdlePeers(t *testing.T) {
	limiter, clock := newTestLimiter(rateLimit{burst: 1, perSecond: 1})
	for i := range maxTrackedPeers {
		limiter.allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	if got := limiter.tracked(); got != maxTrackedPeers {
		t.Fatalf("tracked %d peers, want %d", got, maxTrackedPeers)
	}
	// Everyone goes quiet long enough to refill, then one new peer arrives.
	clock.advance(time.Minute)
	if ok, _ := limiter.allow("10.99.99.99"); !ok {
		t.Fatal("a new peer was refused while every tracked peer was idle")
	}
	if got := limiter.tracked(); got != 1 {
		t.Fatalf("tracked %d peers after the sweep, want just the new one", got)
	}
}

// TestRateLimiterRefusesRatherThanGrowingPastItsBound — when every
// tracked peer is actively spending, admitting an untracked one would
// mean not limiting it at all. The table is the bound, and it holds.
func TestRateLimiterRefusesRatherThanGrowingPastItsBound(t *testing.T) {
	limiter, _ := newTestLimiter(rateLimit{burst: 1, perSecond: 1})
	for i := range maxTrackedPeers {
		limiter.allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	if ok, _ := limiter.allow("10.99.99.99"); ok {
		t.Fatal("an untracked peer was admitted with the table full of active peers")
	}
	if got := limiter.tracked(); got > maxTrackedPeers {
		t.Fatalf("tracked %d peers, past the bound of %d", got, maxTrackedPeers)
	}
}

// TestRateLimiterSweepDoesNotCostThePeerItsCredit — a sweep recomputes
// every bucket. Discarding what it computed would charge a peer for the
// time that passed during someone else's insert.
func TestRateLimiterSweepDoesNotCostThePeerItsCredit(t *testing.T) {
	limiter, clock := newTestLimiter(rateLimit{burst: 4, perSecond: 1})
	limiter.allow("10.0.0.1")
	limiter.allow("10.0.0.1")
	limiter.allow("10.0.0.1")
	limiter.allow("10.0.0.1")
	clock.advance(2 * time.Second)
	// Fill the table so the next insert sweeps.
	for i := range maxTrackedPeers {
		limiter.allow(fmt.Sprintf("10.1.%d.%d", i/256, i%256))
	}
	for i := range 2 {
		if ok, _ := limiter.allow("10.0.0.1"); !ok {
			t.Fatalf("token %d refilled before the sweep was lost by it", i+1)
		}
	}
}

// TestRateLimiterAdmitsWithoutAllocating is the budget the brief sets:
// one map lookup and nothing else on the path every real request takes.
func TestRateLimiterAdmitsWithoutAllocating(t *testing.T) {
	limiter, _ := newTestLimiter(rateLimit{burst: 1e9, perSecond: 1e9})
	limiter.allow("10.0.0.1") // Insert first; the miss path allocates once.
	allocs := testing.AllocsPerRun(200, func() {
		limiter.allow("10.0.0.1")
	})
	if allocs != 0 {
		t.Fatalf("the admitted path allocates %.1f times per request", allocs)
	}
}

// TestPeerKeyDoesNotAllocate — the key is derived per request, so it is
// part of the same budget.
func TestPeerKeyDoesNotAllocate(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:54321", "[::1]:54321", "not-an-address"} {
		allocs := testing.AllocsPerRun(200, func() { _ = peerKey(addr) })
		if allocs != 0 {
			t.Fatalf("peerKey(%q) allocates %.1f times", addr, allocs)
		}
	}
}

// TestPeerKeyMatchesSplitHostPort — peerKey is hand-parsed to keep the
// unreadable-address path out of the allocation budget, so it has to
// agree with the standard splitter on everything net/http actually
// produces.
func TestPeerKeyMatchesSplitHostPort(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:54321",
		"10.0.0.1:80",
		"192.168.1.255:65535",
		"[::1]:54321",
		"[fe80::1%25eth0]:443",
		"[2001:db8::8a2e:370:7334]:8080",
	} {
		want, _, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("SplitHostPort(%q): %v", addr, err)
		}
		if got := peerKey(addr); got != want {
			t.Errorf("peerKey(%q) = %q, SplitHostPort says %q", addr, got, want)
		}
	}
}

// TestPeerKeyKeepsUnbracketedIPv6Distinct — cutting at a colon in a bare
// IPv6 literal would map every address in a subnet onto one bucket, so a
// single peer's burst would refuse its neighbours.
func TestPeerKeyKeepsUnbracketedIPv6Distinct(t *testing.T) {
	if a, b := peerKey("fe80::1"), peerKey("fe80::2"); a == b {
		t.Fatalf("two IPv6 peers collapsed onto the key %q", a)
	}
	if got := peerKey("::1"); got != "::1" {
		t.Fatalf("peerKey(\"::1\") = %q", got)
	}
}

// TestPeerKeyIgnoresThePort — the port changes per connection, so keying
// on the whole address would give every request its own bucket and limit
// nothing at all.
func TestPeerKeyIgnoresThePort(t *testing.T) {
	if a, b := peerKey("10.0.0.1:1111"), peerKey("10.0.0.1:2222"); a != b {
		t.Fatalf("two connections from one host keyed as %q and %q", a, b)
	}
	if got := peerKey("[fe80::1]:443"); got != "fe80::1" {
		t.Fatalf("peerKey on an IPv6 address = %q", got)
	}
	// An address net.SplitHostPort cannot read is still a usable key: one
	// bucket for the whole string is a limit, and no bucket is not.
	if got := peerKey("weird"); got != "weird" {
		t.Fatalf("peerKey on an unparseable address = %q", got)
	}
}

func TestRateLimiterIsSafeUnderConcurrentPeers(t *testing.T) {
	limiter, _ := newTestLimiter(rateLimit{burst: 100, perSecond: 1000})
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			peer := fmt.Sprintf("10.0.0.%d", worker%3)
			for range 500 {
				if ok, _ := limiter.allow(peer); ok {
					admitted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	// Three peers, a burst of 100 each, and a frozen clock: no more than
	// 300 can have been admitted however the goroutines interleaved.
	if got := admitted.Load(); got > 300 {
		t.Fatalf("%d requests admitted; the per-peer budget was not enforced under contention", got)
	}
}

// TestRateLimitedRefusalIsTransientNotTerminal is the one that matters
// most in production: the SPA latches terminal "credential is dead" state
// on a 401/403/404 from the manifest and stops reconnecting. A client
// that merely burst must not be told that.
func TestRateLimitedRefusalIsTransientNotTerminal(t *testing.T) {
	limiter, _ := newTestLimiter(rateLimit{burst: 1, perSecond: 2})
	var served atomic.Int64
	handler := rateLimited(limiter, func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/bootstrap.json", nil)
		req.RemoteAddr = "10.0.0.1:5555"
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	if got := request().Code; got != http.StatusOK {
		t.Fatalf("the first request answered %d", got)
	}
	refused := request()
	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("a peer over budget answered %d, want 429", refused.Code)
	}
	for _, terminal := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		if refused.Code == terminal {
			t.Fatalf("the refusal used %d, which latches the client's terminal state", terminal)
		}
	}
	if got := refused.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After is %q; a client cannot back off by the right amount", got)
	}
	if got := served.Load(); got != 1 {
		t.Fatalf("the wrapped handler ran %d times; a refused request must not reach it", got)
	}
}

// TestHealthAndAssetsAreNotRateLimited — a readiness probe is polled by
// design and one page load is dozens of asset requests. Limiting either
// breaks ordinary use to bound work that is already trivial.
func TestHealthAndAssetsAreNotRateLimited(t *testing.T) {
	f := newIntegrationFixture(t)
	client := &http.Client{Timeout: 3 * time.Second}
	for i := range maxTrackedPeers/16 + 50 {
		resp, err := client.Get("http://" + f.addr + HealthPath)
		if err != nil {
			t.Fatalf("healthz request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("healthz was rate limited after %d probes", i)
		}
	}
}

// TestRateLimitersAreSeparatePerSurface — a reconnect ladder hammering
// the manifest must not lock the same user's CLI out of /rpc.
func TestRateLimitersAreSeparatePerSurface(t *testing.T) {
	srv, err := New(Config{Dispatcher: NewDispatcher(), EventBus: NewEventBus(4), Token: "t"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	limiters := []*rateLimiter{srv.bootstrapLimit, srv.pageURLLimit, srv.scopedRPCLimit}
	for i, limiter := range limiters {
		if limiter == nil {
			t.Fatalf("limiter %d was not built", i)
		}
		for j, other := range limiters {
			if i != j && limiter == other {
				t.Fatalf("limiters %d and %d share a budget", i, j)
			}
		}
	}
	// Spending one surface's whole budget leaves the others untouched.
	for range int(bootstrapRateLimit.burst) + 5 {
		srv.bootstrapLimit.allow("10.0.0.1")
	}
	if ok, _ := srv.bootstrapLimit.allow("10.0.0.1"); ok {
		t.Fatal("the bootstrap budget was not spent")
	}
	if ok, _ := srv.pageURLLimit.allow("10.0.0.1"); !ok {
		t.Fatal("spending the bootstrap budget refused a /pageurl request")
	}
	if ok, _ := srv.scopedRPCLimit.allow("10.0.0.1"); !ok {
		t.Fatal("spending the bootstrap budget refused a /rpc request")
	}
}

// TestNilLimiterPassesThrough — a Server built by a test or a future
// caller without limiters must still serve, rather than refusing
// everything or panicking on the first request.
func TestNilLimiterPassesThrough(t *testing.T) {
	var served atomic.Int64
	handler := rateLimited(nil, func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
	})
	for range 10 {
		req := httptest.NewRequest(http.MethodGet, "/pageurl", nil)
		req.RemoteAddr = "10.0.0.1:5555"
		handler(httptest.NewRecorder(), req)
	}
	if got := served.Load(); got != 10 {
		t.Fatalf("a nil limiter served %d of 10 requests", got)
	}
}

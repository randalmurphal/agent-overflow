package forgeapi

import (
	"errors"
	"testing"
	"time"
)

func TestGates(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1_700_000_000, 0)
	key := gateKey{host: "github.com", pool: "core", fingerprint: "fp"}
	mustLease := func(t *testing.T, g *gates, interactive bool, now time.Time) lease {
		t.Helper()
		l, err := g.check(key, interactive, now)
		if err != nil {
			t.Fatalf("check at %v refused: %v", now.Sub(t0), err)
		}
		return l
	}
	refused := func(t *testing.T, g *gates, interactive bool, now time.Time) *RateLimitedError {
		t.Helper()
		_, err := g.check(key, interactive, now)
		var limited *RateLimitedError
		if !errors.As(err, &limited) {
			t.Fatalf("check at %v = %v, want *RateLimitedError", now.Sub(t0), err)
		}
		return limited
	}

	t.Run("a burst of in-flight 429s doubles once", func(t *testing.T) {
		g := newGates()
		leases := []lease{mustLease(t, g, false, t0), mustLease(t, g, false, t0), mustLease(t, g, false, t0)}
		first := g.limited(leases[0], time.Time{}, t0)
		if !first.Equal(t0.Add(gateFallback)) {
			t.Fatalf("first closure until %v, want +60s", first.Sub(t0))
		}
		for _, l := range leases[1:] {
			if until := g.limited(l, time.Time{}, t0.Add(time.Second)); !until.Equal(t0.Add(gateFallback + time.Second)) {
				t.Fatalf("in-flight refusal moved until to %v, want an extension to +61s", until.Sub(t0))
			}
		}
		if g.byKey[key].closures != 1 {
			t.Fatalf("closures = %d after one burst, want 1", g.byKey[key].closures)
		}
		after := t0.Add(2 * time.Minute)
		next := mustLease(t, g, false, after)
		if until := g.limited(next, time.Time{}, after); !until.Equal(after.Add(2 * gateFallback)) {
			t.Fatalf("second closure until %v, want +120s (doubled once)", until.Sub(after))
		}
	})

	t.Run("fallback doubling stops at 15 minutes", func(t *testing.T) {
		g := newGates()
		now := t0
		var until time.Time
		for range 8 {
			until = g.limited(mustLease(t, g, false, now), time.Time{}, now)
			now = until
		}
		last := g.limited(mustLease(t, g, false, now), time.Time{}, now)
		if last.Sub(now) != gateFallbackMax {
			t.Fatalf("closure after 9 = %v, want the 15 minute cap", last.Sub(now))
		}
	})

	t.Run("success after Until resets the doubling", func(t *testing.T) {
		g := newGates()
		g.limited(mustLease(t, g, false, t0), time.Time{}, t0)
		reopened := t0.Add(gateFallback)
		l := mustLease(t, g, false, reopened)
		g.succeeded(l, reopened)
		if g.byKey[key].closures != 0 {
			t.Fatalf("closures = %d after a success, want 0", g.byKey[key].closures)
		}
		if until := g.limited(mustLease(t, g, false, reopened), time.Time{}, reopened); until.Sub(reopened) != gateFallback {
			t.Fatalf("closure after reset = %v, want 60s", until.Sub(reopened))
		}
	})

	t.Run("a stale lease's success does not reset the doubling", func(t *testing.T) {
		g := newGates()
		stale := mustLease(t, g, false, t0)
		g.limited(mustLease(t, g, false, t0), time.Time{}, t0)
		g.succeeded(stale, t0.Add(time.Hour))
		if g.byKey[key].closures != 1 {
			t.Fatalf("closures = %d, want 1: the lease predates the closure", g.byKey[key].closures)
		}
	})

	t.Run("a new closure never shortens an open one", func(t *testing.T) {
		g := newGates()
		g.limited(mustLease(t, g, false, t0), t0.Add(10*time.Minute), t0)
		late := lease{key: key, gen: g.byKey[key].gen}
		if until := g.limited(late, t0.Add(time.Minute), t0); !until.Equal(t0.Add(10 * time.Minute)) {
			t.Fatalf("until = %v, want the open 10 minute closure", until.Sub(t0))
		}
	})

	t.Run("interactive honors a closed gate", func(t *testing.T) {
		g := newGates()
		g.limited(mustLease(t, g, true, t0), t0.Add(time.Minute), t0)
		limited := refused(t, g, true, t0.Add(time.Second))
		if limited.Reserve || !limited.Until.Equal(t0.Add(time.Minute)) {
			t.Fatalf("limited = %+v", limited)
		}
		mustLease(t, g, true, t0.Add(time.Minute))
	})

	t.Run("background refused under the reserve, interactive spends it to one", func(t *testing.T) {
		g := newGates()
		reset := t0.Add(30 * time.Minute)
		g.observe(key, &RateLimit{Pool: "core", Limit: 5000, Remaining: 499, Reset: reset})
		limited := refused(t, g, false, t0)
		if !limited.Reserve || !limited.Until.Equal(reset) {
			t.Fatalf("background refusal = %+v, want Reserve until reset", limited)
		}
		mustLease(t, g, true, t0)
		g.observe(key, &RateLimit{Pool: "core", Limit: 5000, Remaining: 1, Reset: reset})
		mustLease(t, g, true, t0)
		g.observe(key, &RateLimit{Pool: "core", Limit: 5000, Remaining: 0, Reset: reset})
		if limited := refused(t, g, true, t0); limited.Reserve {
			t.Fatalf("exhaustion reported as the reserve: %+v", limited)
		}
		// A snapshot whose reset has passed no longer applies.
		mustLease(t, g, false, reset)
	})

	t.Run("at the reserve boundary background still runs", func(t *testing.T) {
		g := newGates()
		g.observe(key, &RateLimit{Pool: "core", Limit: 5000, Remaining: 500, Reset: t0.Add(time.Hour)})
		mustLease(t, g, false, t0)
	})

	t.Run("latest reset wins, then the latest response", func(t *testing.T) {
		g := newGates()
		newer, older := t0.Add(time.Hour), t0.Add(time.Minute)
		g.observe(key, &RateLimit{Limit: 5000, Remaining: 4000, Reset: newer})
		g.observe(key, &RateLimit{Limit: 5000, Remaining: 10, Reset: older})
		if q := g.byKey[key].quota; q.Remaining != 4000 {
			t.Fatalf("an older window overwrote the snapshot: %+v", q)
		}
		g.observe(key, &RateLimit{Limit: 5000, Remaining: 3990, Reset: newer})
		if q := g.byKey[key].quota; q.Remaining != 3990 {
			t.Fatalf("the latest response in the same window was dropped: %+v", q)
		}
	})

	t.Run("pools are separate", func(t *testing.T) {
		g := newGates()
		g.limited(mustLease(t, g, false, t0), time.Time{}, t0)
		if _, err := g.check(gateKey{host: key.host, pool: "graphql", fingerprint: key.fingerprint}, false, t0); err != nil {
			t.Fatalf("a closed core gate stalled graphql: %v", err)
		}
	})

	t.Run("dropHost keeps only the current token's gates", func(t *testing.T) {
		g := newGates()
		g.get(key)
		g.get(gateKey{host: key.host, pool: "core", fingerprint: "new"})
		g.get(gateKey{host: "other", pool: "core", fingerprint: "fp"})
		g.dropHost(key.host, "new")
		if _, ok := g.byKey[key]; ok || len(g.byKey) != 2 {
			t.Fatalf("gates after drop = %v", g.byKey)
		}
	})
}

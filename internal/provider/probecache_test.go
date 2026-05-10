package provider

import (
	"testing"
	"time"
)

// The shared ProbeCache is exercised end-to-end via the Claude and Codex
// probe test suites; this file isolates the eviction surface introduced
// for the user-initiated recheck path. A bug in Invalidate (delete the
// wrong key, no-op silently when the key exists) would mask itself in
// the Recheck flow because the next Set immediately repopulates — only
// a dedicated test catches it.

func TestProbeCacheInvalidateRemovesEntry(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	cache.Set("/usr/bin/claude", AccountInfo{SubscriptionType: "Claude Max"})

	if _, ok := cache.Get("/usr/bin/claude"); !ok {
		t.Fatal("precondition: expected cache hit before invalidate")
	}

	cache.Invalidate("/usr/bin/claude")

	if _, ok := cache.Get("/usr/bin/claude"); ok {
		t.Fatal("expected cache miss after Invalidate")
	}
}

func TestProbeCacheInvalidateOnlyAffectsTargetKey(t *testing.T) {
	// Critical: a Recheck for one provider must not nuke the OTHER
	// provider's cached entry. The cache is shared by binary path so
	// a buggy Invalidate that scanned by value (or used a stale key)
	// could blow away unrelated state.
	cache := NewProbeCache(5 * time.Minute)
	cache.Set("/bin/claude", AccountInfo{SubscriptionType: "Claude Max"})
	cache.Set("/bin/codex", AccountInfo{SubscriptionType: "pro"})

	cache.Invalidate("/bin/claude")

	if _, ok := cache.Get("/bin/claude"); ok {
		t.Error("/bin/claude should be evicted")
	}
	got, ok := cache.Get("/bin/codex")
	if !ok {
		t.Fatal("/bin/codex should still be cached")
	}
	if got.SubscriptionType != "pro" {
		t.Errorf("/bin/codex SubscriptionType: got %q, want pro", got.SubscriptionType)
	}
}

func TestProbeCacheInvalidateMissingKeyIsNoOp(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	cache.Set("/bin/codex", AccountInfo{SubscriptionType: "pro"})

	// Invalidating a key that was never set must NOT panic and must
	// NOT touch other entries.
	cache.Invalidate("/bin/never-cached")

	got, ok := cache.Get("/bin/codex")
	if !ok {
		t.Fatal("expected /bin/codex to remain cached")
	}
	if got.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want pro", got.SubscriptionType)
	}
}

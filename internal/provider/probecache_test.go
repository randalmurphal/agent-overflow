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

func claudeKey() ProbeCacheKey {
	return ProbeCacheKey{Binary: "/usr/bin/claude", AccountID: "acct-1", WorkDir: "/home/u"}
}

func TestProbeCacheInvalidateRemovesEntry(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	cache.Set(claudeKey(), AccountInfo{SubscriptionType: "Claude Max"})

	if _, ok := cache.Get(claudeKey()); !ok {
		t.Fatal("precondition: expected cache hit before invalidate")
	}

	cache.Invalidate(claudeKey())

	if _, ok := cache.Get(claudeKey()); ok {
		t.Fatal("expected cache miss after Invalidate")
	}
}

func TestProbeCacheInvalidateOnlyAffectsTargetKey(t *testing.T) {
	// Critical: a Recheck for one provider must not nuke the OTHER
	// provider's cached entry. The cache is shared by binary path so
	// a buggy Invalidate that scanned by value (or used a stale key)
	// could blow away unrelated state.
	cache := NewProbeCache(5 * time.Minute)
	claude := ProbeCacheKey{Binary: "/bin/claude", WorkDir: "/home/u"}
	codex := ProbeCacheKey{Binary: "/bin/codex", WorkDir: "/home/u"}
	cache.Set(claude, AccountInfo{SubscriptionType: "Claude Max"})
	cache.Set(codex, AccountInfo{SubscriptionType: "pro"})

	cache.Invalidate(claude)

	if _, ok := cache.Get(claude); ok {
		t.Error("/bin/claude should be evicted")
	}
	got, ok := cache.Get(codex)
	if !ok {
		t.Fatal("/bin/codex should still be cached")
	}
	if got.SubscriptionType != "pro" {
		t.Errorf("/bin/codex SubscriptionType: got %q, want pro", got.SubscriptionType)
	}
}

func TestProbeCacheInvalidateMissingKeyIsNoOp(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	codex := ProbeCacheKey{Binary: "/bin/codex", WorkDir: "/home/u"}
	cache.Set(codex, AccountInfo{SubscriptionType: "pro"})

	// Invalidating a key that was never set must NOT panic and must
	// NOT touch other entries.
	cache.Invalidate(ProbeCacheKey{Binary: "/bin/never-cached", WorkDir: "/home/u"})

	got, ok := cache.Get(codex)
	if !ok {
		t.Fatal("expected /bin/codex to remain cached")
	}
	if got.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want pro", got.SubscriptionType)
	}
}

// TestProbeCacheScopedPerWorkDir is the cwd half of the key contract: the
// same binary and the same account, probed from two directories, are two
// answers. A project `.claude/settings.json` env block can redirect the CLI
// to a different backend, so a cwd-blind key would let the first workspace
// probed answer for every other one.
func TestProbeCacheScopedPerWorkDir(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	direct := ProbeCacheKey{Binary: "/bin/claude", AccountID: "acct-1", WorkDir: "/home/u"}
	bedrock := ProbeCacheKey{Binary: "/bin/claude", AccountID: "acct-1", WorkDir: "/repos/bedrock"}

	cache.Set(direct, AccountInfo{APIProvider: "firstParty"})

	if _, ok := cache.Get(bedrock); ok {
		t.Fatal("a different WorkDir must miss, not reuse another directory's answer")
	}

	cache.Set(bedrock, AccountInfo{APIProvider: "bedrock"})

	got, ok := cache.Get(direct)
	if !ok || got.APIProvider != "firstParty" {
		t.Errorf("original WorkDir entry = (%v, %v), want (firstParty, true)", got.APIProvider, ok)
	}
	got, ok = cache.Get(bedrock)
	if !ok || got.APIProvider != "bedrock" {
		t.Errorf("second WorkDir entry = (%v, %v), want (bedrock, true)", got.APIProvider, ok)
	}
}

// TestProbeCacheWorkDirTransitions walks the sequences a single-state test
// would miss: a workdir change between probes must not serve the previous
// directory's entry, coming back must still hit, and invalidating one
// directory must leave the other's entry alone.
func TestProbeCacheWorkDirTransitions(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	base := ProbeCacheKey{Binary: "/bin/claude", AccountID: "acct-1"}
	first := base
	first.WorkDir = "/home/u"
	second := base
	second.WorkDir = "/repos/bedrock"

	cache.Set(first, AccountInfo{Email: "one@example.com"})

	// workdir changes -> miss
	if _, ok := cache.Get(second); ok {
		t.Fatal("moving to a new WorkDir must miss")
	}
	cache.Set(second, AccountInfo{Email: "two@example.com"})

	// workdir changes back -> the original entry is still the one served
	got, ok := cache.Get(first)
	if !ok || got.Email != "one@example.com" {
		t.Fatalf("returning to the first WorkDir = (%q, %v), want (one@example.com, true)", got.Email, ok)
	}

	// invalidating one directory leaves the other addressable
	cache.Invalidate(first)
	if _, ok := cache.Get(first); ok {
		t.Error("invalidated WorkDir should miss")
	}
	got, ok = cache.Get(second)
	if !ok || got.Email != "two@example.com" {
		t.Errorf("sibling WorkDir = (%q, %v), want (two@example.com, true)", got.Email, ok)
	}
}

// TestProbeCacheExpiryIsPerKey pairs TTL expiry with the workdir dimension:
// an expired entry must not resurrect when a sibling directory is written,
// and it must be re-probeable afterwards.
func TestProbeCacheExpiryIsPerKey(t *testing.T) {
	cache := NewProbeCache(10 * time.Millisecond)
	first := ProbeCacheKey{Binary: "/bin/claude", WorkDir: "/home/u"}
	second := ProbeCacheKey{Binary: "/bin/claude", WorkDir: "/repos/bedrock"}

	cache.Set(first, AccountInfo{Email: "one@example.com"})
	time.Sleep(20 * time.Millisecond)
	cache.Set(second, AccountInfo{Email: "two@example.com"})

	if _, ok := cache.Get(first); ok {
		t.Error("expired entry must not be served after a sibling write")
	}
	cache.Set(first, AccountInfo{Email: "three@example.com"})
	got, ok := cache.Get(first)
	if !ok || got.Email != "three@example.com" {
		t.Errorf("re-probed entry = (%q, %v), want (three@example.com, true)", got.Email, ok)
	}
}

// TestProbeCacheKeyStringSeparatesDimensions guards the encoding itself: no
// two distinct keys may collapse onto one string. That includes the
// unmanaged case — an account id is opaque wire data, so encoding "no
// managed account" as a placeholder word would let an account literally
// named that impersonate it.
func TestProbeCacheKeyStringSeparatesDimensions(t *testing.T) {
	seen := map[string]ProbeCacheKey{}
	keys := []ProbeCacheKey{
		{Binary: "/bin/claude", AccountID: "a", WorkDir: "/x"},
		{Binary: "/bin/claude", AccountID: "", WorkDir: "/x"},
		{Binary: "/bin/claude", AccountID: "a", WorkDir: "/y"},
		{Binary: "/bin/claudex", AccountID: "a", WorkDir: "/x"},
		// Concatenation traps: the pieces below reassemble into the same
		// characters as a sibling key if any separator were dropped.
		{Binary: "/bin/claude", AccountID: "a\x00cwd=/x", WorkDir: ""},
		{Binary: "", AccountID: "unmanaged", WorkDir: "/x"},
		{Binary: "", AccountID: "", WorkDir: "/x"},
	}
	for _, key := range keys {
		encoded := key.String()
		if prior, dup := seen[encoded]; dup {
			t.Errorf("keys %+v and %+v collide on %q", prior, key, encoded)
			continue
		}
		seen[encoded] = key
	}
}

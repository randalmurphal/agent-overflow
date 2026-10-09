package forgeattach

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/contentcache"
)

// The LRU, expiry and id mechanics are contentcache's and tested there. What
// this package adds is the weight of an attachment and the fallback limits.

func TestCacheWeighsAttachmentsByTheirBytes(t *testing.T) {
	cache := NewCache(100, time.Hour)
	if _, err := cache.Put("a", Attachment{Data: make([]byte, 60), Kind: KindImage}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := cache.Bytes(); got != 60 {
		t.Fatalf("Bytes() = %d, want 60", got)
	}
	_, err := cache.Put("b", Attachment{Data: make([]byte, 101)})
	if !errors.Is(err, ErrTooLargeToCache) {
		t.Fatalf("Put returned %v, want ErrTooLargeToCache", err)
	}
	if err == nil || !strings.Contains(err.Error(), "101") {
		t.Fatalf("Put error = %v, want it to name the payload size", err)
	}
	if _, err := cache.Put("c", Attachment{}); err == nil {
		t.Fatal("Put accepted an empty body")
	}
}

// TestCacheLookupDedupesByKey is the whole reason Lookup exists: a body that
// references the same image three times must spawn one forge CLI.
func TestCacheLookupDedupesByKey(t *testing.T) {
	cache := NewCache(1_000, time.Hour)
	key := CacheKey("gitlab", "g/r", 7, "/uploads/x/a.png")
	stored, err := cache.Put(key, Attachment{Data: make([]byte, 10), Filename: "a.png"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	found, ok := cache.Lookup(key)
	if !ok || found.ID != stored.ID || found.Value.Filename != "a.png" {
		t.Fatalf("Lookup = (%+v, %v), want the entry stored under the key", found, ok)
	}
}

func TestCacheDefaultsReplaceNonPositiveLimits(t *testing.T) {
	cfg := cacheConfig(0, 0)
	if cfg.MaxBytes != DefaultCacheBytes || cfg.TTL != DefaultTTL {
		t.Fatalf("cacheConfig(0, 0) = (%d, %s), want the package defaults", cfg.MaxBytes, cfg.TTL)
	}
	// And the result is a cache New accepts.
	contentcache.New(cfg)
}

package forgeattach

import (
	"time"

	"agent-overflow/internal/contentcache"
)

// Attachment is one fetched attachment held for the byte route.
type Attachment struct {
	// Data is owned by the cache after Put and read as immutable.
	Data     []byte
	MimeType string
	Kind     string
	Filename string
	// Width and Height are the pixel size of an image whose header Go can
	// read (Classification); zero when unknown.
	Width  int
	Height int
}

// Entry is one cached attachment with the content id its ticketed URL names.
type Entry = contentcache.Entry[Attachment]

// Cache holds fetched attachments for the byte route. Bounded because nothing
// downstream frees the bytes, TTL'd because a review pane that closed should
// not pin a video for the life of the process; the frontend re-fetches
// through the same bound method when an expired id 404s.
type Cache = contentcache.Cache[Attachment]

// ErrTooLargeToCache reports a body the cache could never hold.
var ErrTooLargeToCache = contentcache.ErrTooLarge

// NewCache returns an empty cache. A non-positive maxBytes or ttl falls back
// to the package defaults rather than producing a cache that refuses or
// expires everything.
func NewCache(maxBytes int64, ttl time.Duration) *Cache {
	return contentcache.New(cacheConfig(maxBytes, ttl))
}

func cacheConfig(maxBytes int64, ttl time.Duration) contentcache.Config[Attachment] {
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return contentcache.Config[Attachment]{
		MaxBytes: maxBytes,
		TTL:      ttl,
		Size:     func(a Attachment) int64 { return int64(len(a.Data)) },
	}
}

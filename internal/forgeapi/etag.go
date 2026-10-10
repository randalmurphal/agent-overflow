package forgeapi

import (
	"container/list"
	"net/http"
	"sync"
)

// ETag store bounds from docs/architecture/forge-transport.md.
const (
	etagMaxEntries = 256
	etagMaxBytes   = 32 << 20
)

type etagKey struct {
	host        string
	fingerprint string
	method      string
	url         string
}

// etagEntry is a validated answer: its ETag, body and the headers it came
// with (a 304 does not repeat pagination headers).
type etagEntry struct {
	key    etagKey
	etag   string
	body   []byte
	header http.Header
	size   int
}

// etagStore holds the bodies conditional GETs revalidate, LRU-bounded by
// entry count and bytes. A host's entries go when its token changes.
type etagStore struct {
	mu      sync.Mutex
	order   *list.List // front is most recently used; values are *etagEntry
	byKey   map[etagKey]*list.Element
	bytes   int
	maxSize int
	maxLen  int
}

func newETagStore() *etagStore {
	return &etagStore{order: list.New(), byKey: make(map[etagKey]*list.Element), maxSize: etagMaxBytes, maxLen: etagMaxEntries}
}

// lookup returns the held entry and marks it used.
func (s *etagStore) lookup(key etagKey) (*etagEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.byKey[key]
	if !ok {
		return nil, false
	}
	s.order.MoveToFront(el)
	return el.Value.(*etagEntry), true
}

// store holds body under key, replacing any entry, and evicts the least
// recently used entries over either bound. A body larger than the byte
// bound is not held.
func (s *etagStore) store(key etagKey, etag string, body []byte, header http.Header) {
	size := len(body) + headerSize(header) + len(key.url) + len(etag)
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.byKey[key]; ok {
		s.removeLocked(el)
	}
	if size > s.maxSize {
		return
	}
	entry := &etagEntry{key: key, etag: etag, body: append([]byte(nil), body...), header: header.Clone(), size: size}
	s.byKey[key] = s.order.PushFront(entry)
	s.bytes += size
	for s.order.Len() > s.maxLen || s.bytes > s.maxSize {
		s.removeLocked(s.order.Back())
	}
}

func (s *etagStore) remove(key etagKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.byKey[key]; ok {
		s.removeLocked(el)
	}
}

func (s *etagStore) removeLocked(el *list.Element) {
	entry := el.Value.(*etagEntry)
	s.order.Remove(el)
	delete(s.byKey, entry.key)
	s.bytes -= entry.size
}

// dropHost forgets every entry of host whose token is not keep ("" drops
// all of the host's entries).
func (s *etagStore) dropHost(host, keep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for el := s.order.Front(); el != nil; {
		next := el.Next()
		if entry := el.Value.(*etagEntry); entry.key.host == host && entry.key.fingerprint != keep {
			s.removeLocked(el)
		}
		el = next
	}
}

func (s *etagStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order.Init()
	clear(s.byKey)
	s.bytes = 0
}

func headerSize(h http.Header) int {
	n := 0
	for k, values := range h {
		for _, v := range values {
			n += len(k) + len(v)
		}
	}
	return n
}

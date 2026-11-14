// Package reviewimage holds the image bytes the review pane reads from a
// diff's files between the bound method that read them and the byte route
// that serves them. One byte budget covers every byte the store owns:
// cached entries, loads in flight and entries a response is still reading.
// A load reserves its size before it reads, concurrent loads of one key
// share one read, an entry being served is freed only when its last reader
// closes, and a load that cannot reserve waits for room within its
// caller's context and a bounded time, then refuses.
package reviewimage

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrNoRoom is a load refused because the budget stayed full for as long
// as it could wait.
var ErrNoRoom = errors.New("too many review images are loading or being sent; try again")

// ErrCleared is a load that finished after Clear: it published nothing.
var ErrCleared = errors.New("the review images were cleared while this one loaded; try again")

// Entry is one held image.
type Entry struct {
	// ID is what a ticketed URL names: 16 random bytes, unpadded base64url.
	ID       string
	Size     int64
	MimeType string
}

// Content is one entry opened for the byte route. Closing it releases the
// entry for eviction.
type Content struct {
	MimeType string
	StoredAt time.Time
	io.ReadSeekCloser
}

type entry struct {
	id, key  string
	data     []byte
	mime     string
	storedAt time.Time
	readers  int
	// listed is false once the entry left the maps (evicted, expired or
	// cleared) while a reader still held it; its bytes stay charged until
	// the last reader closes.
	listed bool
	elem   *list.Element
}

type load struct {
	done  chan struct{}
	entry Entry
	err   error
}

// Store is safe for concurrent use.
type Store struct {
	budget  int64
	ttl     time.Duration
	maxWait time.Duration
	now     func() time.Time
	// alloc makes a load's buffer and free is told when the store lets go
	// of one; tests count both to measure what the store holds.
	alloc func(n int64) []byte
	free  func(n int64)

	mu    sync.Mutex
	used  int64
	lru   *list.List // front is most recently used; values are *entry
	byID  map[string]*entry
	byKey map[string]*entry
	loads map[string]*load
	// gen counts Clear calls; a load publishes only into the generation
	// it was admitted in.
	gen uint64
	// room is closed, and replaced, whenever bytes are released or an
	// entry becomes evictable.
	room chan struct{}
}

// New returns a store holding at most budget bytes, each entry servable for
// ttl, a load waiting at most maxWait for room.
func New(budget int64, ttl, maxWait time.Duration) *Store {
	if budget <= 0 || ttl <= 0 || maxWait <= 0 {
		panic("reviewimage: New needs a positive budget, ttl and wait")
	}
	return &Store{
		budget: budget, ttl: ttl, maxWait: maxWait, now: time.Now,
		alloc: func(n int64) []byte { return make([]byte, n) },
		free:  func(int64) {},
		lru:   list.New(), byID: map[string]*entry{}, byKey: map[string]*entry{},
		loads: map[string]*load{}, room: make(chan struct{}),
	}
}

// Load answers the entry held under key, or reserves size bytes, has fill
// write the content into a buffer of exactly that size and answer its MIME
// type, and holds the result. Concurrent loads of one key share one fill;
// a fill error is returned to each of them and nothing is held.
func (s *Store) Load(ctx context.Context, key string, size int64, fill func(buf []byte) (string, error)) (Entry, error) {
	if size <= 0 {
		return Entry{}, errors.New("reviewimage: nothing to hold")
	}
	return s.LoadWithin(ctx, key, size, func(take func(n int64) ([]byte, error)) (string, error) {
		buf, err := take(size)
		if err != nil {
			return "", err
		}
		return fill(buf)
	})
}

// LoadWithin is Load for content whose size is known only once reading
// starts: it reserves limit bytes before fill runs, and fill asks for its
// buffer through take, which shrinks the reservation to the n bytes it
// allocates (at most limit, once). From admission until it returns, the
// load's reservation and buffer are released on every path, a panicking
// fill's included (the panic continues). A load that finishes after Clear
// publishes nothing and answers ErrCleared.
func (s *Store) LoadWithin(ctx context.Context, key string, limit int64, fill func(take func(n int64) ([]byte, error)) (string, error)) (Entry, error) {
	if limit <= 0 {
		return Entry{}, errors.New("reviewimage: nothing to hold")
	}
	if limit > s.budget {
		return Entry{}, fmt.Errorf("reviewimage: %d bytes exceeds the %d byte budget", limit, s.budget)
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.maxWait)
	defer cancel()
	refused := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrNoRoom
	}
	s.mu.Lock()
	for {
		s.dropExpiredLocked()
		if held, ok := s.byKey[key]; ok {
			s.lru.MoveToFront(held.elem)
			s.mu.Unlock()
			return held.public(), nil
		}
		if pending, ok := s.loads[key]; ok {
			s.mu.Unlock()
			select {
			case <-pending.done:
				return pending.entry, pending.err
			case <-waitCtx.Done():
				return Entry{}, refused()
			}
		}
		if s.used+limit <= s.budget || s.evictLocked(limit) {
			break
		}
		room := s.room
		s.mu.Unlock()
		select {
		case <-room:
		case <-waitCtx.Done():
			return Entry{}, refused()
		}
		s.mu.Lock()
	}
	s.used += limit
	reserved := limit
	gen := s.gen
	pending := &load{done: make(chan struct{})}
	s.loads[key] = pending
	s.mu.Unlock()

	var buf []byte
	settled := false
	settle := func(mime string, fillErr error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.loads[key] == pending {
			delete(s.loads, key)
		}
		if fillErr == nil && buf == nil {
			fillErr = errors.New("reviewimage: the load took no buffer")
		}
		if fillErr == nil && s.gen != gen {
			fillErr = ErrCleared
		}
		if fillErr != nil {
			if buf != nil {
				s.free(int64(len(buf)))
			}
			s.releaseLocked(reserved)
			pending.err = fillErr
		} else {
			id, idErr := newID()
			if idErr != nil {
				s.free(int64(len(buf)))
				s.releaseLocked(reserved)
				pending.err = idErr
			} else {
				held := &entry{id: id, key: key, data: buf, mime: mime, storedAt: s.now(), listed: true}
				held.elem = s.lru.PushFront(held)
				s.byID[id] = held
				s.byKey[key] = held
				pending.entry = held.public()
			}
		}
		settled = true
		close(pending.done)
	}
	defer func() {
		if !settled {
			recovered := recover()
			settle("", fmt.Errorf("reviewimage: load stopped: %v", recovered))
			if recovered != nil {
				panic(recovered)
			}
		}
	}()
	take := func(n int64) ([]byte, error) {
		if buf != nil {
			return nil, errors.New("reviewimage: the load already took its buffer")
		}
		if n <= 0 || n > limit {
			return nil, fmt.Errorf("reviewimage: %d bytes is outside the load's 1..%d", n, limit)
		}
		s.mu.Lock()
		if n < reserved {
			s.used -= reserved - n
			reserved = n
			s.wakeLocked()
		}
		s.mu.Unlock()
		buf = s.alloc(n)
		return buf, nil
	}
	mime, fillErr := fill(take)
	settle(mime, fillErr)
	return pending.entry, pending.err
}

// Open answers an entry by id for the byte route; it is not freed until
// the content is closed.
func (s *Store) Open(id string) (Content, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpiredLocked()
	held, ok := s.byID[id]
	if !ok {
		return Content{}, false
	}
	held.readers++
	s.lru.MoveToFront(held.elem)
	return Content{MimeType: held.mime, StoredAt: held.storedAt, ReadSeekCloser: &reader{Reader: bytes.NewReader(held.data), store: s, entry: held}}, true
}

// Clear forgets every entry; one a response is reading is freed when it
// closes.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	// A load in flight keeps its bytes charged until it returns, then
	// publishes nothing; a load of the same key after Clear starts afresh.
	s.loads = map[string]*load{}
	for element := s.lru.Front(); element != nil; {
		next := element.Next()
		s.unlistLocked(element.Value.(*entry))
		element = next
	}
}

// Held reports the bytes the store owns: entries, readers' entries and
// loads in flight.
func (s *Store) Held() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

type reader struct {
	*bytes.Reader
	store  *Store
	entry  *entry
	closed bool
}

func (r *reader) Close() error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.entry.readers--
	switch {
	case r.entry.readers > 0:
	case !r.entry.listed:
		r.store.dropLocked(r.entry)
	default:
		// Still held, but evictable now: a waiting load may make room.
		r.store.wakeLocked()
	}
	return nil
}

func (e *entry) public() Entry {
	return Entry{ID: e.id, Size: int64(len(e.data)), MimeType: e.mime}
}

// evictLocked frees unread entries, least recently used first, until size
// more bytes fit; false when they cannot.
func (s *Store) evictLocked(size int64) bool {
	for element := s.lru.Back(); element != nil && s.used+size > s.budget; {
		previous := element.Prev()
		if held := element.Value.(*entry); held.readers == 0 {
			s.unlistLocked(held)
		}
		element = previous
	}
	return s.used+size <= s.budget
}

func (s *Store) dropExpiredLocked() {
	now := s.now()
	for element := s.lru.Back(); element != nil; {
		previous := element.Prev()
		if held := element.Value.(*entry); now.Sub(held.storedAt) >= s.ttl {
			s.unlistLocked(held)
		}
		element = previous
	}
}

// unlistLocked takes an entry out of the maps; its bytes are released now,
// or when its last reader closes.
func (s *Store) unlistLocked(held *entry) {
	s.lru.Remove(held.elem)
	delete(s.byID, held.id)
	if s.byKey[held.key] == held {
		delete(s.byKey, held.key)
	}
	held.listed = false
	if held.readers == 0 {
		s.dropLocked(held)
	}
}

// releaseLocked returns n reserved bytes to the budget.
func (s *Store) releaseLocked(n int64) {
	s.used -= n
	s.wakeLocked()
}

// dropLocked lets go of an entry's bytes.
func (s *Store) dropLocked(held *entry) {
	s.free(int64(len(held.data)))
	s.releaseLocked(int64(len(held.data)))
}

// wakeLocked tells every waiting load to look for room again.
func (s *Store) wakeLocked() {
	close(s.room)
	s.room = make(chan struct{})
}

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reviewimage: content id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

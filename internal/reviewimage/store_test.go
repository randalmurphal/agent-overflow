package reviewimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// meteredStore counts the bytes the store allocated and has not let go of,
// and the most it ever held at once.
type meteredStore struct {
	*Store
	mu        sync.Mutex
	live, max int64
}

func newMetered(budget int64, wait time.Duration) *meteredStore {
	m := &meteredStore{Store: New(budget, time.Hour, wait)}
	m.alloc = func(n int64) []byte {
		m.mu.Lock()
		m.live += n
		m.max = max(m.max, m.live)
		m.mu.Unlock()
		return make([]byte, n)
	}
	m.free = func(n int64) {
		m.mu.Lock()
		m.live -= n
		m.mu.Unlock()
	}
	return m
}

func fillWith(b byte) func([]byte) (string, error) {
	return func(buf []byte) (string, error) {
		for i := range buf {
			buf[i] = b
		}
		return "image/png", nil
	}
}

// Many loads and many readers at once never hold more than the budget:
// a load waits for room that a reader's close or an eviction frees.
func TestConcurrentLoadsAndReadersStayInsideTheBudget(t *testing.T) {
	t.Parallel()
	const budget = 100
	store := newMetered(budget, 10*time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry, err := store.Load(context.Background(), fmt.Sprintf("k%d", i), 60, fillWith(byte(i)))
			if err != nil {
				errs <- err
				return
			}
			content, ok := store.Open(entry.ID)
			if !ok {
				return // evicted before it was opened: a client mints again
			}
			time.Sleep(5 * time.Millisecond)
			data, err := io.ReadAll(content)
			if err != nil || len(data) != 60 || data[0] != byte(i) {
				errs <- fmt.Errorf("read %d: %d bytes, err %v", i, len(data), err)
			}
			_ = content.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if store.max > budget {
		t.Fatalf("the store held %d bytes at once, over the %d byte budget", store.max, budget)
	}
	if store.live != store.Held() {
		t.Fatalf("allocated %d bytes, accounted %d", store.live, store.Held())
	}
}

func TestConcurrentMissesShareOneLoad(t *testing.T) {
	t.Parallel()
	store := newMetered(100, time.Second)
	var fills atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry, err := store.Load(context.Background(), "same", 40, func(buf []byte) (string, error) {
				fills.Add(1)
				<-release
				return "image/png", nil
			})
			if err != nil {
				t.Error(err)
			}
			ids[i] = entry.ID
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if fills.Load() != 1 {
		t.Fatalf("%d fills for one key, want 1", fills.Load())
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("loads answered different entries %v", ids)
		}
	}
	if store.max != 40 {
		t.Fatalf("held %d bytes at most, want one copy of 40", store.max)
	}
}

// An entry a response is reading stays; a load that needs its room waits
// for the close, and past its wait refuses with a sentence.
func TestAnEntryBeingReadIsNotFreed(t *testing.T) {
	t.Parallel()
	store := newMetered(100, 50*time.Millisecond)
	a, err := store.Load(context.Background(), "a", 70, fillWith(1))
	if err != nil {
		t.Fatal(err)
	}
	content, ok := store.Open(a.ID)
	if !ok {
		t.Fatal("a is not open")
	}
	if _, err := store.Load(context.Background(), "b", 70, fillWith(2)); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a load past a read entry = %v, want ErrNoRoom", err)
	}
	if store.Held() != 70 || store.max != 70 {
		t.Fatalf("held %d (max %d) while refusing, want 70", store.Held(), store.max)
	}
	if err := content.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), "b", 70, fillWith(2)); err != nil {
		t.Fatalf("b after a closed: %v", err)
	}
	if store.max > 100 {
		t.Fatalf("held %d bytes at once", store.max)
	}
}

func TestALoadWaitsForAReaderToClose(t *testing.T) {
	t.Parallel()
	store := newMetered(100, 5*time.Second)
	a, err := store.Load(context.Background(), "a", 70, fillWith(1))
	if err != nil {
		t.Fatal(err)
	}
	content, _ := store.Open(a.ID)
	done := make(chan error, 1)
	go func() {
		_, err := store.Load(context.Background(), "b", 70, fillWith(2))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("b loaded while a was being read: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	_ = content.Close()
	if err := <-done; err != nil {
		t.Fatalf("b after a closed: %v", err)
	}
	if store.max > 100 {
		t.Fatalf("held %d bytes at once", store.max)
	}
	if _, ok := store.Open(a.ID); ok {
		t.Fatal("a still resolves after b evicted it")
	}
}

func TestAFailedFillHoldsNothing(t *testing.T) {
	t.Parallel()
	store := newMetered(100, time.Second)
	if _, err := store.Load(context.Background(), "a", 50, func([]byte) (string, error) { return "", errors.New("changed") }); err == nil {
		t.Fatal("a failed fill answered an entry")
	}
	if store.Held() != 0 || store.live != 0 {
		t.Fatalf("held %d, live %d after a failed fill", store.Held(), store.live)
	}
	var fills int
	if _, err := store.Load(context.Background(), "a", 50, func(buf []byte) (string, error) { fills++; return "image/png", nil }); err != nil || fills != 1 {
		t.Fatalf("the next load did not fill again: %v, %d", err, fills)
	}
}

func TestClearForgetsEntriesAndFreesReadOnesOnClose(t *testing.T) {
	t.Parallel()
	store := newMetered(100, time.Second)
	a, _ := store.Load(context.Background(), "a", 30, fillWith(1))
	b, _ := store.Load(context.Background(), "b", 30, fillWith(2))
	content, _ := store.Open(a.ID)
	store.Clear()
	for _, id := range []string{a.ID, b.ID} {
		if _, ok := store.Open(id); ok {
			t.Fatalf("%s resolves after Clear", id)
		}
	}
	if store.Held() != 30 {
		t.Fatalf("held %d after Clear with one reader, want 30", store.Held())
	}
	if data, _ := io.ReadAll(content); len(data) != 30 {
		t.Fatalf("the reader lost its bytes: %d", len(data))
	}
	_ = content.Close()
	_ = content.Close()
	if store.Held() != 0 || store.live != 0 {
		t.Fatalf("held %d, live %d after the reader closed", store.Held(), store.live)
	}
}

func TestEntriesExpire(t *testing.T) {
	t.Parallel()
	store := New(100, time.Minute, time.Second)
	clock := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return clock }
	a, _ := store.Load(context.Background(), "a", 10, fillWith(1))
	clock = clock.Add(time.Minute)
	if _, ok := store.Open(a.ID); ok || store.Held() != 0 {
		t.Fatalf("an expired entry resolves (held %d)", store.Held())
	}
}

func TestLoadRefusesWhatCannotFit(t *testing.T) {
	t.Parallel()
	store := New(100, time.Minute, time.Second)
	if _, err := store.Load(context.Background(), "a", 101, fillWith(1)); err == nil {
		t.Fatal("a load over the budget was accepted")
	}
}

// The reader guard: a competing load that finds no room leaves the entry a
// response is reading listed, so it still opens before that reader closes.
func TestAnEntryBeingReadStaysListedPastAFailedLoad(t *testing.T) {
	t.Parallel()
	store := newMetered(100, 20*time.Millisecond)
	a, err := store.Load(context.Background(), "a", 70, fillWith(1))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.Open(a.ID)
	if _, err := store.Load(context.Background(), "b", 70, fillWith(2)); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("b = %v, want ErrNoRoom", err)
	}
	second, ok := store.Open(a.ID)
	if !ok {
		t.Fatal("a no longer opens after a competing load failed")
	}
	_ = second.Close()
	_ = first.Close()
}

// A waiting load whose caller leaves answers the caller's error at once,
// not ErrNoRoom after the wait, and holds nothing.
func TestCancelingAWaitingLoad(t *testing.T) {
	t.Parallel()
	store := newMetered(100, time.Minute)
	a, _ := store.Load(context.Background(), "a", 70, fillWith(1))
	content, _ := store.Open(a.ID)
	defer func() { _ = content.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := store.Load(ctx, "b", 70, fillWith(2))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a canceled load = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled load kept waiting")
	}
	if store.Held() != 70 {
		t.Fatalf("held %d, want only a", store.Held())
	}
}

// A fill that panics releases its reservation, its buffer and its flight
// before the panic goes on, so the next load of the key fills afresh.
func TestAPanickingFillReleasesEverything(t *testing.T) {
	t.Parallel()
	store := newMetered(100, time.Second)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_, _ = store.Load(context.Background(), "a", 60, func([]byte) (string, error) { panic("boom") })
	}()
	if store.Held() != 0 || store.live != 0 {
		t.Fatalf("held %d, live %d after a panicking fill", store.Held(), store.live)
	}
	if _, err := store.Load(context.Background(), "a", 60, fillWith(1)); err != nil {
		t.Fatalf("the next load of the key: %v", err)
	}
}

// A load admitted before Clear and finishing after it publishes nothing:
// its waiters and its caller get ErrCleared, its bytes are charged until
// it returns, and a load of the key after Clear fills afresh.
func TestALoadFinishingAfterClearPublishesNothing(t *testing.T) {
	t.Parallel()
	store := newMetered(100, time.Second)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := store.Load(context.Background(), "a", 40, func(buf []byte) (string, error) {
			close(started)
			<-release
			return "image/png", nil
		})
		done <- err
	}()
	<-started
	store.Clear()
	if store.Held() != 40 {
		t.Fatalf("held %d during the cleared load, want its 40", store.Held())
	}
	fresh, err := store.Load(context.Background(), "a", 40, fillWith(3))
	if err != nil {
		t.Fatalf("a load after Clear: %v", err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrCleared) {
		t.Fatalf("the cleared load = %v, want ErrCleared", err)
	}
	if store.Held() != 40 || store.live != 40 {
		t.Fatalf("held %d, live %d, want only the fresh entry", store.Held(), store.live)
	}
	content, ok := store.Open(fresh.ID)
	if !ok {
		t.Fatal("the fresh entry does not open")
	}
	if data, _ := io.ReadAll(content); data[0] != 3 {
		t.Fatalf("the key serves %d, want the fresh fill", data[0])
	}
	_ = content.Close()
}

// LoadWithin reserves its limit first, then only what the fill takes.
func TestLoadWithinShrinksItsReservation(t *testing.T) {
	t.Parallel()
	store := newMetered(100, time.Second)
	var during int64
	entry, err := store.LoadWithin(context.Background(), "a", 90, func(take func(int64) ([]byte, error)) (string, error) {
		during = store.Held()
		if _, err := take(91); err == nil {
			t.Error("a take past the limit was granted")
		}
		buf, err := take(10)
		if err != nil {
			return "", err
		}
		if _, err := take(10); err == nil {
			t.Error("a second take was granted")
		}
		copy(buf, "0123456789")
		return "image/svg+xml", nil
	})
	if err != nil || entry.Size != 10 {
		t.Fatalf("entry %+v, err %v", entry, err)
	}
	if during != 90 || store.Held() != 10 || store.max != 10 {
		t.Fatalf("held %d before take and %d after (allocated at most %d), want 90 then 10", during, store.Held(), store.max)
	}
	if _, err := store.LoadWithin(context.Background(), "b", 50, func(func(int64) ([]byte, error)) (string, error) { return "image/png", nil }); err == nil {
		t.Fatal("a fill that took no buffer was held")
	}
	if store.Held() != 10 {
		t.Fatalf("held %d after a fill that took nothing", store.Held())
	}
}

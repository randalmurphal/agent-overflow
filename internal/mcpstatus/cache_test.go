package mcpstatus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

type recordingBus struct {
	mu     sync.Mutex
	events []ServerStatus
}

func (r *recordingBus) Emit(s ServerStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}

func (r *recordingBus) snapshot() []ServerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ServerStatus, len(r.events))
	copy(out, r.events)
	return out
}

func TestCache_GetMissReturnsNotOK(t *testing.T) {
	c := NewCache(time.Minute, nil)
	_, ok := c.Get(Key{Provider: ProviderClaude, Name: "github"})
	if ok {
		t.Fatalf("expected miss, got ok=true")
	}
}

func TestCache_PutThenGetWithinTTL(t *testing.T) {
	bus := &recordingBus{}
	c := NewCache(time.Minute, bus)
	k := Key{Provider: ProviderClaude, Name: "github"}
	c.Put(ServerStatus{Key: k, Status: StatusConnected, Source: SourceLiveSession})
	got, ok := c.Get(k)
	if !ok {
		t.Fatal("expected hit")
	}
	if got.Status != StatusConnected {
		t.Fatalf("status = %q, want connected", got.Status)
	}
	if len(bus.snapshot()) != 1 {
		t.Fatalf("expected one emission, got %d", len(bus.snapshot()))
	}
}

func TestCache_PutThenGetAfterTTL(t *testing.T) {
	now := time.Now()
	clock := now
	c := NewWith(50*time.Millisecond, nil, func() time.Time { return clock })
	k := Key{Provider: ProviderClaude, Name: "github"}
	c.Put(ServerStatus{Key: k, Status: StatusConnected, Source: SourceLiveSession})
	clock = now.Add(time.Second)
	if _, ok := c.Get(k); ok {
		t.Fatal("expected stale miss after TTL elapsed")
	}
}

func TestCache_InvalidateEmitsUnknown(t *testing.T) {
	bus := &recordingBus{}
	c := NewCache(time.Minute, bus)
	k := Key{Provider: ProviderClaude, Name: "github"}
	c.Put(ServerStatus{Key: k, Status: StatusConnected, Source: SourceLiveSession})
	c.Invalidate(k)
	events := bus.snapshot()
	if len(events) != 2 {
		t.Fatalf("expected put+invalidate emissions, got %d", len(events))
	}
	if events[1].Status != StatusUnknown {
		t.Fatalf("expected StatusUnknown on invalidate, got %q", events[1].Status)
	}
}

func TestCache_InvalidateMissingKeyIsNoop(t *testing.T) {
	bus := &recordingBus{}
	c := NewCache(time.Minute, bus)
	c.Invalidate(Key{Provider: ProviderClaude, Name: "absent"})
	if events := bus.snapshot(); len(events) != 0 {
		t.Fatalf("expected no emissions for missing-key invalidate, got %d", len(events))
	}
}

func TestCache_InvalidateProvider_ClearsOnlyOneProvider(t *testing.T) {
	bus := &recordingBus{}
	c := NewCache(time.Minute, bus)
	c.Put(ServerStatus{Key: Key{Provider: ProviderClaude, Name: "github"}, Status: StatusConnected})
	c.Put(ServerStatus{Key: Key{Provider: ProviderCodex, Name: "github"}, Status: StatusConnected})
	c.InvalidateProvider(ProviderClaude)
	if _, ok := c.Get(Key{Provider: ProviderClaude, Name: "github"}); ok {
		t.Fatal("expected claude entry to be cleared")
	}
	if _, ok := c.Get(Key{Provider: ProviderCodex, Name: "github"}); !ok {
		t.Fatal("expected codex entry to survive")
	}
}

func TestCache_CrossProviderNameNoCollision(t *testing.T) {
	c := NewCache(time.Minute, nil)
	c.Put(ServerStatus{Key: Key{Provider: ProviderClaude, Name: "github"}, Status: StatusConnected})
	c.Put(ServerStatus{Key: Key{Provider: ProviderCodex, Name: "github"}, Status: StatusNeedsAuth})
	cl, _ := c.Get(Key{Provider: ProviderClaude, Name: "github"})
	co, _ := c.Get(Key{Provider: ProviderCodex, Name: "github"})
	if cl.Status != StatusConnected || co.Status != StatusNeedsAuth {
		t.Fatalf("entries collided: cl=%q co=%q", cl.Status, co.Status)
	}
}

type stubFetcher struct {
	mu      sync.Mutex
	calls   int32
	results []ServerStatus
	err     error
	delay   time.Duration
}

func (s *stubFetcher) Fetch(ctx context.Context, _ Provider) ([]ServerStatus, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ServerStatus, len(s.results))
	copy(out, s.results)
	return out, s.err
}

func TestGetOrFetch_CacheHitSkipsFetcher(t *testing.T) {
	c := NewCache(time.Minute, nil)
	k := Key{Provider: ProviderClaude, Name: "github"}
	c.Put(ServerStatus{Key: k, Status: StatusConnected})
	f := &stubFetcher{}
	got, err := c.GetOrFetch(context.Background(), k, f, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Status != StatusConnected {
		t.Fatalf("got status %q", got.Status)
	}
	if calls := atomic.LoadInt32(&f.calls); calls != 0 {
		t.Fatalf("fetcher called %d times despite cache hit", calls)
	}
}

func TestGetOrFetch_ForceBypassesCache(t *testing.T) {
	c := NewCache(time.Minute, nil)
	k := Key{Provider: ProviderClaude, Name: "github"}
	c.Put(ServerStatus{Key: k, Status: StatusConnected})
	f := &stubFetcher{results: []ServerStatus{{Key: k, Status: StatusNeedsAuth}}}
	got, err := c.GetOrFetch(context.Background(), k, f, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Status != StatusNeedsAuth {
		t.Fatalf("force fetch returned %q", got.Status)
	}
}

func TestGetOrFetch_SingleFlight_OneFetchAcrossConcurrentCallers(t *testing.T) {
	c := NewCache(time.Minute, nil)
	k := Key{Provider: ProviderClaude, Name: "github"}
	f := &stubFetcher{
		results: []ServerStatus{{Key: k, Status: StatusConnected}},
		delay:   50 * time.Millisecond,
	}

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.GetOrFetch(context.Background(), k, f, false)
		}()
	}
	wg.Wait()

	if calls := atomic.LoadInt32(&f.calls); calls != 1 {
		t.Fatalf("expected 1 fetch under single-flight, got %d", calls)
	}
}

func TestGetOrFetch_FetcherErrorIsReturned(t *testing.T) {
	c := NewCache(time.Minute, nil)
	k := Key{Provider: ProviderClaude, Name: "github"}
	expected := errors.New("boom")
	f := &stubFetcher{err: expected}
	_, err := c.GetOrFetch(context.Background(), k, f, false)
	if !errors.Is(err, expected) {
		t.Fatalf("expected fetcher error to propagate, got %v", err)
	}
}

func TestGetOrFetch_StampsEphemeralSourceWhenFetcherOmitsIt(t *testing.T) {
	c := NewCache(time.Minute, nil)
	k := Key{Provider: ProviderClaude, Name: "github"}
	f := &stubFetcher{results: []ServerStatus{{Key: k, Status: StatusConnected /* Source intentionally empty */}}}
	got, err := c.GetOrFetch(context.Background(), k, f, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Source != SourceEphemeralFetch {
		t.Fatalf("returned value missing source stamp: %+v", got)
	}
	cached, _ := c.Get(k)
	if cached.Source != SourceEphemeralFetch {
		t.Fatalf("expected Source=%q stamp, got %q", SourceEphemeralFetch, cached.Source)
	}
}

func TestRefreshProvider_SingleFlight_OneFetchAcrossConcurrentCallers(t *testing.T) {
	c := NewCache(time.Minute, nil)
	f := &stubFetcher{
		results: []ServerStatus{
			{Key: Key{Provider: ProviderClaude, Name: "github"}, Status: StatusConnected},
			{Key: Key{Provider: ProviderClaude, Name: "linear"}, Status: StatusNeedsAuth},
		},
		delay: 50 * time.Millisecond,
	}

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.RefreshProvider(context.Background(), ProviderClaude, f)
		}()
	}
	wg.Wait()

	if calls := atomic.LoadInt32(&f.calls); calls != 1 {
		t.Fatalf("expected 1 fetch under single-flight, got %d", calls)
	}
}

func TestRefreshProvider_PopulatesCache(t *testing.T) {
	c := NewCache(time.Minute, nil)
	gh := Key{Provider: ProviderClaude, Name: "github"}
	lin := Key{Provider: ProviderClaude, Name: "linear"}
	f := &stubFetcher{results: []ServerStatus{
		{Key: gh, Status: StatusConnected},
		{Key: lin, Status: StatusNeedsAuth},
	}}
	_, err := c.RefreshProvider(context.Background(), ProviderClaude, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, ok := c.Get(gh); !ok || got.Status != StatusConnected {
		t.Fatalf("github not cached: ok=%v status=%q", ok, got.Status)
	}
	if got, ok := c.Get(lin); !ok || got.Status != StatusNeedsAuth {
		t.Fatalf("linear not cached: ok=%v status=%q", ok, got.Status)
	}
}

func TestRefreshProvider_StampsEphemeralSourceOnReturnAndCache(t *testing.T) {
	c := NewCache(time.Minute, nil)
	k := Key{Provider: ProviderClaude, Name: "github"}
	f := &stubFetcher{results: []ServerStatus{
		{Key: k, Status: StatusConnected /* Source intentionally empty */},
	}}
	got, err := c.RefreshProvider(context.Background(), ProviderClaude, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Source != SourceEphemeralFetch {
		t.Fatalf("returned slice missing source stamp: %+v", got)
	}
	cached, ok := c.Get(k)
	if !ok || cached.Source != SourceEphemeralFetch {
		t.Fatalf("cache missing source stamp: ok=%v source=%q", ok, cached.Source)
	}
}

func TestRefreshProvider_FetcherErrorIsReturned(t *testing.T) {
	c := NewCache(time.Minute, nil)
	expected := errors.New("boom")
	f := &stubFetcher{err: expected}
	_, err := c.RefreshProvider(context.Background(), ProviderClaude, f)
	if !errors.Is(err, expected) {
		t.Fatalf("expected fetcher error to propagate, got %v", err)
	}
}

// TestRefreshProvider_BypassesTTLCache pins the documented contract
// that RefreshProvider is a forced refresh — even when a fresh cache
// entry exists for one of the keys, the fetcher runs and the returned
// payload reflects the fetcher's view, not the stale cache.
func TestRefreshProvider_BypassesTTLCache(t *testing.T) {
	c := NewCache(time.Minute, nil)
	k := Key{Provider: ProviderClaude, Name: "github"}
	c.Put(ServerStatus{Key: k, Status: StatusConnected, Source: SourceLiveSession})
	f := &stubFetcher{results: []ServerStatus{
		{Key: k, Status: StatusNeedsAuth, Source: SourceEphemeralFetch},
	}}
	got, err := c.RefreshProvider(context.Background(), ProviderClaude, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&f.calls) != 1 {
		t.Fatal("fetcher was not invoked despite cache hit")
	}
	if len(got) != 1 || got[0].Status != StatusNeedsAuth {
		t.Fatalf("expected refreshed value to win, got %+v", got)
	}
}

// TestRefreshProvider_EmptyResultIsHandled pins that a fetcher
// legitimately reporting zero servers (user removed every entry)
// doesn't panic and surfaces as no error + empty slice. Cache should
// also report empty for that provider.
func TestRefreshProvider_EmptyResultIsHandled(t *testing.T) {
	c := NewCache(time.Minute, nil)
	f := &stubFetcher{results: nil}
	got, err := c.RefreshProvider(context.Background(), ProviderClaude, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d entries", len(got))
	}
	if snap := c.SnapshotProvider(ProviderClaude); len(snap) != 0 {
		t.Fatalf("expected empty provider snapshot, got %d", len(snap))
	}
}

// TestRefreshProvider_ConcurrentCallersReceiveIndependentSlices is the
// regression guard for the shared-slice race: under -race, two
// callers receiving the same backing array would trip the detector
// when one of them sorts in place. cloneStatuses must hand each
// caller its own slice. Probabilistic — relies on -race to catch the
// race; without -race the sort still mutates two callers' views to
// the same end state so the assertion below also catches the regression.
func TestRefreshProvider_ConcurrentCallersReceiveIndependentSlices(t *testing.T) {
	c := NewCache(time.Minute, nil)
	f := &stubFetcher{
		results: []ServerStatus{
			{Key: Key{Provider: ProviderClaude, Name: "zeta"}, Status: StatusConnected},
			{Key: Key{Provider: ProviderClaude, Name: "alpha"}, Status: StatusConnected},
			{Key: Key{Provider: ProviderClaude, Name: "mike"}, Status: StatusConnected},
		},
		delay: 30 * time.Millisecond,
	}

	const N = 8
	got := make([][]ServerStatus, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			res, err := c.RefreshProvider(context.Background(), ProviderClaude, f)
			if err != nil {
				t.Errorf("caller %d unexpected error: %v", i, err)
				return
			}
			got[i] = res
			// Concurrent in-place sort — if peers share the backing
			// array, this races (caught by go test -race).
			sortByName(res)
		}()
	}
	wg.Wait()

	// Even without -race, the asymmetric mutation should be visible:
	// each caller's slice ends in its own sorted state. If they
	// shared the backing array, after-the-fact ordering still matches
	// (all sorted the same direction) so the strongest invariant we
	// can assert from outside is unique addressability — peeking at
	// the slice header pointers.
	pointers := map[uintptr]int{}
	for i, slice := range got {
		if len(slice) == 0 {
			t.Fatalf("caller %d got empty slice", i)
		}
		hdr := sliceHeader(slice)
		pointers[hdr]++
	}
	if len(pointers) != N {
		t.Fatalf("expected %d distinct backing arrays, got %d (single-flight handed out shared slices)", N, len(pointers))
	}
}

func sortByName(s []ServerStatus) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Name > s[j].Name; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func sliceHeader(s []ServerStatus) uintptr {
	if len(s) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&s[0]))
}

func TestRefreshProvider_DifferentProvidersDoNotSerialize(t *testing.T) {
	// Barrier-based concurrency check: both fetchers must enter
	// before the test releases them. If RefreshProvider serialized
	// the two providers, the second caller would block until the
	// first returned, and entered would only reach 1 within the
	// barrier window. No wall-clock thresholds.
	var entered atomic.Int32
	release := make(chan struct{})
	f := &barrierFetcher{
		entered: &entered,
		release: release,
		results: []ServerStatus{{Key: Key{Provider: ProviderClaude, Name: "github"}, Status: StatusConnected}},
	}
	c := NewCache(time.Minute, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = c.RefreshProvider(context.Background(), ProviderClaude, f)
	}()
	go func() {
		defer wg.Done()
		_, _ = c.RefreshProvider(context.Background(), ProviderCodex, f)
	}()

	deadline := time.After(2 * time.Second)
	for entered.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("only %d fetchers entered within deadline (gate serialized providers)", entered.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(release)
	wg.Wait()
}

type barrierFetcher struct {
	entered *atomic.Int32
	release <-chan struct{}
	results []ServerStatus
}

func (b *barrierFetcher) Fetch(ctx context.Context, _ Provider) ([]ServerStatus, error) {
	b.entered.Add(1)
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([]ServerStatus, len(b.results))
	copy(out, b.results)
	return out, nil
}

// TestRefreshProvider_ContextCancelDuringInflight_FreesWaiterAndCompletesFirst
// pins both halves of the asymmetric cancel contract: the waiter
// returns context.Canceled when it gives up, AND the first caller
// (whose context wasn't cancelled) still receives the populated
// result. Cancelling a waiter must not poison the in-flight fetch.
func TestRefreshProvider_ContextCancelDuringInflight_FreesWaiterAndCompletesFirst(t *testing.T) {
	c := NewCache(time.Minute, nil)
	f := &stubFetcher{
		results: []ServerStatus{{Key: Key{Provider: ProviderClaude, Name: "github"}, Status: StatusConnected}},
		delay:   200 * time.Millisecond,
	}

	type result struct {
		statuses []ServerStatus
		err      error
	}
	firstDone := make(chan result, 1)
	go func() {
		s, err := c.RefreshProvider(context.Background(), ProviderClaude, f)
		firstDone <- result{statuses: s, err: err}
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() {
		_, err := c.RefreshProvider(ctx, ProviderClaude, f)
		waitDone <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled for waiter, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not return after context cancel")
	}

	select {
	case r := <-firstDone:
		if r.err != nil {
			t.Fatalf("first caller error %v — waiter cancellation poisoned the flight", r.err)
		}
		if len(r.statuses) != 1 || r.statuses[0].Status != StatusConnected {
			t.Fatalf("first caller unexpected result: %+v", r.statuses)
		}
	case <-time.After(time.Second):
		t.Fatal("first caller did not return after fetch delay")
	}
}

func TestSnapshot_FiltersStale(t *testing.T) {
	now := time.Now()
	clock := now
	c := NewWith(50*time.Millisecond, nil, func() time.Time { return clock })
	c.Put(ServerStatus{Key: Key{Provider: ProviderClaude, Name: "github"}, Status: StatusConnected})
	clock = now.Add(time.Second)
	c.Put(ServerStatus{Key: Key{Provider: ProviderCodex, Name: "linear"}, Status: StatusConnected})
	snap := c.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected only the fresh entry, got %d entries", len(snap))
	}
	if snap[0].Name != "linear" {
		t.Fatalf("expected linear to survive, got %q", snap[0].Name)
	}
}

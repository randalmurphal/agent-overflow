package codexskills

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func sample(cwd string, names ...string) CwdSkills {
	entry := CwdSkills{Cwd: cwd}
	for _, name := range names {
		entry.Skills = append(entry.Skills, Skill{Name: name, Path: "/skills/" + name, Enabled: true})
	}
	return entry
}

func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestKeySeparatesBinaryAndCwd(t *testing.T) {
	// The separator has to be a byte a path cannot contain, or
	// ("codex", "/a/b") and ("codex/a", "/b") would collide and one
	// workspace's skills would be served as another's.
	if Key("codex", "/a/b") == Key("codex/a", "/b") {
		t.Fatal("binary and cwd must not be able to collide across the separator")
	}
	if Key(" codex ", " /repo ") != Key("codex", "/repo") {
		t.Fatal("Key must trim its inputs so a stray space is not a second cache entry")
	}
}

func TestCacheServesWithinTTLThenRefetches(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, time.Second, fixedClock(&now))

	calls := 0
	fetch := func(context.Context) (CwdSkills, error) {
		calls++
		return sample("/repo", "review"), nil
	}

	for i := 0; i < 3; i++ {
		got, err := c.Get(context.Background(), Key("codex", "/repo"), fetch)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got.Skills) != 1 || got.Skills[0].Name != "review" {
			t.Fatalf("Get = %+v, want one review skill", got)
		}
	}
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 inside the TTL", calls)
	}

	now = now.Add(2 * time.Minute)
	if _, err := c.Get(context.Background(), Key("codex", "/repo"), fetch); err != nil {
		t.Fatalf("Get after expiry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2 after the TTL expired", calls)
	}
}

func TestCacheKeysOnCwd(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, time.Second, fixedClock(&now))

	fetchFor := func(cwd string, name string) Fetch {
		return func(context.Context) (CwdSkills, error) { return sample(cwd, name), nil }
	}
	if _, err := c.Get(context.Background(), Key("codex", "/a"), fetchFor("/a", "alpha")); err != nil {
		t.Fatalf("Get /a: %v", err)
	}
	got, err := c.Get(context.Background(), Key("codex", "/b"), fetchFor("/b", "beta"))
	if err != nil {
		t.Fatalf("Get /b: %v", err)
	}
	// A cwd-less key would hand /a's skills back here, which is the whole
	// reason the dimension exists.
	if len(got.Skills) != 1 || got.Skills[0].Name != "beta" {
		t.Fatalf("second workspace = %+v, want its own skills", got)
	}
}

func TestCacheSharesErrorsRatherThanEmptyLists(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, 30*time.Second, fixedClock(&now))

	wantErr := errors.New("boom")
	calls := 0
	fetch := func(context.Context) (CwdSkills, error) {
		calls++
		return CwdSkills{}, wantErr
	}

	for i := 0; i < 2; i++ {
		got, err := c.Get(context.Background(), Key("codex", "/repo"), fetch)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want the fetch error; a failure must never read as an empty skill list", err)
		}
		if len(got.Skills) != 0 {
			t.Fatalf("failed Get returned skills: %+v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 (errors are cached briefly)", calls)
	}

	now = now.Add(31 * time.Second)
	if _, err := c.Get(context.Background(), Key("codex", "/repo"), fetch); !errors.Is(err, wantErr) {
		t.Fatalf("err after error TTL = %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2 after the error TTL expired", calls)
	}
}

func TestCacheSingleFlightsConcurrentGets(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, time.Second, fixedClock(&now))

	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	fetch := func(context.Context) (CwdSkills, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return sample("/repo", "review"), nil
	}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]CwdSkills, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.Get(context.Background(), Key("codex", "/repo"), fetch)
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			results[i] = got
		}()
	}
	// Give the racers time to pile onto the in-flight entry before the
	// fetch is allowed to finish.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 across %d concurrent Gets", calls, racers)
	}
	for i, got := range results {
		if len(got.Skills) != 1 {
			t.Fatalf("racer %d got %+v, want the shared result", i, got)
		}
	}
}

// Transition coverage: an invalidation that lands WHILE a read is in
// flight must not be undone by that read's result.
func TestResetDuringInFlightFetchIsNotUndone(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, time.Second, fixedClock(&now))

	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	fetch := func(context.Context) (CwdSkills, error) {
		calls++
		if calls == 1 {
			close(started)
			<-release
			return sample("/repo", "stale"), nil
		}
		return sample("/repo", "fresh"), nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.Get(context.Background(), Key("codex", "/repo"), fetch); err != nil {
			t.Errorf("first Get: %v", err)
		}
	}()

	<-started
	c.Reset() // the skills/changed edge, mid-read
	close(release)
	<-done

	got, err := c.Get(context.Background(), Key("codex", "/repo"), fetch)
	if err != nil {
		t.Fatalf("Get after reset: %v", err)
	}
	if len(got.Skills) != 1 || got.Skills[0].Name != "fresh" {
		t.Fatalf("Get after reset = %+v, want a refetch; the racing read must not have repopulated the cache", got)
	}
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", calls)
	}
}

// Transition coverage: Reset with nothing cached is a no-op, and the next
// read still populates normally.
func TestResetWithNoCachedEntryThenPopulates(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, time.Second, fixedClock(&now))
	c.Reset()

	calls := 0
	fetch := func(context.Context) (CwdSkills, error) {
		calls++
		return sample("/repo", "review"), nil
	}
	if _, err := c.Get(context.Background(), Key("codex", "/repo"), fetch); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Get(context.Background(), Key("codex", "/repo"), fetch); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls)
	}
}

func TestInvalidateThenRefetch(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, time.Second, fixedClock(&now))

	calls := 0
	fetch := func(context.Context) (CwdSkills, error) {
		calls++
		return sample("/repo", "review"), nil
	}
	key := Key("codex", "/repo")
	if _, err := c.Get(context.Background(), key, fetch); err != nil {
		t.Fatalf("Get: %v", err)
	}
	c.Invalidate(key)
	if _, err := c.Get(context.Background(), key, fetch); err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", calls)
	}
	// Another key must be untouched by a single-key invalidation.
	other := Key("codex", "/other")
	if _, err := c.Get(context.Background(), other, fetch); err != nil {
		t.Fatalf("Get other: %v", err)
	}
	if _, err := c.Get(context.Background(), other, fetch); err != nil {
		t.Fatalf("second Get other: %v", err)
	}
	if calls != 3 {
		t.Fatalf("fetch calls = %d, want 3 (the other key stayed cached)", calls)
	}
}

func TestRefreshBypassesTheCachedEntry(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, time.Second, fixedClock(&now))

	calls := 0
	fetch := func(context.Context) (CwdSkills, error) {
		calls++
		return sample("/repo", "review"), nil
	}
	key := Key("codex", "/repo")
	if _, err := c.Get(context.Background(), key, fetch); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Refresh(context.Background(), key, fetch); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2 (Refresh ignores a live entry)", calls)
	}
	// Refresh repopulates, so a following Get is served from cache.
	if _, err := c.Get(context.Background(), key, fetch); err != nil {
		t.Fatalf("Get after Refresh: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2 (Refresh must repopulate)", calls)
	}
}

func TestCacheHandsOutDefensiveCopies(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewWith(time.Minute, time.Second, fixedClock(&now))

	fetch := func(context.Context) (CwdSkills, error) {
		entry := sample("/repo", "review")
		entry.Errors = []LoadError{{Path: "/repo/.codex/skills", Message: "permission denied"}}
		return entry, nil
	}
	key := Key("codex", "/repo")
	first, err := c.Get(context.Background(), key, fetch)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	first.Skills[0].Name = "mutated"
	first.Errors[0].Message = "mutated"

	second, err := c.Get(context.Background(), key, fetch)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if second.Skills[0].Name != "review" || second.Errors[0].Message != "permission denied" {
		t.Fatalf("cached entry was mutated through a handed-out slice: %+v", second)
	}
}

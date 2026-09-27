package highlightapp

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSeedLifecycleRemoteGateAndPurge(t *testing.T) {
	var remote atomic.Bool
	remote.Store(true)
	var mu sync.Mutex
	var events []SeedEvent
	service := New(Config{HasRemoteClient: remote.Load, EmitSeed: func(event SeedEvent) { mu.Lock(); events = append(events, event); mu.Unlock() }})
	service.ObserveAssistantText("t1", "i1", "```go\nx := 1", false)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 1 })
	service.ObserveAssistantText("t1", "i1", "```go\nx := 1\n```", true)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 2 })
	if got := seedStateCount(service); got != 0 {
		t.Fatalf("states after final = %d", got)
	}
	mu.Lock()
	if len(events) != 2 || !events[1].Final || events[1].ContentKey == "" {
		t.Fatalf("events = %+v", events)
	}
	mu.Unlock()

	remote.Store(false)
	service.ObserveAssistantText("t2", "i1", "```go\ny := 2\n```", true)
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(events) != 2 {
		t.Fatalf("remote-gated events = %d, want 2", len(events))
	}
	mu.Unlock()

	remote.Store(true)
	service.ObserveAssistantText("t3", "i1", "```go\nz := 3", false)
	waitFor(t, func() bool { return seedStateCount(service) == 1 })
	service.PurgeThread("t3")
	if got := seedStateCount(service); got != 0 {
		t.Fatalf("states after purge = %d", got)
	}
}

func TestSeedSkipsInvalidOversizeAndLanguagelessFences(t *testing.T) {
	var mu sync.Mutex
	var events []SeedEvent
	service := New(Config{HasRemoteClient: func() bool { return true }, EmitSeed: func(event SeedEvent) { mu.Lock(); events = append(events, event); mu.Unlock() }})
	for _, text := range []string{"```\nplain\n```", "```go\n" + strings.Repeat("x", seedMaxSourceBytes+1) + "\n```", "```go\n\xff\n```"} {
		service.ObserveAssistantText("t", "i", text, true)
	}
	waitFor(t, func() bool { return service.seeder.ephemeralWorkers.Load() == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

func TestBuildPersistedCodeSpansGuardsAndOpenFence(t *testing.T) {
	service := New(Config{})
	blob := service.BuildPersistedCodeSpans("```go\nx := 1")
	var spans PersistedCodeSpans
	if err := json.Unmarshal(blob, &spans); err != nil {
		t.Fatal(err)
	}
	if len(spans.Blocks) != 1 || spans.Blocks[0].ContentKey == "" {
		t.Fatalf("spans = %+v", spans)
	}
	for _, text := range []string{"", "no fences", strings.Repeat("x", seedMaxScanBytes+1)} {
		if got := service.BuildPersistedCodeSpans(text); got != nil {
			t.Fatalf("BuildPersistedCodeSpans(%d bytes) = %s", len(text), got)
		}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func seedStateCount(service *Service) int {
	service.seeder.mu.Lock()
	defer service.seeder.mu.Unlock()
	return len(service.seeder.states)
}

// TestSeedStateHoldsNoTextAndEmptyFinalReleases streams a row to a remote
// client: once its tick is processed the row's state holds no text, and an
// empty final tick releases the state without seeding anything.
func TestSeedStateHoldsNoTextAndEmptyFinalReleases(t *testing.T) {
	var mu sync.Mutex
	var events []SeedEvent
	service := New(Config{HasRemoteClient: func() bool { return true }, EmitSeed: func(event SeedEvent) { mu.Lock(); events = append(events, event); mu.Unlock() }})
	service.ObserveAssistantText("t1", "i1", "```go\nx := 1", false)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 1 })
	waitFor(t, func() bool {
		service.seeder.mu.Lock()
		defer service.seeder.mu.Unlock()
		state := service.seeder.states["t1|i1"]
		return state != nil && !state.busy
	})
	service.seeder.mu.Lock()
	held := service.seeder.states["t1|i1"].pending.text
	service.seeder.mu.Unlock()
	if held != "" {
		t.Fatalf("idle state holds %d bytes of text", len(held))
	}
	service.ObserveAssistantText("t1", "i1", "", true)
	if got := seedStateCount(service); got != 0 {
		t.Fatalf("states after an empty final = %d, want 0", got)
	}
	if n := service.seeder.ephemeralWorkers.Load(); n != 0 {
		t.Fatalf("an empty final started %d workers", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("an empty final seeded: %+v", events)
	}
}

// TestEmptyFinalKeepsAQueuedFinalTick ends a row twice, as a stop does: a
// final tick with the row's text, then the settle's empty final. The
// second must not drop the first while it waits behind a busy worker.
func TestEmptyFinalKeepsAQueuedFinalTick(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var events []SeedEvent
	service := New(Config{HasRemoteClient: func() bool { return true }, EmitSeed: func(event SeedEvent) {
		mu.Lock()
		events = append(events, event)
		first := len(events) == 1
		mu.Unlock()
		if first {
			<-release
		}
	}})
	service.ObserveAssistantText("t1", "i1", "```go\nx := 1", false)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 1 })
	service.ObserveAssistantText("t1", "i1", "```go\nx := 1\n", true)
	service.ObserveAssistantText("t1", "i1", "", true)
	close(release)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 2 })
	mu.Lock()
	final := events[1]
	mu.Unlock()
	if !final.Final || final.ContentKey == "" {
		t.Fatalf("second seed = %+v, want the row's final seed", final)
	}
	waitFor(t, func() bool { return seedStateCount(service) == 0 })
}

package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/highlight"
)

func collectHighlightSeeds(a *App) (*[]HighlightSeedEvent, *sync.Mutex) {
	events := &[]HighlightSeedEvent{}
	mu := &sync.Mutex{}
	a.testEmitHook = func(name string, data any) {
		if name != "highlight:seed" {
			return
		}
		evt, ok := data.(HighlightSeedEvent)
		if !ok {
			return
		}
		mu.Lock()
		*events = append(*events, evt)
		mu.Unlock()
	}
	return events, mu
}

// waitForSeeds polls until the captured slice has at least want
// entries (the seed worker runs on its own goroutine).
func waitForSeeds(t *testing.T, events *[]HighlightSeedEvent, mu *sync.Mutex, want int) []HighlightSeedEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := make([]HighlightSeedEvent, len(*events))
		copy(got, *events)
		mu.Unlock()
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d seed events, have %d: %#v", want, len(got), got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (a *App) seedStateCount() int {
	a.highlightSeeder.mu.Lock()
	defer a.highlightSeeder.mu.Unlock()
	return len(a.highlightSeeder.states)
}

func waitForSeedStates(t *testing.T, a *App, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.seedStateCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d seed states, have %d", want, a.seedStateCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHighlightSeedPushLifecycle(t *testing.T) {
	a := &App{remoteClientProbeFn: func() bool { return true }}
	events, mu := collectHighlightSeeds(a)

	closed := "```python\ndef f():\n    pass\n```"
	streaming := closed + "\nprose\n```go\nfunc main() {"
	a.observeAssistantTextStream("t1", "i1", streaming, false)

	got := waitForSeeds(t, events, mu, 2)
	if !got[0].Final || got[0].Lang != "python" || got[0].ContentKey == "" {
		t.Fatalf("closed fence seed not final with content key: %#v", got[0])
	}
	if got[0].ThreadID != "t1" || got[0].ItemID != "i1" {
		t.Fatalf("seed must carry its stream identity: %#v", got[0])
	}
	wantKey := highlight.FrontendContentKey("def f():\n    pass")
	if got[0].ContentKey != wantKey {
		t.Fatalf("content key = %q, want %q", got[0].ContentKey, wantKey)
	}
	if got[1].Final || got[1].Lang != "go" || got[1].ContentKey != "" {
		t.Fatalf("open fence seed should be non-final without content key: %#v", got[1])
	}
	if len(got[1].LineHashes) != 1 {
		t.Fatalf("open fence line hashes = %v, want 1 entry", got[1].LineHashes)
	}

	// The open fence grows, then the item settles with the fence
	// closed: the growth tick re-pushes only the open fence (the
	// closed python fence stays behind the watermark), and the final
	// tick pushes it once as final.
	grown := streaming + "\n\tprintln()"
	a.observeAssistantTextStream("t1", "i1", grown, false)
	got = waitForSeeds(t, events, mu, 3)
	if got[2].Final || got[2].Lang != "go" || len(got[2].LineHashes) != 2 {
		t.Fatalf("grown open fence seed wrong: %#v", got[2])
	}

	final := grown + "\n}\n```\n"
	a.observeAssistantTextStream("t1", "i1", final, true)
	got = waitForSeeds(t, events, mu, 4)
	if !got[3].Final || got[3].Lang != "go" {
		t.Fatalf("settle seed not final: %#v", got[3])
	}
	if got[3].ContentKey != highlight.FrontendContentKey("func main() {\n\tprintln()\n}") {
		t.Fatalf("settle seed content key wrong: %#v", got[3])
	}
	waitForSeedStates(t, a, 0)
}

func TestHighlightSeedSkipsWithoutRemoteClient(t *testing.T) {
	a := &App{remoteClientProbeFn: func() bool { return false }}
	events, mu := collectHighlightSeeds(a)

	a.observeAssistantTextStream("t1", "i1", "```python\npass\n```", false)
	a.observeAssistantTextStream("t1", "i1", "```python\npass\n```", true)

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*events) != 0 {
		t.Fatalf("expected no seeds without a remote client, got %#v", *events)
	}
	if a.seedStateCount() != 0 {
		t.Fatalf("expected no retained state, got %d", a.seedStateCount())
	}
}

// A remote client disconnecting mid-stream must not strand the item's
// scanner state past its settle.
func TestHighlightSeedDropsStateWhenRemoteLeavesBeforeSettle(t *testing.T) {
	remote := true
	var remoteMu sync.Mutex
	a := &App{remoteClientProbeFn: func() bool {
		remoteMu.Lock()
		defer remoteMu.Unlock()
		return remote
	}}
	collectHighlightSeeds(a)

	a.observeAssistantTextStream("t1", "i1", "```python\npass", false)
	waitForSeedStates(t, a, 1)

	remoteMu.Lock()
	remote = false
	remoteMu.Unlock()
	a.observeAssistantTextStream("t1", "i1", "```python\npass\n```", true)
	waitForSeedStates(t, a, 0)
}

// A whole-block message can settle without ever hitting a flush
// window; its final tick is the only tick and must still seed.
func TestHighlightSeedFinalTickWithoutPriorState(t *testing.T) {
	a := &App{remoteClientProbeFn: func() bool { return true }}
	events, mu := collectHighlightSeeds(a)

	a.observeAssistantTextStream("t1", "i1", "```python\npass\n```", true)
	got := waitForSeeds(t, events, mu, 1)
	if !got[0].Final || got[0].Lang != "python" {
		t.Fatalf("ephemeral final seed wrong: %#v", got[0])
	}
	if a.seedStateCount() != 0 {
		t.Fatalf("ephemeral final tick registered state: %d", a.seedStateCount())
	}
}

// Final-only ticks bypass seedMaxStates by design (nothing registers),
// so they carry their own in-flight worker bound: a saturated counter
// drops the seed (RPC path covers) without corrupting the budget.
func TestHighlightSeedEphemeralFinalTickDropsPastWorkerCap(t *testing.T) {
	a := &App{remoteClientProbeFn: func() bool { return true }}
	events, mu := collectHighlightSeeds(a)

	a.highlightSeeder.ephemeralWorkers.Store(seedMaxEphemeralWorkers)
	a.observeAssistantTextStream("t1", "i1", "```python\npass\n```", true)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := len(*events)
	mu.Unlock()
	if got != 0 {
		t.Fatalf("expected saturated ephemeral tick to drop, got %d events", got)
	}
	if load := a.highlightSeeder.ephemeralWorkers.Load(); load != seedMaxEphemeralWorkers {
		t.Fatalf("drop must restore the counter, got %d", load)
	}

	// The cap applies only to unregistered final-only ticks: a stream
	// with registered state settles through its own worker and must
	// still seed while the ephemeral budget is exhausted. (The open
	// fence seeds once non-final, then once final at settle.)
	a.observeAssistantTextStream("t1", "i2", "```python\npass", false)
	// Wait for the open-fence seed before settling, or the two ticks
	// coalesce and only the final one is processed.
	waitForSeeds(t, events, mu, 1)
	a.observeAssistantTextStream("t1", "i2", "```python\npass\n```", true)
	seeds := waitForSeeds(t, events, mu, 2)
	if !seeds[1].Final || seeds[1].ItemID != "i2" {
		t.Fatalf("registered stream must settle past a saturated ephemeral cap: %#v", seeds)
	}
	waitForSeedStates(t, a, 0)

	// With budget available again, an ephemeral tick runs and the
	// counter returns to its floor once the worker finishes.
	a.highlightSeeder.ephemeralWorkers.Store(0)
	a.observeAssistantTextStream("t1", "i3", "```go\nok := true\n```", true)
	waitForSeeds(t, events, mu, 3)
	deadline := time.Now().Add(5 * time.Second)
	for a.highlightSeeder.ephemeralWorkers.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("ephemeral worker counter stuck at %d", a.highlightSeeder.ephemeralWorkers.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHighlightSeedSkipsLanguagelessAndOversizeFences(t *testing.T) {
	a := &App{remoteClientProbeFn: func() bool { return true }}
	events, mu := collectHighlightSeeds(a)

	big := "```python\n" + strings.Repeat("x = 1\n", seedMaxSourceBytes/6+1) + "```"
	text := "```\nplain\n```\n" + big + "\n```go\nok := true\n```"
	a.observeAssistantTextStream("t1", "i1", text, true)

	got := waitForSeeds(t, events, mu, 1)
	if len(got) != 1 || got[0].Lang != "go" {
		t.Fatalf("expected only the go fence to seed, got %#v", got)
	}
}

// Invalid UTF-8 fence content must never seed: the wire's U+FFFD
// replacement would make the client's hash chain MATCH while the spans
// cover the original byte lengths — misaligned colors, not a cache miss.
func TestHighlightSeedSkipsInvalidUTF8Fence(t *testing.T) {
	a := &App{remoteClientProbeFn: func() bool { return true }}
	events, mu := collectHighlightSeeds(a)

	text := "```python\nbad = \"\xff\xfe\"\n```\n```go\nok := true\n```"
	a.observeAssistantTextStream("t1", "i1", text, true)

	got := waitForSeeds(t, events, mu, 1)
	if len(got) != 1 || got[0].Lang != "go" {
		t.Fatalf("expected only the valid fence to seed, got %#v", got)
	}
}

// A tick whose text regressed below the fence watermark (buffer
// replaced, not extended — retry, revert) must reseed from scratch
// instead of skipping the replacement's first fences.
func TestHighlightSeedResetsWatermarkOnRegression(t *testing.T) {
	a := &App{remoteClientProbeFn: func() bool { return true }}
	events, mu := collectHighlightSeeds(a)

	a.observeAssistantTextStream("t1", "i1", "```python\none = 1\n```\n```python\ntwo = 2\n```", false)
	waitForSeeds(t, events, mu, 2)

	// Replacement stream: fewer fences than the watermark (2).
	a.observeAssistantTextStream("t1", "i1", "```go\nthree := 3\n```", true)
	got := waitForSeeds(t, events, mu, 3)
	if got[2].Lang != "go" || !got[2].Final {
		t.Fatalf("replacement fence must seed after regression reset: %#v", got[2])
	}
	waitForSeedStates(t, a, 0)
}

// Session teardown purges a thread's stranded scanner states (a killed
// stream never delivers the final tick that would clear them) without
// touching other threads'.
func TestHighlightSeederPurgeThread(t *testing.T) {
	a := &App{remoteClientProbeFn: func() bool { return true }}
	collectHighlightSeeds(a)

	a.observeAssistantTextStream("t1", "i1", "```python\npass", false)
	a.observeAssistantTextStream("t2", "i1", "```python\npass", false)
	waitForSeedStates(t, a, 2)

	a.highlightSeeder.purgeThread("t1")
	waitForSeedStates(t, a, 1)
	a.highlightSeeder.mu.Lock()
	_, t2Alive := a.highlightSeeder.states["t2|i1"]
	a.highlightSeeder.mu.Unlock()
	if !t2Alive {
		t.Fatal("purgeThread must not touch other threads' states")
	}

	// Purging an absent thread is a no-op, and the surviving stream
	// still settles normally afterwards.
	a.highlightSeeder.purgeThread("t-absent")
	a.observeAssistantTextStream("t2", "i1", "```python\npass\n```", true)
	waitForSeedStates(t, a, 0)
}

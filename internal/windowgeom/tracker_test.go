package windowgeom

import (
	"testing"
	"time"
)

// neverFires is a debounce long enough that the timer never fires during a
// test; delivery is forced via Flush, which calls the sink synchronously.
const neverFires = time.Hour

func TestTrackerFlushPersistsSeededSample(t *testing.T) {
	var got Geometry
	tr := NewTracker(neverFires, func(g Geometry) { got = g }, Geometry{})

	tr.Record(Sample{Bounds: Rect{X: 50, Y: 60, Width: 800, Height: 600}, Display: primary})
	tr.Flush()

	if !got.Valid {
		t.Fatal("sink geometry not valid")
	}
	if got.X != 50 || got.Y != 60 || got.Width != 800 || got.Height != 600 {
		t.Fatalf("sink geometry = %+v, want 50,60,800,600", got)
	}
}

func TestTrackerIgnoresMinimizedSamples(t *testing.T) {
	calls := 0
	tr := NewTracker(neverFires, func(Geometry) { calls++ }, Geometry{})

	tr.Record(Sample{Bounds: Rect{X: -32000, Y: -32000, Width: 160, Height: 28}, Minimized: true})
	tr.Flush()

	if calls != 0 {
		t.Fatalf("sink called %d times, want 0 (minimized samples dropped, nothing else recorded)", calls)
	}
}

func TestTrackerKeepsNormalRectWhileMaximized(t *testing.T) {
	initial := Geometry{X: 100, Y: 120, Width: 900, Height: 700, Display: primary, Valid: true}
	var got Geometry
	tr := NewTracker(neverFires, func(g Geometry) { got = g }, initial)

	// User maximizes: the event reports the maximized bounds.
	tr.Record(Sample{Bounds: Rect{X: 0, Y: 0, Width: 1920, Height: 1080}, Maximized: true, Display: primary})
	tr.Flush()

	if !got.Maximized {
		t.Fatal("got.Maximized = false, want true")
	}
	if got.X != 100 || got.Y != 120 || got.Width != 900 || got.Height != 700 {
		t.Fatalf("normal rect = %+v, want preserved 100,120,900,700 (not the maximized bounds)", got)
	}
}

func TestTrackerMaximizedFirstSampleDoesNotSeedNormalRect(t *testing.T) {
	// First observation (nothing restored) is itself maximized: the maximized
	// bounds must NOT become the normal rect, otherwise un-maximize would
	// restore a monitor-sized window. The flag is recorded; the rect stays zero
	// until a real normal sample arrives.
	var got Geometry
	tr := NewTracker(neverFires, func(g Geometry) { got = g }, Geometry{})

	tr.Record(Sample{Bounds: Rect{X: 0, Y: 0, Width: 1920, Height: 1080}, Maximized: true, Display: primary})
	tr.Flush()

	if !got.Valid || !got.Maximized {
		t.Fatalf("got = %+v, want valid + maximized", got)
	}
	if got.Width != 0 || got.Height != 0 || got.X != 0 || got.Y != 0 {
		t.Fatalf("normal rect = %+v, want zero (maximized bounds not seeded as normal rect)", got)
	}
	if got.Display != primary {
		t.Fatalf("got.Display = %+v, want %+v", got.Display, primary)
	}
}

func TestTrackerFlushNoOpsWhenEmpty(t *testing.T) {
	calls := 0
	tr := NewTracker(neverFires, func(Geometry) { calls++ }, Geometry{})
	tr.Flush()
	if calls != 0 {
		t.Fatalf("sink called %d times on empty tracker, want 0", calls)
	}
}

func TestTrackerStopsRecordingAfterFlush(t *testing.T) {
	calls := 0
	tr := NewTracker(neverFires, func(Geometry) { calls++ }, Geometry{})
	tr.Record(Sample{Bounds: Rect{X: 0, Y: 0, Width: 800, Height: 600}})
	tr.Flush() // 1 delivery
	tr.Record(Sample{Bounds: Rect{X: 10, Y: 10, Width: 800, Height: 600}})
	tr.Flush() // must not deliver again
	if calls != 1 {
		t.Fatalf("sink called %d times, want 1 (post-Flush records ignored)", calls)
	}
}

func TestTrackerDebouncesBurstIntoOneWrite(t *testing.T) {
	delivered := make(chan Geometry, 8)
	tr := NewTracker(20*time.Millisecond, func(g Geometry) { delivered <- g }, Geometry{})

	// A burst of moves within the debounce window should collapse to one write.
	for i := 0; i < 5; i++ {
		tr.Record(Sample{Bounds: Rect{X: i, Y: i, Width: 800, Height: 600}})
	}

	select {
	case got := <-delivered:
		if got.X != 4 || got.Y != 4 {
			t.Fatalf("delivered geometry = %d,%d, want last sample 4,4", got.X, got.Y)
		}
	case <-time.After(time.Second):
		t.Fatal("no write delivered within timeout")
	}

	select {
	case extra := <-delivered:
		t.Fatalf("second write delivered (%+v), want burst coalesced to one", extra)
	case <-time.After(80 * time.Millisecond):
	}
}

package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func compactingEmissions(emissions *emissionLog) []CompactingStateEvent {
	var out []CompactingStateEvent
	for _, e := range emissions.snapshot() {
		if e.eventName != "provider:compacting" {
			continue
		}
		if payload, ok := e.data.(CompactingStateEvent); ok {
			out = append(out, payload)
		}
	}
	return out
}

func compactionStatusEvent(t *testing.T, threadID string, meta provider.CompactionStatusMeta, at time.Time) provider.ProviderEvent {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return provider.ProviderEvent{
		Kind:      provider.EventCompactionStatus,
		ThreadID:  threadID,
		Meta:      raw,
		Timestamp: at,
	}
}

func TestCompacting_OpenAndExplicitClose(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	opened := time.UnixMilli(1_754_000_000_000)

	if err := router.Handle(compactionStatusEvent(t, "t1", provider.CompactionStatusMeta{Active: true}, opened)); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := router.Handle(compactionStatusEvent(t, "t1", provider.CompactionStatusMeta{Result: "success"}, opened.Add(2*time.Minute))); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := compactingEmissions(emissions)
	if len(got) != 2 {
		t.Fatalf("emissions = %+v, want open+close", got)
	}
	if !got[0].Active || got[0].SinceUnixMs != opened.UnixMilli() {
		t.Fatalf("open frame = %+v", got[0])
	}
	if got[1].Active || got[1].SinceUnixMs != 0 {
		t.Fatalf("close frame = %+v", got[1])
	}
}

// Claude re-emits `status:"compacting"` as a 30s keep-alive on
// remote-bridged sessions; repeats must neither restart the window's
// anchor nor spam the frontend with duplicate frames.
func TestCompacting_RepeatedOpenIsIdempotent(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	opened := time.UnixMilli(1_754_000_000_000)

	for i := 0; i < 3; i++ {
		evt := compactionStatusEvent(t, "t1", provider.CompactionStatusMeta{Active: true}, opened.Add(time.Duration(i)*30*time.Second))
		if err := router.Handle(evt); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}

	got := compactingEmissions(emissions)
	if len(got) != 1 {
		t.Fatalf("emissions = %+v, want exactly one open frame", got)
	}
	if got[0].SinceUnixMs != opened.UnixMilli() {
		t.Fatalf("since = %d, want the FIRST frame's anchor %d", got[0].SinceUnixMs, opened.UnixMilli())
	}
	if snap := router.LiveStateSnapshotForThread("t1"); snap.CompactingSinceUnixMs != opened.UnixMilli() {
		t.Fatalf("snapshot since = %d, want %d", snap.CompactingSinceUnixMs, opened.UnixMilli())
	}
}

// A close with no open window emits nothing — the frontend never saw an
// open frame, so an inactive frame would be noise (and on a fresh
// session, a stale-looking signal).
func TestCompacting_CloseWithoutOpenEmitsNothing(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	err := router.Handle(compactionStatusEvent(t, "t1", provider.CompactionStatusMeta{Result: "failed", ErrorMessage: "API Error: Request was aborted."}, time.Now()))
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := compactingEmissions(emissions); len(got) != 0 {
		t.Fatalf("emissions = %+v, want none", got)
	}
}

// The compact boundary is both providers' success signal and must close
// the window even when no explicit close frame arrived (Codex's open is
// item/started; its completed item IS the boundary).
func TestCompacting_CompactBoundaryClosesWindow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(compactionStatusEvent(t, "t1", provider.CompactionStatusMeta{Active: true}, time.Now())); err != nil {
		t.Fatalf("open: %v", err)
	}
	err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCompactBoundary,
		ThreadID:  "t1",
		Content:   "Context compacted",
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("boundary: %v", err)
	}

	got := compactingEmissions(emissions)
	if len(got) != 2 || got[1].Active {
		t.Fatalf("emissions = %+v, want open then close", got)
	}
	if snap := router.LiveStateSnapshotForThread("t1"); snap.CompactingSinceUnixMs != 0 {
		t.Fatalf("snapshot still compacting: %d", snap.CompactingSinceUnixMs)
	}
}

// A turn boundary closes the window defensively: a failed Codex
// compaction abandons its item (no completed half, no boundary), so the
// turn's own completion is the only reliable close on that path.
func TestCompacting_TurnCompleteClosesWindow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(compactionStatusEvent(t, "t1", provider.CompactionStatusMeta{Active: true}, time.Now())); err != nil {
		t.Fatalf("open: %v", err)
	}
	err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	})
	if err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	got := compactingEmissions(emissions)
	if len(got) != 2 || got[1].Active {
		t.Fatalf("emissions = %+v, want open then close", got)
	}
}

// Transition coverage for the sweeps: CleanupThread and MarkThreadActive
// both drop the window WITHOUT emitting (the frontend clears its copy on
// the session-teardown path), and a fresh open after either sweep works.
func TestCompacting_SweepsDropWindowSilently(t *testing.T) {
	sweeps := []struct {
		name  string
		sweep func(router *Router)
	}{
		{"cleanupThread", func(router *Router) { router.CleanupThread("t1") }},
		{"markThreadActive", func(router *Router) { router.MarkThreadActive("t1") }},
	}
	for _, tc := range sweeps {
		t.Run(tc.name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")

			if err := router.Handle(compactionStatusEvent(t, "t1", provider.CompactionStatusMeta{Active: true}, time.Now())); err != nil {
				t.Fatalf("open: %v", err)
			}
			tc.sweep(router)
			if snap := router.LiveStateSnapshotForThread("t1"); snap.CompactingSinceUnixMs != 0 {
				t.Fatalf("snapshot survived %s: %d", tc.name, snap.CompactingSinceUnixMs)
			}
			if got := compactingEmissions(emissions); len(got) != 1 {
				t.Fatalf("emissions after %s = %+v, want only the original open", tc.name, got)
			}

			// A replacement session must be able to open a fresh window.
			router.MarkThreadActive("t1")
			reopened := time.UnixMilli(1_754_000_900_000)
			if err := router.Handle(compactionStatusEvent(t, "t1", provider.CompactionStatusMeta{Active: true}, reopened)); err != nil {
				t.Fatalf("reopen: %v", err)
			}
			if snap := router.LiveStateSnapshotForThread("t1"); snap.CompactingSinceUnixMs != reopened.UnixMilli() {
				t.Fatalf("reopen snapshot = %d, want %d", snap.CompactingSinceUnixMs, reopened.UnixMilli())
			}
		})
	}
}

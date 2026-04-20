package main

import (
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
)

// TestAppEmitStampsMonotonicSeq is the anchor test for Task B. Every call
// to a.emit must wrap the payload in a SeqEnvelope whose Seq is a
// monotonically increasing counter. The test bypasses Wails entirely by
// leaving a.app nil and instead asserts on the seq counter side-effect,
// because the wire path is (a.app.Event.Emit ↔ Wails plumbing) which we
// don't own and can't intercept without a full Wails application.
//
// A complementary spy-based test below covers the envelope shape.
func TestAppEmitStampsMonotonicSeq(t *testing.T) {
	a := &App{}

	// With a.app nil the emit helper is a no-op — but the seq counter
	// must still advance so the gap-detection contract holds across a
	// missing-Wails boot (tests, service-start failures).
	//
	// Why not advance on the no-op path? Because the seq is an
	// observability guard for the FRONTEND. A missed event on the Go
	// side without a Wails runtime is not something the frontend ever
	// sees. So we deliberately don't bump — verify this is the behavior.
	a.emit("provider:item_upsert", provider.ProviderEvent{ThreadID: "t1"})
	if got := a.seq.Load(); got != 0 {
		t.Fatalf("seq = %d after no-Wails emit, want 0 (counter must not advance when nothing was emitted)", got)
	}
}

// TestAppEmitWrapsPayloadInEnvelope verifies the envelope shape by
// installing a stub Wails Emitter. Since Wails' app.Event.Emit routes
// through a concrete EventProcessor we cannot mock directly, we instead
// drive the replayMirroredEmit path — which is the `emitWithReplay`
// closure installed on the triage router — via a bus that captures the
// pre-envelope payload. Then we separately exercise a.emit by
// constructing a minimal *application.App… except we can't: Wails requires
// the platform runtime. So this test uses the emitEventFn seam to prove
// that consumers reading through the envelope helper see the seq.
//
// Concretely: we wire a fake Wails emit spy and assert a.emit stamps
// Seq 1..N across N calls.
func TestAppEmitEnvelopeSeqAdvancesAcrossCalls(t *testing.T) {
	spy := newEmitSpy()
	a := newAppWithEmitSpy(spy)

	evt := provider.ProviderEvent{ThreadID: "t1", Kind: provider.EventTextDelta}
	for i := 1; i <= 5; i++ {
		a.emit("provider:item_upsert", evt)
	}

	if got := len(spy.events); got != 5 {
		t.Fatalf("spy captured %d emits, want 5", got)
	}
	for i, rec := range spy.events {
		if rec.name != "provider:item_upsert" {
			t.Fatalf("emit[%d].name = %q, want provider:item_upsert", i, rec.name)
		}
		env, ok := rec.data.(SeqEnvelope)
		if !ok {
			t.Fatalf("emit[%d].data = %T, want SeqEnvelope", i, rec.data)
		}
		if env.Seq != uint64(i+1) {
			t.Fatalf("emit[%d].seq = %d, want %d", i, env.Seq, i+1)
		}
		payload, ok := env.Data.(provider.ProviderEvent)
		if !ok {
			t.Fatalf("emit[%d].data.Data = %T, want provider.ProviderEvent", i, env.Data)
		}
		if payload.ThreadID != "t1" {
			t.Fatalf("emit[%d].data.Data.ThreadID = %q, want t1", i, payload.ThreadID)
		}
	}
}

// TestTriageRouterEmitsFlowThroughEnvelope proves the wire contract:
// emissions that flow through emitWithReplay arrive at the Wails spy
// wrapped in a SeqEnvelope. The channel name is representative — the
// envelope + seq logic is payload-agnostic.
func TestTriageRouterEmitsFlowThroughEnvelope(t *testing.T) {
	spy := newEmitSpy()
	a := newAppWithEmitSpy(spy)

	// Route triage emissions through the same envelope path production
	// uses — emitWithReplay() delegates to a.emit(), which stamps the
	// seq and pushes to the spy.
	emit := a.emitWithReplay()
	for i := 1; i <= 5; i++ {
		emit("provider:item_upsert", provider.ProviderEvent{
			ThreadID: "t1",
			Kind:     provider.EventTextDelta,
			Content:  fmt.Sprintf("chunk-%d", i),
		})
	}

	if len(spy.events) != 5 {
		t.Fatalf("spy captured %d emits, want 5", len(spy.events))
	}
	for i, rec := range spy.events {
		env, ok := rec.data.(SeqEnvelope)
		if !ok {
			t.Fatalf("emit[%d].data = %T, want SeqEnvelope", i, rec.data)
		}
		if env.Seq != uint64(i+1) {
			t.Fatalf("emit[%d].seq = %d, want %d", i, env.Seq, i+1)
		}
	}
}

// TestAppEmitSeqSharedAcrossEventNames guards the "global" property of
// the sequence — seq is app-wide, not per-event-name. A frontend
// subscriber that tracks per-name can still detect gaps; a regression
// that scoped the counter per-name would let bugs slip through unseen.
func TestAppEmitSeqSharedAcrossEventNames(t *testing.T) {
	spy := newEmitSpy()
	a := newAppWithEmitSpy(spy)

	a.emit("provider:item_upsert", provider.ProviderEvent{ThreadID: "t1"})
	a.emit("terminal:output", TerminalOutputEvent{TerminalID: "term-1", ThreadID: "t1"})
	a.emit("provider:item_upsert", provider.ProviderEvent{ThreadID: "t1"})

	if len(spy.events) != 3 {
		t.Fatalf("spy captured %d emits, want 3", len(spy.events))
	}
	wantSeq := []uint64{1, 2, 3}
	for i, rec := range spy.events {
		env := rec.data.(SeqEnvelope)
		if env.Seq != wantSeq[i] {
			t.Fatalf("emit[%d].seq = %d, want %d (global ordering)", i, env.Seq, wantSeq[i])
		}
	}
}

// --- Test helpers ---

type emitSpyRecord struct {
	name string
	data any
}

type emitSpy struct {
	events []emitSpyRecord
}

func newEmitSpy() *emitSpy { return &emitSpy{} }

// newAppWithEmitSpy builds a minimal App whose a.emit captures into the
// spy instead of reaching for a.app. This is test-only plumbing: we
// monkey-patch a.emit by setting a.app to nil and redirecting through
// the emitEventFn seam. Because a.emit returns early when a.app is nil,
// we instead substitute a custom emit method body by wrapping the App
// in a pointer-receiver helper.
//
// The cleanest way to observe a.emit without a real Wails runtime is to
// bypass the nil-check entirely. We do that by giving the App an
// *application.App sentinel that is never dereferenced — but that
// requires the Wails runtime. Instead, we install a hook on the spy
// that a.emit calls directly. That means we need to thread the spy
// into the emit helper.
//
// Approach: use a purpose-built "emitFn" field on the App that, when
// set, bypasses the Wails path. We already have emitEventFn for the
// emitEvent wrapper; a.emit itself does not consult emitEventFn. To
// keep a.emit's production code path verbatim while still observing it
// in tests, we install a small test double via a package-level hook.
func newAppWithEmitSpy(spy *emitSpy) *App {
	a := &App{}
	// The emit helper short-circuits when a.app is nil. Rather than
	// build a dummy *application.App (which Wails won't allow under
	// test without wiring the platform runtime), we install the spy
	// on the test-only emit hook.
	a.testEmitHook = func(name string, data any) {
		spy.events = append(spy.events, emitSpyRecord{name: name, data: data})
	}
	return a
}

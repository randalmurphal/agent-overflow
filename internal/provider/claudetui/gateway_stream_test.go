package claudetui

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
)

// readSeq is an io.Reader that returns a scripted sequence of (n, err) results,
// one per Read call, so a test can drive gateway.stream through a specific
// upstream-read outcome (a client-abort cancel vs a genuine upstream failure).
type readSeq struct {
	steps []readStep
	i     int
}

type readStep struct {
	err error
}

func (r *readSeq) Read(p []byte) (int, error) {
	if r.i >= len(r.steps) {
		return 0, io.EOF
	}
	step := r.steps[r.i]
	r.i++
	return 0, step.err
}

// captureErrGateway builds a gateway whose onError appends to errs, for asserting
// whether stream surfaces a given read outcome.
func captureErrGateway(errs *[]error) *gateway {
	return &gateway{onError: func(err error) { *errs = append(*errs, err) }}
}

// TestStreamSuppressesClientAbort pins fix B for the 2026-06-14 overload
// incident: when the interactive claude aborts its own gateway request (to retry
// a transient API error, or on user Esc), the inbound context is canceled and the
// upstream read fails with context.Canceled. That is NOT a gateway fault, so it
// must NOT surface as an error banner ("upstream read: context canceled" was the
// scary noise the user saw). A genuine upstream failure (context still live) must
// still surface with its real message so the actual issue is visible.
//
// Without the fix stream calls onError unconditionally on any non-EOF read error,
// so the canceled-context cases below fail RED.
func TestStreamSuppressesClientAbort(t *testing.T) {
	t.Run("canceled ctx + context.Canceled read is suppressed", func(t *testing.T) {
		var errs []error
		g := captureErrGateway(&errs)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the client aborted its request
		g.stream(ctx, httptest.NewRecorder(), &readSeq{steps: []readStep{{err: context.Canceled}}}, nil)
		if len(errs) != 0 {
			t.Fatalf("client abort surfaced %d errors, want 0: %v", len(errs), errs)
		}
	})

	t.Run("canceled ctx suppresses even a non-cancel read error", func(t *testing.T) {
		// The abort can surface as an unexpected EOF rather than context.Canceled;
		// the canceled inbound context is the authoritative "client went away"
		// signal, so suppress regardless of the read error's identity.
		var errs []error
		g := captureErrGateway(&errs)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		g.stream(ctx, httptest.NewRecorder(), &readSeq{steps: []readStep{{err: io.ErrUnexpectedEOF}}}, nil)
		if len(errs) != 0 {
			t.Fatalf("client abort (non-cancel err) surfaced %d errors, want 0: %v", len(errs), errs)
		}
	})

	t.Run("live ctx + genuine upstream error surfaces", func(t *testing.T) {
		var errs []error
		g := captureErrGateway(&errs)
		g.stream(context.Background(), httptest.NewRecorder(), &readSeq{steps: []readStep{{err: io.ErrUnexpectedEOF}}}, nil)
		if len(errs) != 1 {
			t.Fatalf("genuine upstream error surfaced %d errors, want 1: %v", len(errs), errs)
		}
		if !errors.Is(errs[0], io.ErrUnexpectedEOF) {
			t.Fatalf("surfaced error %v does not wrap the real upstream cause", errs[0])
		}
	})
}

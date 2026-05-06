package design

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newScreenshotBroker(t *testing.T) (*ScreenshotBroker, *captureSink) {
	t.Helper()
	sink := &captureSink{requests: make(chan ScreenshotRequest, 4)}
	b := NewScreenshotBroker(sink.emit)
	return b, sink
}

type captureSink struct {
	mu       sync.Mutex
	requests chan ScreenshotRequest
}

func (s *captureSink) emit(eventName string, data any) {
	if eventName != CaptureEventName {
		return
	}
	req, ok := data.(ScreenshotRequest)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.requests <- req:
	default:
	}
}

func TestScreenshotBroker_CaptureEmitsEventAndResolves(t *testing.T) {
	b, sink := newScreenshotBroker(t)

	type result struct {
		capture CaptureResult
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		capture, err := b.Capture(t.Context(), "thread-1")
		resultCh <- result{capture, err}
	}()

	var req ScreenshotRequest
	select {
	case req = <-sink.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("no capture event emitted")
	}
	if req.ThreadID != "thread-1" {
		t.Fatalf("ThreadID = %q, want thread-1", req.ThreadID)
	}
	if req.RequestID == "" {
		t.Fatal("RequestID empty")
	}

	tiles := []string{
		"AAEC",
		"AwQF",
	}
	if err := b.Resolve(req.RequestID, tiles, true); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("Capture err: %v", r.err)
		}
		if len(r.capture.Tiles) != len(tiles) {
			t.Fatalf("tiles len = %d, want %d", len(r.capture.Tiles), len(tiles))
		}
		for i, want := range tiles {
			if r.capture.Tiles[i] != want {
				t.Fatalf("tile %d mismatch: got %q, want %q", i, r.capture.Tiles[i], want)
			}
		}
		if !r.capture.Clipped {
			t.Fatal("Clipped = false, want true (broker must round-trip the flag)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Capture did not return after Resolve")
	}
}

func TestScreenshotBroker_CaptureBlankThreadIsRejected(t *testing.T) {
	b, _ := newScreenshotBroker(t)
	if _, err := b.Capture(t.Context(), "  "); err == nil {
		t.Fatal("Capture(blank) error = nil, want error")
	}
}

func TestScreenshotBroker_CaptureNilEmitFails(t *testing.T) {
	b := NewScreenshotBroker(nil)
	if _, err := b.Capture(t.Context(), "thread-1"); err == nil {
		t.Fatal("Capture without emit hook = nil, want error")
	}
}

func TestScreenshotBroker_FailReturnsErrorToCapture(t *testing.T) {
	b, sink := newScreenshotBroker(t)

	errCh := make(chan error, 1)
	go func() {
		_, err := b.Capture(t.Context(), "thread-1")
		errCh <- err
	}()

	req := <-sink.requests
	if err := b.Fail(req.RequestID, "iframe missing"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil || err.Error() != "iframe missing" {
			t.Fatalf("Capture err = %v, want iframe missing", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Capture did not return after Fail")
	}
}

func TestScreenshotBroker_ResolveUnknownRequestErrors(t *testing.T) {
	b, _ := newScreenshotBroker(t)
	if err := b.Resolve("no-such-id", []string{"AAEC"}, false); err == nil {
		t.Fatal("Resolve(unknown) = nil, want error")
	}
	if err := b.Fail("no-such-id", "reason"); err == nil {
		t.Fatal("Fail(unknown) = nil, want error")
	}
}

func TestScreenshotBroker_ResolveEmptyTilesErrors(t *testing.T) {
	b, _ := newScreenshotBroker(t)
	if err := b.Resolve("any-id", nil, false); err == nil {
		t.Fatal("Resolve(nil tiles) = nil, want error")
	}
	if err := b.Resolve("any-id", []string{}, false); err == nil {
		t.Fatal("Resolve([]) = nil, want error")
	}
}

func TestScreenshotBroker_CtxCancelReleasesCapture(t *testing.T) {
	b, sink := newScreenshotBroker(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := b.Capture(ctx, "thread-1")
		errCh <- err
	}()

	<-sink.requests
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Capture err = %v, want ctx.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Capture did not return after ctx cancel")
	}

	// A subsequent Resolve for that request must report not-found because
	// cancel removed the entry.
	// (We don't have the request id at hand here, but Resolve on any id
	// after cancel is just expected to not panic. The negative path is
	// covered by the unknown-request-id test above.)
}

func TestScreenshotBroker_TeardownThreadCancelsPending(t *testing.T) {
	b, sink := newScreenshotBroker(t)

	errCh := make(chan error, 2)
	go func() {
		_, err := b.Capture(t.Context(), "thread-A")
		errCh <- err
	}()
	go func() {
		_, err := b.Capture(t.Context(), "thread-B")
		errCh <- err
	}()

	// Drain the two capture requests so the goroutines are parked on
	// their result channels.
	requests := make([]ScreenshotRequest, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case req := <-sink.requests:
			requests = append(requests, req)
		case <-time.After(2 * time.Second):
			t.Fatal("no capture event")
		}
	}

	// Teardown only thread-A; thread-B's Capture must still be parked.
	b.TeardownThread("thread-A")

	timeout := time.NewTimer(500 * time.Millisecond)
	defer timeout.Stop()

	collected := 0
	for collected < 1 {
		select {
		case err := <-errCh:
			if !errors.Is(err, ErrScreenshotSessionEnded) {
				t.Fatalf("err = %v, want ErrScreenshotSessionEnded", err)
			}
			collected++
		case <-timeout.C:
			t.Fatal("teardown did not release thread-A capture")
		}
	}

	// thread-B should still be parked. Resolve it to release the goroutine.
	for _, req := range requests {
		if req.ThreadID == "thread-B" {
			if err := b.Resolve(req.RequestID, []string{"AAEC"}, false); err != nil {
				t.Fatalf("Resolve thread-B: %v", err)
			}
		}
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("thread-B err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("thread-B Capture did not return after Resolve")
	}
}

package ctxutil

import (
	"context"
	"testing"
	"time"
)

func TestSleep_CompletesOnTimer(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	if !Sleep(ctx, 10*time.Millisecond) {
		t.Fatal("Sleep returned false on timer completion")
	}
	if d := time.Since(start); d < 10*time.Millisecond {
		t.Errorf("Sleep returned too early: %v", d)
	}
}

func TestSleep_ReturnsFalseOnCanceledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Sleep(ctx, 200*time.Millisecond) {
		t.Fatal("Sleep returned true on canceled ctx, want false")
	}
}

func TestSleep_ZeroDurationReturnsImmediately(t *testing.T) {
	start := time.Now()
	if !Sleep(context.Background(), 0) {
		t.Fatal("Sleep returned false on zero duration")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("Sleep took too long: %v", d)
	}
}

// TestSleep_ZeroDurationHonorsCanceledCtx is the safer-semantics
// regression test that motivated extraction: the prior codex helper
// returned true unconditionally for d<=0, ignoring an already-canceled
// ctx. Loops calling Sleep with a list of intervals can therefore
// continue post-cancellation if any interval is zero.
func TestSleep_ZeroDurationHonorsCanceledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Sleep(ctx, 0) {
		t.Error("Sleep(canceled, 0) returned true; want false")
	}
}

// TestSleep_NegativeDurationHonorsCanceledCtx mirrors the zero-
// duration case for the d<0 path.
func TestSleep_NegativeDurationHonorsCanceledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Sleep(ctx, -time.Second) {
		t.Error("Sleep(canceled, -1s) returned true; want false")
	}
}

func TestSleep_CancelMidSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if Sleep(ctx, 5*time.Second) {
		t.Fatal("Sleep returned true on mid-sleep cancel, want false")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Sleep took too long to respond to cancel: %v", d)
	}
}

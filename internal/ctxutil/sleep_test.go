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

// TestSleep_ReturnsFalseOnCanceledCtx covers every duration. The zero and
// negative rows are the regression that motivated extraction: the prior
// codex helper returned true unconditionally for d<=0, ignoring an
// already-canceled ctx, so a loop over a list of intervals could continue
// after cancellation if any interval was zero.
func TestSleep_ReturnsFalseOnCanceledCtx(t *testing.T) {
	for _, d := range []time.Duration{200 * time.Millisecond, 0, -time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if Sleep(ctx, d) {
				t.Fatalf("Sleep(canceled, %v) returned true, want false", d)
			}
		})
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

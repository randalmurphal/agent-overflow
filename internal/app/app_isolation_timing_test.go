package app

import (
	"testing"
	"time"
)

// The product bounds a harness knob replaces stay what they were, and a
// knob moves every bound derived from it together.
func TestThreadAgentPeerBoundDerivesEveryPeerBound(t *testing.T) {
	product := threadAgentPeerBound(0)
	if got := [4]time.Duration{product.list(), product.serve(), product.backoffFirst(), product.backoffMax()}; got != [4]time.Duration{
		5 * time.Second, 2500 * time.Millisecond, 5 * time.Second, time.Minute,
	} {
		t.Fatalf("product peer bounds = %v; want 5s list, 2.5s serve, 5s first backoff, 1m ceiling", got)
	}
	short := threadAgentPeerBound(time.Second)
	if got := [4]time.Duration{short.list(), short.serve(), short.backoffFirst(), short.backoffMax()}; got != [4]time.Duration{
		time.Second, 500 * time.Millisecond, time.Second, 12 * time.Second,
	} {
		t.Fatalf("1s peer bounds = %v; want each scaled with the listing bound", got)
	}
	a := &App{}
	a.threadAgentPeers.bound = short
	if got := a.ownerOpReach(); got != time.Second {
		t.Fatalf("owner op reach = %v; want the listing bound", got)
	}
}

func TestThreadWakeRetryDelayScalesWithItsBase(t *testing.T) {
	ladder := func(a *App) []time.Duration {
		var out []time.Duration
		for attempts := int64(1); attempts <= threadWakeRetryAttempts; attempts++ {
			out = append(out, a.threadWakeRetryDelay(attempts))
		}
		return out
	}
	product := ladder(&App{})
	if product[0] != 30*time.Second || product[len(product)-1] != 10*time.Minute {
		t.Fatalf("product wake retry ladder = %v; want 30s up to a 10m ceiling", product)
	}
	short := ladder(&App{wakeRetryBaseOverride: 2 * time.Second})
	for i := range product {
		if short[i]*15 != product[i] {
			t.Fatalf("2s wake retry ladder = %v; want the product ladder %v scaled by 2/30", short, product)
		}
	}
}

func TestAgentSettledWindowSpan(t *testing.T) {
	if got := (&App{}).agentSettledWindowSpan(); got != 30*time.Second {
		t.Fatalf("product settled window = %v; want 30s", got)
	}
	if got := (&App{agentSettledWindowOverride: time.Second}).agentSettledWindowSpan(); got != time.Second {
		t.Fatalf("settled window with a 1s override = %v", got)
	}
}

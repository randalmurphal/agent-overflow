package harnessrpc

import "testing"

func TestHarnessBackgroundFetchPassRunsOnePassOnTheHost(t *testing.T) {
	h, host := newHarnessTestHost(t)
	if err := h.HarnessBackgroundFetchPass(t.Context()); err != nil {
		t.Fatalf("HarnessBackgroundFetchPass: %v", err)
	}
	if host.fetchPasses != 1 {
		t.Fatalf("the host ran %d passes, want 1", host.fetchPasses)
	}

	if err := New(Config{}).HarnessBackgroundFetchPass(t.Context()); err == nil {
		t.Fatal("a harness with no host answered a pass as done")
	}
}

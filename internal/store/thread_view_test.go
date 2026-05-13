package store

import "testing"

func TestResolvedSessionRefPrefersLiveSessionRef(t *testing.T) {
	thread := Thread{SessionRef: "live-ref", PendingForkRef: "fork-ref"}
	if got := thread.ResolvedSessionRef(); got != "live-ref" {
		t.Fatalf("ResolvedSessionRef = %q, want %q", got, "live-ref")
	}
}

func TestResolvedSessionRefFallsBackToPendingForkRef(t *testing.T) {
	thread := Thread{SessionRef: "", PendingForkRef: "fork-ref"}
	if got := thread.ResolvedSessionRef(); got != "fork-ref" {
		t.Fatalf("ResolvedSessionRef = %q, want %q", got, "fork-ref")
	}
}

func TestResolvedSessionRefEmptyWhenBothUnset(t *testing.T) {
	thread := Thread{}
	if got := thread.ResolvedSessionRef(); got != "" {
		t.Fatalf("ResolvedSessionRef = %q, want empty string", got)
	}
}

package store

import "testing"

func TestScopedTurnIDSeparatesThreadsAndPreservesProviderIdentity(t *testing.T) {
	const providerID = "wire-turn"
	a := ScopedTurnID("thread-a", providerID, 7)
	b := ScopedTurnID("thread-b", providerID, 7)
	if a != "thread-a:wire-turn" || b != "thread-b:wire-turn" {
		t.Fatalf("scoped ids = %q / %q", a, b)
	}
	if a == b {
		t.Fatalf("provider id %q collided across threads", providerID)
	}
	if got := ScopedTurnID(" thread-a ", " ", 7); got != "thread-a:7" {
		t.Fatalf("provider-less scoped id = %q, want thread-a:7", got)
	}
}

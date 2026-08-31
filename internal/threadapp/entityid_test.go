package threadapp

import (
	"testing"

	"agent-overflow/internal/entityid"
)

// A thread id is what a client keys its stores, its IndexedDB replica and
// its deep links by, and a client may hold several backends at once
// (docs/specs/remote-access.md §10). A mint that ever returned something
// short or sequential would collide across backends silently, so the
// production default is pinned here rather than left to the test seam
// every other test in this package injects.
func TestNewIDMintsGloballyUniqueThreadIDs(t *testing.T) {
	service := &Service{}
	first := service.newID()
	second := service.newID()
	if !entityid.Valid(first) {
		t.Fatalf("newID() = %q, which is not a globally unique entity id", first)
	}
	if !entityid.Valid(second) {
		t.Fatalf("newID() = %q, which is not a globally unique entity id", second)
	}
	if first == second {
		t.Fatalf("newID() repeated %q", first)
	}
}

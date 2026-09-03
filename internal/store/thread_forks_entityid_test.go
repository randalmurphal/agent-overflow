package store

import (
	"testing"

	"agent-overflow/internal/entityid"
)

// A forked thread is a second thread-id mint site, and the id it produces
// is addressed by clients that may hold several backends at once
// (docs/specs/remote-access.md §10). Deriving it from the source id — a
// suffix, a counter — is the regression this pins.
func TestBuildForkedThreadMintsGloballyUniqueID(t *testing.T) {
	source := Thread{ID: entityid.New(), ProjectID: "p", Title: "T", Provider: "claude"}

	first := BuildForkedThread(source)
	second := BuildForkedThread(source)

	if !entityid.Valid(first.ID) {
		t.Fatalf("forked thread id = %q, which is not a globally unique entity id", first.ID)
	}
	if first.ID == source.ID || first.ID == second.ID {
		t.Fatalf("forked thread id %q is not fresh (source %q, sibling %q)", first.ID, source.ID, second.ID)
	}
}

// Package triage — direct tests for the generic map helpers in maps.go.
// deleteByPrefix is exercised heavily through the turn-lifecycle and
// cleanup paths, but because it's generic we test the shape directly
// here so refactors can fail fast without setting up a full router.

package triage

import "testing"

func TestDeleteByPrefixRemovesMatchingKeys(t *testing.T) {
	m := map[string]int{
		"thread-a|0|foo": 1,
		"thread-a|1|bar": 2,
		"thread-b|0|baz": 3,
	}

	deleteByPrefix(m, "thread-a|")

	if _, ok := m["thread-a|0|foo"]; ok {
		t.Errorf("thread-a|0|foo should have been deleted")
	}
	if _, ok := m["thread-a|1|bar"]; ok {
		t.Errorf("thread-a|1|bar should have been deleted")
	}
	if v, ok := m["thread-b|0|baz"]; !ok || v != 3 {
		t.Errorf("thread-b|0|baz should have been preserved; got (%d, %v)", v, ok)
	}
}

func TestDeleteByPrefixNoMatchesLeavesMapUnchanged(t *testing.T) {
	m := map[string]string{
		"alpha": "1",
		"beta":  "2",
	}

	deleteByPrefix(m, "nothing-matches")

	if len(m) != 2 {
		t.Errorf("map length = %d, want 2", len(m))
	}
	if m["alpha"] != "1" || m["beta"] != "2" {
		t.Errorf("map content changed: %+v", m)
	}
}

func TestDeleteByPrefixEmptyPrefixRemovesEverything(t *testing.T) {
	// An empty prefix is a legitimate call shape: HasPrefix(x, "") is
	// always true for any x. Callers shouldn't do this by accident, but
	// the contract is well-defined and worth pinning down.
	m := map[string]int{"a": 1, "b": 2, "c": 3}

	deleteByPrefix(m, "")

	if len(m) != 0 {
		t.Errorf("empty prefix should drain the map; len=%d remaining=%+v", len(m), m)
	}
}

func TestDeleteByPrefixNilMapSafe(t *testing.T) {
	// Iterating a nil map is valid Go and produces zero iterations; the
	// helper must not panic on this path since several cleanup sites can
	// reach it with an uninitialised map during partial router setup.
	var m map[string]struct{}

	// Using defer + recover here to make a panic fail the test with a
	// clear message rather than crashing the suite.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("deleteByPrefix panicked on nil map: %v", r)
		}
	}()

	deleteByPrefix(m, "thread-a|")
}

func TestDeleteByPrefixPreservesOtherPrefixMatches(t *testing.T) {
	// Regression guard: deleteByPrefix uses strings.HasPrefix, not an
	// exact match. Make sure a prefix that is a strict substring of
	// another key doesn't accidentally match the longer key if the
	// prefix itself doesn't line up with the key's start.
	m := map[string]int{
		"alpha-1":    1,
		"alpha-beta": 2,
		"beta-alpha": 3,
	}

	deleteByPrefix(m, "alpha-")

	if _, ok := m["alpha-1"]; ok {
		t.Errorf("alpha-1 should have been deleted")
	}
	if _, ok := m["alpha-beta"]; ok {
		t.Errorf("alpha-beta should have been deleted")
	}
	if _, ok := m["beta-alpha"]; !ok {
		t.Errorf("beta-alpha should have been preserved")
	}
}

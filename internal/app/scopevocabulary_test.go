package app

import (
	"sort"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/transport"
)

// TestScopeVocabularyMatchesIdentity pins one spelling of the scope
// vocabulary across the two packages that hold it.
//
// Neither can import the other: identity persists into internal/store
// and transport stays store-free, so transport RESTATES the ten names
// identity declares. A restatement with nobody checking it is a typo
// waiting to happen, and the typo's shape is bad in both directions — a
// name only transport knows is an annotation no session can ever be
// granted, and a name only identity knows is a grant no method accepts.
//
// This package imports both, which is what makes the check possible
// here and nowhere else. It fails in both directions on purpose.
func TestScopeVocabularyMatchesIdentity(t *testing.T) {
	granted := make(map[string]bool, len(identity.Scopes))
	for _, scope := range identity.Scopes {
		granted[string(scope)] = true
	}

	declared := make(map[string]bool, len(transport.Scopes))
	for _, scope := range transport.Scopes {
		declared[string(scope)] = true
	}

	// transport's set is identity's ten plus `host`, exactly.
	want := make(map[string]bool, len(granted)+1)
	for name := range granted {
		want[name] = true
	}
	want[string(transport.ScopeHost)] = true

	for _, name := range sortedKeys(want) {
		if !declared[name] {
			t.Errorf("identity declares scope %q and transport does not: a grant no annotation can name", name)
		}
	}
	for _, name := range sortedKeys(declared) {
		if !want[name] {
			t.Errorf("transport declares scope %q and identity does not: an annotation no session can be granted", name)
		}
	}

	// `host` is a method property, never a grant. identity says so in
	// the Scope doc comment; this is the assertion behind the sentence.
	if granted[string(transport.ScopeHost)] {
		t.Errorf("identity.Scopes contains %q; a session row could then claim a scope that means "+
			"'this call has no remote form'", transport.ScopeHost)
	}

	// Every declared scope resolves to a tier. An unplaced scope answers
	// the zero tier, which is neither observe nor a refusal anyone wrote.
	for _, scope := range transport.Scopes {
		if !scope.Valid() {
			t.Errorf("transport scope %q has no tier row", scope)
		}
	}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

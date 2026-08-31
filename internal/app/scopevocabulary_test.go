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
// and transport stays store-free, so transport RESTATES the grantable
// names identity declares. A restatement with nobody checking it is a
// typo waiting to happen, and the typo's shape is bad in both directions
// — a name only transport knows is an annotation no session can ever be
// granted, and a name only identity knows is a grant no method accepts.
//
// The two exceptions are deliberate and enumerated below: `host` and
// `session` are method PROPERTIES, so transport declares them and
// identity must not.
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

	// transport's set is identity's grantable names plus the two values
	// that are method properties rather than grants, exactly.
	transportOnly := []transport.Scope{transport.ScopeSession, transport.ScopeHost}
	want := make(map[string]bool, len(granted)+len(transportOnly))
	for name := range granted {
		want[name] = true
	}
	for _, scope := range transportOnly {
		want[string(scope)] = true
	}

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

	// Neither transport-only value is a grant. identity says so in the
	// Scope doc comment; this is the assertion behind the sentence. A
	// session row claiming `host` would claim a call has a remote form
	// after all, and one claiming `session` would name an authority the
	// gate never reads — it admits on session presence alone.
	for _, scope := range transportOnly {
		if granted[string(scope)] {
			t.Errorf("identity.Scopes contains %q, which is a method property rather than a grant", scope)
		}
	}

	// Every declared scope resolves to a tier. An unplaced scope answers
	// the zero tier, which is neither observe nor a refusal anyone wrote.
	for _, scope := range transport.Scopes {
		if !scope.Valid() {
			t.Errorf("transport scope %q has no tier row", scope)
		}
	}
}

// TestObserveScopesAreTheObserveTier pins the OTHER restatement between
// these two packages: identity.ObserveScopes is what a view-only pairing
// mints, and "view-only" means transport's observe tier — the tier the
// per-RPC gate compares. identity cannot say that itself (it has no tier
// table and may not import one), so the sentence in its doc comment is
// checked here, in both directions.
//
// A scope demoted to observe and not added to ObserveScopes is a surface
// a view-only device is silently denied; one promoted out of observe and
// left in it is authority a view-only device silently keeps.
func TestObserveScopesAreTheObserveTier(t *testing.T) {
	inMint := make(map[string]bool, len(identity.ObserveScopes))
	for _, scope := range identity.ObserveScopes {
		inMint[string(scope)] = true
		if tier := transport.Scope(scope).Tier(); tier != transport.TierObserve {
			t.Errorf("view-only mints %q, which enforces at tier %d rather than observe", scope, tier)
		}
	}
	for _, scope := range transport.Scopes {
		if scope.Tier() == transport.TierObserve && !inMint[string(scope)] {
			t.Errorf("%q is observe-tier and a view-only device is not granted it", scope)
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

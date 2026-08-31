package transport

import (
	"os"
	"strings"
	"testing"
)

// TestFrontendScopeVocabularyMatches pins frontend/src/lib/transport/
// scopes.ts to this package's Scopes, in both directions and in order.
//
// The TS union deliberately degrades on an unknown name — a tab stays
// loaded across a backend update, so a NEW scope reading as unknown is
// the designed behavior. What must not happen silently is drift in the
// shipped set: a scope renamed on one side would leave paired sessions
// holding grants the client has no word for, disabling every surface
// keyed on it with no error anywhere. Textual on purpose, same as
// identity's TestFrontendHintsCoverEveryRefusal: the literal is a flat
// list of quoted strings by construction.
func TestFrontendScopeVocabularyMatches(t *testing.T) {
	const modulePath = "../../frontend/src/lib/transport/scopes.ts"
	source, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("read %s: %v", modulePath, err)
	}
	declared := frontendScopeList(t, string(source))

	if len(declared) != len(Scopes) {
		t.Fatalf("%s declares %d scopes, this package declares %d",
			modulePath, len(declared), len(Scopes))
	}
	for i, scope := range Scopes {
		if declared[i] != string(scope) {
			t.Errorf("position %d: %s declares %q, this package declares %q — the two lists share the spec table's order and a move is drift",
				i, modulePath, declared[i], scope)
		}
	}
}

// TestFrontendExecuteTierMatches pins scopes.ts's EXECUTE_SCOPES to this
// package's TierExecute rows, in both directions.
//
// That list is what the frontend's view-only predicate is derived from
// (`isViewOnly`), so a scope that gained or lost the execute tier here
// without moving there would leave a session labelled read-only while it
// holds a capability that changes state — or the reverse, which drops the
// indicator for a session that genuinely cannot act.
func TestFrontendExecuteTierMatches(t *testing.T) {
	const modulePath = "../../frontend/src/lib/transport/scopes.ts"
	source, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("read %s: %v", modulePath, err)
	}
	declared := frontendScopeListNamed(t, string(source),
		"export const EXECUTE_SCOPES: readonly Scope[] = [")

	var want []string
	for _, scope := range Scopes {
		if scope.Tier() == TierExecute {
			want = append(want, string(scope))
		}
	}
	if len(declared) != len(want) {
		t.Fatalf("%s declares %d execute-tier scopes (%v), this package declares %d (%v)",
			modulePath, len(declared), declared, len(want), want)
	}
	for i, scope := range want {
		if declared[i] != scope {
			t.Errorf("position %d: %s declares %q, this package's execute tier has %q",
				i, modulePath, declared[i], scope)
		}
	}
}

// frontendScopeList extracts the SCOPES array literal's quoted entries.
func frontendScopeList(t *testing.T, module string) []string {
	t.Helper()
	return frontendScopeListNamed(t, module, "export const SCOPES: readonly Scope[] = [")
}

// frontendScopeListNamed extracts one flat array literal's quoted entries.
func frontendScopeListNamed(t *testing.T, module, marker string) []string {
	t.Helper()
	start := strings.Index(module, marker)
	if start < 0 {
		t.Fatalf("no %q in scopes.ts; the gate is reading the wrong shape", marker)
	}
	rest := module[start+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatalf("%q literal never closes", marker)
	}
	var names []string
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if len(line) >= 2 && line[0] == '\'' && line[len(line)-1] == '\'' {
			names = append(names, line[1:len(line)-1])
		}
	}
	if len(names) == 0 {
		t.Fatalf("%q literal parsed to nothing; the gate is reading the wrong shape", marker)
	}
	return names
}

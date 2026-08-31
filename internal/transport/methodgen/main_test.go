package main

import (
	"strings"
	"testing"
)

// fixtureSpecs are the two testdata receivers, given in REVERSE name
// order on purpose: scanReceivers must sort the merged table, so the
// emitted order cannot depend on which spec was listed first.
//
// The beta spec also exercises the label indirection — source type
// Beta, registered as "svc.App" — which is the shape a service
// promoted out of the root uses to keep its method IDs
// (docs/architecture/root-decomposition.md § Wire compatibility).
var fixtureSpecs = []receiverSpec{
	{Dir: "testdata/beta", Receiver: "Beta", Package: "svc", TypeName: "App"},
	{Dir: "testdata/alpha", Receiver: "Alpha", Package: "main"},
}

// fixtureScopes is the vocabulary the fixtures annotate against. A
// hand-written set rather than the real one, so a scope added to
// internal/transport/scopes.go does not silently change what these
// tests exercise; TestLoadScopeVocabulary_ReadsTheDeclaredSet covers
// the real file.
var fixtureScopes = map[string]bool{
	"threads:read":   true,
	"files:read":     true,
	"settings:write": true,
	"host":           true,
}

// TestScanReceivers_MergesSpecsFromDifferentDirs is the multi-receiver
// gate: two specs in two directories produce one table, sorted by
// name, with each entry's FQN built from ITS OWN spec's labels.
func TestScanReceivers_MergesSpecsFromDifferentDirs(t *testing.T) {
	entries, err := scanReceivers(".", fixtureSpecs, map[string]bool{"Startup": true}, fixtureScopes)
	if err != nil {
		t.Fatalf("scanReceivers: %v", err)
	}

	// Sorted-by-name merge across both dirs. AlsoBeta/DoBeta come from
	// the spec listed FIRST and still sort around DoAlpha.
	wantOrder := []string{"AlsoBeta", "DoAlpha", "DoBeta", "SharedName"}
	gotOrder := make([]string, len(entries))
	for i, e := range entries {
		gotOrder[i] = e.Name
	}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("collected %v, want %v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("collected %v, want %v", gotOrder, wantOrder)
		}
	}

	// Per-receiver labels: each FQN uses its own spec's package and
	// type name, and the ID is the hash of that exact string.
	wantFQN := map[string]string{
		"DoAlpha":    "main.Alpha.DoAlpha",
		"SharedName": "main.Alpha.SharedName",
		"DoBeta":     "svc.App.DoBeta",
		"AlsoBeta":   "svc.App.AlsoBeta",
	}
	for _, e := range entries {
		if want := wantFQN[e.Name]; e.FQN != want {
			t.Errorf("%s: FQN = %q, want %q", e.Name, e.FQN, want)
		}
		if want := fnvHash(e.FQN); e.ID != want {
			t.Errorf("%s: ID = %d, want fnvHash(%q) = %d", e.Name, e.ID, e.FQN, want)
		}
	}
}

// TestScanReceivers_SkipRules pins what a spec must NOT collect: a
// second type sharing the scanned directory, unexported methods,
// //wails:ignore, value receivers, *_test.go files, and the internal
// skip set.
func TestScanReceivers_SkipRules(t *testing.T) {
	entries, err := scanReceivers(".", fixtureSpecs, map[string]bool{"Startup": true}, fixtureScopes)
	if err != nil {
		t.Fatalf("scanReceivers: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name] = true
	}
	for _, name := range []string{
		"DoOther",            // different receiver in the same directory
		"IgnoredAlpha",       // //wails:ignore
		"ValueReceiverAlpha", // value receiver
		"Startup",            // internal skip set
		"TestOnlyAlpha",      // *_test.go
		"BareAlphaFunc",      // no receiver
		"unexportedAlpha",    // unexported
		"AlsoGamma",          // Gamma is in the beta dir but has no spec
	} {
		if got[name] {
			t.Errorf("%q was collected but must be skipped", name)
		}
	}
}

// TestScanReceivers_DuplicateNameAcrossSpecs matches the dispatcher's
// byName collision rule: name dispatch shares one namespace across
// receivers, so the same method name from two specs is a codegen
// error naming both FQNs — not a silently shadowed entry.
func TestScanReceivers_DuplicateNameAcrossSpecs(t *testing.T) {
	specs := []receiverSpec{
		{Dir: "testdata/alpha", Receiver: "Alpha", Package: "main"},
		{Dir: "testdata/beta", Receiver: "Gamma", Package: "svc", TypeName: "App"},
	}
	_, err := scanReceivers(".", specs, nil, fixtureScopes)
	if err == nil {
		t.Fatal("want a collision error for SharedName declared on both receivers, got nil")
	}
	for _, want := range []string{"SharedName", "main.Alpha.SharedName", "svc.App.SharedName"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("collision error %q does not name %q", err, want)
		}
	}
}

// TestScanReceivers_MissingDir surfaces a mistyped spec directory as
// an error rather than an empty table — a silently-skipped receiver
// would strip every one of its methods from the wire.
func TestScanReceivers_MissingDir(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/nope", Receiver: "Alpha", Package: "main"}}
	if _, err := scanReceivers(".", specs, nil, fixtureScopes); err == nil {
		t.Fatal("want an error for a spec naming a missing directory, got nil")
	}
}

// TestReceiverSpecs_TodaysConfig pins the shipped configuration: the
// internal/app implementation promoted by the repo-root wrapper and
// registered as main.App, and nothing else. Harness is
// deliberately absent (see the receiverSpecs doc comment) — adding a
// spec here changes the production allow-list and the LAN-safety
// classification gate that partners it, so it must be a deliberate
// edit rather than a drive-by.
func TestReceiverSpecs_TodaysConfig(t *testing.T) {
	if len(receiverSpecs) != 1 {
		t.Fatalf("receiverSpecs = %+v, want exactly the root App spec", receiverSpecs)
	}
	got := receiverSpecs[0]
	want := receiverSpec{Dir: "internal/app", Receiver: "App", Package: "main"}
	if got != want {
		t.Fatalf("receiverSpecs[0] = %+v, want %+v", got, want)
	}
	if got.fqnType() != "App" {
		t.Fatalf("fqnType() = %q, want %q", got.fqnType(), "App")
	}
}

// TestScanReceivers_RefusesUnannotatedMethods is the completeness gate
// the spec moved out of the test suite and into the generator (§5): an
// unclassified method must stop the run, not merely fail a later test,
// because the generated table is what puts the name on the wire.
//
// Every offending name is listed. One run has to tell a developer
// everything they have to classify, or a wave of new methods becomes a
// wave of failing runs.
func TestScanReceivers_RefusesUnannotatedMethods(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/unclassified", Receiver: "Delta", Package: "main"}}
	_, err := scanReceivers(".", specs, nil, fixtureScopes)
	if err == nil {
		t.Fatal("want a refusal for the unannotated fixture methods, got nil")
	}
	for _, want := range []string{"Unannotated", "AlsoUnannotated", "//ao:scope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Annotated,") || strings.Contains(err.Error(), " Annotated ") {
		t.Errorf("refusal %q names the correctly annotated control method", err)
	}
}

// TestScanReceivers_RefusesUndeclaredScope closes the other half: an
// annotation is only a classification if the name means something. A
// typo one character off a real scope reads correct in review and would
// otherwise land as a scope no tier table places, which is a method no
// grant can ever admit.
func TestScanReceivers_RefusesUndeclaredScope(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/badscope", Receiver: "Epsilon", Package: "main"}}
	_, err := scanReceivers(".", specs, nil, fixtureScopes)
	if err == nil {
		t.Fatal("want a refusal for the undeclared scope, got nil")
	}
	for _, want := range []string{"Typo", "threads:reed", "threads:read"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q — the faulty method, its typo, and the declared set", err, want)
		}
	}
}

// TestScanReceivers_ParsesStepUp pins the optional directive. It rides
// the same doc comment as the scope and marks the calls that re-key the
// system (docs/specs/remote-access.md §4), so a silently dropped
// //ao:stepup is a mandatory per-call proof quietly becoming optional.
func TestScanReceivers_ParsesStepUp(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/stepup", Receiver: "Zeta", Package: "main"}}
	entries, err := scanReceivers(".", specs, nil, fixtureScopes)
	if err != nil {
		t.Fatalf("scanReceivers: %v", err)
	}
	got := map[string]MethodEntry{}
	for _, e := range entries {
		got[e.Name] = e
	}
	if len(got) != 2 {
		t.Fatalf("collected %d entries, want 2: %v", len(got), got)
	}
	if e := got["Reconfigures"]; !e.StepUp || e.Scope != "settings:write" {
		t.Errorf("Reconfigures = {Scope:%q StepUp:%v}, want {settings:write true}", e.Scope, e.StepUp)
	}
	if e := got["Ordinary"]; e.StepUp || e.Scope != "settings:write" {
		t.Errorf("Ordinary = {Scope:%q StepUp:%v}, want {settings:write false}", e.Scope, e.StepUp)
	}
}

// TestLoadScopeVocabulary_ReadsTheDeclaredSet reads the real
// internal/transport/scopes.go, because the generator's whole defence
// against a typo is that the vocabulary comes from the same file the
// tier table does. A parse that silently collected nothing would refuse
// every annotation in the tree; one that collected the wrong constants
// would accept a name no gate places.
func TestLoadScopeVocabulary_ReadsTheDeclaredSet(t *testing.T) {
	scopes, err := loadScopeVocabulary("../../..")
	if err != nil {
		t.Fatalf("loadScopeVocabulary: %v", err)
	}
	for _, want := range []string{"threads:read", "files:read", "threads:operate", "access:admin", "host"} {
		if !scopes[want] {
			t.Errorf("declared vocabulary is missing %q", want)
		}
	}
	if scopes["Scope"] || scopes[""] {
		t.Errorf("vocabulary collected a non-value: %v", scopes)
	}
	// The count is pinned so a constant deleted from the block fails
	// here rather than in whichever annotation used it.
	if len(scopes) != 11 {
		t.Errorf("collected %d scopes (%v), want the ten grantable names plus host", len(scopes), scopes)
	}
}

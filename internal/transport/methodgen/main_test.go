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

// TestScanReceivers_MergesSpecsFromDifferentDirs is the multi-receiver
// gate: two specs in two directories produce one table, sorted by
// name, with each entry's FQN built from ITS OWN spec's labels.
func TestScanReceivers_MergesSpecsFromDifferentDirs(t *testing.T) {
	entries, err := scanReceivers(".", fixtureSpecs, map[string]bool{"Startup": true})
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
	entries, err := scanReceivers(".", fixtureSpecs, map[string]bool{"Startup": true})
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
	_, err := scanReceivers(".", specs, nil)
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
	if _, err := scanReceivers(".", specs, nil); err == nil {
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

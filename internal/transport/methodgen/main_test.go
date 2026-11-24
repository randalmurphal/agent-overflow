package main

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
	"time"
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

// fixtureRoutes is the route vocabulary the fixtures annotate against,
// hand-written for the reason fixtureScopes is;
// TestLoadRouteVocabulary_ReadsTheDeclaredSet covers the real file.
var fixtureRoutes = map[string]bool{
	"thread":    true,
	"project":   true,
	"workspace": true,
	"home":      true,
	"selected":  true,
	"all":       true,
}

// TestScanReceivers_MergesSpecsFromDifferentDirs is the multi-receiver
// gate: two specs in two directories produce one table, sorted by
// name, with each entry's FQN built from ITS OWN spec's labels.
func TestScanReceivers_MergesSpecsFromDifferentDirs(t *testing.T) {
	entries, err := scanReceivers(".", fixtureSpecs, map[string]bool{"Startup": true}, fixtureScopes, fixtureRoutes)
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
	entries, err := scanReceivers(".", fixtureSpecs, map[string]bool{"Startup": true}, fixtureScopes, fixtureRoutes)
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
	_, err := scanReceivers(".", specs, nil, fixtureScopes, fixtureRoutes)
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
	if _, err := scanReceivers(".", specs, nil, fixtureScopes, fixtureRoutes); err == nil {
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

// TestScanReceivers_RefusesAnUnclassifiedMethod is the completeness gate
// the spec moved out of the test suite and into the generator (§5): an
// unclassified method must stop the run, not merely fail a later test,
// because the generated table is what puts the name on the wire.
//
// Every offending name is listed. One run has to tell a developer
// everything they have to classify, or a wave of new methods becomes a
// wave of failing runs. The correctly classified control method in each
// fixture is not named.
func TestScanReceivers_RefusesAnUnclassifiedMethod(t *testing.T) {
	for _, tc := range []struct {
		name, dir, receiver string
		// wants are the faulty methods, their typo and the declared set.
		wants, unnamed []string
	}{
		{"no scope", "testdata/unclassified", "Delta",
			[]string{"Unannotated", "AlsoUnannotated", "//ao:scope"}, []string{"Annotated,", " Annotated "}},
		// An annotation is only a classification if the name means
		// something: a typo one character off a real scope reads correct in
		// review and would land as a scope no tier table places, which is a
		// method no grant can ever admit.
		{"undeclared scope", "testdata/badscope", "Epsilon",
			[]string{"Typo", "threads:reed", "threads:read"}, nil},
		// A method nobody routed is one a multi-backend client would answer
		// from whichever socket happened to be first: a wrong answer that
		// looks like a right one.
		{"no route", "testdata/unrouted", "Theta",
			[]string{"Unrouted", "AlsoUnrouted", "//ao:route"}, []string{"Routed,", " Routed "}},
		// A typo one transposition off a real route would land as a value no
		// client can resolve.
		{"undeclared route", "testdata/badroute", "Iota",
			[]string{"Typo", "hoem", "home"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs := []receiverSpec{{Dir: tc.dir, Receiver: tc.receiver, Package: "main"}}
			_, err := scanReceivers(".", specs, nil, fixtureScopes, fixtureRoutes)
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
			for _, control := range tc.unnamed {
				if strings.Contains(err.Error(), control) {
					t.Errorf("refusal %q names the correctly classified control method", err)
				}
			}
		})
	}
}

// TestScanReceivers_ParsesStepUp pins the optional directive. It rides
// the same doc comment as the scope and marks the calls that re-key the
// system (docs/specs/remote-access.md §4), so a silently dropped
// //ao:stepup is a mandatory per-call proof quietly becoming optional.
func TestScanReceivers_ParsesStepUp(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/stepup", Receiver: "Zeta", Package: "main"}}
	entries, err := scanReceivers(".", specs, nil, fixtureScopes, fixtureRoutes)
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

// TestScanReceivers_ParsesRemote pins the optional //ao:remote directive.
// A dropped one is a remote-only call that a build without remote access
// would quietly answer.
func TestScanReceivers_ParsesRemote(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/remote", Receiver: "Zeta", Package: "main"}}
	entries, err := scanReceivers(".", specs, nil, fixtureScopes, fixtureRoutes)
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
	if !got["PairsAComputer"].Remote {
		t.Error("PairsAComputer lost its //ao:remote mark")
	}
	if got["Local"].Remote {
		t.Error("Local is marked remote without the directive")
	}
	rendered, err := renderFile(entries)
	if err != nil {
		t.Fatalf("renderFile: %v", err)
	}
	if !strings.Contains(string(rendered), `Name: "PairsAComputer", ID: `) ||
		!strings.Contains(string(rendered), `Remote: true}, // main.Zeta.PairsAComputer`) {
		t.Errorf("rendered table does not carry the Remote column:\n%s", rendered)
	}
}

// TestScanReceivers_ParsesCallerBound pins the optional //ao:callerbound
// directive. A dropped one only keeps a read running after its caller
// left; a spurious one lets a disconnect cut a write.
func TestScanReceivers_ParsesCallerBound(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/callerbound", Receiver: "Zeta", Package: "main"}}
	entries, err := scanReceivers(".", specs, nil, fixtureScopes, fixtureRoutes)
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
	if !got["Reads"].CallerBound {
		t.Error("Reads lost its //ao:callerbound mark")
	}
	if got["Writes"].CallerBound {
		t.Error("Writes is caller-bound without the directive")
	}
	rendered, err := renderFile(entries)
	if err != nil {
		t.Fatalf("renderFile: %v", err)
	}
	if !strings.Contains(string(rendered), `CallerBound: true}, // main.Zeta.Reads`) ||
		strings.Contains(string(rendered), `CallerBound: true}, // main.Zeta.Writes`) {
		t.Errorf("rendered table does not carry the CallerBound column:\n%s", rendered)
	}
}

// TestScanReceivers_ParsesBudget pins the optional //ao:budget directive
// into both tables: a dropped one has every client give up on the method at
// the default wait while its server side keeps running.
func TestScanReceivers_ParsesBudget(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/budget", Receiver: "Eta", Package: "main"}}
	entries, err := scanReceivers(".", specs, nil, fixtureScopes, fixtureRoutes)
	if err != nil {
		t.Fatalf("scanReceivers: %v", err)
	}
	got := map[string]MethodEntry{}
	for _, e := range entries {
		got[e.Name] = e
	}
	if got["Slow"].Budget != 10*time.Minute || got["Quick"].Budget != 0 {
		t.Fatalf("budgets = Slow %v, Quick %v; want 10m and none", got["Slow"].Budget, got["Quick"].Budget)
	}
	rendered, err := renderFile(entries)
	if err != nil {
		t.Fatalf("renderFile: %v", err)
	}
	if !strings.Contains(string(rendered), `Budget: 10 * time.Minute}, // main.Eta.Slow`) ||
		strings.Contains(string(rendered), `Budget: 10 * time.Minute}, // main.Eta.Quick`) {
		t.Errorf("rendered table does not carry the Budget column:\n%s", rendered)
	}
	ts := string(renderTS(entries))
	slow := fmt.Sprintf("\t%d: 600000, // Slow\n", got["Slow"].ID)
	if !strings.Contains(ts, slow) || strings.Contains(ts, "// Quick\n};") ||
		!strings.Contains(ts, "export const DEFAULT_METHOD_BUDGET_MS = 45000;") ||
		!strings.Contains(ts, "export const METHOD_ANSWER_MARGIN_MS = 15000;") {
		t.Errorf("TS mirror does not carry the budgets:\n%s", ts)
	}
}

// A budget directive that does not say what its author meant fails the run.
func TestMethodAnnotations_RefusesABadBudget(t *testing.T) {
	for _, tc := range []struct{ name, lines string }{
		{"not a duration", "//ao:budget ten minutes"},
		{"no value", "//ao:budget"},
		{"no longer than the default", "//ao:budget 45s"},
		{"past the ceiling", "//ao:budget 25h"},
		{"two of them", "//ao:budget 2m\n//ao:budget 3m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := &ast.CommentGroup{}
			for _, line := range strings.Split(tc.lines, "\n") {
				doc.List = append(doc.List, &ast.Comment{Text: line})
			}
			if _, err := methodAnnotations(doc); err == nil {
				t.Fatalf("%q was accepted", tc.lines)
			}
		})
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
	for _, want := range []string{"threads:read", "files:read", "settings:read", "threads:operate", "access:admin", "session", "host"} {
		if !scopes[want] {
			t.Errorf("declared vocabulary is missing %q", want)
		}
	}
	if scopes["Scope"] || scopes[""] {
		t.Errorf("vocabulary collected a non-value: %v", scopes)
	}
	// The count is pinned so a constant deleted from the block fails
	// here rather than in whichever annotation used it.
	if len(scopes) != 16 {
		t.Errorf("collected %d scopes (%v), want the fourteen grantable names plus session and host", len(scopes), scopes)
	}
}

// TestScanReceivers_InfersRouteFromTheFirstParameter pins the half of
// the Route column nobody writes down. Thread and project ids are
// unique across BACKENDS (internal/entityid), so a client resolves one
// with no other help — which is why exactly those two are inferred and
// every other id is authored.
//
// The context and casing cases are here because both are real in the
// tree: a bound method may take a leading ctx (stripped by the TS
// bindings) and the same parameter is spelled threadID in one file and
// threadId in another. A route that changed with either would be a
// routing decision made by a typo.
func TestScanReceivers_InfersRouteFromTheFirstParameter(t *testing.T) {
	specs := []receiverSpec{{Dir: "testdata/routes", Receiver: "Eta", Package: "main"}}
	entries, err := scanReceivers(".", specs, nil, fixtureScopes, fixtureRoutes)
	if err != nil {
		t.Fatalf("scanReceivers: %v", err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = e.Route
	}
	want := map[string]string{
		"Threaded":    "thread",
		"ThreadedCtx": "thread",
		"Projected":   "project",
		"Overridden":  "home",
		"Declared":    "all",

		"Workspaced":          "workspace",
		"WorkspacedQualified": "workspace",
	}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for name, route := range want {
		if got[name] != route {
			t.Errorf("%s: route = %q, want %q", name, got[name], route)
		}
	}
}

// TestLoadRouteVocabulary_ReadsTheDeclaredSet reads the real
// internal/transport/routes.go, for the reason its scope counterpart
// reads scopes.go: the generator's whole defence against a typo is that
// the vocabulary comes from the file the client mirror is generated
// against.
func TestLoadRouteVocabulary_ReadsTheDeclaredSet(t *testing.T) {
	routes, err := loadRouteVocabulary("../../..")
	if err != nil {
		t.Fatalf("loadRouteVocabulary: %v", err)
	}
	for _, want := range []string{"thread", "project", "workspace", "home", "selected", "all"} {
		if !routes[want] {
			t.Errorf("declared vocabulary is missing %q", want)
		}
	}
	if routes["MethodRoute"] || routes[""] {
		t.Errorf("vocabulary collected a non-value: %v", routes)
	}
	if len(routes) != 6 {
		t.Errorf("collected %d routes (%v), want exactly the six §10 names", len(routes), routes)
	}
}

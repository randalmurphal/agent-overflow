package identity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReasonSetIsClosed reads reason.go itself rather than the compiled
// values, because the property under test is about the SOURCE: a constant
// added to the block without a code, or a code added without a constant,
// both compile. The first produces an empty wire spelling that reads as
// "admitted"; the second is an entry nothing can ever emit, which the
// frontend would carry forever without showing it.
func TestReasonSetIsClosed(t *testing.T) {
	constants := reasonConstantsInOrder(t)
	if len(constants) != len(reasonCodes) {
		t.Fatalf("reason.go declares %d constants but %d codes: %v",
			len(constants), len(reasonCodes), constants)
	}
	for ordinal, name := range constants {
		code := reasonCodes[ordinal]
		if name == "ReasonNone" {
			if code != "" {
				t.Fatalf("ReasonNone carries the code %q; the admitted case has no code", code)
			}
			continue
		}
		if code == "" {
			t.Fatalf("%s (ordinal %d) has no wire code, so a refusal would read as admitted",
				name, ordinal)
		}
		if Reason(ordinal).Code() != code {
			t.Fatalf("%s resolves to %q but the table holds %q",
				name, Reason(ordinal).Code(), code)
		}
	}
}

// TestReasonOrdinalsAreContiguous — reasonCodes is a slice indexed by
// ordinal, so a gap would silently be an empty code, and Code() would
// report a real refusal as admitted.
func TestReasonOrdinalsAreContiguous(t *testing.T) {
	for ordinal, code := range reasonCodes {
		if ordinal == int(ReasonNone) {
			continue
		}
		if code == "" {
			t.Fatalf("ordinal %d has no code: the constant block left a gap", ordinal)
		}
	}
}

// TestReasonCodesAreUniqueAndWireShaped — two constants sharing a code
// would make ReasonFromCode lossy, and a code with prose or capitals in
// it would not survive being a stable token in a log, an audit row, and a
// client bundle at once.
func TestReasonCodesAreUniqueAndWireShaped(t *testing.T) {
	shape := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	seen := map[string]Reason{}
	for ordinal, code := range reasonCodes {
		reason := Reason(ordinal)
		if reason == ReasonNone {
			continue
		}
		if !shape.MatchString(code) {
			t.Fatalf("%s has the code %q, which is not a lower_snake token", reason, code)
		}
		if prior, ok := seen[code]; ok {
			t.Fatalf("%s and %s share the code %q", prior, reason, code)
		}
		seen[code] = reason
	}
}

func TestReasonRoundTripsThroughItsCode(t *testing.T) {
	for ordinal := range reasonCodes {
		reason := Reason(ordinal)
		got, known := ReasonFromCode(reason.Code())
		if !known {
			t.Fatalf("%s did not round trip: code %q is unknown", reason, reason.Code())
		}
		if got != reason {
			t.Fatalf("%s round tripped to %s", reason, got)
		}
	}
}

// TestUnknownCodeIsRefusedRatherThanAdmitted — a code from a newer
// backend than this build knows must not resolve to the admitted value.
// That is the one mistranslation with no visible symptom.
func TestUnknownCodeIsRefusedRatherThanAdmitted(t *testing.T) {
	got, known := ReasonFromCode("reason_from_a_later_phase")
	if known {
		t.Fatal("an unknown code reported itself as known")
	}
	if !got.Refused() {
		t.Fatalf("an unknown code resolved to %s, which admits the presentation", got)
	}
}

// TestOutOfRangeReasonNeverReadsAsAdmitted covers the value nobody writes
// on purpose: a Reason built from an integer, or one that outlived a
// truncated table.
func TestOutOfRangeReasonNeverReadsAsAdmitted(t *testing.T) {
	beyond := Reason(len(reasonCodes) + 7)
	if beyond.Code() == "" {
		t.Fatal("an out-of-range Reason produced an empty code")
	}
	if !beyond.Refused() {
		t.Fatal("an out-of-range Reason reported itself as admitted")
	}
}

// TestFrontendHintsCoverEveryRefusal pins the client-side presentation
// module against this package's set, in both directions.
//
// A refusal with no hint is the failure this guards: the backend refuses,
// the client shows its generic fallback, and the one instruction that
// would have resolved it — a clock setting, a re-pair — is never shown.
// The other direction matters less but still costs: an entry for a code
// nothing emits is prose nobody will ever read, kept true forever.
//
// The check lives here rather than in the TS suite because this package
// owns the vocabulary. The frontend mirrors it.
func TestFrontendHintsCoverEveryRefusal(t *testing.T) {
	const modulePath = "../../frontend/src/lib/transport/authReason.ts"
	source, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("read %s: %v", modulePath, err)
	}
	module := string(source)

	declared := frontendReasonCodes(t, module)
	for ordinal, code := range reasonCodes {
		if Reason(ordinal) == ReasonNone {
			continue
		}
		if !declared[code] {
			t.Errorf("%s has no entry in %s, so it would show the generic refusal",
				code, modulePath)
			continue
		}
		delete(declared, code)
		// The presentation map is keyed by the same spelling. TypeScript's
		// Record type enforces this at check time; asserting it here means
		// a rename cannot pass the Go gate on the code list alone.
		if !strings.Contains(module, "\n  "+code+": {") {
			t.Errorf("%s is declared but has no presentation entry in %s", code, modulePath)
		}
	}
	for code := range declared {
		t.Errorf("%s declares a hint for %q, which no refusal path emits", modulePath, code)
	}
}

// frontendReasonCodes extracts the AUTH_REASON_CODES literal. Textual on
// purpose: a Go test that could evaluate TypeScript would be a bigger
// dependency than the property is worth, and the literal is a flat list of
// quoted strings by construction.
func frontendReasonCodes(t *testing.T, module string) map[string]bool {
	t.Helper()
	const marker = "export const AUTH_REASON_CODES = ["
	start := strings.Index(module, marker)
	if start < 0 {
		t.Fatalf("no %q in the presentation module; the gate is reading the wrong shape", marker)
	}
	rest := module[start+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatal("the AUTH_REASON_CODES literal is not closed")
	}
	codes := map[string]bool{}
	for _, match := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(rest[:end], -1) {
		codes[match[1]] = true
	}
	if len(codes) == 0 {
		t.Fatal("the presentation module declares no codes")
	}
	return codes
}

// reasonConstantsInOrder parses reason.go and returns the Reason
// constants in declaration order, which is their ordinal order because
// the block is an iota run.
func reasonConstantsInOrder(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("reason.go")
	if err != nil {
		t.Fatalf("read reason.go: %v", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "reason.go", source, 0)
	if err != nil {
		t.Fatalf("parse reason.go: %v", err)
	}
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if strings.HasPrefix(name.Name, "Reason") {
					names = append(names, name.Name)
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("reason.go declares no Reason constants; the gate is reading the wrong file")
	}
	return names
}

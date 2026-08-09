package def

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoopBoundParsesAndMarshalsBothAuthoredForms(t *testing.T) {
	cases := []struct {
		name     string
		yamlText string
		jsonText string
		want     LoopBound
	}{
		{"literal", "max: 2\n", `{"max":2}`, LiteralBound(2)},
		{"reference", "max: fix-budget\n", `{"max":"fix-budget"}`, RefBound("fix-budget")},
		{"dotted reference", "max: plan.rounds\n", `{"max":"plan.rounds"}`, RefBound("plan.rounds")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fromYAML Route
			if err := yaml.Unmarshal([]byte(tc.yamlText), &fromYAML); err != nil {
				t.Fatalf("yaml decode: %v", err)
			}
			if fromYAML.Max != tc.want {
				t.Fatalf("yaml decoded max = %+v, want %+v", fromYAML.Max, tc.want)
			}
			var fromJSON Route
			if err := json.Unmarshal([]byte(tc.jsonText), &fromJSON); err != nil {
				t.Fatalf("json decode: %v", err)
			}
			if fromJSON.Max != tc.want {
				t.Fatalf("json decoded max = %+v, want %+v", fromJSON.Max, tc.want)
			}
			encodedYAML, err := yaml.Marshal(Route{Max: tc.want})
			if err != nil {
				t.Fatalf("yaml encode: %v", err)
			}
			if string(encodedYAML) != tc.yamlText {
				t.Fatalf("yaml re-encoded as %q, want %q", encodedYAML, tc.yamlText)
			}
			encodedJSON, err := json.Marshal(Route{Max: tc.want})
			if err != nil {
				t.Fatalf("json encode: %v", err)
			}
			if string(encodedJSON) != tc.jsonText {
				t.Fatalf("json re-encoded as %s, want %s", encodedJSON, tc.jsonText)
			}
		})
	}
}

// A route that declares no bound must persist without the key, exactly as it
// did when the field was a plain int — a snapshot that suddenly carried
// `"max": 0` on every forward route would be a wire change nothing asked for.
func TestUndeclaredLoopBoundStaysAbsentInBothEncodings(t *testing.T) {
	encodedJSON, err := json.Marshal(Route{To: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encodedJSON) != `{"to":"done"}` {
		t.Fatalf("undeclared bound encoded as %s", encodedJSON)
	}
	encodedYAML, err := yaml.Marshal(Route{To: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encodedYAML) != "to: done\n" {
		t.Fatalf("undeclared bound encoded as %q", encodedYAML)
	}
}

// A run freezes its definition and never re-validates it, so every snapshot
// written before the reference form existed still carries `max` as a plain JSON
// integer. Decoding one has to produce the literal bound the YAML did, and
// re-encoding it has to leave it an integer.
func TestLoopBoundDecodesLegacyIntegerSnapshots(t *testing.T) {
	const snapshot = `{"id":"legacy","name":"Legacy","phases":[{"id":"review","driver":"agent",` +
		`"gate":{"routes":[{"loop":"build","max":2},` +
		`{"human":{"approve":"done","reject":{"loop":"build","max":3}}}]}}]}`
	var workflow Workflow
	if err := json.Unmarshal([]byte(snapshot), &workflow); err != nil {
		t.Fatalf("decode frozen snapshot: %v", err)
	}
	routes := workflow.Phases[0].Gate.Routes
	if routes[0].Max != LiteralBound(2) {
		t.Fatalf("legacy loop max = %+v, want the literal 2", routes[0].Max)
	}
	if routes[1].Human.Reject.Max != LiteralBound(3) {
		t.Fatalf("legacy reject max = %+v, want the literal 3", routes[1].Human.Reject.Max)
	}
	reencoded, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"max":2`, `"max":3`} {
		if !strings.Contains(string(reencoded), want) {
			t.Fatalf("re-encoded snapshot lacks %s:\n%s", want, reencoded)
		}
	}
}

func TestLoopBoundRefusesEveryOtherScalarShape(t *testing.T) {
	for _, text := range []string{"max: [2]\n", "max: {ref: budget}\n", "max: true\n", "max: 2.5\n"} {
		var route Route
		if err := yaml.Unmarshal([]byte(text), &route); err == nil {
			t.Errorf("yaml %q was accepted as a loop bound", strings.TrimSpace(text))
		}
	}
	for _, text := range []string{`{"max":[2]}`, `{"max":{"ref":"budget"}}`, `{"max":true}`, `{"max":2.5}`} {
		var route Route
		if err := json.Unmarshal([]byte(text), &route); err == nil {
			t.Errorf("json %s was accepted as a loop bound", text)
		}
	}
}

func TestLoopBoundResolveRequiresAWholeCountOfAtLeastOne(t *testing.T) {
	vars := map[string]any{
		"budget":     3,
		"as-float":   float64(4),
		"as-number":  json.Number("5"),
		"nested":     map[string]any{"rounds": 6},
		"fractional": 2.5,
		"zero":       0,
		"negative":   -1,
		"text":       "many",
		"huge":       json.Number("99999999999999999999"),
	}
	for _, tc := range []struct {
		name  string
		bound LoopBound
		want  int
	}{
		{"literal", LiteralBound(2), 2},
		{"int variable", RefBound("budget"), 3},
		{"float variable", RefBound("as-float"), 4},
		{"json number variable", RefBound("as-number"), 5},
		{"dotted variable", RefBound("nested.rounds"), 6},
		{"padded reference", RefBound("  budget  "), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.bound.Resolve(vars)
			if err != nil || got != tc.want {
				t.Fatalf("Resolve = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		bound LoopBound
	}{
		{"undeclared", LoopBound{}},
		{"literal zero", LiteralBound(0)},
		{"literal negative", LiteralBound(-1)},
		{"blank reference", RefBound("   ")},
		{"missing variable", RefBound("absent")},
		{"non-numeric variable", RefBound("text")},
		{"fractional variable", RefBound("fractional")},
		{"zero variable", RefBound("zero")},
		{"negative variable", RefBound("negative")},
		{"out-of-range variable", RefBound("huge")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.bound.Resolve(vars); err == nil {
				t.Fatalf("Resolve accepted %+v as the bound %d", tc.bound, got)
			}
		})
	}
}

// A seeded budget is a workflow input like any other, so the whole definition
// has to validate with nothing else changed.
func TestValidateAcceptsASeededLoopBudget(t *testing.T) {
	resolved := validResolved(t)
	resolved.Workflow.Inputs["fix-budget"] = Variable{Schema: JSONSchema{Type: "number"}}
	resolved.Workflow.Phases[2].Gate.Routes[1].Max = RefBound("fix-budget")
	result := Validate(resolved, validBindings(), nil)
	if !result.Valid() {
		t.Fatalf("seeded loop budget findings:\n%s", formatFindings(result.Findings))
	}
}

func TestValidateSeededLoopBoundReferences(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ResolvedWorkflow)
		want   string
	}{
		{"blank reference", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[1].Max = RefBound("   ")
		}, "requires a non-empty max reference"},
		{"unresolved reference", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[1].Max = RefBound("fix-budget")
		}, `max reference "fix-budget" does not resolve`},
		{"non-numeric reference", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[1].Max = RefBound("goal")
		}, `must name a number-typed variable; it is "string"`},
		{"optional producer", func(r *ResolvedWorkflow) {
			r.Workflow.Inputs["fix-budget"] = Variable{Schema: JSONSchema{Type: "number"}, Optional: true}
			r.Workflow.Phases[2].Gate.Routes[1].Max = RefBound("fix-budget")
		}, `optional producer "fix-budget" cannot seed a loop budget`},
		{"non-dominating producer", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Outputs["rounds"] = Variable{Schema: JSONSchema{Type: "number"}}
			r.Workflow.Phases[1].Gate.Routes = append(r.Workflow.Phases[1].Gate.Routes,
				Route{Loop: "plan", Max: RefBound("review.rounds")})
		}, `producer phase "review" for max "review.rounds" does not dominate this phase`},
		{"human reject reference", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[0] = Route{Human: &HumanRoute{
				Approve: "done",
				Reject:  &LoopTarget{Loop: "implement", Max: RefBound("fix-budget")},
			}}
		}, `max reference "fix-budget" does not resolve`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved := validResolved(t)
			tc.mutate(&resolved)
			result := Validate(resolved, validBindings(), nil)
			if !hasFindingMessage(result.Findings, "gate.loop-max", tc.want) {
				t.Fatalf("missing gate.loop-max finding %q:\n%s", tc.want, formatFindings(result.Findings))
			}
		})
	}
}

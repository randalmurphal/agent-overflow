package def

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestEffortParsesAndValidatesOnAgentTurns is the round trip: `effort:` survives
// strict YAML parsing on every element that runs a model turn — an agent phase,
// a fan-out unit template, and the join — and a definition that declares them
// validates clean.
func TestEffortParsesAndValidatesOnAgentTurns(t *testing.T) {
	dir := t.TempDir()
	for name, body := range fanOutPrompts() {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "workflow.yaml")
	definition := `
id: port
name: Port
inputs:
  goal:
    schema:
      type: string
phases:
  - id: plan
    driver: agent
    provider: claude
    model: sonnet
    effort: max
    prompt: plan.md
    inputs:
      goal:
        schema:
          type: string
    outputs:
      sections:
        schema:
          type: array
          items:
            type: object
            properties:
              path:
                type: string
            required: [path]
    gate:
      routes:
        - to: port
  - id: port
    shape: fan-out
    over: plan.sections
    as: section
    unit:
      id: port-section
      provider: claude
      model: sonnet
      effort: low
      prompt: unit.md
      access: write
    join:
      id: merge
      provider: claude
      model: sonnet
      effort: none
      prompt: join.md
    inputs:
      plan.sections:
        schema:
          type: array
          items:
            type: object
            properties:
              path:
                type: string
            required: [path]
    outputs:
      merged:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
`
	if err := os.WriteFile(path, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := workflow.Phases[0].Effort; got != string(EffortMax) {
		t.Errorf("phase effort = %q, want %q", got, EffortMax)
	}
	if got := workflow.Phases[1].Unit.Effort; got != string(EffortLow) {
		t.Errorf("unit effort = %q, want %q", got, EffortLow)
	}
	if got := workflow.Phases[1].Join.Effort; got != string(EffortNone) {
		t.Errorf("join effort = %q, want %q", got, EffortNone)
	}
	requireValid(t, Validate(
		ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: path}, validBindings(), nil,
	))
}

// TestEffortIsRefusedWhereNoTurnRuns pins placement. `effort:` configures a
// model turn, so it is legal exactly where provider/model are and a finding
// everywhere else — a tool phase runs a command, and a call phase, a fan-out
// phase, and a call unit run no work of their own at all. Each case asserts the
// SAME code the provider/model refusal carries, because the two are one rule.
func TestEffortIsRefusedWhereNoTurnRuns(t *testing.T) {
	t.Run("tool phase", func(t *testing.T) {
		resolved := validResolved(t)
		// Phase index 2 is the `driver: tool` check phase.
		resolved.Workflow.Phases[2].Effort = string(EffortHigh)
		result := Validate(resolved, validBindings(), nil)
		requireFinding(t, result, "phase.effort", "effort requires driver: agent")
	})

	t.Run("call phase", func(t *testing.T) {
		caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
		caller.Phases[1].Effort = string(EffortHigh)
		result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
		requireFinding(t, result, "phase.call", "provider/model/effort/prompt is not valid on a call phase")
	})

	t.Run("fan-out phase", func(t *testing.T) {
		workflow := dynamicFanOutWorkflow()
		workflow.Phases[1].Effort = string(EffortHigh)
		result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
		requireFinding(t, result, "phase.fan-out", "provider/model/effort/prompt is not valid on a fan-out phase")
	})

	t.Run("command unit", func(t *testing.T) {
		workflow := staticFanOutWorkflow()
		workflow.Phases[1].FanOut[0] = Unit{ID: "alpha", Command: "report", Effort: string(EffortHigh)}
		result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
		requireFinding(t, result, "phase.fan-out-unit",
			"a unit declares a command, provider/model/effort/prompt, or call, not more than one")
	})

	t.Run("call unit", func(t *testing.T) {
		workflow := callFanOutWorkflow()
		workflow.Phases[1].Unit.Effort = string(EffortHigh)
		result := validateCallFanOut(t, workflow)
		requireFinding(t, result, "phase.fan-out-unit", "provider/model/effort/prompt is not valid on a call unit")
	})
}

// TestUnknownEffortTierIsAFinding pins the closed vocabulary. An unrecognised
// tier is refused rather than coerced: silently running at the model default
// would make a typo indistinguishable from a deliberate omission.
func TestUnknownEffortTierIsAFinding(t *testing.T) {
	t.Run("phase", func(t *testing.T) {
		resolved := validResolved(t)
		resolved.Workflow.Phases[0].Effort = "hard"
		result := Validate(resolved, validBindings(), nil)
		message := requireFinding(t, result, "phase.effort", `unknown effort "hard"`).Message
		// The finding has to list the vocabulary, or the author's only route to
		// the right spelling is reading our source.
		for _, tier := range EffortTierNames() {
			if !strings.Contains(message, tier) {
				t.Fatalf("finding %q does not name the tier %q", message, tier)
			}
		}
	})

	t.Run("unit", func(t *testing.T) {
		workflow := dynamicFanOutWorkflow()
		workflow.Phases[1].Unit.Effort = "hard"
		result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
		requireFinding(t, result, "phase.effort", `unknown effort "hard"`)
	})

	t.Run("join", func(t *testing.T) {
		workflow := dynamicFanOutWorkflow()
		workflow.Phases[1].Join.Effort = "hard"
		result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
		requireFinding(t, result, "phase.effort", `unknown effort "hard"`)
	})
}

// TestPublishedSchemaEffortEnumMatchesTheTierVocabulary keeps the editor's
// completion list and the validator's refusal in lockstep. The schema is what an
// author sees while typing; a tier missing from one side reads as either a
// phantom option or a spurious squiggle.
func TestPublishedSchemaEffortEnumMatchesTheTierVocabulary(t *testing.T) {
	var published map[string]any
	if err := json.Unmarshal(AuthoringSchema(), &published); err != nil {
		t.Fatalf("published schema is invalid JSON: %v", err)
	}
	defs := published["$defs"].(map[string]any)
	raw, ok := defs["effort"].(map[string]any)["enum"].([]any)
	if !ok {
		t.Fatal("published schema declares no $defs/effort enum")
	}
	enum := make([]string, 0, len(raw))
	for _, value := range raw {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("effort enum holds a non-string value %v", value)
		}
		enum = append(enum, text)
	}
	if !reflect.DeepEqual(enum, EffortTierNames()) {
		t.Fatalf("published effort enum %v does not match the tier vocabulary %v", enum, EffortTierNames())
	}
	// Both elements that may declare one point at that single enum, so neither
	// can drift on its own.
	for _, element := range []string{"phase", "unit"} {
		properties := defs[element].(map[string]any)["properties"].(map[string]any)
		effort, declared := properties["effort"].(map[string]any)
		if !declared {
			t.Fatalf("published %s schema declares no effort property", element)
		}
		if effort["$ref"] != "#/$defs/effort" {
			t.Errorf("published %s effort property = %v, want a $defs/effort reference", element, effort)
		}
	}
}

// TestEffortTierVocabularyIsClosedAndIsolated covers the accessors the schema,
// the diagnostics, and the root-package drift test all read.
func TestEffortTierVocabularyIsClosedAndIsolated(t *testing.T) {
	for _, tier := range EffortTiers() {
		if !KnownEffortTier(string(tier)) {
			t.Errorf("KnownEffortTier(%q) = false for a declared tier", tier)
		}
	}
	for _, name := range []string{"", " high", "HIGH", "hard", "extreme"} {
		if KnownEffortTier(name) {
			t.Errorf("KnownEffortTier(%q) = true; the vocabulary is closed", name)
		}
	}
	before := EffortTiers()
	mutated := EffortTiers()
	mutated[0] = "tampered"
	if !reflect.DeepEqual(before, EffortTiers()) {
		t.Fatal("EffortTiers returned a slice aliasing package state")
	}
	names := EffortTierNames()
	if len(names) != len(before) {
		t.Fatalf("EffortTierNames returned %d names for %d tiers", len(names), len(before))
	}
	for index, tier := range before {
		if names[index] != string(tier) {
			t.Fatalf("EffortTierNames[%d] = %q, want %q", index, names[index], tier)
		}
	}
}

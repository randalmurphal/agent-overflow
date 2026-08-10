package def

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// `non_goals:` is the author's standing boundary and freezes with the snapshot,
// so it has to survive the round trip every other authored field does.
func TestNonGoalsParseAndFreeze(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(path, []byte(`
id: port
name: Port
non_goals:
  - Do not redesign the build system.
  - Do not widen the public API.
phases:
  - id: run
    driver: agent
    provider: claude
    model: sonnet
    prompt: run.md
    gate:
      routes:
        - to: done
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.md"), []byte("run it"), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Do not redesign the build system.", "Do not widen the public API."}
	if strings.Join(workflow.NonGoals, "|") != strings.Join(want, "|") {
		t.Fatalf("parsed non-goals = %v", workflow.NonGoals)
	}
	requireValid(t, Validate(ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: path}, nil, nil))

	// A snapshot is JSON, and a frozen definition is decoded and never
	// re-validated — so the list has to come back off the wire intact or every
	// element of a running campaign silently loses its boundary.
	encoded, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Workflow
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if strings.Join(decoded.NonGoals, "|") != strings.Join(want, "|") {
		t.Fatalf("non-goals did not survive the snapshot round trip: %v", decoded.NonGoals)
	}
}

// Every bound is a finding rather than a silent trim: a non-goal quietly
// dropped is exactly the boundary an element then crosses.
func TestNonGoalBoundsAreFindings(t *testing.T) {
	overLong := make([]string, MaxNonGoals+1)
	for index := range overLong {
		overLong[index] = "a stated boundary"
	}
	for _, testCase := range []struct {
		name     string
		nonGoals []string
		contains string
	}{
		{"blank entry", []string{"a stated boundary", "   "}, "non-goal 1 is blank"},
		{"over-long entry", []string{strings.Repeat("x", MaxNonGoalRunes+1)}, "the maximum is 500"},
		{"too many", overLong, "exceed the maximum of 12"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolved := validResolved(t)
			resolved.Workflow.NonGoals = testCase.nonGoals
			requireFinding(t, Validate(resolved, validBindings(), nil), "workflow.non-goals", testCase.contains)
		})
	}
}

// A rune-counted bound must not refuse a list that is inside it in runes and
// over it in bytes: a non-goal written in a non-ASCII language is still one
// sentence.
func TestNonGoalLengthIsCountedInRunes(t *testing.T) {
	resolved := validResolved(t)
	resolved.Workflow.NonGoals = []string{strings.Repeat("é", MaxNonGoalRunes)}
	requireValid(t, Validate(resolved, validBindings(), nil))
}

// The published schema is what an editor holds a workflow to before Go reads
// it, so a definition Go accepts must not be flagged there — and vice versa.
func TestPublishedSchemaCarriesNonGoals(t *testing.T) {
	var published map[string]any
	if err := json.Unmarshal(AuthoringSchema(), &published); err != nil {
		t.Fatalf("published schema is invalid JSON: %v", err)
	}
	nonGoals, ok := published["properties"].(map[string]any)["non_goals"].(map[string]any)
	if !ok {
		t.Fatal("published schema lacks the non_goals key used by the Go format")
	}
	if nonGoals["maxItems"] != float64(MaxNonGoals) {
		t.Errorf("published maxItems = %v, want %d", nonGoals["maxItems"], MaxNonGoals)
	}
	items := nonGoals["items"].(map[string]any)
	if items["maxLength"] != float64(MaxNonGoalRunes) {
		t.Errorf("published maxLength = %v, want %d", items["maxLength"], MaxNonGoalRunes)
	}
	var authored any
	if err := yaml.Unmarshal([]byte("[\"a stated boundary\"]"), &authored); err != nil {
		t.Fatal(err)
	}
	if err := validateFixtureSchema(authored, nonGoals, published, "$.non_goals"); err != nil {
		t.Fatalf("published schema refused a valid non-goals list: %v", err)
	}
}

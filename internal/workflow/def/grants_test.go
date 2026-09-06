package def

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseReadsPhaseGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.yaml")
	writeFile(t, path, `id: granted
name: Granted
phases:
  - id: work
    driver: agent
    provider: codex
    model: test-model
    prompt: work.md
    grants: [start-run, introspect]
    gate:
      routes:
        - to: done
`)
	writeFile(t, filepath.Join(filepath.Dir(path), "work.md"), "do it\n")
	workflow, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	got := workflow.Phases[0].Grants
	if len(got) != 2 || got[0] != string(GrantStartRun) || got[1] != string(GrantIntrospect) {
		t.Fatalf("grants = %v", got)
	}
	result := Validate(ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: path}, nil, nil)
	if !result.Valid() {
		t.Fatalf("granted workflow findings:\n%s", formatFindings(result.Findings))
	}
}

func TestValidateGrantsRefusesUnknownDuplicateAndToolPhases(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Phase)
		want   string
	}{
		{"unknown", func(p *Phase) { p.Grants = []string{"report-back"} }, `unknown grant "report-back"`},
		{"duplicate", func(p *Phase) { p.Grants = []string{"schedule", "schedule"} }, `grant "schedule" is declared more than once`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			resolved := validResolved(t)
			test.mutate(&resolved.Workflow.Phases[0])
			result := Validate(resolved, validBindings(), nil)
			if !hasFindingMessage(result.Findings, "phase.grants", test.want) {
				t.Fatalf("missing phase.grants finding %q:\n%s", test.want, formatFindings(result.Findings))
			}
		})
	}

	t.Run("tool phase", func(t *testing.T) {
		resolved := validResolved(t)
		// Phase index 2 is the fixture's `driver: tool` check phase.
		resolved.Workflow.Phases[2].Grants = []string{string(GrantIntrospect)}
		result := Validate(resolved, validBindings(), nil)
		if !hasFindingMessage(result.Findings, "phase.grants", "grants require an agent driver") {
			t.Fatalf("tool phase grant was accepted:\n%s", formatFindings(result.Findings))
		}
	})

	// A fan-out phase has no driver of its own, so "does anything here hold a
	// session" is answered by its units and its join — the app scopes every one
	// of their tokens from the phase's frozen grants.
	t.Run("fan-out with an agent unit", func(t *testing.T) {
		workflow := dynamicFanOutWorkflow()
		workflow.Phases[1].Grants = []string{string(GrantIntrospect)}
		result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
		if !result.Valid() {
			t.Fatalf("fan-out grant was refused:\n%s", formatFindings(result.Findings))
		}
	})

	t.Run("fan-out with only tool units", func(t *testing.T) {
		workflow := dynamicFanOutWorkflow()
		phase := &workflow.Phases[1]
		phase.Grants = []string{string(GrantIntrospect)}
		phase.Unit = &Unit{ID: "port-section", Command: "merge-branches"}
		phase.Join = &Unit{ID: "merge", Command: "merge-branches"}
		result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
		if !hasFindingMessage(result.Findings, "phase.grants", "grants require an agent session") {
			t.Fatalf("all-tool fan-out grant was accepted:\n%s", formatFindings(result.Findings))
		}
	})

	t.Run("call phase", func(t *testing.T) {
		phase := Phase{ID: "invoke", Shape: ShapeCall, Call: "child", Grants: []string{string(GrantStartRun)}}
		findings := validateCall(phase, "phase")
		joined := formatFindings(findings)
		if !strings.Contains(joined, "grants") {
			t.Fatalf("call phase grant was accepted:\n%s", joined)
		}
	})
}

// A run freezes its definition, so a phase's grants have to survive the JSON
// round trip the snapshot is stored as — a grant lost in freezing would silently
// downgrade a re-entered phase's authority.
func TestPhaseGrantsSurviveSnapshotRoundTrip(t *testing.T) {
	resolved := validResolved(t)
	resolved.Workflow.Phases[0].Grants = []string{string(GrantStartRun), string(GrantUpdateNotes)}
	encoded, err := json.Marshal(resolved.Workflow)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Workflow
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := decoded.Phases[0].Grants
	if len(got) != 2 || got[0] != string(GrantStartRun) || got[1] != string(GrantUpdateNotes) {
		t.Fatalf("round-tripped grants = %v", got)
	}
	if len(decoded.Phases[1].Grants) != 0 {
		t.Fatalf("ungranted phase gained grants: %v", decoded.Phases[1].Grants)
	}
}

func TestGrantNamesIsTheClosedSet(t *testing.T) {
	want := []string{"introspect", "remote-commands", "resolve", "schedule", "start-run", "update-notes"}
	got := GrantNames()
	if len(got) != len(want) {
		t.Fatalf("GrantNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GrantNames() = %v, want %v", got, want)
		}
	}
	// report-back is named in spec §5 but deliberately not implemented in v1;
	// admitting it here would let a workflow declare authority nothing honours.
	if KnownGrant("report-back") {
		t.Fatal("report-back is not a v1 grant")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func hasFindingMessage(findings []Finding, code, substring string) bool {
	for _, finding := range findings {
		if finding.Code == code && strings.Contains(finding.Message, substring) {
			return true
		}
	}
	return false
}

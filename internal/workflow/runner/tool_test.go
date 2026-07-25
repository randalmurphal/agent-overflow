package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/workflow/def"
)

func TestSynthesizedToolEnvelopeCarriesTheExitStatus(t *testing.T) {
	phase := def.Phase{ID: "build", Driver: def.DriverTool, Check: "go-build", Outputs: map[string]def.Variable{
		"details":  {Schema: def.JSONSchema{Type: "string"}, Optional: true},
		"required": {Schema: def.JSONSchema{Type: "string"}},
	}}
	for _, exitCode := range []int{0, 3} {
		payload, err := SynthesizedToolEnvelope(def.PhaseEnvelope(phase), exitCode)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Status  string         `json:"status"`
			Outputs map[string]any `json:"outputs"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Status != "done" {
			t.Fatalf("status = %q, want done: a non-zero exit is a result, not a failure", envelope.Status)
		}
		if passed, ok := envelope.Outputs[def.ToolOutputPassed].(bool); !ok || passed != (exitCode == 0) {
			t.Fatalf("exit %d produced passed = %v", exitCode, envelope.Outputs[def.ToolOutputPassed])
		}
		if code, ok := envelope.Outputs[def.ToolOutputExitCode].(float64); !ok || int(code) != exitCode {
			t.Fatalf("exit %d produced exit-code = %v", exitCode, envelope.Outputs[def.ToolOutputExitCode])
		}
		value, declared := envelope.Outputs["details"]
		if !declared || value != nil {
			t.Fatalf("optional authored output = %v (declared %v), want an explicit null", value, declared)
		}
		if _, invented := envelope.Outputs["required"]; invented {
			t.Fatal("synthesis invented a required authored output")
		}
		// The whole point: a required authored output must fail here so the
		// phase parks instead of advancing on a fabricated contract.
		if err := def.ValidateEnvelope(phase, payload); err == nil {
			t.Fatal("synthesized envelope satisfied a phase declaring a required output")
		}
	}
}

func TestApplyToolOutputsOwnsTheSystemOutputs(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		payload  string
		exitCode int
		want     string
	}{
		{
			name:    "adds the system outputs the command cannot know",
			payload: `{"status":"done","outputs":{"report":"green"},"question":null,"reason":null}`,
			want:    `{"outputs":{"exit-code":0,"passed":true,"report":"green"},"question":null,"reason":null,"status":"done"}`,
		},
		{
			name:     "overrides a command that guessed",
			payload:  `{"status":"done","outputs":{"passed":true,"exit-code":0},"question":null,"reason":null}`,
			exitCode: 7,
			want:     `{"outputs":{"exit-code":7,"passed":false},"question":null,"reason":null,"status":"done"}`,
		},
		{
			name:    "fills a null outputs branch",
			payload: `{"status":"done","outputs":null,"question":null,"reason":null}`,
			want:    `{"outputs":{"exit-code":0,"passed":true},"question":null,"reason":null,"status":"done"}`,
		},
		{
			name:    "leaves the stuck branch alone",
			payload: `{"status":"stuck","outputs":null,"question":null,"reason":"toolchain missing"}`,
			want:    `{"status":"stuck","outputs":null,"question":null,"reason":"toolchain missing"}`,
		},
		{
			name:    "passes malformed payloads to post-validation untouched",
			payload: `not json`,
			want:    `not json`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := string(ApplyToolOutputs(json.RawMessage(testCase.payload), testCase.exitCode))
			if got != testCase.want {
				t.Fatalf("ApplyToolOutputs = %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestToolNarrativeRecordsCommandOutcomeAndOutput(t *testing.T) {
	narrative := ToolNarrative(ToolReport{
		PhaseID: "build", Attempt: 2, Binding: `check "go-build"`,
		Argv:      []string{"go", "build", "./..."},
		Workspace: "/tmp/worktree", Duration: 1500 * time.Millisecond,
		Outcome: "the command exited", Exited: true, ExitCode: 2,
		Envelope: ToolEnvelopeSynthesized,
		Findings: []def.EnvelopeFinding{{Path: "$.outputs.report", Message: "property is required"}},
		Output:   "compile failed\n", Truncated: true,
	})
	for _, want := range []string{
		"# Tool phase build (attempt 2)",
		`check "go-build"`,
		`["go", "build", "./..."]`,
		"/tmp/worktree",
		"- Exit code: 2",
		"- Duration: 1.5s",
		"synthesized from the process exit status",
		"$.outputs.report: property is required",
		"must write a control envelope",
		"truncated",
		"compile failed",
	} {
		if !strings.Contains(narrative, want) {
			t.Fatalf("narrative missing %q:\n%s", want, narrative)
		}
	}
}

func TestToolNarrativeFencesBacktickHeavyOutputAndEmptyOutput(t *testing.T) {
	narrative := ToolNarrative(ToolReport{
		PhaseID: "build", Attempt: 1, Argv: []string{"make"},
		Outcome: "the command exited", Exited: true,
		Envelope: ToolEnvelopeWritten,
		Output:   "before\n```\nfence\n```\nafter",
	})
	if !strings.Contains(narrative, "````\nbefore") {
		t.Fatalf("output containing a fence was not escaped:\n%s", narrative)
	}
	empty := ToolNarrative(ToolReport{
		PhaseID: "build", Attempt: 1, Argv: []string{"make"},
		Outcome: "the command exited", Exited: true, Envelope: ToolEnvelopeSynthesized,
	})
	if !strings.Contains(empty, "(no output)") {
		t.Fatalf("silent command narrative:\n%s", empty)
	}
	if strings.Contains(empty, "Envelope validation failed") {
		t.Fatalf("clean attempt reported findings:\n%s", empty)
	}
}

func TestAttemptPathsShareOneAttemptDirectory(t *testing.T) {
	narrative, err := NarrativePath("/data", "item", "build", 3)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := EnvelopePath("/data", "item", "build", 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/data/workflow-runs/item/build.3/narrative.md"; narrative != want {
		t.Fatalf("narrative path = %q, want %q", narrative, want)
	}
	if want := "/data/workflow-runs/item/build.3/envelope.json"; envelope != want {
		t.Fatalf("envelope path = %q, want %q", envelope, want)
	}
	if _, err := EnvelopePath("/data", "item", "build", 0); err == nil {
		t.Fatal("attempt 0 produced a path")
	}
}

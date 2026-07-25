package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/workflow/def"
)

// ToolEnvelopeSource records where a tool phase attempt's control envelope came
// from, which is the difference between "the command told us what happened" and
// "we read its exit status".
type ToolEnvelopeSource string

const (
	// ToolEnvelopeWritten means the command wrote the AO_ENVELOPE file.
	ToolEnvelopeWritten ToolEnvelopeSource = "written"
	// ToolEnvelopeSynthesized means the runner built the envelope from the
	// process exit status because the command wrote nothing.
	ToolEnvelopeSynthesized ToolEnvelopeSource = "synthesized"
	// ToolEnvelopeAbsent means the attempt never got far enough to produce one.
	ToolEnvelopeAbsent ToolEnvelopeSource = "absent"
)

// SynthesizedToolEnvelope is the envelope a tool phase gets when its command
// exits without writing one. A non-zero exit is deliberately not a phase
// failure: it is `passed: false`, and the phase's gate decides what that means.
//
// Beyond the two system outputs it can only fill in the author's *optional*
// outputs, as null — the declared way to say "no value". A required authored
// output has no honest synthesized value, so the envelope fails post-validation
// and the phase parks telling the human its command must write AO_ENVELOPE.
func SynthesizedToolEnvelope(phase def.Phase, exitCode int) (json.RawMessage, error) {
	outputs := map[string]any{
		def.ToolOutputPassed:   exitCode == 0,
		def.ToolOutputExitCode: exitCode,
	}
	for name, declaration := range def.PhaseOutputs(phase) {
		if _, system := outputs[name]; system || !declaration.Optional {
			continue
		}
		outputs[name] = nil
	}
	envelope := struct {
		Status   string         `json:"status"`
		Outputs  map[string]any `json:"outputs"`
		Question *string        `json:"question"`
		Reason   *string        `json:"reason"`
	}{Status: "done", Outputs: outputs}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode synthesized tool envelope: %w", err)
	}
	return encoded, nil
}

// ApplyToolOutputs stamps the system-owned outputs onto an envelope the command
// wrote. A command cannot know its own exit status while writing the file, so
// the runner supplies `passed` and `exit-code`; the command owns every other
// output. Only a `done` envelope carries outputs at all, so the question and
// stuck branches pass through untouched.
//
// Anything that does not decode is returned unchanged: this is not a validator,
// and post-validation reports the real problem with better findings than a
// rewrite could.
func ApplyToolOutputs(payload json.RawMessage, exitCode int) json.RawMessage {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(payload, &envelope) != nil {
		return payload
	}
	var status string
	if json.Unmarshal(envelope["status"], &status) != nil || status != "done" {
		return payload
	}
	outputs := make(map[string]json.RawMessage)
	if raw, declared := envelope["outputs"]; declared && string(bytes.TrimSpace(raw)) != "null" {
		if json.Unmarshal(raw, &outputs) != nil {
			return payload
		}
	}
	passed, err := json.Marshal(exitCode == 0)
	if err != nil {
		return payload
	}
	code, err := json.Marshal(exitCode)
	if err != nil {
		return payload
	}
	outputs[def.ToolOutputPassed] = passed
	outputs[def.ToolOutputExitCode] = code
	encodedOutputs, err := json.Marshal(outputs)
	if err != nil {
		return payload
	}
	envelope["outputs"] = encodedOutputs
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return payload
	}
	return encoded
}

// ToolReport is everything the system knows about one tool phase attempt after
// its process is gone. It renders to the phase narrative — the human-facing
// record of what ran, how it ended, and what it printed.
type ToolReport struct {
	PhaseID   string
	Attempt   int
	Binding   string
	Argv      []string
	Workspace string
	Duration  time.Duration
	// Outcome is a one-line summary owned by the caller ("exited", "killed by
	// the inactivity watchdog", ...). Exited/ExitCode carry the process status
	// when the process ran to completion.
	Outcome   string
	Exited    bool
	ExitCode  int
	Envelope  ToolEnvelopeSource
	Findings  []def.EnvelopeFinding
	Output    string
	Truncated bool
}

// ToolNarrative renders the system-written narrative for one tool phase
// attempt. Agent phases write their own; a tool phase has no author, so the
// runner writes this instead — same file, same read paths.
func ToolNarrative(report ToolReport) string {
	var narrative strings.Builder
	fmt.Fprintf(&narrative, "# Tool phase %s (attempt %d)\n\n", report.PhaseID, report.Attempt)
	if report.Binding != "" {
		fmt.Fprintf(&narrative, "- Binding: %s\n", report.Binding)
	}
	fmt.Fprintf(&narrative, "- Command: %s\n", FormatArgv(report.Argv))
	if report.Workspace != "" {
		fmt.Fprintf(&narrative, "- Workspace: %s\n", report.Workspace)
	}
	fmt.Fprintf(&narrative, "- Outcome: %s\n", report.Outcome)
	if report.Exited {
		fmt.Fprintf(&narrative, "- Exit code: %d\n", report.ExitCode)
	}
	fmt.Fprintf(&narrative, "- Duration: %s\n", report.Duration.Round(time.Millisecond))
	fmt.Fprintf(&narrative, "- Envelope: %s\n", toolEnvelopeDescription(report.Envelope))

	if len(report.Findings) > 0 {
		narrative.WriteString("\n## Envelope validation failed\n\n")
		for _, finding := range report.Findings {
			fmt.Fprintf(&narrative, "- %s: %s\n", finding.Path, finding.Message)
		}
		narrative.WriteString("\n")
		narrative.WriteString(toolEnvelopeGuidance(report.Envelope))
		narrative.WriteString("\n")
	}

	narrative.WriteString("\n## Output (stdout and stderr")
	if report.Truncated {
		narrative.WriteString(", truncated to the last bytes")
	}
	narrative.WriteString(")\n\n")
	output := strings.TrimRight(report.Output, "\n")
	if strings.TrimSpace(output) == "" {
		narrative.WriteString("(no output)\n")
		return narrative.String()
	}
	fence := outputFence(output)
	narrative.WriteString(fence)
	narrative.WriteString("\n")
	narrative.WriteString(output)
	narrative.WriteString("\n")
	narrative.WriteString(fence)
	narrative.WriteString("\n")
	return narrative.String()
}

func toolEnvelopeDescription(source ToolEnvelopeSource) string {
	switch source {
	case ToolEnvelopeWritten:
		return "written by the command to AO_ENVELOPE"
	case ToolEnvelopeSynthesized:
		return "synthesized from the process exit status"
	default:
		return "not produced"
	}
}

func toolEnvelopeGuidance(source ToolEnvelopeSource) string {
	if source == ToolEnvelopeSynthesized {
		return "The command exited without writing the file named by the AO_ENVELOPE environment variable, " +
			"so only the exit status was available. This phase declares outputs an exit status cannot supply: " +
			"the command must write a control envelope to that path."
	}
	return "The control envelope the command wrote to AO_ENVELOPE does not satisfy this phase's contract. " +
		"A deterministic command gets no feedback turn, so the run parks here for a human."
}

// outputFence returns a code fence long enough to contain output that itself
// contains backtick fences.
func outputFence(output string) string {
	longest := 0
	run := 0
	for _, char := range output {
		if char == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	if longest < 3 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}

// FormatArgv renders an argument vector for human-facing diagnostics.
func FormatArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for index, argument := range argv {
		quoted[index] = strconv.Quote(argument)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

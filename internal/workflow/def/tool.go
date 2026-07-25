package def

// Tool-driver phases always report the deterministic command's result, whether
// or not the command writes its own envelope. These two outputs are therefore
// part of every tool phase's contract without being authored, so a gate can
// route on a check result (`when: eq: {ref: build.passed, value: false}`)
// against any bound command.
//
// Names follow the authoring grammar ([a-z0-9-]+) like every other reference in
// the system, and `exit-code` is a `number` because that is the only numeric
// type the variable vocabulary has — the value is always an integral exit
// status.
const (
	ToolOutputPassed   = "passed"
	ToolOutputExitCode = "exit-code"
)

// implicitToolOutputs returns a fresh copy of the system-owned tool outputs.
func implicitToolOutputs() map[string]Variable {
	return map[string]Variable{
		ToolOutputPassed: {Schema: JSONSchema{
			Type:        "boolean",
			Description: "True when the command exited 0.",
		}},
		ToolOutputExitCode: {Schema: JSONSchema{
			Type:        "number",
			Description: "The command's process exit status.",
		}},
	}
}

// ReservedToolOutput reports whether a name belongs to the tool driver. Authors
// may not declare it: the driver always supplies it, and a redefinition would
// let a gate read a different type than the runner produces.
func ReservedToolOutput(name string) bool {
	_, reserved := implicitToolOutputs()[name]
	return reserved
}

// PhaseOutputs is the phase's complete output contract: everything the author
// declared plus the implicit tool outputs. Envelope schema generation, envelope
// post-validation, and variable resolution all read outputs through this one
// function, so no path can disagree about what a phase produces.
//
// The returned map is read-only: for an agent phase it is the authored map
// itself.
func PhaseOutputs(phase Phase) map[string]Variable {
	if phase.Driver != DriverTool {
		return phase.Outputs
	}
	implicit := implicitToolOutputs()
	merged := make(map[string]Variable, len(phase.Outputs)+len(implicit))
	for name, output := range phase.Outputs {
		merged[name] = output
	}
	// System outputs win. Validation rejects the collision as well; this keeps
	// runtime coherent for a snapshot frozen before that rule existed.
	for name, output := range implicit {
		merged[name] = output
	}
	return merged
}

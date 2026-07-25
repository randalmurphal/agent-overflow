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
// declared plus the implicit tool outputs when a command is what produces the
// phase's envelope. Envelope schema generation, envelope post-validation, and
// variable resolution all read outputs through this one function, so no path can
// disagree about what a phase produces.
//
// The returned map is read-only: for an agent phase it is the authored map
// itself.
func PhaseOutputs(phase Phase) map[string]Variable {
	if !PhaseProducesToolEnvelope(phase) {
		return phase.Outputs
	}
	return withToolOutputs(phase.Outputs)
}

// PhaseProducesToolEnvelope reports whether a deterministic command produces the
// phase's envelope. For a single-shape phase that is its own `driver:`; for a
// fan-out it is the join's, because the join's envelope IS the phase's — a
// command join reports `passed` and `exit-code` for the phase exactly as a
// `driver: tool` phase does. The fan-out's work units are irrelevant here: their
// envelopes answer their own contracts and reach the gate only through the join.
func PhaseProducesToolEnvelope(phase Phase) bool {
	if phase.EffectiveShape() == ShapeFanOut {
		return phase.Join != nil && phase.Join.EffectiveDriver() == DriverTool
	}
	return phase.Driver == DriverTool
}

// UnitOutputs is one fan-out unit's complete output contract. A unit's driver
// is discriminated by its binding rather than declared, so this is the same
// rule PhaseOutputs applies: a unit that runs a command always reports `passed`
// and `exit-code` on top of whatever it declared, and an agent unit reports
// exactly what it declared — nothing when it declared nothing.
//
// The returned map is read-only, like PhaseOutputs'.
func UnitOutputs(unit Unit) map[string]Variable {
	if unit.EffectiveDriver() != DriverTool {
		return unit.Outputs
	}
	return withToolOutputs(unit.Outputs)
}

// withToolOutputs merges the system-owned tool outputs over an authored set.
// System outputs win. Validation rejects the collision as well; this keeps
// runtime coherent for a snapshot frozen before that rule existed.
func withToolOutputs(authored map[string]Variable) map[string]Variable {
	implicit := implicitToolOutputs()
	merged := make(map[string]Variable, len(authored)+len(implicit))
	for name, output := range authored {
		merged[name] = output
	}
	for name, output := range implicit {
		merged[name] = output
	}
	return merged
}

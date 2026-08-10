package def

import "reflect"

// ApplyInheritedInputSchemas resolves the one thing the authoring format cannot
// state and the author should not have to repeat: a phase input bound straight
// to a workflow input, declaring no schema of its own, takes the workflow
// input's schema.
//
// It exists because the duplication is the bulk of a real definition. A phase
// that consumes `campaign-goal` has to restate the multiline string schema the
// workflow input already declared, in full, once per phase that reads it —
// roughly forty percent of the YAML of the campaign this came from, all of it a
// copy of something twenty lines above, and every copy a place the two can
// silently disagree the next time one of them is edited.
//
// The rule is narrow on purpose:
//
//   - The binding must be EXACTLY a workflow input name. A dotted path into one
//     (`plan.tasks`, `criteria.id`) declares a narrower thing than the whole
//     value, so there is nothing unambiguous to copy, and a phase-output
//     reference has a producer whose contract the consumer is deliberately
//     allowed to restate more narrowly.
//   - The phase input must declare NO schema at all — the zero value. An author
//     who wrote `schema: {description: …}` and forgot the type gets today's
//     `schema.type` finding, because a half-declaration is a mistake rather than
//     an omission.
//   - An explicit schema WINS and is NOT checked against the workflow input's.
//     Narrowing is the point of restating one (a phase that accepts only two of
//     an input's enum values), and the ordinary producer/consumer type check
//     (`schemasCompatible`, `variable.type`) already refuses a restatement that
//     is not a narrowing.
//
// Only the SCHEMA is inherited. `optional:` stays authored, because it states
// what this phase requires rather than what the workflow accepts, and the
// existing `variable.optionality` finding is what holds the two together.
//
// It is applied at PARSE time — the one transition from authored bytes to a
// Workflow — so validation, prompt-template checking, the frozen run snapshot,
// and the definition listing all read one resolved contract. Resolving it in
// more than one consumer is what would let two of them disagree.
//
// Phase inputs are the whole surface. Units and joins declare no inputs of their
// own (`UnitDeclarations` / `JoinDeclarations` copy the phase's), and a call
// edge's `args:` map child input names to caller references with no schema to
// omit, so both inherit through the phase without a rule of their own.
func ApplyInheritedInputSchemas(workflow Workflow) Workflow {
	if len(workflow.Inputs) == 0 || len(workflow.Phases) == 0 {
		return workflow
	}
	var phases []Phase
	for phaseIndex, phase := range workflow.Phases {
		inherited := inheritedPhaseInputs(workflow.Inputs, phase.Inputs)
		if inherited == nil {
			continue
		}
		if phases == nil {
			// Copied only once something actually inherits, so a definition that
			// declares every schema is returned untouched rather than reallocated.
			phases = append([]Phase(nil), workflow.Phases...)
		}
		phases[phaseIndex].Inputs = inherited
	}
	if phases == nil {
		return workflow
	}
	workflow.Phases = phases
	return workflow
}

// inheritedPhaseInputs returns a replacement input map, or nil when nothing in
// this phase inherits. The caller uses that nil to leave the authored workflow
// untouched: this package is pure, and a resolver that rewrote its input would
// make "the definition on disk" and "the definition in hand" two different
// things for every caller that holds both.
func inheritedPhaseInputs(inputs map[string]Variable, declared map[string]Variable) map[string]Variable {
	var resolved map[string]Variable
	for name, variable := range declared {
		if !inheritsInputSchema(inputs, name, variable) {
			continue
		}
		if resolved == nil {
			resolved = make(map[string]Variable, len(declared))
			for existing, value := range declared {
				resolved[existing] = value
			}
		}
		variable.Schema = inputs[name].Schema
		resolved[name] = variable
	}
	return resolved
}

// inheritsInputSchema is the rule itself, in one predicate so the resolver and
// any future reader of it cannot describe the edges differently.
func inheritsInputSchema(inputs map[string]Variable, name string, declared Variable) bool {
	if reservedInputName(name) {
		// Reserved at both ends already; completing a declaration both sites refuse
		// would only make the refusal harder to read.
		return false
	}
	if !reflect.DeepEqual(declared.Schema, JSONSchema{}) {
		return false
	}
	_, bound := inputs[name]
	return bound
}

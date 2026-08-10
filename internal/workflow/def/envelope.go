package def

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const DefaultEnvelopeSizeCap = 64 * 1024

// EnvelopeNarrativeField is the optional control field an element may deliver
// its narrative in. It exists because Codex applies a turn's `outputSchema` to
// EVERY assistant message in that turn, not just the last one: an element under
// a schema physically cannot send prose, so "send your narrative as a message
// before your envelope" is unfollowable there. Carrying the account inside the
// envelope works identically on both providers.
//
// It is a control field, not an output: outputs nest under `outputs`, so an
// author may still declare an output literally named `narrative` and the two
// never meet. It is legal on every status — a done, a question, and a stuck
// element all did work worth an account — and the app strips it before the
// envelope reaches the engine, so no gate, join result, or persisted envelope
// ever carries prose.
const EnvelopeNarrativeField = "narrative"

// EnvelopeMemoryField is the optional control field an element records campaign
// memory in. It exists for the same reason `narrative` does and is lifted at the
// same seam: a `read-only` element runs in a session that denies file writes and
// cannot reliably run a command either, so the CLI verb a writing element uses
// (`agent-overflow memory add`) is not a channel available to it. Carrying the
// notes in the envelope works identically on both providers.
//
// It is a control field, not an output, so an author may still declare an output
// named `memory` and the two never meet. It is legal on every status — an
// element that got stuck learned the most worth recording — and the app strips
// it before the envelope reaches the engine, so no gate, join result, or
// persisted envelope ever carries one.
const EnvelopeMemoryField = "memory"

// envelopeControlFields is the closed set of top-level names an envelope may
// carry. `status` is the only one post-validation requires literally: it is the
// discriminator, and a document without it is not an envelope at all.
//
// The generated schema requires all five, because strict mode has no optional —
// only required-and-nullable (providerschema) — so a provider under a schema
// emits every key and answers null for the ones it has nothing to say with.
// Post-validation reads back what that null MEANT rather than the null itself:
// an absent `question`, `reason`, or `narrative` is the null a provider would
// have sent, and an absent `outputs` is an empty one. The keys a schema forces
// onto a provider are not a debt a hand-written envelope owes — a tool
// command's, and every envelope frozen before a field existed, simply omit
// them, and the branch and declaration rules below judge them identically
// either way.
var (
	envelopeControlFields   = []string{"status", "outputs", "question", "reason", EnvelopeNarrativeField, EnvelopeMemoryField}
	envelopeAllowedFieldSet = func() map[string]bool {
		allowed := make(map[string]bool, len(envelopeControlFields))
		for _, name := range envelopeControlFields {
			allowed[name] = true
		}
		return allowed
	}()
)

// EnvelopeContract is what one control envelope must satisfy: the declared
// outputs, plus the element name diagnostics blame. A phase attempt and a
// fan-out unit produce the same envelope shape from different declarations, so
// schema generation and post-validation are written once against this and never
// twice — a unit cannot drift from the rules a phase is held to.
type EnvelopeContract struct {
	owner   string
	outputs map[string]Variable
	// accounts records that this contract carries the merge-join obligation,
	// and accounted is the exact set of unit ids it is held to. The flag is
	// separate from the slice because a join over ZERO units still owes empty
	// `merged` / `blocked` lists — nil and empty must not read the same.
	accounts  bool
	accounted []string
}

// PhaseEnvelope is the contract a phase attempt's envelope answers. A fan-out
// phase's join answers it too: the join's envelope IS the phase's.
func PhaseEnvelope(phase Phase) EnvelopeContract {
	return EnvelopeContract{owner: fmt.Sprintf("phase %q", phase.ID), outputs: PhaseOutputs(phase)}
}

// JoinEnvelope is the contract a fan-out phase's JOIN answers: the phase's own
// contract, plus — when the join declares `accounts_for_units: true` — the
// exact set of unit ids its `merged` / `blocked` outputs must account for.
//
// The ids are PASSED rather than derived. They come from the store, which this
// package does not know about, and passing them is what keeps the set the join
// is JUDGED against identical to the set it was SHOWN under the reserved
// `units` binding — a verification against some other list would refuse a join
// for failing to mention a unit it was never told existed.
//
// A join that did not opt in gets `PhaseEnvelope(phase)` exactly, so nothing
// about the existing contract changes for the definitions that do not use it.
func JoinEnvelope(phase Phase, unitIDs []string) EnvelopeContract {
	contract := PhaseEnvelope(phase)
	if phase.Join == nil || !phase.Join.AccountsForUnits {
		return contract
	}
	contract.accounts = true
	contract.accounted = append([]string(nil), unitIDs...)
	return contract
}

// UnitEnvelope is the contract one fan-out work unit's envelope answers. A unit
// that declares no outputs gets the control-only envelope: status, question,
// and reason, with nothing under `outputs` to answer — the run still has to
// learn done/question/stuck from it.
func UnitEnvelope(unit Unit) EnvelopeContract {
	return EnvelopeContract{owner: fmt.Sprintf("unit %q", unit.ID), outputs: UnitOutputs(unit)}
}

// Outputs returns the declared outputs. The map is read-only, exactly as
// PhaseOutputs' is — callers read a contract, they never edit one.
func (c EnvelopeContract) Outputs() map[string]Variable { return c.outputs }

// EnvelopeSchema generates the provider-compatible flat control schema for a
// phase. It is the phase-level shorthand for PhaseEnvelope(phase).Schema().
func EnvelopeSchema(phase Phase) ([]byte, error) { return PhaseEnvelope(phase).Schema() }

// Schema generates the provider-compatible flat control schema.
func (c EnvelopeContract) Schema() ([]byte, error) {
	outputProperties := make(map[string]any, len(c.outputs))
	outputRequired := make([]string, 0, len(c.outputs))
	for name, output := range c.outputs {
		if !idPattern.MatchString(name) {
			return nil, fmt.Errorf("%s output %q has invalid name", c.owner, name)
		}
		if findings := validateSchemaDefinition(output.Schema, fmt.Sprintf("%s output %q", c.owner, name)); len(findings) > 0 {
			return nil, fmt.Errorf("%s", findings[0].Error())
		}
		property := providerSchema(output.Schema)
		if output.Optional {
			nullable(property, output.Schema.Type)
		}
		outputProperties[name] = property
		outputRequired = append(outputRequired, name)
	}
	sort.Strings(outputRequired)
	// No "$schema" declaration: Claude's CLI validator has no draft 2020-12
	// meta-schema registered and rejects the whole schema if the URI names one
	// ("no schema with key or ref ..."), which fails the phase before its
	// session starts. Both providers accept the vocabulary below without it.
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"done", "question", "stuck"}},
			"outputs": map[string]any{
				"type":                 []string{"object", "null"},
				"additionalProperties": false,
				"properties":           outputProperties,
				"required":             outputRequired,
			},
			"question": map[string]any{"type": []string{"string", "null"}},
			"reason":   map[string]any{"type": []string{"string", "null"}},
			// The element fills it when it was asked to (a read-only element, which
			// cannot write the narrative file) and answers null otherwise.
			EnvelopeNarrativeField: map[string]any{"type": []string{"string", "null"}},
			EnvelopeMemoryField:    envelopeMemorySchema(),
		},
		"required": []string{EnvelopeMemoryField, EnvelopeNarrativeField, "outputs", "question", "reason", "status"},
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode envelope schema for %s: %w", c.owner, err)
	}
	return encoded, nil
}

// providerSchema converts one authored variable schema into the provider-facing
// JSON Schema vocabulary.
//
// This is an explicit allow-list rather than a marshal of JSONSchema, because
// both providers validate in strict mode and reject unknown keywords: an
// authoring-only hint such as `multiline` reaching the wire fails the phase
// ("strict mode: unknown keyword"). Building the output keyword by keyword
// means a field added to JSONSchema later cannot leak onto the wire by merely
// existing — it has to be opted in here.
//
// Object subschemas are always closed and always require every declared
// property. Codex enforces both ('additionalProperties' is required to be
// supplied and to be false; 'required' ... including every key in properties),
// and a property the author left out of `required` is widened to accept null,
// which is the only way strict mode can express an optional field.
func providerSchema(schema JSONSchema) map[string]any {
	property := map[string]any{"type": schema.Type}
	if len(schema.Enum) > 0 {
		property["enum"] = append([]any(nil), schema.Enum...)
	}
	if schema.Format != "" {
		property["format"] = schema.Format
	}
	if schema.Description != "" {
		property["description"] = schema.Description
	}
	if schema.Minimum != nil {
		property["minimum"] = *schema.Minimum
	}
	if schema.Maximum != nil {
		property["maximum"] = *schema.Maximum
	}
	if schema.MinLength != nil {
		property["minLength"] = *schema.MinLength
	}
	if schema.MaxLength != nil {
		property["maxLength"] = *schema.MaxLength
	}
	if schema.MinItems != nil {
		property["minItems"] = *schema.MinItems
	}
	if schema.MaxItems != nil {
		property["maxItems"] = *schema.MaxItems
	}
	if schema.Type == "array" && schema.Items != nil {
		property["items"] = providerSchema(*schema.Items)
	}
	if schema.Type == "object" {
		required := make(map[string]bool, len(schema.Required))
		for _, name := range schema.Required {
			required[name] = true
		}
		properties := make(map[string]any, len(schema.Properties))
		names := make([]string, 0, len(schema.Properties))
		for name, child := range schema.Properties {
			encoded := providerSchema(child)
			if !required[name] {
				nullable(encoded, child.Type)
			}
			properties[name] = encoded
			names = append(names, name)
		}
		sort.Strings(names)
		property["properties"] = properties
		property["required"] = names
		property["additionalProperties"] = false
	}
	return property
}

// nullable widens an encoded schema so a provider may answer null for it. An
// enum must admit null explicitly: null would satisfy the widened `type` and
// still fail the enum check.
func nullable(encoded map[string]any, baseType string) {
	encoded["type"] = []string{baseType, "null"}
	if enum, ok := encoded["enum"].([]any); ok {
		encoded["enum"] = append(enum, nil)
	}
}

// EnvelopeFinding is a feedback-ready payload error.
type EnvelopeFinding struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// EnvelopeValidationError contains all envelope failures.
type EnvelopeValidationError struct {
	Findings []EnvelopeFinding `json:"findings"`
}

func (e *EnvelopeValidationError) Error() string {
	parts := make([]string, 0, len(e.Findings))
	for _, finding := range e.Findings {
		parts = append(parts, finding.Path+": "+finding.Message)
	}
	return strings.Join(parts, "; ")
}

// ValidateEnvelope applies structural, value, branch, and size validation to a
// phase attempt's envelope. It is the phase-level shorthand for
// PhaseEnvelope(phase).Validate(payload, sizeCap...).
func ValidateEnvelope(phase Phase, payload []byte, sizeCap ...int) error {
	return PhaseEnvelope(phase).Validate(payload, sizeCap...)
}

// Validate applies structural, value, branch, and size validation.
// The optional sizeCap overrides DefaultEnvelopeSizeCap.
func (c EnvelopeContract) Validate(payload []byte, sizeCap ...int) error {
	capBytes := DefaultEnvelopeSizeCap
	if len(sizeCap) > 1 {
		return &EnvelopeValidationError{Findings: []EnvelopeFinding{{Path: "$", Message: "at most one size cap may be supplied"}}}
	}
	if len(sizeCap) == 1 {
		if sizeCap[0] < 1 {
			return &EnvelopeValidationError{Findings: []EnvelopeFinding{{Path: "$", Message: "size cap must be at least 1 byte"}}}
		}
		capBytes = sizeCap[0]
	}
	if len(payload) > capBytes {
		return &EnvelopeValidationError{Findings: []EnvelopeFinding{{Path: "$", Message: fmt.Sprintf("envelope is %d bytes; maximum is %d (write large content to a file and return its path)", len(payload), capBytes)}}}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return &EnvelopeValidationError{Findings: []EnvelopeFinding{{Path: "$", Message: "invalid JSON: " + err.Error()}}}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values are not allowed")
		}
		return &EnvelopeValidationError{Findings: []EnvelopeFinding{{Path: "$", Message: "invalid trailing JSON: " + err.Error()}}}
	}
	var findings []EnvelopeFinding
	for name := range raw {
		if !envelopeAllowedFieldSet[name] {
			findings = append(findings, EnvelopeFinding{Path: "$." + name, Message: "property is not allowed"})
		}
	}
	var status string
	if data, ok := raw["status"]; !ok {
		findings = append(findings, EnvelopeFinding{Path: "$.status", Message: "property is required"})
	} else if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, &status) != nil {
		findings = append(findings, EnvelopeFinding{Path: "$.status", Message: "must be a string"})
	}
	var question, reason, narrative *string
	decodeNullableString(raw["question"], "$.question", &question, &findings)
	decodeNullableString(raw["reason"], "$.reason", &reason, &findings)
	// Type-checked and then deliberately left out of every branch rule below: an
	// element that finished, one that has to ask, and one that got stuck all did
	// work worth an account, so refusing the narrative anywhere would burn the
	// element's single envelope retry on the one field that is never a mistake.
	decodeNullableString(raw[EnvelopeNarrativeField], "$."+EnvelopeNarrativeField, &narrative, &findings)
	// `memory` sits outside the branch rules for the same reason: a done, a
	// question, and a stuck element all learned things worth recording.
	validateEnvelopeMemory(raw[EnvelopeMemoryField], &findings)
	// Absent, null, and unreadable all leave no object to read; only the last has
	// anything said about it, and `outputsReadable` is what keeps the declaration
	// rules below from burying that one finding under one per declaration.
	var outputs map[string]any
	outputsReadable := true
	if data, ok := raw["outputs"]; ok && !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		if err := decoder.Decode(&outputs); err != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.outputs", Message: "must be an object or null"})
			outputs, outputsReadable = nil, false
		}
	}
	// A `done` element owes every declared output whether or not it carried an
	// object, so an absent one is judged as the empty object it is: the envelope
	// is told which deliverables are missing, never that a container is. Under
	// the other statuses nothing is owed until an object is actually carried,
	// which is itself the branch finding below.
	declaredOutputs := c.outputs
	if outputsReadable && (outputs != nil || status == "done") {
		for name := range outputs {
			if _, ok := declaredOutputs[name]; !ok {
				findings = append(findings, EnvelopeFinding{Path: "$.outputs." + name, Message: "property is not allowed"})
			}
		}
		for name, declaration := range declaredOutputs {
			value, ok := outputs[name]
			if !ok {
				findings = append(findings, EnvelopeFinding{Path: "$.outputs." + name, Message: "property is required"})
				continue
			}
			if value == nil {
				if !declaration.Optional && status == "done" {
					findings = append(findings, EnvelopeFinding{Path: "$.outputs." + name, Message: "required output must not be null when status is done"})
				}
				continue
			}
			for _, message := range validateJSONValue(declaration.Schema, value, "$.outputs."+name) {
				findings = append(findings, EnvelopeFinding{Path: "$.outputs." + name, Message: strings.TrimPrefix(message, "$.outputs."+name+" ")})
			}
		}
	}
	// The merge-join obligation applies to a `done` envelope alone: a join that
	// asks a question or gets stuck produced no result to account for, and
	// demanding the lists there would refuse the very envelope that says the
	// join could not decide.
	if c.accounts && status == "done" && outputsReadable {
		findings = append(findings, c.accountingFindings(outputs)...)
	}
	switch status {
	case "done":
		if question != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.question", Message: "must be null when status is done"})
		}
		if reason != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.reason", Message: "must be null when status is done"})
		}
	case "question":
		if question == nil || strings.TrimSpace(*question) == "" {
			findings = append(findings, EnvelopeFinding{Path: "$.question", Message: "must be a non-empty string when status is question"})
		}
		if outputs != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.outputs", Message: "must be null when status is question"})
		}
		if reason != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.reason", Message: "must be null when status is question"})
		}
	case "stuck":
		if reason == nil || strings.TrimSpace(*reason) == "" {
			findings = append(findings, EnvelopeFinding{Path: "$.reason", Message: "must be a non-empty string when status is stuck"})
		}
		if outputs != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.outputs", Message: "must be null when status is stuck"})
		}
		if question != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.question", Message: "must be null when status is stuck"})
		}
	default:
		if status != "" {
			findings = append(findings, EnvelopeFinding{Path: "$.status", Message: "must be done, question, or stuck"})
		}
	}
	if len(findings) > 0 {
		sort.SliceStable(findings, func(i, j int) bool { return findings[i].Path < findings[j].Path })
		return &EnvelopeValidationError{Findings: findings}
	}
	return nil
}

// SplitEnvelopeNarrative separates the authored narrative from a control
// envelope, returning the narrative text and the envelope with the field
// removed. It is the one seam that keeps prose out of everything downstream:
// gate evaluation, a join's `units` results, call synthesis, and the persisted
// attempt envelope all read the returned payload.
//
// Anything that does not decode as a JSON object, and any envelope that carries
// no narrative, is returned byte-for-byte unchanged — this is not a validator,
// and a payload that failed post-validation must reach the human as it was
// written. A narrative that is present but not a string strips the same way and
// yields no text: post-validation has already reported the type finding, and
// leaving the field in place would hand the engine the prose slot it exists to
// keep empty.
func SplitEnvelopeNarrative(payload json.RawMessage) (string, json.RawMessage) {
	var envelope map[string]json.RawMessage
	if len(payload) == 0 || json.Unmarshal(payload, &envelope) != nil {
		return "", payload
	}
	raw, present := envelope[EnvelopeNarrativeField]
	if !present {
		return "", payload
	}
	delete(envelope, EnvelopeNarrativeField)
	stripped, err := json.Marshal(envelope)
	if err != nil {
		return "", payload
	}
	var narrative string
	if json.Unmarshal(raw, &narrative) != nil {
		return "", stripped
	}
	return narrative, stripped
}

// EnvelopeAccount reads the human-readable account out of a document that is
// SHAPED like a control envelope, and reports whether it was one at all.
//
// "Shaped like" is a top-level `status` key, which is the identity every
// envelope has and no narrative does. It is deliberately weaker than the
// document-identity test narrative recovery applies to the accepted envelope:
// under Codex every assistant message of a schema-constrained turn is envelope
// JSON, so recovery has to recognize the ones that are NOT this turn's accepted
// envelope and lift their account rather than writing raw JSON into a file a
// human is meant to read. The account is the narrative if there is one, else the
// stuck reason; an envelope-shaped document with neither carries no account.
func EnvelopeAccount(payload []byte) (string, bool) {
	var envelope map[string]json.RawMessage
	if len(payload) == 0 || json.Unmarshal(payload, &envelope) != nil {
		return "", false
	}
	if _, shaped := envelope["status"]; !shaped {
		return "", false
	}
	for _, field := range []string{EnvelopeNarrativeField, "reason"} {
		var value string
		if json.Unmarshal(envelope[field], &value) == nil && strings.TrimSpace(value) != "" {
			return value, true
		}
	}
	return "", true
}

func decodeNullableString(data json.RawMessage, path string, target **string, findings *[]EnvelopeFinding) {
	if data == nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		*findings = append(*findings, EnvelopeFinding{Path: path, Message: "must be a string or null"})
		return
	}
	*target = &value
}

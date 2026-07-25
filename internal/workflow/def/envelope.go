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

// EnvelopeContract is what one control envelope must satisfy: the declared
// outputs, plus the element name diagnostics blame. A phase attempt and a
// fan-out unit produce the same envelope shape from different declarations, so
// schema generation and post-validation are written once against this and never
// twice — a unit cannot drift from the rules a phase is held to.
type EnvelopeContract struct {
	owner   string
	outputs map[string]Variable
}

// PhaseEnvelope is the contract a phase attempt's envelope answers. A fan-out
// phase's join answers it too: the join's envelope IS the phase's.
func PhaseEnvelope(phase Phase) EnvelopeContract {
	return EnvelopeContract{owner: fmt.Sprintf("phase %q", phase.ID), outputs: PhaseOutputs(phase)}
}

// UnitEnvelope is the contract one fan-out work unit's envelope answers. A unit
// that declares no outputs gets the control-only envelope: status, question,
// and reason, with `outputs` present but always null.
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
		},
		"required": []string{"outputs", "question", "reason", "status"},
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
	allowed := map[string]bool{"status": true, "outputs": true, "question": true, "reason": true}
	for name := range raw {
		if !allowed[name] {
			findings = append(findings, EnvelopeFinding{Path: "$." + name, Message: "property is not allowed"})
		}
	}
	for _, name := range []string{"status", "outputs", "question", "reason"} {
		if _, ok := raw[name]; !ok {
			findings = append(findings, EnvelopeFinding{Path: "$." + name, Message: "property is required"})
		}
	}
	var status string
	if data, ok := raw["status"]; ok {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, &status) != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.status", Message: "must be a string"})
		}
	}
	var question, reason *string
	decodeNullableString(raw["question"], "$.question", &question, &findings)
	decodeNullableString(raw["reason"], "$.reason", &reason, &findings)
	var outputs map[string]any
	if data, ok := raw["outputs"]; ok && !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		if err := decoder.Decode(&outputs); err != nil {
			findings = append(findings, EnvelopeFinding{Path: "$.outputs", Message: "must be an object or null"})
		}
	}
	declaredOutputs := c.outputs
	if outputs != nil {
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
	switch status {
	case "done":
		if outputs == nil {
			findings = append(findings, EnvelopeFinding{Path: "$.outputs", Message: "must be non-null when status is done"})
		}
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

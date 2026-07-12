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

// EnvelopeSchema generates the provider-compatible flat control schema.
func EnvelopeSchema(phase Phase) ([]byte, error) {
	outputProperties := make(map[string]any, len(phase.Outputs))
	outputRequired := make([]string, 0, len(phase.Outputs))
	for name, output := range phase.Outputs {
		if !idPattern.MatchString(name) {
			return nil, fmt.Errorf("phase %q output %q has invalid name", phase.ID, name)
		}
		if findings := validateSchemaDefinition(output.Schema, fmt.Sprintf("phase %q output %q", phase.ID, name)); len(findings) > 0 {
			return nil, fmt.Errorf("%s", findings[0].Error())
		}
		property, err := schemaMap(output.Schema)
		if err != nil {
			return nil, fmt.Errorf("encode phase %q output %q schema: %w", phase.ID, name, err)
		}
		if output.Optional {
			property["type"] = []string{output.Schema.Type, "null"}
			if enum, ok := property["enum"].([]any); ok {
				property["enum"] = append(enum, nil)
			}
		}
		outputProperties[name] = property
		outputRequired = append(outputRequired, name)
	}
	sort.Strings(outputRequired)
	schema := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
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
		return nil, fmt.Errorf("encode envelope schema for phase %q: %w", phase.ID, err)
	}
	return encoded, nil
}

func schemaMap(schema JSONSchema) (map[string]any, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("decode marshaled schema: %w", err)
	}
	return result, nil
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

// ValidateEnvelope applies structural, value, branch, and size validation.
// The optional sizeCap overrides DefaultEnvelopeSizeCap.
func ValidateEnvelope(phase Phase, payload []byte, sizeCap ...int) error {
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
	if outputs != nil {
		for name := range outputs {
			if _, ok := phase.Outputs[name]; !ok {
				findings = append(findings, EnvelopeFinding{Path: "$.outputs." + name, Message: "property is not allowed"})
			}
		}
		for name, declaration := range phase.Outputs {
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

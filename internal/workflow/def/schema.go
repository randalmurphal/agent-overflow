package def

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
)

// JSONSchema is the supported JSON-Schema fragment vocabulary for variables.
type JSONSchema struct {
	Type                 string                `yaml:"type" json:"type"`
	Enum                 []any                 `yaml:"enum,omitempty" json:"enum,omitempty"`
	Format               string                `yaml:"format,omitempty" json:"format,omitempty"`
	Multiline            bool                  `yaml:"multiline,omitempty" json:"multiline,omitempty"`
	Description          string                `yaml:"description,omitempty" json:"description,omitempty"`
	Items                *JSONSchema           `yaml:"items,omitempty" json:"items,omitempty"`
	Properties           map[string]JSONSchema `yaml:"properties,omitempty" json:"properties,omitempty"`
	Required             []string              `yaml:"required,omitempty" json:"required,omitempty"`
	AdditionalProperties *bool                 `yaml:"additionalProperties,omitempty" json:"additionalProperties,omitempty"`
	Minimum              *float64              `yaml:"minimum,omitempty" json:"minimum,omitempty"`
	Maximum              *float64              `yaml:"maximum,omitempty" json:"maximum,omitempty"`
	MinLength            *int                  `yaml:"minLength,omitempty" json:"minLength,omitempty"`
	MaxLength            *int                  `yaml:"maxLength,omitempty" json:"maxLength,omitempty"`
	MinItems             *int                  `yaml:"minItems,omitempty" json:"minItems,omitempty"`
	MaxItems             *int                  `yaml:"maxItems,omitempty" json:"maxItems,omitempty"`
}

//go:embed workflow.schema.json
var authoringSchema []byte

// AuthoringSchema returns an isolated copy of the published authoring schema.
func AuthoringSchema() []byte { return append([]byte(nil), authoringSchema...) }

// validateVariableDeclaration checks one authored variable whole: its schema,
// plus the fields that are legal only on a reserved binding. It is what every
// ordinary declaration site calls, so `window:` — a history binding's field —
// cannot sit unread on a workflow input, a phase output, or a unit output.
func validateVariableDeclaration(variable Variable, element string) []Finding {
	findings := validateSchemaDefinition(variable.Schema, element)
	if variable.Window != 0 {
		findings = append(findings, finding("variable.window", element, fmt.Sprintf("window is valid only on a %s<phase> input binding", HistoryPrefix)))
	}
	return findings
}

func validateSchemaDefinition(schema JSONSchema, element string) []Finding {
	var findings []Finding
	switch schema.Type {
	case "string", "number", "boolean", "array", "object":
	case "":
		findings = append(findings, finding("schema.type", element, "schema type is required"))
	default:
		findings = append(findings, finding("schema.type", element, fmt.Sprintf("unsupported schema type %q", schema.Type)))
	}
	if schema.Type == "array" && schema.Items == nil {
		findings = append(findings, finding("schema.items", element, "array schema requires items"))
	}
	if schema.Type != "array" && schema.Items != nil {
		findings = append(findings, finding("schema.items", element, "items is valid only for array schemas"))
	}
	if schema.Items != nil {
		findings = append(findings, validateSchemaDefinition(*schema.Items, element+".items")...)
	}
	if schema.Type != "object" && (len(schema.Properties) != 0 || len(schema.Required) != 0) {
		findings = append(findings, finding("schema.object", element, "properties and required are valid only for object schemas"))
	}
	if schema.Type != "object" && schema.AdditionalProperties != nil {
		findings = append(findings, finding("schema.object", element, "additionalProperties is valid only for object schemas"))
	}
	if schema.Type != "number" && (schema.Minimum != nil || schema.Maximum != nil) {
		findings = append(findings, finding("schema.bounds", element, "minimum and maximum are valid only for number schemas"))
	}
	if schema.Minimum != nil && schema.Maximum != nil && *schema.Minimum > *schema.Maximum {
		findings = append(findings, finding("schema.bounds", element, "minimum must not exceed maximum"))
	}
	if schema.Type != "string" && (schema.MinLength != nil || schema.MaxLength != nil) {
		findings = append(findings, finding("schema.length", element, "minLength and maxLength are valid only for string schemas"))
	}
	if schema.Type != "string" && schema.Multiline {
		findings = append(findings, finding("schema.multiline", element, "multiline is valid only for string schemas"))
	}
	if schema.Type != "array" && (schema.MinItems != nil || schema.MaxItems != nil) {
		findings = append(findings, finding("schema.items", element, "minItems and maxItems are valid only for array schemas"))
	}
	if invalidRange(schema.MinLength, schema.MaxLength) {
		findings = append(findings, finding("schema.length", element, "length bounds must be non-negative and minLength must not exceed maxLength"))
	}
	if invalidRange(schema.MinItems, schema.MaxItems) {
		findings = append(findings, finding("schema.items", element, "item bounds must be non-negative and minItems must not exceed maxItems"))
	}
	seenEnumValues := make(map[string]struct{}, len(schema.Enum))
	for _, value := range schema.Enum {
		if value == nil || !literalMatches(schema, value) {
			findings = append(findings, finding("schema.enum", element, fmt.Sprintf("enum value %v does not match type %q", value, schema.Type)))
			continue
		}
		// A duplicate enum value is a dead line: no supplied input can ever
		// mean the second occurrence, and downstream renderers keying on the
		// values (the intake form's <select>) have to repair the repeat.
		// Refused here so the author learns at validation, not by watching.
		key := enumValueKey(value)
		if _, duplicate := seenEnumValues[key]; duplicate {
			findings = append(findings, finding("schema.enum", element, fmt.Sprintf("duplicate enum value %v", value)))
			continue
		}
		seenEnumValues[key] = struct{}{}
	}
	for name, property := range schema.Properties {
		findings = append(findings, validateSchemaDefinition(property, element+".properties."+name)...)
	}
	for _, name := range schema.Required {
		if _, ok := schema.Properties[name]; !ok {
			findings = append(findings, finding("schema.required", element, fmt.Sprintf("required property %q is not declared", name)))
		}
	}
	return findings
}

func invalidRange(minimum, maximum *int) bool {
	return minimum != nil && *minimum < 0 ||
		maximum != nil && *maximum < 0 ||
		minimum != nil && maximum != nil && *minimum > *maximum
}

func validateJSONValue(schema JSONSchema, value any, path string) []string {
	if value == nil {
		return []string{path + " must not be null"}
	}
	var errors []string
	switch schema.Type {
	case "string":
		v, ok := value.(string)
		if !ok {
			return []string{path + " must be a string"}
		}
		if schema.MinLength != nil && len([]rune(v)) < *schema.MinLength {
			errors = append(errors, fmt.Sprintf("%s must contain at least %d characters", path, *schema.MinLength))
		}
		if schema.MaxLength != nil && len([]rune(v)) > *schema.MaxLength {
			errors = append(errors, fmt.Sprintf("%s must contain at most %d characters", path, *schema.MaxLength))
		}
	case "number":
		v, ok := value.(float64)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			return []string{path + " must be a finite number"}
		}
		if schema.Minimum != nil && v < *schema.Minimum {
			errors = append(errors, fmt.Sprintf("%s must be >= %v", path, *schema.Minimum))
		}
		if schema.Maximum != nil && v > *schema.Maximum {
			errors = append(errors, fmt.Sprintf("%s must be <= %v", path, *schema.Maximum))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return []string{path + " must be a boolean"}
		}
	case "array":
		v, ok := value.([]any)
		if !ok {
			return []string{path + " must be an array"}
		}
		if schema.MinItems != nil && len(v) < *schema.MinItems {
			errors = append(errors, fmt.Sprintf("%s must contain at least %d items", path, *schema.MinItems))
		}
		if schema.MaxItems != nil && len(v) > *schema.MaxItems {
			errors = append(errors, fmt.Sprintf("%s must contain at most %d items", path, *schema.MaxItems))
		}
		if schema.Items != nil {
			for i, item := range v {
				errors = append(errors, validateJSONValue(*schema.Items, item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	case "object":
		v, ok := value.(map[string]any)
		if !ok {
			return []string{path + " must be an object"}
		}
		for _, required := range schema.Required {
			if child, ok := v[required]; !ok || child == nil {
				errors = append(errors, fmt.Sprintf("%s.%s is required", path, required))
			}
		}
		for name, child := range v {
			property, declared := schema.Properties[name]
			if !declared {
				if schema.AdditionalProperties != nil && !*schema.AdditionalProperties {
					errors = append(errors, fmt.Sprintf("%s.%s is not allowed", path, name))
				}
				continue
			}
			if child == nil {
				// A null here is either a required property, already reported
				// once by the loop above, or an optional one. Optional object
				// properties arrive as null rather than absent because strict
				// mode makes providers emit every declared key, so null is how
				// they spell "absent" — matching how optional top-level outputs
				// are read in ValidateEnvelope.
				continue
			}
			errors = append(errors, validateJSONValue(property, child, path+"."+name)...)
		}
	}
	if len(schema.Enum) > 0 {
		matched := false
		for _, allowed := range schema.Enum {
			if reflect.DeepEqual(value, allowed) || numericallyEqual(value, allowed) {
				matched = true
				break
			}
		}
		if !matched {
			encoded, _ := json.Marshal(schema.Enum)
			errors = append(errors, fmt.Sprintf("%s must be one of %s", path, encoded))
		}
	}
	return errors
}

// ValidateInputs typechecks one run's supplied workflow inputs against the
// same schema validator used for authored variable contracts. Undeclared
// inputs are rejected so a typo cannot be persisted as an inert seed.
//
// It is the WHOLE-object answer: every required input has to be present. A
// caller changing one value on a run that already started asks ValidateInput
// instead, which is the per-value half this shares — one validator, two
// questions, so a seed accepted at start and a seed accepted later can never be
// judged by different rules.
func ValidateInputs(workflow Workflow, inputs map[string]any) []string {
	// Both loops walk a SORTED name list, so a caller printing several refusals
	// prints them in the same order twice: map iteration would otherwise reshuffle
	// one refusal's text between two identical requests, and the missing-required
	// half is the one a first run is most likely to hit several of at once.
	errors := make([]string, 0)
	for _, name := range sortedNames(workflow.Inputs) {
		if variable := workflow.Inputs[name]; variable.Optional {
			continue
		}
		if _, present := inputs[name]; !present {
			errors = append(errors, fmt.Sprintf("$.seeds.%s is required", name))
		}
	}
	for _, name := range sortedNames(inputs) {
		errors = append(errors, ValidateInput(workflow, name, inputs[name])...)
	}
	return errors
}

// sortedNames is the deterministic iteration order both halves of ValidateInputs
// take over their maps.
func sortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidateInput typechecks one supplied input value against the workflow's
// declaration of it. An undeclared name is refused here rather than left to the
// caller to notice: a value the workflow never declared is inert wherever it is
// written, so no caller of this function has a legitimate reason to accept one.
func ValidateInput(workflow Workflow, name string, value any) []string {
	variable, declared := workflow.Inputs[name]
	if !declared {
		return []string{fmt.Sprintf("$.seeds.%s is not declared by workflow %q", name, workflow.ID)}
	}
	return validateJSONValue(variable.Schema, value, "$.seeds."+name)
}

// enumValueKey renders one TYPE-VALID enum value into the identity duplicate
// detection compares. Numbers normalize through the same float64 conversion
// predicate comparison uses (numberAsFloat64), so `1` and `1.0` are one value;
// array/object literals key by their JSON encoding, since Go slices and maps
// are not comparable. Values of different schema types cannot cross-collide:
// literalMatches has already pinned every value reaching this to the schema's
// one declared type.
func enumValueKey(value any) string {
	if number, ok := numberAsFloat64(value); ok {
		return fmt.Sprintf("%g", number)
	}
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return fmt.Sprintf("%t", typed)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		// Unmarshalable values cannot come out of YAML/JSON decoding; fall
		// back to the fmt rendering rather than treating them as one value.
		return fmt.Sprintf("%#v", value)
	}
	return string(encoded)
}

func numericallyEqual(left, right any) bool {
	leftNumber, leftOK := numberAsFloat64(left)
	rightNumber, rightOK := numberAsFloat64(right)
	return leftOK && rightOK && leftNumber == rightNumber
}

func numberAsFloat64(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float32:
		return float64(number), true
	case float64:
		return number, true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

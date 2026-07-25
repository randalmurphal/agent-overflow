package def

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
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
	for _, value := range schema.Enum {
		if value == nil || !literalMatches(schema, value) {
			findings = append(findings, finding("schema.enum", element, fmt.Sprintf("enum value %v does not match type %q", value, schema.Type)))
		}
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
func ValidateInputs(workflow Workflow, inputs map[string]any) []string {
	errors := make([]string, 0)
	for name, variable := range workflow.Inputs {
		value, present := inputs[name]
		if !present {
			if !variable.Optional {
				errors = append(errors, fmt.Sprintf("$.seeds.%s is required", name))
			}
			continue
		}
		errors = append(errors, validateJSONValue(variable.Schema, value, "$.seeds."+name)...)
	}
	for name := range inputs {
		if _, declared := workflow.Inputs[name]; !declared {
			errors = append(errors, fmt.Sprintf("$.seeds.%s is not declared by workflow %q", name, workflow.ID))
		}
	}
	return errors
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

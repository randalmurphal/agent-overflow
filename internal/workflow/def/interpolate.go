package def

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	templateToken = regexp.MustCompile(`{{[^{}]*}}`)
	validRef      = regexp.MustCompile(`^[a-z0-9-]+(?:\.[a-z0-9-]+)*$`)
)

// ValidateTemplate reports malformed or undeclared interpolation references.
func ValidateTemplate(text string, declarations map[string]Variable) []string {
	var errors []string
	for _, token := range templateToken.FindAllString(text, -1) {
		ref := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(token, "{{"), "}}"))
		if !validRef.MatchString(ref) {
			errors = append(errors, fmt.Sprintf("template token %q is not a valid variable reference", token))
			continue
		}
		if _, ok := declarationForPath(declarations, ref); !ok {
			errors = append(errors, fmt.Sprintf("template reference %q is not declared", ref))
		}
	}
	stripped := templateToken.ReplaceAllString(text, "")
	if strings.Contains(stripped, "{{") || strings.Contains(stripped, "}}") {
		errors = append(errors, "template contains malformed braces")
	}
	return errors
}

// Interpolate substitutes declared values once. Inserted text is never scanned.
func Interpolate(text string, declarations map[string]Variable, values map[string]any) (string, error) {
	if errors := ValidateTemplate(text, declarations); len(errors) > 0 {
		return "", fmt.Errorf("interpolate template: %s", strings.Join(errors, "; "))
	}
	var interpolationErr error
	result := templateToken.ReplaceAllStringFunc(text, func(token string) string {
		ref := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(token, "{{"), "}}"))
		declaration, _ := declarationForPath(declarations, ref)
		value, present := valueAtPath(values, ref)
		if !present || value == nil {
			if declaration.Optional {
				return "(not provided)"
			}
			interpolationErr = fmt.Errorf("required variable %q is not provided", ref)
			return token
		}
		if text, ok := value.(string); ok {
			return text
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			interpolationErr = fmt.Errorf("encode variable %q: %w", ref, err)
			return token
		}
		return string(encoded)
	})
	if interpolationErr != nil {
		return "", interpolationErr
	}
	return result, nil
}

func declarationForPath(declarations map[string]Variable, ref string) (Variable, bool) {
	if declaration, ok := declarations[ref]; ok {
		return declaration, true
	}
	parts := strings.Split(ref, ".")
	for i := len(parts) - 1; i > 0; i-- {
		base := strings.Join(parts[:i], ".")
		declaration, ok := declarations[base]
		if !ok {
			continue
		}
		schema := declaration.Schema
		optional := declaration.Optional
		for _, field := range parts[i:] {
			if schema.Type != "object" {
				return Variable{}, false
			}
			child, ok := schema.Properties[field]
			if !ok {
				return Variable{}, false
			}
			if !contains(schema.Required, field) {
				optional = true
			}
			schema = child
		}
		return Variable{Schema: schema, Optional: optional}, true
	}
	return Variable{}, false
}

func valueAtPath(values map[string]any, ref string) (any, bool) {
	if value, ok := values[ref]; ok {
		return value, true
	}
	parts := strings.Split(ref, ".")
	for i := len(parts) - 1; i > 0; i-- {
		value, ok := values[strings.Join(parts[:i], ".")]
		if !ok {
			continue
		}
		for _, field := range parts[i:] {
			object, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			value, ok = object[field]
			if !ok {
				return nil, false
			}
		}
		return value, true
	}
	return nil, false
}

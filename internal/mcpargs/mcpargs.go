// Package mcpargs decodes the arguments of one MCP tool call under the
// closed schema every built-in tool server shares.
//
// It is a leaf: the tool servers depend on it, and it depends on nothing
// of theirs. The transport that carries a call (internal/threadmcp) accepts
// client metadata and protocol extensions in the envelope, but the
// arguments themselves are closed, so a typo in a field name is a refusal
// the model can act on rather than a silently ignored value.
//
// Every message returned here is written for the model: it names the field
// and the type the schema wants, and never echoes the value supplied.
package mcpargs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// maxFieldNameBytes bounds an unknown field name quoted back to the model,
// so a caller cannot use a refusal to echo a payload of its own.
const maxFieldNameBytes = 128

// Decode parses one tool call's arguments into target. Absent arguments
// decode as an empty object; an unknown field, a wrong type or trailing
// JSON is refused.
func Decode(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("Tool arguments must be a JSON object with the fields listed in the tool schema.")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var mismatch *json.UnmarshalTypeError
		if errors.As(err, &mismatch) {
			return fmt.Errorf("Argument %q must be %s. Check the tool schema.", mismatch.Field, expectedKind(mismatch.Type.Kind()))
		}
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			if len(field) > maxFieldNameBytes {
				field = field[:maxFieldNameBytes] + "…"
			}
			return fmt.Errorf("Unknown argument %s. Use only fields listed in the tool schema.", field)
		}
		return fmt.Errorf("Invalid argument JSON. Supply one object matching the tool schema.")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("Tool arguments contain extra JSON. Supply exactly one object.")
	}
	return nil
}

// expectedKind names a JSON type the way a schema does, so a refusal
// speaks the model's vocabulary rather than Go's.
func expectedKind(kind reflect.Kind) string {
	switch kind {
	case reflect.Int, reflect.Int64:
		return "an integer"
	case reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Struct, reflect.Map:
		return "an object"
	default:
		return kind.String()
	}
}

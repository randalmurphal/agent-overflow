// Package providerschema checks a structured-output JSON Schema against the
// strict-mode rules both provider CLIs enforce.
//
// Claude Code (--json-schema) and the Codex app-server (outputSchema) each
// validate the schema before the turn starts and fail the whole phase when it
// breaks one of the rules below. Every rule here corresponds to an observed
// hard failure, verified against Claude Code 2.1.219 and codex-cli 0.145.0:
//
//	$schema draft 2020-12  claude: "--json-schema is not a valid JSON Schema:
//	                       no schema with key or ref \"https://json-schema.org/
//	                       draft/2020-12/schema\""
//	unknown keyword        claude: "strict mode: unknown keyword: \"multiline\""
//	open object            codex 400 invalid_json_schema: "'additionalProperties'
//	                       is required to be supplied and to be false"
//	partial required       codex 400 invalid_json_schema: "'required' is required
//	                       to be supplied and to be an array including every key
//	                       in properties"
//
// The rules are the union of both CLIs' demands, so a schema that passes here
// is accepted by either provider.
package providerschema

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Draft07 is the only $schema declaration verified to pass both CLIs. Omitting
// the keyword entirely also works and is what the workflow envelope generator
// does; any other value is rejected here as unverified rather than assumed.
const Draft07 = "http://json-schema.org/draft-07/schema#"

// vocabulary is the keyword set both CLIs accept. Claude runs its validator in
// strict mode, so an unlisted keyword is a hard failure rather than an ignored
// annotation — authoring-only hints must be stripped before generation.
var vocabulary = map[string]bool{
	"type": true, "enum": true, "format": true, "description": true,
	"items": true, "properties": true, "required": true, "additionalProperties": true,
	"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
	"minItems": true, "maxItems": true,
}

// Violation is one reason a provider CLI would reject the schema.
type Violation struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (v Violation) Error() string { return v.Path + ": " + v.Message }

// Validate reports every rule the schema breaks. An empty result means both
// provider CLIs accept it.
func Validate(schema []byte) []Violation {
	return validate(schema, true)
}

// ValidateClaude reports only the rules CLAUDE's `--json-schema` validator
// enforces: the `$schema` draft, the strict-mode keyword vocabulary, and a
// declared `type` on every node. The two object rules Validate also applies
// (`additionalProperties: false`, a `required` naming every property) are
// codex-400 rejections; Claude accepts an open object and a partial
// `required`, and the package doc records that tolerance.
//
// It exists for a schema that is CLAUDE'S ALONE and can never be re-targeted:
// `internal/commitmsg` and `internal/threadtitle` each keep a separate Claude
// constant from their Codex one, and the Claude commit-message schema
// deliberately requires only `subject` so an empty body stays legal. Judging
// those against the union would report a working invocation as broken.
//
// Anything sent to BOTH providers — every generated workflow envelope schema —
// must still go through Validate. Reaching for this function to quiet a
// violation on a shared schema re-opens the exact class of defect the union
// exists to catch.
func ValidateClaude(schema []byte) []Violation {
	return validate(schema, false)
}

func validate(schema []byte, strictObjects bool) []Violation {
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return []Violation{{Path: "$", Message: "invalid JSON: " + err.Error()}}
	}
	var violations []Violation
	if declared, ok := root["$schema"]; ok && declared != Draft07 {
		violations = append(violations, Violation{
			Path:    "$",
			Message: fmt.Sprintf("$schema %v is not accepted by every provider; omit it or declare %q", declared, Draft07),
		})
	}
	return append(violations, walk(root, "$", strictObjects)...)
}

func walk(node map[string]any, path string, strictObjects bool) []Violation {
	var violations []Violation
	for _, keyword := range sortedKeys(node) {
		if path == "$" && keyword == "$schema" {
			continue
		}
		if !vocabulary[keyword] {
			violations = append(violations, Violation{
				Path:    path,
				Message: fmt.Sprintf("keyword %q is outside the vocabulary both CLIs accept", keyword),
			})
		}
	}
	if _, declared := node["type"]; !declared {
		violations = append(violations, Violation{Path: path, Message: "schema must declare a type"})
	}
	if items, ok := node["items"].(map[string]any); ok {
		violations = append(violations, walk(items, path+".items", strictObjects)...)
	}
	properties, _ := node["properties"].(map[string]any)
	for _, name := range sortedKeys(properties) {
		if child, ok := properties[name].(map[string]any); ok {
			violations = append(violations, walk(child, path+".properties."+name, strictObjects)...)
		}
	}
	if !strictObjects || !declaresObject(node["type"]) {
		return violations
	}
	if closed, ok := node["additionalProperties"].(bool); !ok || closed {
		violations = append(violations, Violation{
			Path:    path,
			Message: "object must set additionalProperties to false",
		})
	}
	required := make(map[string]bool)
	if list, ok := node["required"].([]any); ok {
		for _, name := range list {
			if text, ok := name.(string); ok {
				required[text] = true
			}
		}
	}
	for _, name := range sortedKeys(properties) {
		if !required[name] {
			violations = append(violations, Violation{
				Path:    path,
				Message: fmt.Sprintf("property %q must be listed in required (express optional as a nullable type)", name),
			})
		}
	}
	return violations
}

// declaresObject reports whether a type declaration covers objects, including
// the ["object","null"] union form used for optional values.
func declaresObject(declared any) bool {
	switch value := declared.(type) {
	case string:
		return value == "object"
	case []any:
		for _, entry := range value {
			if entry == "object" {
				return true
			}
		}
	}
	return false
}

func sortedKeys(node map[string]any) []string {
	names := make([]string, 0, len(node))
	for name := range node {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

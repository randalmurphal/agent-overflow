package def

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseValidFixtureAndPublishedSchemaStayInLockstep(t *testing.T) {
	workflow, err := ParseFile("testdata/valid/workflow.yaml")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if workflow.ID != "build-review" || len(workflow.Phases) != 3 {
		t.Fatalf("unexpected fixture: id=%q phases=%d", workflow.ID, len(workflow.Phases))
	}
	var published map[string]any
	if err := json.Unmarshal(AuthoringSchema(), &published); err != nil {
		t.Fatalf("published schema is invalid JSON: %v", err)
	}
	properties := published["properties"].(map[string]any)
	for _, key := range []string{"id", "name", "inputs", "outputs", "phases", "default_step_mode", "cleanup"} {
		if _, ok := properties[key]; !ok {
			t.Errorf("published schema lacks top-level key %q used by Go format", key)
		}
	}
	defs := published["$defs"].(map[string]any)
	phase := defs["phase"].(map[string]any)["properties"].(map[string]any)
	for _, key := range []string{"id", "driver", "shape", "provider", "model", "prompt", "watchdog", "inputs", "outputs", "check", "command", "resources", "commands", "capabilities", "mcp", "access", "fan_out", "join", "gate"} {
		if _, ok := phase[key]; !ok {
			t.Errorf("published schema lacks phase key %q used by Go format", key)
		}
	}
	fixtureData, err := os.ReadFile("testdata/valid/workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var authored any
	if err := yaml.Unmarshal(fixtureData, &authored); err != nil {
		t.Fatal(err)
	}
	if err := validateFixtureSchema(authored, published, published, "$"); err != nil {
		t.Fatalf("fixture does not validate against published schema: %v", err)
	}
}

func validateFixtureSchema(value any, schema, root map[string]any, path string) error {
	if ref, ok := schema["$ref"].(string); ok {
		const prefix = "#/$defs/"
		if !strings.HasPrefix(ref, prefix) {
			return fmt.Errorf("%s: unsupported schema reference %q", path, ref)
		}
		definition, ok := root["$defs"].(map[string]any)[strings.TrimPrefix(ref, prefix)].(map[string]any)
		if !ok {
			return fmt.Errorf("%s: unresolved schema reference %q", path, ref)
		}
		return validateFixtureSchema(value, definition, root, path)
	}
	if allowed, ok := schema["enum"].([]any); ok {
		matched := false
		for _, candidate := range allowed {
			if reflect.DeepEqual(value, candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value %v is not in enum %v", path, value, allowed)
		}
	}
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object, got %T", path, value)
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, required := range stringValues(schema["required"]) {
			if _, exists := object[required]; !exists {
				return fmt.Errorf("%s.%s: required property is missing", path, required)
			}
		}
		for name, child := range object {
			if property, exists := properties[name].(map[string]any); exists {
				if err := validateFixtureSchema(child, property, root, path+"."+name); err != nil {
					return err
				}
				continue
			}
			switch additional := schema["additionalProperties"].(type) {
			case bool:
				if !additional {
					return fmt.Errorf("%s.%s: additional property is not allowed", path, name)
				}
			case map[string]any:
				if err := validateFixtureSchema(child, additional, root, path+"."+name); err != nil {
					return err
				}
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array, got %T", path, value)
		}
		if minimum, ok := schema["minItems"].(float64); ok && len(array) < int(minimum) {
			return fmt.Errorf("%s: expected at least %d items", path, int(minimum))
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for index, item := range array {
				if err := validateFixtureSchema(item, itemSchema, root, fmt.Sprintf("%s[%d]", path, index)); err != nil {
					return err
				}
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s: expected string, got %T", path, value)
		}
		if minimum, ok := schema["minLength"].(float64); ok && len([]rune(text)) < int(minimum) {
			return fmt.Errorf("%s: string is shorter than %d", path, int(minimum))
		}
		if pattern, ok := schema["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(text) {
			return fmt.Errorf("%s: string does not match %q", path, pattern)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: expected boolean, got %T", path, value)
		}
	case "integer":
		if _, ok := value.(int); !ok {
			return fmt.Errorf("%s: expected integer, got %T", path, value)
		}
	case "number":
		switch value.(type) {
		case int, float64:
		default:
			return fmt.Errorf("%s: expected number, got %T", path, value)
		}
	}
	return nil
}

func stringValues(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := ParseFile("testdata/invalid/unknown-field.yaml")
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("ParseFile error = %v, want strict unknown-field error", err)
	}
}

func TestParseFileRejectsOversizedDefinition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.yaml")
	if err := os.WriteFile(path, []byte(strings.Repeat("#", int(MaxDefinitionBytes)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ParseFile(path)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized definition error = %v", err)
	}
}

func TestResolvePrecedenceIdentityAndDuplicates(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	minimal := func(id, name string) string {
		return "id: " + id + "\nname: " + name + "\nphases:\n  - id: one\n    driver: tool\n    gate:\n      routes:\n        - to: done\n"
	}
	if err := os.WriteFile(filepath.Join(shared, "not-the-id.yaml"), []byte(minimal("same", "shared")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "project.yaml"), []byte(minimal("same", "project")), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve([]Source{{Dir: shared, Scope: ScopeShared}, {Dir: project, Scope: ScopeProject}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Workflow.Name != "project" || resolved[0].Scope != ScopeProject {
		t.Fatalf("project precedence failed: %+v", resolved)
	}
	if err := os.WriteFile(filepath.Join(shared, "duplicate.yml"), []byte(minimal("same", "duplicate")), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Resolve([]Source{{Dir: shared, Scope: ScopeShared}})
	if err == nil || !strings.Contains(err.Error(), "duplicated in shared scope") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestResolveRejectsDeclaredInvalidID(t *testing.T) {
	_, err := Resolve([]Source{{Dir: "testdata/invalid", Scope: ScopeShared}})
	if err == nil {
		t.Fatal("expected invalid fixture directory to fail resolution")
	}
}

func TestResolveDerivesHumanGateCount(t *testing.T) {
	dir := t.TempDir()
	definition := `id: gated
name: Gated
phases:
  - id: review
    driver: agent
    gate:
      routes:
        - when:
            exists: review.ready
          human:
            approve: done
            reject:
              loop: review
              max: 1
        - human:
            approve: done
            reject:
              loop: review
              max: 1
  - id: finish
    driver: tool
    gate:
      routes:
        - park: manual
`
	if err := os.WriteFile(filepath.Join(dir, "gated.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve([]Source{{Dir: dir, Scope: ScopeShared}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].HumanGateCount != 1 {
		t.Fatalf("resolved human gates = %+v, want one phase", resolved)
	}
}

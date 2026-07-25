package starters

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
)

type documentedBindings struct {
	checks     map[string]bool
	commands   map[string]bool
	capacities map[string]bool
}

func (b documentedBindings) HasCheck(name string) bool   { return b.checks[name] }
func (b documentedBindings) HasCommand(name string) bool { return b.commands[name] }

// Capacity reports 1 for every documented resource: the starters document
// which names a project must bind, not how wide the project runs them.
func (b documentedBindings) Capacity(name string) (int, bool) {
	if !b.capacities[name] {
		return 0, false
	}
	return 1, true
}

func TestEmbeddedStartersAreCompleteAndValid(t *testing.T) {
	if got, want := List(), []string{"build-and-validate", "multi-lens-review", "poll-jira-and-start"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for _, name := range List() {
		t.Run(name, func(t *testing.T) {
			set, err := Fetch(name)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			var definitionPath string
			var definitionData []byte
			for _, file := range set.Files {
				path := filepath.Join(dir, file.Name)
				if err := os.WriteFile(path, file.Data, 0o600); err != nil {
					t.Fatalf("write starter file %q: %v", file.Name, err)
				}
				if file.Name == "workflow.yaml" {
					definitionPath = path
					definitionData = file.Data
				} else if filepath.Ext(file.Name) == ".md" {
					assertPromptContract(t, file.Name, string(file.Data))
				}
			}
			if definitionPath == "" {
				t.Fatal("starter has no workflow.yaml")
			}
			workflow, err := def.ParseFile(definitionPath)
			if err != nil {
				t.Fatal(err)
			}
			documented := parseDocumentedBindings(t, definitionData)
			used := usedBindings(workflow)
			assertSameNames(t, "checks", documented.checks, used.checks)
			assertSameNames(t, "commands", documented.commands, used.commands)
			assertSameNames(t, "capacities", documented.capacities, used.capacities)
			result := def.Validate(def.ResolvedWorkflow{
				Workflow: workflow,
				Scope:    def.ScopeShared,
				Path:     definitionPath,
			}, documented)
			if !result.Valid() || result.BindingStatus != def.BindingsChecked {
				t.Fatalf("starter validation = %+v", result)
			}
		})
	}
}

func TestFetchReturnsIsolatedData(t *testing.T) {
	first, err := Fetch("build-and-validate")
	if err != nil {
		t.Fatal(err)
	}
	first.Files[0].Data[0] = 'x'
	second, err := Fetch("build-and-validate")
	if err != nil {
		t.Fatal(err)
	}
	if second.Files[0].Data[0] == 'x' {
		t.Fatal("Fetch returned shared mutable data")
	}
	if _, err := Fetch("missing"); err == nil || !strings.Contains(err.Error(), "unknown workflow starter") {
		t.Fatalf("Fetch missing error = %v", err)
	}
}

func parseDocumentedBindings(t *testing.T, data []byte) documentedBindings {
	t.Helper()
	result := documentedBindings{
		checks:     map[string]bool{},
		commands:   map[string]bool{},
		capacities: map[string]bool{},
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		comment := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		for key, destination := range map[string]map[string]bool{
			"checks": result.checks, "commands": result.commands, "capacities": result.capacities,
		} {
			prefix := key + ":"
			if !strings.HasPrefix(comment, prefix) {
				continue
			}
			seen[key] = true
			values := strings.TrimSpace(strings.TrimPrefix(comment, prefix))
			if values == "(none)" {
				continue
			}
			for _, value := range strings.Split(values, ",") {
				name := strings.TrimSpace(value)
				if name == "" {
					t.Fatalf("empty documented %s binding", key)
				}
				destination[name] = true
			}
		}
	}
	for _, key := range []string{"checks", "commands", "capacities"} {
		if !seen[key] {
			t.Fatalf("leading YAML comments do not document %s bindings", key)
		}
	}
	return result
}

func usedBindings(workflow def.Workflow) documentedBindings {
	result := documentedBindings{
		checks:     map[string]bool{},
		commands:   map[string]bool{},
		capacities: map[string]bool{},
	}
	for _, phase := range workflow.Phases {
		if phase.Check != "" {
			result.checks[phase.Check] = true
		}
		if phase.Command != "" {
			result.commands[phase.Command] = true
		}
		for _, command := range phase.Commands {
			result.commands[command] = true
		}
		for _, capacity := range phase.Resources {
			result.capacities[capacity] = true
		}
		for _, unit := range phase.UnitDefinitions() {
			if unit.Command != "" {
				result.commands[unit.Command] = true
			}
		}
		if phase.Join != nil && phase.Join.Command != "" {
			result.commands[phase.Join.Command] = true
		}
	}
	return result
}

func assertSameNames(t *testing.T, kind string, documented, used map[string]bool) {
	t.Helper()
	if !reflect.DeepEqual(documented, used) {
		t.Errorf("%s bindings: documented=%v used=%v", kind, sortedNames(documented), sortedNames(used))
	}
}

func sortedNames(names map[string]bool) []string {
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func assertPromptContract(t *testing.T, name, prompt string) {
	t.Helper()
	for _, status := range []string{"done", "question", "stuck"} {
		if !strings.Contains(prompt, status) {
			t.Errorf("prompt %q does not explain %q status", name, status)
		}
	}
	for offset := 0; ; {
		relative := strings.Index(prompt[offset:], "{{")
		if relative < 0 {
			break
		}
		tokenStart := offset + relative
		prefix := prompt[:tokenStart]
		openStart := strings.LastIndex(prefix, "<untrusted-")
		if openStart < 0 {
			t.Errorf("prompt %q has an interpolation outside an untrusted-data delimiter", name)
			break
		}
		openEndRelative := strings.Index(prompt[openStart:], ">")
		if openEndRelative < 0 || openStart+openEndRelative >= tokenStart {
			t.Errorf("prompt %q has a malformed untrusted-data opening delimiter", name)
			break
		}
		openEnd := openStart + openEndRelative
		tag := prompt[openStart+1 : openEnd]
		closeTag := "</" + tag + ">"
		if strings.LastIndex(prefix, closeTag) > openStart || !strings.Contains(prompt[tokenStart:], closeTag) {
			t.Errorf("prompt %q has an interpolation outside matched <%s> delimiters", name, tag)
			break
		}
		offset = tokenStart + 2
	}
	if strings.Contains(prompt, "lorem") || strings.Contains(prompt, "TODO") {
		t.Errorf("prompt %q contains placeholder content", name)
	}
}

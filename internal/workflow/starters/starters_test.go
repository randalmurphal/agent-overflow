package starters

import (
	"fmt"
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

// DeclaredMaxFanOutWidth reports 0 — no declared ceiling — for the same reason
// Capacity reports 1: a starter documents the names a project must bind, never
// the width that project chooses to allow. Starters are therefore held to
// def.DefaultMaxFanOutWidth, which is the ceiling a project with no profile of
// its own gets.
func (b documentedBindings) DeclaredMaxFanOutWidth() int { return 0 }

// starterCalls resolves one starter's call edges against the rest of the
// embedded set. It exists because a starter may CALL another one — the campaign
// spine calls itself for the next wave and calls the task lane for every unit —
// and a dry-run with no resolver reports `call.unresolved` per edge rather than
// pronouncing an unchecked call graph valid. Resolving against the embedded set
// is also the assertion that matters: the pair a user scaffolds has to compose.
type starterCalls struct {
	workflows map[string]def.ResolvedWorkflow
}

func (c starterCalls) ResolveCall(id string) (def.ResolvedWorkflow, error) {
	resolved, ok := c.workflows[id]
	if !ok {
		return def.ResolvedWorkflow{}, fmt.Errorf("no embedded starter declares workflow id %q", id)
	}
	return resolved, nil
}

// materializedStarter is one embedded starter written to disk, which is what
// prompt-template validation needs: the definition's path is how sibling prompt
// files are found, for a child workflow exactly as for the root.
type materializedStarter struct {
	resolved   def.ResolvedWorkflow
	documented documentedBindings
}

func TestEmbeddedStartersAreCompleteAndValid(t *testing.T) {
	want := []string{
		"build-and-validate", "converge-on-review", "multi-lens-review",
		"poll-jira-and-start", "port-campaign", "port-one-task",
	}
	if got := List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}

	// Every starter is written out and parsed before any of them is validated,
	// so a call edge resolves to the same definition a user would have on disk.
	starters := make(map[string]materializedStarter, len(want))
	calls := starterCalls{workflows: make(map[string]def.ResolvedWorkflow, len(want))}
	for _, name := range List() {
		resolved, documented := materializeStarter(t, name)
		if existing, duplicate := starters[resolved.Workflow.ID]; duplicate {
			t.Fatalf("starters %q and %q both declare workflow id %q", existing.resolved.Path, resolved.Path, resolved.Workflow.ID)
		}
		starters[resolved.Workflow.ID] = materializedStarter{resolved: resolved, documented: documented}
		calls.workflows[resolved.Workflow.ID] = resolved
	}

	for _, name := range List() {
		t.Run(name, func(t *testing.T) {
			// The starter directory name is the workflow id: `workflow new
			// port-campaign` has to reach the definition that calls
			// `port-one-task` by that name, and the call edge names the id.
			starter, ok := starters[name]
			if !ok {
				t.Fatalf("no embedded starter declares workflow id %q", name)
			}
			used := usedBindings(starter.resolved.Workflow)
			assertSameNames(t, "checks", starter.documented.checks, used.checks)
			assertSameNames(t, "commands", starter.documented.commands, used.commands)
			assertSameNames(t, "capacities", starter.documented.capacities, used.capacities)

			result := def.Validate(starter.resolved, starter.documented, calls)
			if !result.Valid() || result.BindingStatus != def.BindingsChecked {
				t.Fatalf("starter validation = %+v", result)
			}
		})
	}
}

// materializeStarter writes one starter to its own directory, checks its prompt
// contract, and returns it resolved.
func materializeStarter(t *testing.T, name string) (def.ResolvedWorkflow, documentedBindings) {
	t.Helper()
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
			assertPromptContract(t, name+"/"+file.Name, string(file.Data))
		}
	}
	if definitionPath == "" {
		t.Fatalf("starter %q has no workflow.yaml", name)
	}
	workflow, err := def.ParseFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	return def.ResolvedWorkflow{
		Workflow: workflow,
		Scope:    def.ScopeShared,
		Path:     definitionPath,
	}, parseDocumentedBindings(t, definitionData)
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

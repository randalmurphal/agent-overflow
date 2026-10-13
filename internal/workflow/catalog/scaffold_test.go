package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/starters"
)

// TestEveryStarterCanBeScaffolded scaffolds the whole shipped set into ONE
// scope and validates each definition against the others. A starter may call
// another starter (the campaign spine calls the task lane for every unit and
// calls itself for the next wave), so validating one in isolation would only
// prove that a call edge cannot be checked, which is not the question.
func TestEveryStarterCanBeScaffolded(t *testing.T) {
	configRoot := t.TempDir()
	targetDir := filepath.Join(configRoot, "workflows")
	definitionPaths := make(map[string]string, len(starters.List()))
	for _, name := range starters.List() {
		// Scaffolded under the id the starter documents, which is the id a
		// sibling's `call:` edge names.
		files, definitionPath, err := scaffoldFiles(name, name, targetDir)
		if err != nil {
			t.Fatalf("scaffold %q: %v", name, err)
		}
		if err := writeScaffold(files, configRoot, targetDir); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
		definitionPaths[name] = definitionPath
	}
	calls, err := NewCallResolver(configRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range starters.List() {
		t.Run(name, func(t *testing.T) {
			workflow, err := def.ParseFile(definitionPaths[name])
			if err != nil {
				t.Fatal(err)
			}
			result := def.Validate(
				def.ResolvedWorkflow{Workflow: workflow, Scope: def.ScopeShared, Path: definitionPaths[name]},
				nil, calls,
			)
			if !result.Valid() {
				t.Fatalf("scaffold findings = %+v", result.Findings)
			}
		})
	}
}

// TestScaffoldRenamesSelfCallEdges pins the one rewrite a renamed definition
// cannot do without: a workflow that calls ITSELF for the next round must call
// the id it was scaffolded under, or the loop points at a definition the user
// never created. Calls to a companion workflow keep the starter's documented
// id, because the scaffolder does not know what the user called that one.
func TestScaffoldRenamesSelfCallEdges(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "workflows")
	files, definitionPath, err := scaffoldFiles("port-campaign", "my-campaign", targetDir)
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, file := range files {
		if file.path == definitionPath {
			data = file.data
		}
	}
	if data == nil {
		t.Fatalf("scaffold produced no definition at %q", definitionPath)
	}
	workflow, err := def.ParseBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	var selfCalls, unitCalls []string
	for _, phase := range workflow.Phases {
		if phase.IsCall() {
			selfCalls = append(selfCalls, phase.CallTarget())
		}
		for _, unit := range phase.UnitDefinitions() {
			if unit.IsCall() {
				unitCalls = append(unitCalls, unit.CallTarget())
			}
		}
	}
	if len(selfCalls) != 1 || selfCalls[0] != "my-campaign" {
		t.Fatalf("self-call targets = %v, want [my-campaign]", selfCalls)
	}
	if len(unitCalls) != 1 || unitCalls[0] != "port-one-task" {
		t.Fatalf("unit call targets = %v, want [port-one-task]", unitCalls)
	}
}

// EVERY sibling is scoped to the id being created, not only the prompts. A
// scope is one flat directory shared by every workflow in it, so a starter
// carrying a non-prompt sibling (the campaign's reference merge script) would
// otherwise collide with itself the second time it is scaffolded there, and the
// second scaffold would be refused naming a file the user never chose.
func TestScaffoldNamespacesEverySiblingNotOnlyPrompts(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "workflows")
	first, _, err := scaffoldFiles("port-campaign", "campaign-one", targetDir)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := scaffoldFiles("port-campaign", "campaign-two", targetDir)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	script := ""
	for _, file := range first {
		name := filepath.Base(file.path)
		names[name] = true
		if name != "campaign-one.yaml" && !strings.HasPrefix(name, "campaign-one-") {
			t.Fatalf("sibling %q was not scoped to the id being created", name)
		}
		if filepath.Ext(name) == ".py" {
			script = name
		}
	}
	// The case that motivated the rule: a sibling that is not a prompt.
	if script == "" {
		t.Fatal("port-campaign no longer ships the non-prompt sibling this rule exists for")
	}
	for _, file := range second {
		if name := filepath.Base(file.path); names[name] {
			t.Fatalf("scaffolding the same starter twice collides on %q", name)
		}
	}
}

func TestScaffoldRenamesStarterAndRefusesOverwrite(t *testing.T) {
	configRoot := t.TempDir()
	result, err := Scaffold(configRoot, "build-and-validate", "custom-build", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 4 || result.Validation.BindingStatus != def.BindingsUnchecked || !result.Validation.Valid() {
		t.Fatalf("scaffold result = %+v", result)
	}
	assertPublishedSchema(t, filepath.Join(configRoot, "workflow.schema.json"))
	definitionPath := filepath.Join(configRoot, "workflows", "custom-build.yaml")
	workflow, err := def.ParseFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.ID != "custom-build" {
		t.Fatalf("workflow id = %q", workflow.ID)
	}
	for _, phase := range workflow.Phases {
		if phase.Prompt != "" && !strings.HasPrefix(phase.Prompt, "custom-build-") {
			t.Errorf("phase %q prompt was not namespaced: %q", phase.ID, phase.Prompt)
		}
	}

	original, err := os.ReadFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(configRoot, "build-and-validate", "custom-build", ""); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("overwrite error = %v", err)
	}
	after, err := os.ReadFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("overwrite refusal changed the existing definition")
	}
}

func TestScaffoldBlankCreatesMinimalValidProjectWorkflow(t *testing.T) {
	configRoot := t.TempDir()
	result, err := Scaffold(configRoot, "blank", "ticket-triage", "sample")
	if err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(configRoot, "projects", "sample", "workflows", "ticket-triage.yaml")
	workflow, err := def.ParseFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	validation := def.Validate(def.ResolvedWorkflow{Workflow: workflow, Scope: def.ScopeProject, Path: definitionPath}, nil, nil)
	if !validation.Valid() {
		t.Fatalf("blank scaffold findings = %+v", validation.Findings)
	}
	created := strings.Join(result.Created, "\n")
	if !strings.Contains(created, definitionPath) || !strings.Contains(created, "ticket-triage-run.md") || result.Validation.BindingStatus != def.BindingsUnchecked {
		t.Fatalf("result = %+v", result)
	}
	assertPublishedSchema(t, filepath.Join(configRoot, "projects", "sample", "workflow.schema.json"))
}

func TestScaffoldSurfacesProfileBindingFindingsAfterCreatingFiles(t *testing.T) {
	configRoot := t.TempDir()
	profileDir := filepath.Join(configRoot, "projects", "sample")
	mustMkdirAll(t, profileDir)
	mustWriteFile(t, filepath.Join(profileDir, "profile.yaml"), "base_branch: main\n")

	result, err := Scaffold(configRoot, "port-campaign", "campaign", "sample")
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, finding := range result.Validation.Findings {
		codes[finding.Code] = true
	}
	if result.Validation.Valid() || result.Validation.BindingStatus != "checked" || !codes["binding.command"] || !codes["binding.capacity"] {
		t.Fatalf("validation = %+v", result.Validation)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "workflows", "campaign.yaml")); err != nil {
		t.Fatalf("created definition: %v", err)
	}
}

func TestScaffoldRejectsUnknownStarterAndInvalidIDWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name, starter, id, want string
	}{
		{name: "starter", starter: "missing", id: "valid", want: "unknown workflow starter"},
		{name: "id", starter: "blank", id: "../escape", want: "invalid workflow id"},
		{name: "empty id", starter: "blank", id: " ", want: "a workflow id is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configRoot := t.TempDir()
			if _, err := Scaffold(configRoot, test.starter, test.id, ""); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if _, err := os.Stat(filepath.Join(configRoot, "workflows")); !os.IsNotExist(err) {
				t.Fatalf("workflow directory exists after rejected scaffold: %v", err)
			}
		})
	}
}

func TestScaffoldMalformedProfileDoesNotCreateFiles(t *testing.T) {
	configRoot := t.TempDir()
	profileDir := filepath.Join(configRoot, "projects", "sample")
	mustMkdirAll(t, profileDir)
	mustWriteFile(t, filepath.Join(profileDir, "profile.yaml"), "unknown: true\n")

	if _, err := Scaffold(configRoot, "blank", "safe", "sample"); err == nil || !strings.Contains(err.Error(), filepath.Join(profileDir, "profile.yaml")) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "workflows")); !os.IsNotExist(err) {
		t.Fatalf("workflow directory exists after malformed profile: %v", err)
	}
}

func TestScaffoldRefusesDuplicateDeclaredID(t *testing.T) {
	configRoot := t.TempDir()
	workflowDir := filepath.Join(configRoot, "workflows")
	mustMkdirAll(t, workflowDir)
	mustWriteFile(t, filepath.Join(workflowDir, "legacy-name.yaml"), workflowYAML("duplicate", "Existing workflow"))

	_, err := Scaffold(configRoot, "blank", "duplicate", "")
	if err == nil || !strings.Contains(err.Error(), "already declared") || !strings.Contains(err.Error(), "legacy-name.yaml") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workflowDir, "duplicate.yaml")); !os.IsNotExist(err) {
		t.Fatalf("duplicate definition was created: %v", err)
	}
}

func TestScaffoldRejectsSymlinkedScopeComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on some Windows hosts")
	}
	for _, test := range []struct {
		name    string
		project string
	}{
		{name: "workflow directory"},
		{name: "projects directory", project: "sample"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configRoot := t.TempDir()
			outside := t.TempDir()
			link := filepath.Join(configRoot, "workflows")
			if test.project != "" {
				link = filepath.Join(configRoot, "projects")
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if _, err := Scaffold(configRoot, "blank", "safe", test.project); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
				t.Fatalf("error = %v", err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("files escaped the config scope: %v", entries)
			}
		})
	}
}

func TestStartersListsEveryRegisteredStarterAndBlank(t *testing.T) {
	names := Starters()
	for _, name := range append(starters.List(), "blank") {
		if !slices.Contains(names, name) {
			t.Errorf("Starters() = %v, missing %q", names, name)
		}
	}
}

func TestScaffoldReusesMatchingSchemaAndLeavesDifferingSchemaUntouched(t *testing.T) {
	configRoot := t.TempDir()
	for _, id := range []string{"first", "second"} {
		if _, err := Scaffold(configRoot, "blank", id, ""); err != nil {
			t.Fatalf("scaffold %s: %v", id, err)
		}
	}
	schemaPath := filepath.Join(configRoot, "workflow.schema.json")
	mustWriteFile(t, schemaPath, "{}\n")
	result, err := Scaffold(configRoot, "blank", "third", "")
	if err != nil || !strings.Contains(result.Note, "differs from this build's; left untouched") {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "workflows", "third.yaml")); err != nil {
		t.Fatalf("scaffold did not proceed past a differing schema: %v", err)
	}
	if data, err := os.ReadFile(schemaPath); err != nil || string(data) != "{}\n" {
		t.Fatalf("differing schema was modified: data=%q err=%v", data, err)
	}
	if slices.Contains(result.Created, schemaPath) {
		t.Fatalf("untouched schema reported as created: %v", result.Created)
	}
}

func TestDirectoryHandlePreventsSwapEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory rename and symlink setup differs on Windows")
	}
	configRoot := t.TempDir()
	scopeDir := filepath.Join(configRoot, "workflows")
	mustMkdirAll(t, scopeDir)
	root, err := openNestedRoot(configRoot, scopeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	movedDir := filepath.Join(configRoot, "original-workflows")
	if err := os.Rename(scopeDir, movedDir); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, scopeDir); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusiveAt(root, "safe.yaml", filepath.Join(scopeDir, "safe.yaml"), []byte("safe")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "safe.yaml")); !os.IsNotExist(err) {
		t.Fatalf("directory swap redirected write outside config root: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(movedDir, "safe.yaml")); err != nil || string(data) != "safe" {
		t.Fatalf("confined write data=%q err=%v", data, err)
	}
}

func assertPublishedSchema(t *testing.T, path string) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published schema: %v", err)
	}
	if !bytes.Equal(actual, def.AuthoringSchema()) {
		t.Fatalf("published schema %q does not match def.AuthoringSchema", path)
	}
}

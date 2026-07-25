package aocli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/starters"
)

func TestEveryStarterCanBeScaffolded(t *testing.T) {
	for _, name := range starters.List() {
		t.Run(name, func(t *testing.T) {
			configRoot := t.TempDir()
			targetDir := filepath.Join(configRoot, "workflows")
			files, definitionPath, err := scaffoldFiles(name, "custom-"+name, targetDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeScaffold(files, configRoot, targetDir); err != nil {
				t.Fatal(err)
			}
			workflow, err := def.ParseFile(definitionPath)
			if err != nil {
				t.Fatal(err)
			}
			result := def.Validate(def.ResolvedWorkflow{Workflow: workflow, Scope: def.ScopeShared, Path: definitionPath}, nil, nil)
			if !result.Valid() {
				t.Fatalf("scaffold findings = %+v", result.Findings)
			}
		})
	}
}

func TestNewScaffoldsRenamedStarterAndRefusesOverwrite(t *testing.T) {
	configRoot := t.TempDir()
	args := []string{"workflow", "new", "build-and-validate", "--id", "custom-build", "--config-root", configRoot, "--json"}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr); code != exitOK {
		t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
	}
	var result scaffoldResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode scaffold JSON: %v\n%s", err, stdout.String())
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
	stdout.Reset()
	stderr.Reset()
	if code := Run(args, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), "refusing to overwrite") {
		t.Fatalf("overwrite exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("overwrite refusal changed the existing definition")
	}
}

func TestNewBlankCreatesMinimalValidProjectWorkflow(t *testing.T) {
	configRoot := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "new", "blank", "--id", "ticket-triage", "--project", "sample", "--config-root", configRoot}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
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
	if !strings.Contains(stdout.String(), definitionPath) || !strings.Contains(stdout.String(), "ticket-triage-run.md") || !strings.Contains(stdout.String(), "bindings: unchecked") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	assertPublishedSchema(t, filepath.Join(configRoot, "projects", "sample", "workflow.schema.json"))
}

func TestNewSurfacesProfileBindingFindingsAfterCreatingFiles(t *testing.T) {
	configRoot := t.TempDir()
	profileDir := filepath.Join(configRoot, "projects", "sample")
	mustMkdirAll(t, profileDir)
	mustWriteFile(t, filepath.Join(profileDir, "profile.yaml"), "base_branch: main\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "new", "poll-jira-and-start", "--id", "jira-poll", "--project", "sample", "--config-root", configRoot}, &stdout, &stderr)
	if code != exitFindings || !strings.Contains(stdout.String(), "bindings: checked") || !strings.Contains(stdout.String(), "binding.command") || !strings.Contains(stdout.String(), "binding.capacity") {
		t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(profileDir, "workflows", "jira-poll.yaml")); err != nil {
		t.Fatalf("created definition: %v", err)
	}
}

func TestNewRejectsUnknownStarterAndInvalidIDWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "starter", args: []string{"workflow", "new", "missing", "--id", "valid"}, want: "unknown workflow starter"},
		{name: "id", args: []string{"workflow", "new", "blank", "--id", "../escape"}, want: "invalid workflow id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configRoot := t.TempDir()
			args := append(test.args, "--config-root", configRoot)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(filepath.Join(configRoot, "workflows")); !os.IsNotExist(err) {
				t.Fatalf("workflow directory exists after rejected scaffold: %v", err)
			}
		})
	}
}

func TestNewMalformedProfileDoesNotCreateFiles(t *testing.T) {
	configRoot := t.TempDir()
	profileDir := filepath.Join(configRoot, "projects", "sample")
	mustMkdirAll(t, profileDir)
	mustWriteFile(t, filepath.Join(profileDir, "profile.yaml"), "unknown: true\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "new", "blank", "--id", "safe", "--project", "sample", "--config-root", configRoot}, &stdout, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), filepath.Join(profileDir, "profile.yaml")) {
		t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(profileDir, "workflows")); !os.IsNotExist(err) {
		t.Fatalf("workflow directory exists after malformed profile: %v", err)
	}
}

func TestNewRefusesDuplicateDeclaredID(t *testing.T) {
	configRoot := t.TempDir()
	workflowDir := filepath.Join(configRoot, "workflows")
	mustMkdirAll(t, workflowDir)
	mustWriteFile(t, filepath.Join(workflowDir, "legacy-name.yaml"), workflowYAML("duplicate", "Existing workflow"))

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "new", "blank", "--id", "duplicate", "--config-root", configRoot}, &stdout, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "already declared") || !strings.Contains(stderr.String(), "legacy-name.yaml") {
		t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(workflowDir, "duplicate.yaml")); !os.IsNotExist(err) {
		t.Fatalf("duplicate definition was created: %v", err)
	}
}

func TestNewRejectsSymlinkedScopeComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on some Windows hosts")
	}
	for _, test := range []struct {
		name    string
		project bool
	}{
		{name: "workflow directory"},
		{name: "projects directory", project: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			configRoot := t.TempDir()
			outside := t.TempDir()
			link := filepath.Join(configRoot, "workflows")
			args := []string{"workflow", "new", "blank", "--id", "safe", "--config-root", configRoot}
			if test.project {
				link = filepath.Join(configRoot, "projects")
				args = append(args, "--project", "sample")
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), "must not be a symlink") {
				t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
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

func TestNewHelpListsRegisteredStarters(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run([]string{"workflow", "new", "--help"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
	}
	for _, name := range append(starters.List(), "blank") {
		if !strings.Contains(stdout.String(), "  "+name+"\n") {
			t.Errorf("help does not list %q:\n%s", name, stdout.String())
		}
	}
}

func TestNewReusesMatchingSchemaAndLeavesDifferingSchemaUntouched(t *testing.T) {
	configRoot := t.TempDir()
	for _, id := range []string{"first", "second"} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := Run([]string{"workflow", "new", "blank", "--id", id, "--config-root", configRoot}, &stdout, &stderr); code != exitOK {
			t.Fatalf("new %s exit code = %d; stdout=%q stderr=%q", id, code, stdout.String(), stderr.String())
		}
	}
	schemaPath := filepath.Join(configRoot, "workflow.schema.json")
	mustWriteFile(t, schemaPath, "{}\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "new", "blank", "--id", "third", "--config-root", configRoot}, &stdout, &stderr)
	if code != exitOK || !strings.Contains(stderr.String(), "differs from this build's; left untouched") {
		t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(configRoot, "workflows", "third.yaml")); err != nil {
		t.Fatalf("scaffold did not proceed past a differing schema: %v", err)
	}
	if data, err := os.ReadFile(schemaPath); err != nil || string(data) != "{}\n" {
		t.Fatalf("differing schema was modified: data=%q err=%v", data, err)
	}
	if strings.Contains(stdout.String(), schemaPath) {
		t.Fatalf("untouched schema reported as created: stdout=%q", stdout.String())
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

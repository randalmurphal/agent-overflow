package aocli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/project"
)

func TestValidateExitCodes(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	mustMkdirAll(t, sharedDir)
	validPath := filepath.Join(sharedDir, "valid.yaml")
	mustWriteFile(t, validPath, workflowYAML("valid", "Valid workflow"))
	mustWriteFile(t, filepath.Join(sharedDir, "findings.yaml"), workflowYAML("findings", ""))

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "valid path",
			args:       []string{"workflow", "validate", validPath},
			wantCode:   exitOK,
			wantStdout: "bindings: unchecked\n",
		},
		{
			name:       "findings by id",
			args:       []string{"--config-root", configRoot, "workflow", "validate", "--id", "findings"},
			wantCode:   exitFindings,
			wantStdout: "workflow \"findings\": workflow.name: name is required",
		},
		{
			name:       "unknown id",
			args:       []string{"workflow", "validate", "--config-root", configRoot, "--id", "missing"},
			wantCode:   exitError,
			wantStderr: "workflow id \"missing\" was not found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr); code != test.wantCode {
				t.Fatalf("Run exit code = %d, want %d; stderr=%q", code, test.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), test.wantStdout)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

func TestListResolvedViewIncludesShadowing(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", "sample", "workflows")
	mustMkdirAll(t, sharedDir)
	mustMkdirAll(t, projectDir)
	mustWriteFile(t, filepath.Join(sharedDir, "same.yaml"), workflowYAML("same", "Shared name"))
	mustWriteFile(t, filepath.Join(sharedDir, "shared.yaml"), workflowYAML("shared-only", "Shared only"))
	projectPath := filepath.Join(projectDir, "same.yaml")
	mustWriteFile(t, projectPath, workflowYAML("same", "Project name"))

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "list", "--config-root", configRoot, "--project", "sample", "--json"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
	}
	var entries []listEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode list JSON: %v\noutput: %s", err, stdout.String())
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want two", entries)
	}
	if entries[0].ID != "same" || entries[0].Name != "Project name" || entries[0].Scope != "project" || entries[0].Path != projectPath || !entries[0].ShadowsShared {
		t.Errorf("shadowing entry = %+v", entries[0])
	}
	if entries[1].ID != "shared-only" || entries[1].Scope != "shared" || entries[1].ShadowsShared {
		t.Errorf("shared entry = %+v", entries[1])
	}
}

func TestJSONShapes(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	mustMkdirAll(t, sharedDir)
	validPath := filepath.Join(sharedDir, "valid.yaml")
	mustWriteFile(t, validPath, workflowYAML("valid", "Valid workflow"))

	tests := []struct {
		name     string
		args     []string
		wantKeys []string
	}{
		{
			name:     "validation result",
			args:     []string{"workflow", "validate", "--json", validPath},
			wantKeys: []string{"bindingStatus", "findings"},
		},
		{
			name:     "list entry",
			args:     []string{"workflow", "list", "--json", "--config-root", configRoot},
			wantKeys: []string{"bindingStatus", "findings", "id", "name", "path", "scope", "shadowsShared"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr); code != exitOK {
				t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
			}
			var object map[string]json.RawMessage
			if test.name == "list entry" {
				var array []map[string]json.RawMessage
				if err := json.Unmarshal(stdout.Bytes(), &array); err != nil || len(array) != 1 {
					t.Fatalf("decode list JSON: entries=%d err=%v output=%s", len(array), err, stdout.String())
				}
				object = array[0]
			} else if err := json.Unmarshal(stdout.Bytes(), &object); err != nil {
				t.Fatalf("decode validation JSON: %v", err)
			}
			if len(object) != len(test.wantKeys) {
				t.Fatalf("JSON keys = %v, want exactly %v", mapKeys(object), test.wantKeys)
			}
			for _, key := range test.wantKeys {
				if _, ok := object[key]; !ok {
					t.Errorf("JSON lacks key %q: %s", key, stdout.String())
				}
			}
			if test.name == "validation result" {
				var result struct {
					Findings      []json.RawMessage `json:"findings"`
					BindingStatus string            `json:"bindingStatus"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatalf("decode typed validation result: %v", err)
				}
				if result.Findings == nil || len(result.Findings) != 0 || result.BindingStatus != "unchecked" {
					t.Fatalf("validation result = %+v, want empty findings array and unchecked bindings", result)
				}
			}
		})
	}
}

func TestValidationJSONIncludesTypedFindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.yaml")
	mustWriteFile(t, path, workflowYAML("findings", ""))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run([]string{"workflow", "validate", "--json", path}, &stdout, &stderr); code != exitFindings {
		t.Fatalf("Run exit code = %d, want %d; stderr=%q", code, exitFindings, stderr.String())
	}
	var result struct {
		Findings []struct {
			Code    string `json:"code"`
			Element string `json:"element"`
			Message string `json:"message"`
		} `json:"findings"`
		BindingStatus string `json:"bindingStatus"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode validation JSON: %v", err)
	}
	if len(result.Findings) != 1 || result.Findings[0].Code != "workflow.name" || result.Findings[0].Element != "workflow \"findings\"" || result.Findings[0].Message != "name is required" || result.BindingStatus != "unchecked" {
		t.Fatalf("validation result = %+v", result)
	}
}

// A list with nothing in it says so. Printing nothing reads as a command that
// did not work, and the second line names the one input whose absence explains a
// blank answer for a project-scoped definition.
func TestListWithNoWorkflowDirectoriesSaysSo(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := RunWithEnv(
		[]string{"workflow", "list", "--config-root", t.TempDir()},
		noEnv, &stdout, &stderr,
	)
	if code != exitOK {
		t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
	}
	for _, want := range []string{
		"No workflows are configured here.",
		"Project workflows need --project <slug>, or a session that sets " + EnvProject + ".",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("zero-workflow output %q is missing %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("zero-workflow stderr = %q, want empty", stderr.String())
	}
}

// noEnv (exec_test.go) is the empty session environment. Offline commands read
// AO_PROJECT, so a test that asserts on scope resolution must inject an
// environment rather than inherit the developer's own.

// With a project scope resolved, the blank answer is not explained by a missing
// flag, so the second line must not appear — an instruction that cannot help is
// worse than none.
func TestListWithAProjectScopeOmitsTheProjectHint(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := RunWithEnv(
		[]string{"workflow", "list", "--config-root", t.TempDir(), "--project", "repo-a"},
		noEnv, &stdout, &stderr,
	)
	if code != exitOK {
		t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No workflows are configured here.") {
		t.Fatalf("zero-workflow output = %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "need --project") {
		t.Fatalf("a scoped list still asked for --project: %q", stdout.String())
	}
}

// The offline half cannot infer a project, and a cold agent inside a session has
// no way to learn the slug — so AO_PROJECT supplies the scope, and an explicit
// --project always wins over it.
func TestOfflineCommandsDefaultTheProjectScopeFromTheSession(t *testing.T) {
	configRoot := t.TempDir()
	sessionDir := filepath.Join(project.ConfigDir(configRoot, "repo-a"), "workflows")
	otherDir := filepath.Join(project.ConfigDir(configRoot, "repo-b"), "workflows")
	mustMkdirAll(t, sessionDir)
	mustMkdirAll(t, otherDir)
	mustWriteFile(t, filepath.Join(sessionDir, "session-only.yaml"), workflowYAML("session-only", "Session only"))
	mustWriteFile(t, filepath.Join(otherDir, "other-only.yaml"), workflowYAML("other-only", "Other only"))
	sessionEnv := func(name string) (string, bool) {
		if name == EnvProject {
			return "repo-a", true
		}
		return "", false
	}

	t.Run("list defaults from the session", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := RunWithEnv(
			[]string{"workflow", "list", "--config-root", configRoot}, sessionEnv, &stdout, &stderr,
		); code != exitOK {
			t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "id=session-only") {
			t.Fatalf("list did not resolve the session's project scope: %q", stdout.String())
		}
	})

	t.Run("an explicit flag wins", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := RunWithEnv(
			[]string{"workflow", "list", "--config-root", configRoot, "--project", "repo-b"},
			sessionEnv, &stdout, &stderr,
		); code != exitOK {
			t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "id=other-only") || strings.Contains(stdout.String(), "id=session-only") {
			t.Fatalf("--project did not override %s: %q", EnvProject, stdout.String())
		}
	})

	t.Run("validate --id defaults from the session", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := RunWithEnv(
			[]string{"workflow", "validate", "--config-root", configRoot, "--id", "session-only"},
			sessionEnv, &stdout, &stderr,
		); code != exitOK {
			t.Fatalf("exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("without the session variable the project scope is invisible", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := RunWithEnv(
			[]string{"workflow", "validate", "--config-root", configRoot, "--id", "session-only"},
			noEnv, &stdout, &stderr,
		); code != exitError {
			t.Fatalf("exit code = %d, want %d", code, exitError)
		}
		if !strings.Contains(stderr.String(), `workflow id "session-only" was not found`) {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})

	// A malformed AO_PROJECT is named by where it came from: the fix is not
	// something the caller typed.
	t.Run("a malformed session slug names itself", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := RunWithEnv([]string{"workflow", "list", "--config-root", configRoot}, func(name string) (string, bool) {
			if name == EnvProject {
				return "../outside", true
			}
			return "", false
		}, &stdout, &stderr)
		if code != exitError || !strings.Contains(stderr.String(), EnvProject+": invalid project slug") {
			t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
		}
	})
}

// A directory under a workflow source is skipped in silence by design, which is
// how a hand-authored `<id>/workflow.yaml` becomes invisible. Both surfaces that
// a human reads must say so, and validate's failure must carry it: "not found"
// for an id whose file is right there is otherwise unexplainable.
func TestSkippedWorkflowDirectoriesAreReported(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	nested := filepath.Join(sharedDir, "nested-flow")
	mustMkdirAll(t, nested)
	mustWriteFile(t, filepath.Join(nested, "workflow.yaml"), workflowYAML("nested-flow", "Nested"))
	wantNote := "note: " + nested + " is a directory and was skipped — a workflow is a flat <id>.yaml beside its <id>-*.md prompts"

	t.Run("list", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := RunWithEnv(
			[]string{"workflow", "list", "--config-root", configRoot}, noEnv, &stdout, &stderr,
		); code != exitOK {
			t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), wantNote) {
			t.Fatalf("list stderr = %q, want %q", stderr.String(), wantNote)
		}
		if !strings.Contains(stdout.String(), "No workflows are configured here.") {
			t.Fatalf("list stdout = %q, want the empty-list line", stdout.String())
		}
	})

	// --json keeps its document: the note is a fact about the directory, not a
	// row of the requested list, so a machine reader loses neither.
	t.Run("list --json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := RunWithEnv(
			[]string{"workflow", "list", "--config-root", configRoot, "--json"}, noEnv, &stdout, &stderr,
		); code != exitOK {
			t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), wantNote) {
			t.Fatalf("list --json stderr = %q, want %q", stderr.String(), wantNote)
		}
		if strings.TrimSpace(stdout.String()) != "[]" {
			t.Fatalf("list --json stdout = %q, want the empty list document", stdout.String())
		}
	})

	t.Run("validate --id", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := RunWithEnv(
			[]string{"workflow", "validate", "--config-root", configRoot, "--id", "nested-flow"},
			noEnv, &stdout, &stderr,
		)
		if code != exitError {
			t.Fatalf("exit code = %d, want %d", code, exitError)
		}
		if !strings.Contains(stderr.String(), `workflow id "nested-flow" was not found`) {
			t.Fatalf("validate stderr = %q", stderr.String())
		}
		if !strings.Contains(stderr.String(), wantNote) {
			t.Fatalf("validate stderr = %q, want %q", stderr.String(), wantNote)
		}
	})
}

func TestProjectSlugCannotEscapeConfigRoot(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "list", "--config-root", t.TempDir(), "--project", "../outside"}, &stdout, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "invalid project slug") {
		t.Fatalf("Run exit code = %d, stderr=%q", code, stderr.String())
	}
}

func TestHelpAtEveryCommandLevel(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "root", args: []string{"--help"}, want: "Usage: agent-overflow "},
		{name: "workflow", args: []string{"workflow", "--help"}, want: "Usage: agent-overflow workflow <command>"},
		{name: "new", args: []string{"workflow", "new", "--help"}, want: "Usage: agent-overflow workflow new"},
		{name: "validate", args: []string{"workflow", "validate", "--help"}, want: "Usage: agent-overflow workflow validate"},
		{name: "list", args: []string{"workflow", "list", "--help"}, want: "Usage: agent-overflow workflow list"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr); code != exitOK {
				t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stdout.String()+stderr.String(), test.want) {
				t.Fatalf("help output = %q, want %q", stdout.String()+stderr.String(), test.want)
			}
		})
	}
}

// The app binary decides "CLI invocation or boot" from Commands(), so a verb
// Run dispatches but Commands omits would be unreachable as `agent-overflow
// <verb>`, and a verb Commands reports but Run rejects would swallow a boot.
// Both directions are pinned here because only the table can satisfy both.
func TestCommandsMatchesWhatRunDispatches(t *testing.T) {
	names := Commands()
	if len(names) == 0 {
		t.Fatal("Commands() is empty")
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Fatalf("Commands() repeats %q", name)
		}
		seen[name] = true
		if !IsCommand(name) {
			t.Fatalf("IsCommand(%q) = false for a name Commands() reports", name)
		}
		// A dispatched command reaches its own handler: it may still refuse the
		// empty argument list, but never as "unknown command", which is the one
		// answer that would mean Run does not know the name at all.
		var stdout, stderr bytes.Buffer
		RunWithEnv([]string{name}, func(string) (string, bool) { return "", false }, &stdout, &stderr)
		if strings.Contains(stderr.String(), "unknown command") {
			t.Fatalf("Run rejected %q as unknown: %q", name, stderr.String())
		}
	}
	if IsCommand("frobnicate") {
		t.Fatal("IsCommand accepted a name no table row declares")
	}
	var stdout, stderr bytes.Buffer
	if code := RunWithEnv([]string{"frobnicate"}, func(string) (string, bool) { return "", false }, &stdout, &stderr); code != exitError {
		t.Fatalf("an unknown command exited %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("an unknown command did not say so: %q", stderr.String())
	}
}

func workflowYAML(id, name string) string {
	return "id: " + id + "\nname: " + name + "\nphases:\n  - id: run\n    driver: tool\n    check: test\n    gate:\n      routes:\n        - to: done\n"
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mapKeys(object map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}

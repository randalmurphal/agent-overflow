package aocli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/project"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/starters"
)

// `workflow validate` has two forms — a path and an `--id` — and they have to
// be the same question. Scope is not only how a definition is FOUND: it is what
// a `call:` edge resolves against and where the project profile's bindings come
// from, so the path form reading no scope at all reported call targets that do
// not resolve for a definition the id form validated clean.

// scaffoldStarters writes every embedded starter into one source directory
// under its documented id, which is the id a sibling's `call:` edge names.
func scaffoldStarters(t *testing.T, configRoot, targetDir string) map[string]string {
	t.Helper()
	paths := make(map[string]string, len(starters.List()))
	for _, name := range starters.List() {
		files, definitionPath, err := scaffoldFiles(name, name, targetDir)
		if err != nil {
			t.Fatalf("scaffold %q: %v", name, err)
		}
		if err := writeScaffold(files, configRoot, targetDir); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
		paths[name] = definitionPath
	}
	return paths
}

// validateJSON runs one `workflow validate` invocation and decodes its typed
// result, so the two forms are compared on the structure rather than on prose.
func validateJSON(t *testing.T, args []string, lookupEnv func(string) (string, bool)) (def.ValidationResult, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runValidate(append([]string{"--json"}, args...), "", lookupEnv, &stdout, &stderr)
	if stderr.Len() != 0 {
		return def.ValidationResult{}, code, stderr.String()
	}
	var result def.ValidationResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode %v output %q: %v", args, stdout.String(), err)
	}
	return result, code, ""
}

// TestValidateByIDAndByPathAgreeOverEveryStarter is the round trip: in both
// scopes, over every shipped definition, `validate <path>` and `validate --id`
// return the same typed result and the same exit code — and that result is
// clean, so "they agree" cannot be satisfied by both being wrong.
func TestValidateByIDAndByPathAgreeOverEveryStarter(t *testing.T) {
	for _, scope := range []struct {
		name      string
		sourceDir func(configRoot string) string
		// lookupEnv is what a cold agent inside a session actually has: the
		// project slug on AO_PROJECT. No form is given an extra flag.
		lookupEnv func(string) (string, bool)
	}{
		{
			name:      "shared",
			sourceDir: func(configRoot string) string { return filepath.Join(configRoot, "workflows") },
			lookupEnv: func(string) (string, bool) { return "", false },
		},
		{
			name: "project",
			sourceDir: func(configRoot string) string {
				return filepath.Join(project.ConfigDir(configRoot, "acme"), "workflows")
			},
			lookupEnv: func(name string) (string, bool) {
				if name == EnvProject {
					return "acme", true
				}
				return "", false
			},
		},
	} {
		t.Run(scope.name, func(t *testing.T) {
			configRoot := t.TempDir()
			paths := scaffoldStarters(t, configRoot, scope.sourceDir(configRoot))
			for _, name := range starters.List() {
				t.Run(name, func(t *testing.T) {
					byPath, pathCode, pathErr := validateJSON(t, []string{"--config-root", configRoot, paths[name]}, scope.lookupEnv)
					if pathErr != "" {
						t.Fatalf("validate by path failed: %s", pathErr)
					}
					byID, idCode, idErr := validateJSON(t, []string{"--config-root", configRoot, "--id", name}, scope.lookupEnv)
					if idErr != "" {
						t.Fatalf("validate by id failed: %s", idErr)
					}
					if !reflect.DeepEqual(byPath, byID) {
						t.Fatalf("the two forms disagree:\n by path: %+v\n by id:   %+v", byPath, byID)
					}
					if pathCode != idCode {
						t.Fatalf("exit codes = %d (path) and %d (id)", pathCode, idCode)
					}
					if !byID.Valid() {
						t.Fatalf("a shipped starter does not validate: %+v", byID.Findings)
					}
				})
			}
		})
	}
}

// TestValidateByPathReadsTheScopeTheFileSitsIn is the mechanism behind the
// round trip, stated on its own: a project-scoped definition validated by path
// with NO project named anywhere still resolves its call edges under that
// project, because the scope a file is in is a fact about the file.
func TestValidateByPathReadsTheScopeTheFileSitsIn(t *testing.T) {
	configRoot := t.TempDir()
	paths := scaffoldStarters(t, configRoot, filepath.Join(project.ConfigDir(configRoot, "acme"), "workflows"))

	// port-campaign calls the task lane for every unit and calls itself for the
	// next wave, so an unscoped read reports both edges as unresolvable.
	result, _, stderr := validateJSON(t,
		[]string{"--config-root", configRoot, paths["port-campaign"]},
		func(string) (string, bool) { return "", false })
	if stderr != "" {
		t.Fatalf("validate by path failed: %s", stderr)
	}
	if !result.Valid() {
		t.Fatalf("a project-scoped definition validated by path reported findings: %+v", result.Findings)
	}
}

// TestValidateByPathAcceptsAnExplicitProject — a definition drafted OUTSIDE the
// config root (a repo checkout, before it is installed) has no scope to derive,
// and `--project` is how its author names the project whose call graph and
// bindings it is meant to run under. Before, the flag was refused outright with
// a path.
func TestValidateByPathAcceptsAnExplicitProject(t *testing.T) {
	configRoot := t.TempDir()
	scaffoldStarters(t, configRoot, filepath.Join(project.ConfigDir(configRoot, "acme"), "workflows"))

	// The same definition, drafted somewhere the config root knows nothing about.
	draftDir := filepath.Join(t.TempDir(), "drafts")
	draftPaths := scaffoldStarters(t, draftDir, draftDir)

	unscoped, _, stderr := validateJSON(t,
		[]string{"--config-root", configRoot, draftPaths["port-campaign"]},
		func(string) (string, bool) { return "", false })
	if stderr != "" {
		t.Fatalf("validate failed: %s", stderr)
	}
	// The draft's own siblings are not installed anywhere, so its companion call
	// edge is genuinely unresolvable — that is what --project supplies.
	if unscoped.Valid() {
		t.Fatal("a draft calling uninstalled workflows validated clean; the fixture proves nothing")
	}

	scoped, _, stderr := validateJSON(t,
		[]string{"--config-root", configRoot, "--project", "acme", draftPaths["port-campaign"]},
		func(string) (string, bool) { return "", false })
	if stderr != "" {
		t.Fatalf("validate --project failed: %s", stderr)
	}
	if !scoped.Valid() {
		t.Fatalf("--project did not supply the call graph: %+v", scoped.Findings)
	}
}

// The round trip over a RENAMED scaffold, which is what `workflow new --id
// <new>` actually produces most of the time. Renaming rewrites the prompt
// siblings, the `prompt:` fields that name them, and the definition's calls to
// ITSELF — three rewrites, any of which could leave a reference the resolver
// cannot follow. Greping the scaffolded bytes (below) proves no mention of the
// old id survived; only validating proves the new ones resolve.
//
// One starter is renamed per case and the rest keep their documented ids,
// because a call to a DIFFERENT starter deliberately does not follow `--id` —
// renaming everything would dangle exactly the edges that are supposed to hold.
func TestValidateAgreesOverAScaffoldRenamedByID(t *testing.T) {
	for _, name := range starters.List() {
		t.Run(name, func(t *testing.T) {
			configRoot := t.TempDir()
			sourceDir := filepath.Join(project.ConfigDir(configRoot, "acme"), "workflows")
			renamed := "renamed-" + name
			var definitionPath string
			for _, other := range starters.List() {
				id := other
				if other == name {
					id = renamed
				}
				files, path, err := scaffoldFiles(other, id, sourceDir)
				if err != nil {
					t.Fatalf("scaffold %q as %q: %v", other, id, err)
				}
				if err := writeScaffold(files, configRoot, sourceDir); err != nil {
					t.Fatalf("write %q: %v", id, err)
				}
				if other == name {
					definitionPath = path
				}
			}

			lookupEnv := func(variable string) (string, bool) {
				if variable == EnvProject {
					return "acme", true
				}
				return "", false
			}
			byPath, pathCode, pathErr := validateJSON(t,
				[]string{"--config-root", configRoot, definitionPath}, lookupEnv)
			if pathErr != "" {
				t.Fatalf("validate by path failed: %s", pathErr)
			}
			byID, idCode, idErr := validateJSON(t,
				[]string{"--config-root", configRoot, "--id", renamed}, lookupEnv)
			if idErr != "" {
				t.Fatalf("validate --id %q failed: %s", renamed, idErr)
			}
			if !reflect.DeepEqual(byPath, byID) || pathCode != idCode {
				t.Fatalf("the two forms disagree over a renamed scaffold:\n by path: %+v (%d)\n by id:   %+v (%d)",
					byPath, pathCode, byID, idCode)
			}
			if !byID.Valid() {
				t.Fatalf("a starter scaffolded under a different id does not validate: %+v", byID.Findings)
			}
		})
	}
}

// TestScaffoldedRenameLeavesNoReferenceBehind — `workflow new --id <new>`
// rewrites every internal reference it scaffolds, so a renamed set has no
// mention of the starter's own id left in it. A prompt or a script still
// pointing at `<starter>-*.md` would resolve to a file the user never created.
func TestScaffoldedRenameLeavesNoReferenceBehind(t *testing.T) {
	for _, name := range starters.List() {
		t.Run(name, func(t *testing.T) {
			targetDir := filepath.Join(t.TempDir(), "workflows")
			files, _, err := scaffoldFiles(name, "renamed-"+name, targetDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				base := filepath.Base(file.path)
				if !strings.HasPrefix(base, "renamed-"+name) {
					t.Fatalf("sibling %q was not scoped to the new id", base)
				}
				// The starter's own id may survive only as part of the new one
				// (`renamed-port-campaign`); anywhere else it is a dangling
				// reference to a file this scaffold did not write.
				for _, line := range strings.Split(string(file.data), "\n") {
					stripped := strings.ReplaceAll(line, "renamed-"+name, "")
					if strings.Contains(stripped, name+"-") || strings.Contains(stripped, name+".yaml") {
						t.Errorf("%s: reference to the starter id survived the rename: %s", base, strings.TrimSpace(line))
					}
				}
			}
		})
	}
}

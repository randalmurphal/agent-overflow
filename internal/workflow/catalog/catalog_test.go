package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/project"
)

func TestValidateReportsFindingsAndUnknownIDs(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	mustMkdirAll(t, sharedDir)
	validPath := filepath.Join(sharedDir, "valid.yaml")
	mustWriteFile(t, validPath, workflowYAML("valid", "Valid workflow"))
	mustWriteFile(t, filepath.Join(sharedDir, "findings.yaml"), workflowYAML("findings", ""))

	valid, err := ValidatePath(configRoot, "", validPath)
	if err != nil || !valid.Valid() || valid.BindingStatus != "unchecked" || valid.Findings == nil {
		t.Fatalf("valid = %+v, err = %v; want no findings as an empty array and unchecked bindings", valid, err)
	}
	findings, err := ValidateID(configRoot, "", "findings")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Findings) != 1 || findings.Findings[0].Code != "workflow.name" ||
		findings.Findings[0].Element != `workflow "findings"` || findings.Findings[0].Message != "name is required" {
		t.Fatalf("findings = %+v", findings.Findings)
	}
	if _, err := ValidateID(configRoot, "", "missing"); err == nil || !strings.Contains(err.Error(), `workflow id "missing" was not found`) {
		t.Fatalf("unknown id error = %v", err)
	}
}

func TestListResolvedViewIncludesShadowing(t *testing.T) {
	// Listing reports paths under the EvalSymlinks-resolved config root, so
	// resolve here too (/var vs /private/var on macOS temp dirs).
	configRoot := canonicalTempDir(t)
	sharedDir := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", "sample", "workflows")
	mustMkdirAll(t, sharedDir)
	mustMkdirAll(t, projectDir)
	mustWriteFile(t, filepath.Join(sharedDir, "same.yaml"), workflowYAML("same", "Shared name"))
	mustWriteFile(t, filepath.Join(sharedDir, "shared.yaml"), workflowYAML("shared-only", "Shared only"))
	projectPath := filepath.Join(projectDir, "same.yaml")
	mustWriteFile(t, projectPath, workflowYAML("same", "Project name"))

	listing, err := List(configRoot, "sample")
	if err != nil {
		t.Fatal(err)
	}
	entries := listing.Workflows
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

// An empty listing is an empty array, never null, so a reader always has a
// document to read.
func TestEmptyListingIsAnEmptyArray(t *testing.T) {
	listing, err := List(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if listing.Workflows == nil || len(listing.Workflows) != 0 || len(listing.Notes) != 0 {
		t.Fatalf("listing = %+v", listing)
	}
}

// The project scope is the caller's: another project's definitions are not in
// it.
func TestListAndValidateReadOnlyTheNamedProject(t *testing.T) {
	configRoot := t.TempDir()
	sessionDir := filepath.Join(project.ConfigDir(configRoot, "repo-a"), "workflows")
	otherDir := filepath.Join(project.ConfigDir(configRoot, "repo-b"), "workflows")
	mustMkdirAll(t, sessionDir)
	mustMkdirAll(t, otherDir)
	mustWriteFile(t, filepath.Join(sessionDir, "session-only.yaml"), workflowYAML("session-only", "Session only"))
	mustWriteFile(t, filepath.Join(otherDir, "other-only.yaml"), workflowYAML("other-only", "Other only"))

	listing, err := List(configRoot, "repo-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Workflows) != 1 || listing.Workflows[0].ID != "session-only" {
		t.Fatalf("listing = %+v", listing.Workflows)
	}
	if _, err := ValidateID(configRoot, "repo-a", "session-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateID(configRoot, "repo-a", "other-only"); err == nil {
		t.Fatal("another project's definition resolved")
	}
	if _, err := ValidateID(configRoot, "", "session-only"); err == nil {
		t.Fatal("a project definition resolved without a project")
	}
}

// A directory under a workflow source is skipped in silence by design, which
// is how a hand-authored `<id>/workflow.yaml` becomes invisible. The listing
// says so, and validation's failure carries it: "not found" for an id whose
// file is right there is otherwise unexplainable.
func TestSkippedWorkflowDirectoriesAreReported(t *testing.T) {
	// The skip note names the EvalSymlinks-resolved directory, so resolve here
	// too (/var vs /private/var on macOS temp dirs).
	configRoot := canonicalTempDir(t)
	nested := filepath.Join(configRoot, "workflows", "nested-flow")
	mustMkdirAll(t, nested)
	mustWriteFile(t, filepath.Join(nested, "workflow.yaml"), workflowYAML("nested-flow", "Nested"))
	wantNote := "note: " + nested + " is a directory and was skipped; a workflow is a flat <id>.yaml beside its <id>-*.md prompts"

	listing, err := List(configRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Workflows) != 0 || len(listing.Notes) != 1 || listing.Notes[0] != wantNote {
		t.Fatalf("listing = %+v, want no workflows and the note %q", listing, wantNote)
	}
	_, err = ValidateID(configRoot, "", "nested-flow")
	if err == nil || !strings.Contains(err.Error(), `workflow id "nested-flow" was not found`) || !strings.Contains(err.Error(), wantNote) {
		t.Fatalf("validate error = %v, want not-found carrying %q", err, wantNote)
	}
}

func TestProjectSlugCannotEscapeConfigRoot(t *testing.T) {
	configRoot := t.TempDir()
	if _, err := List(configRoot, "../outside"); err == nil || !strings.Contains(err.Error(), "invalid project slug") {
		t.Fatalf("List error = %v", err)
	}
	if _, err := ValidateID(configRoot, "../outside", "x"); err == nil || !strings.Contains(err.Error(), "invalid project slug") {
		t.Fatalf("ValidateID error = %v", err)
	}
	if _, err := Scaffold(configRoot, "blank", "x", "../outside"); err == nil || !strings.Contains(err.Error(), "invalid project slug") {
		t.Fatalf("Scaffold error = %v", err)
	}
}

func workflowYAML(id, name string) string {
	return "id: " + id + "\nname: " + name + "\nphases:\n  - id: run\n    driver: tool\n    check: test\n    gate:\n      routes:\n        - to: done\n"
}

// canonicalTempDir is t.TempDir() with symlinks resolved, for tests comparing
// against output derived from the EvalSymlinks-resolved config root (on macOS
// the default TMPDIR is a /var symlink into /private/var).
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
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

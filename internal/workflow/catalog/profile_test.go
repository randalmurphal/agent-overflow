package catalog

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectProfileBacksValidateAndList(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", "sample")
	mustMkdirAll(t, sharedDir)
	mustMkdirAll(t, projectDir)
	mustWriteFile(t, filepath.Join(sharedDir, "valid.yaml"), workflowYAML("valid", "Valid workflow"))
	mustWriteFile(t, filepath.Join(projectDir, "profile.yaml"), "checks:\n  test: [make, test]\n")

	result, err := ValidateID(configRoot, "sample", "valid")
	if err != nil {
		t.Fatal(err)
	}
	if result.BindingStatus != "checked" || len(result.Findings) != 0 {
		t.Fatalf("validation result = %+v", result)
	}
	listing, err := List(configRoot, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Workflows) != 1 || listing.Workflows[0].BindingStatus != "checked" || len(listing.Workflows[0].Findings) != 0 {
		t.Fatalf("list entries = %+v", listing.Workflows)
	}
}

func TestAbsentProjectProfileLeavesBindingsUnchecked(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	mustMkdirAll(t, sharedDir)
	mustWriteFile(t, filepath.Join(sharedDir, "valid.yaml"), workflowYAML("valid", "Valid workflow"))

	result, err := ValidateID(configRoot, "sample", "valid")
	if err != nil || result.BindingStatus != "unchecked" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestMalformedProjectProfileIsAnError(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	profilePath := filepath.Join(configRoot, "projects", "sample", "profile.yaml")
	mustMkdirAll(t, sharedDir)
	mustMkdirAll(t, filepath.Dir(profilePath))
	mustWriteFile(t, filepath.Join(sharedDir, "valid.yaml"), workflowYAML("valid", "Valid workflow"))
	mustWriteFile(t, profilePath, "unknown: true\n")

	_, validateErr := ValidateID(configRoot, "sample", "valid")
	_, listErr := List(configRoot, "sample")
	for name, err := range map[string]error{"validate": validateErr, "list": listErr} {
		if err == nil || !strings.Contains(err.Error(), profilePath) || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("%s error = %v, want the profile path and the parse failure", name, err)
		}
	}
}

func TestProjectProfileBindingFindingsAreVisible(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", "sample")
	mustMkdirAll(t, sharedDir)
	mustMkdirAll(t, projectDir)
	mustWriteFile(t, filepath.Join(sharedDir, "valid.yaml"), workflowYAML("valid", "Valid workflow"))
	mustWriteFile(t, filepath.Join(projectDir, "profile.yaml"), "base_branch: main\n")

	result, err := ValidateID(configRoot, "sample", "valid")
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid() || result.BindingStatus != "checked" || len(result.Findings) == 0 || result.Findings[0].Code != "binding.check" {
		t.Fatalf("result = %+v, want a binding.check finding", result)
	}
}

// TestFanOutWidthReportIsInformational pins decision D19's dry-run half: a
// static fan-out wider than the provider capacity it will actually get is
// reported, but the workflow stays valid: the run throttles, it does not break.
func TestFanOutWidthReportIsInformational(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", "sample")
	mustMkdirAll(t, sharedDir)
	mustMkdirAll(t, projectDir)
	mustWriteFile(t, filepath.Join(sharedDir, "wide.yaml"), fanOutWorkflowYAML())
	for _, prompt := range []string{"left.md", "right.md", "merge.md"} {
		mustWriteFile(t, filepath.Join(sharedDir, prompt), "do the work\n")
	}
	mustWriteFile(t, filepath.Join(projectDir, "profile.yaml"), "capacities:\n  \"provider:claude\": 1\n")

	result, err := ValidateID(configRoot, "sample", "wide")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid() || len(result.Findings) != 0 || len(result.Reports) != 1 || result.Reports[0].Code != "fan-out.width" {
		t.Fatalf("validation result = %+v, want one report and no findings", result)
	}
}

func fanOutWorkflowYAML() string {
	return `id: wide
name: Wide fan-out
phases:
  - id: work
    shape: fan-out
    fan_out:
      - id: left
        provider: claude
        model: sonnet
        prompt: left.md
      - id: right
        provider: claude
        model: sonnet
        prompt: right.md
    join:
      id: merge
      provider: claude
      model: sonnet
      prompt: merge.md
    gate:
      routes:
        - to: done
`
}

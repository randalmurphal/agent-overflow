package aocli

import (
	"bytes"
	"encoding/json"
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

	t.Run("validate", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := Run([]string{"workflow", "validate", "--config-root", configRoot, "--project", "sample", "--id", "valid", "--json"}, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
		}
		var result struct {
			BindingStatus string            `json:"bindingStatus"`
			Findings      []json.RawMessage `json:"findings"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.BindingStatus != "checked" || len(result.Findings) != 0 {
			t.Fatalf("validation result = %+v", result)
		}
	})

	t.Run("list", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := Run([]string{"workflow", "list", "--config-root", configRoot, "--project", "sample", "--json"}, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("Run exit code = %d; stderr=%q", code, stderr.String())
		}
		var entries []listEntry
		if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].BindingStatus != "checked" || len(entries[0].Findings) != 0 {
			t.Fatalf("list entries = %+v", entries)
		}
	})
}

func TestAbsentProjectProfileLeavesBindingsUnchecked(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	mustMkdirAll(t, sharedDir)
	mustWriteFile(t, filepath.Join(sharedDir, "valid.yaml"), workflowYAML("valid", "Valid workflow"))

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "validate", "--config-root", configRoot, "--project", "sample", "--id", "valid"}, &stdout, &stderr)
	if code != exitOK || !strings.Contains(stdout.String(), "bindings: unchecked") {
		t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestMalformedProjectProfileIsOperationalError(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	profilePath := filepath.Join(configRoot, "projects", "sample", "profile.yaml")
	mustMkdirAll(t, sharedDir)
	mustMkdirAll(t, filepath.Dir(profilePath))
	mustWriteFile(t, filepath.Join(sharedDir, "valid.yaml"), workflowYAML("valid", "Valid workflow"))
	mustWriteFile(t, profilePath, "unknown: true\n")

	for _, command := range [][]string{
		{"workflow", "validate", "--config-root", configRoot, "--project", "sample", "--id", "valid"},
		{"workflow", "list", "--config-root", configRoot, "--project", "sample"},
	} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := Run(command, &stdout, &stderr); code != exitError {
			t.Fatalf("Run(%v) exit code = %d; stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), profilePath) || !strings.Contains(stderr.String(), "unknown") {
			t.Errorf("Run(%v) stderr = %q, want profile path and parse failure", command, stderr.String())
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

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "validate", "--config-root", configRoot, "--project", "sample", "--id", "valid"}, &stdout, &stderr)
	if code != exitFindings || !strings.Contains(stdout.String(), "binding.check") || !strings.Contains(stdout.String(), "bindings: checked") {
		t.Fatalf("Run exit code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// TestFanOutWidthReportIsInformational pins decision D19's dry-run half: a
// static fan-out wider than the provider capacity it will actually get is
// reported, but the workflow stays valid — the run throttles, it does not break.
func TestFanOutWidthReportIsInformational(t *testing.T) {
	configRoot := t.TempDir()
	sharedDir := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", "sample")
	mustMkdirAll(t, sharedDir)
	mustMkdirAll(t, projectDir)
	mustWriteFile(t, filepath.Join(sharedDir, "wide.yaml"), fanOutWorkflowYAML())
	for _, prompt := range []string{"work.md", "left.md", "right.md", "merge.md"} {
		mustWriteFile(t, filepath.Join(sharedDir, prompt), "do the work\n")
	}
	mustWriteFile(t, filepath.Join(projectDir, "profile.yaml"), "capacities:\n  \"provider:claude\": 1\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"workflow", "validate", "--config-root", configRoot, "--project", "sample", "--id", "wide"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run exit code = %d, want %d; stdout=%q stderr=%q", code, exitOK, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "note: ") || !strings.Contains(stdout.String(), "fan-out.width") {
		t.Fatalf("stdout = %q, want an informational width note", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"workflow", "validate", "--config-root", configRoot, "--project", "sample", "--id", "wide", "--json"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run --json exit code = %d; stderr=%q", code, stderr.String())
	}
	var result struct {
		Findings []json.RawMessage `json:"findings"`
		Reports  []struct {
			Code string `json:"code"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 0 || len(result.Reports) != 1 || result.Reports[0].Code != "fan-out.width" {
		t.Fatalf("validation result = %+v, want one report and no findings", result)
	}
}

func fanOutWorkflowYAML() string {
	return `id: wide
name: Wide fan-out
phases:
  - id: work
    driver: agent
    shape: fan-out
    provider: claude
    model: sonnet
    prompt: work.md
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

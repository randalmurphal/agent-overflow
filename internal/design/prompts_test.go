package design

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDesignSystemPromptReturnsDefault(t *testing.T) {
	got := LoadDesignSystemPrompt(t.TempDir(), "")

	if got != defaultDesignSystemPrompt {
		t.Fatalf("LoadDesignSystemPrompt() = %q, want default prompt", got)
	}
	if !strings.Contains(got, "get_design_diagnostics") {
		t.Fatal("default prompt should mention get_design_diagnostics")
	}
	if !strings.Contains(got, "read_screenshot") {
		t.Fatal("default prompt should mention read_screenshot")
	}
	if !strings.Contains(got, "Anti-slop") {
		t.Fatal("default prompt should include the anti-slop section")
	}
	if strings.Contains(got, "# Project context") {
		t.Fatal("empty projectPath should NOT append project-context section")
	}
}

func TestLoadDesignSystemPromptUsesOverride(t *testing.T) {
	configDir := t.TempDir()
	overrideDir := filepath.Join(configDir, "prompts")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	override := "custom design prompt\n"
	overridePath := filepath.Join(overrideDir, designPromptOverrideName)
	if err := os.WriteFile(overridePath, []byte(override), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got := LoadDesignSystemPrompt(configDir, "")
	if got != override {
		t.Fatalf("LoadDesignSystemPrompt() = %q, want %q", got, override)
	}
}

// TestLoadDesignSystemPromptAppendsProjectContext pins the load-bearing
// behavior for the agent's reference scope: when a project workspace
// path is known, the system prompt MUST disclose it so the agent knows
// it can read existing components/colors/typography from that absolute
// path. Without it the agent's CWD (the per-thread design workdir) is
// the only thing it sees, and it has no idea a project repo even
// exists.
func TestLoadDesignSystemPromptAppendsProjectContext(t *testing.T) {
	projectPath := "/home/user/repos/example-app"
	got := LoadDesignSystemPrompt(t.TempDir(), projectPath)

	if !strings.HasPrefix(got, defaultDesignSystemPrompt) {
		t.Fatal("project-context prompt should still start with the base default prompt")
	}
	if !strings.Contains(got, "# Project context") {
		t.Fatal("non-empty projectPath should append the Project context section")
	}
	if !strings.Contains(got, projectPath) {
		t.Fatalf("project context should include the absolute project path %q", projectPath)
	}
	if !strings.Contains(got, "Never write into the\nproject repo") {
		t.Fatal("project context should warn against writing into the project repo")
	}
}

// TestLoadDesignSystemPromptAppendsProjectContextAfterOverride pins
// that user-level overrides still receive the project-context suffix.
// The override controls stylistic guidance; the project-context block
// is structural information about the runtime environment and must
// stay attached even when the prompt body is replaced.
func TestLoadDesignSystemPromptAppendsProjectContextAfterOverride(t *testing.T) {
	configDir := t.TempDir()
	overrideDir := filepath.Join(configDir, "prompts")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	override := "custom design prompt\n"
	overridePath := filepath.Join(overrideDir, designPromptOverrideName)
	if err := os.WriteFile(overridePath, []byte(override), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	projectPath := "/home/user/repos/example-app"
	got := LoadDesignSystemPrompt(configDir, projectPath)

	if !strings.HasPrefix(got, override) {
		t.Fatal("override + projectPath should still start with the override body")
	}
	if !strings.Contains(got, "# Project context") {
		t.Fatal("override + projectPath should still get the Project context section")
	}
	if !strings.Contains(got, projectPath) {
		t.Fatalf("project context should include the absolute project path %q", projectPath)
	}
}

// TestLoadDesignSystemPromptIgnoresWhitespaceOnlyProjectPath pins that
// a workspace path that's only whitespace is treated as absent — the
// designSessionConfig caller passes thread.WorkspacePath unfiltered,
// and an unset path occasionally serializes to " " or "\n" depending
// on storage layer round-trips.
func TestLoadDesignSystemPromptIgnoresWhitespaceOnlyProjectPath(t *testing.T) {
	got := LoadDesignSystemPrompt(t.TempDir(), "   \n\t  ")

	if got != defaultDesignSystemPrompt {
		t.Fatalf("whitespace-only projectPath should be treated as empty; got %d extra chars",
			len(got)-len(defaultDesignSystemPrompt))
	}
}

package design

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDesignSystemPromptReturnsDefault(t *testing.T) {
	got := LoadDesignSystemPrompt(t.TempDir())

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

	got := LoadDesignSystemPrompt(configDir)
	if got != override {
		t.Fatalf("LoadDesignSystemPrompt() = %q, want %q", got, override)
	}
}

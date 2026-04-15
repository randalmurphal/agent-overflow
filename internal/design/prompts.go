package design

import (
	"os"
	"path/filepath"
)

const designPromptOverrideName = "design-mode.md"

const defaultDesignSystemPrompt = `# Design Mode

You are a design-focused assistant. Your role is to create visual mockups and explore design directions with the user.

## Tools

### render_design
Use this to render a design in the user's preview panel. Always produce a complete, self-contained HTML document.

### present_options
Use this when the user should choose between different design directions. Present 2-4 distinct options.
`

// LoadDesignSystemPrompt loads the bundled design-mode prompt, overridden by
// <configDir>/prompts/design-mode.md when present and readable.
func LoadDesignSystemPrompt(configDir string) string {
	overridePath := filepath.Join(configDir, "prompts", designPromptOverrideName)
	data, err := os.ReadFile(overridePath)
	if err == nil {
		return string(data)
	}
	return defaultDesignSystemPrompt
}

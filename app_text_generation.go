package main

import (
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/textgen"
)

// resolveTextGenerationConfig assembles the textgen.Config for the
// caller's chosen provider. The CLI executor seam (a.textGenerationExecutor)
// is wired here so production calls go through textgen.ExecCLI while tests
// can inject a fake.
func (a *App) resolveTextGenerationConfig() textgen.Config {
	s := a.currentSettings()
	providerKind := strings.TrimSpace(s.TextGenerationProvider)
	if providerKind == "" {
		providerKind = settings.DefaultSettings.TextGenerationProvider
	}

	execFn := a.textGenerationExecutor
	if execFn == nil {
		execFn = textgen.ExecCLI
	}

	cfg := textgen.Config{
		Provider: providerKind,
		Effort:   strings.TrimSpace(s.TextGenerationReasoningEffort),
		Exec:     execFn,
	}

	switch providerKind {
	case string(provider.Codex):
		cfg.Binary = a.providerBinaryPath(string(provider.Codex))
		cfg.Model = strings.TrimSpace(s.TextGenerationModel)
		if cfg.Model == "" {
			cfg.Model = textgen.DefaultCodexModel
		}
		if cfg.Effort == "" {
			cfg.Effort = settings.DefaultSettings.TextGenerationReasoningEffort
		}
	case string(provider.Claude):
		cfg.Binary = a.providerBinaryPath(string(provider.Claude))
		cfg.Model = strings.TrimSpace(s.TextGenerationModel)
		if cfg.Model == "" {
			cfg.Model = textgen.DefaultClaudeModel
		}
	}

	return cfg
}

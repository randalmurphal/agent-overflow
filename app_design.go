package main

import (
	"fmt"

	"agent-overflow/internal/design"
	"agent-overflow/internal/store"
)

type designSessionConfig struct {
	Prompt     string
	MCPServers map[string]any
}

func (a *App) designSessionConfig(thread store.Thread) (designSessionConfig, error) {
	if thread.Mode != "design" {
		return designSessionConfig{}, nil
	}

	cfg := designSessionConfig{
		Prompt: design.LoadDesignSystemPrompt(a.configDir),
	}
	if thread.Provider != "codex" {
		return cfg, nil
	}
	if a.designMCP == nil {
		return designSessionConfig{}, fmt.Errorf("design MCP server unavailable")
	}

	servers, err := a.designMCP.RegisterThread(thread.ID)
	if err != nil {
		return designSessionConfig{}, err
	}
	cfg.MCPServers = servers
	return cfg, nil
}

func (a *App) teardownDesignThread(threadID string) {
	if a.reactor != nil {
		a.reactor.TeardownThread(threadID)
	}
	if a.designMCP != nil {
		a.designMCP.UnregisterThread(threadID)
	}
}

// ListDesignArtifacts returns persisted design artifacts for a thread.
func (a *App) ListDesignArtifacts(threadID string) ([]design.DesignArtifact, error) {
	if a.artifacts == nil {
		return nil, fmt.Errorf("design artifact store unavailable")
	}
	return a.artifacts.List(threadID, "")
}

// GetDesignArtifactHTML returns the stored HTML for an artifact.
func (a *App) GetDesignArtifactHTML(threadID, artifactID string) (string, error) {
	if a.artifacts == nil {
		return "", fmt.Errorf("design artifact store unavailable")
	}
	return a.artifacts.Get(threadID, artifactID)
}

// ChooseDesignOption resolves a pending design-choice request.
func (a *App) ChooseDesignOption(threadID, requestID, optionID string) error {
	if a.reactor == nil {
		return fmt.Errorf("design reactor unavailable")
	}
	return a.reactor.ChooseOption(threadID, requestID, optionID)
}

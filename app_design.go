package main

import (
	"encoding/base64"
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

// SaveDesignArtifactPng persists a base64-encoded PNG capture next to an
// artifact's HTML on disk. The frontend listens for design:artifact events,
// runs captureHtmlToPng at desktop viewport, and uploads the result here so
// the bundled "Send to chat" handoffs can attach the rendered image without
// re-running the capture each time.
func (a *App) SaveDesignArtifactPng(threadID, artifactID string, b64 string) error {
	if a.artifacts == nil {
		return fmt.Errorf("design artifact store unavailable")
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode png base64: %w", err)
	}
	return a.artifacts.SavePNG(threadID, artifactID, data)
}

// GetDesignArtifactPng returns a base64-encoded PNG capture of an artifact, or
// the empty string if no PNG has been saved yet (capture failed, was skipped,
// or hasn't run for this artifact). The frontend uses this to attach the
// rendered image when handing off a design to a chat thread.
func (a *App) GetDesignArtifactPng(threadID, artifactID string) (string, error) {
	if a.artifacts == nil {
		return "", fmt.Errorf("design artifact store unavailable")
	}
	data, err := a.artifacts.GetPNG(threadID, artifactID)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

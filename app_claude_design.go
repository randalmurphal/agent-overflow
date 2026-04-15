package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/design"
	"agent-overflow/internal/provider"
)

const (
	claudeDesignRenderTool  = "render_design"
	claudeDesignOptionsTool = "present_options"
)

type providerToolStartMeta struct {
	ToolName string          `json:"toolName"`
	Input    json.RawMessage `json:"input"`
}

func (a *App) handleClaudeDesignTool(evt provider.ProviderEvent) {
	if evt.Kind != provider.EventToolStart || a.reactor == nil || a.store == nil {
		return
	}

	toolName := strings.TrimSpace(evt.ItemType)
	if toolName != claudeDesignRenderTool && toolName != claudeDesignOptionsTool {
		return
	}

	thread, err := a.store.GetThread(evt.ThreadID)
	if err != nil {
		log.Printf("design runtime: load thread %s: %v", evt.ThreadID, err)
		return
	}
	if thread.Provider != string(provider.Claude) || thread.InteractionMode != "design" {
		return
	}

	var meta providerToolStartMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		a.emitProviderErrorEvent(evt.ThreadID, fmt.Sprintf("design tool %s: invalid input: %v", toolName, err))
		return
	}
	if len(meta.Input) == 0 {
		a.emitProviderErrorEvent(evt.ThreadID, fmt.Sprintf("design tool %s: missing input", toolName))
		return
	}

	switch toolName {
	case claudeDesignRenderTool:
		go a.runClaudeDesignRender(evt.ThreadID, meta.Input)
	case claudeDesignOptionsTool:
		go a.runClaudeDesignOptions(evt.ThreadID, meta.Input)
	}
}

func (a *App) runClaudeDesignRender(threadID string, rawInput json.RawMessage) {
	var input design.RenderInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		a.emitProviderErrorEvent(threadID, fmt.Sprintf("design tool %s: %v", claudeDesignRenderTool, err))
		return
	}
	if _, err := a.reactor.Render(threadID, input); err != nil {
		a.emitProviderErrorEvent(threadID, fmt.Sprintf("design tool %s: %v", claudeDesignRenderTool, err))
	}
}

func (a *App) runClaudeDesignOptions(threadID string, rawInput json.RawMessage) {
	var input design.PresentOptionsInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		a.emitProviderErrorEvent(threadID, fmt.Sprintf("design tool %s: %v", claudeDesignOptionsTool, err))
		return
	}

	result, err := a.reactor.PresentOptions(context.Background(), threadID, input)
	if err != nil {
		if err.Error() != "design mode session ended" {
			a.emitProviderErrorEvent(threadID, fmt.Sprintf("design tool %s: %v", claudeDesignOptionsTool, err))
		}
		return
	}

	if err := a.sendMessage(threadID, formatClaudeDesignChoice(result)); err != nil {
		a.emitProviderErrorEvent(threadID, fmt.Sprintf("design option selection: %v", err))
	}
}

func (a *App) emitProviderErrorEvent(threadID, content string) {
	evt := provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  threadID,
		Content:   content,
		Timestamp: time.Now(),
	}

	if a.triage != nil {
		if err := a.triage.Handle(evt); err != nil {
			log.Printf("provider error emit for %s: %v", threadID, err)
		}
		return
	}
	if a.app != nil {
		a.app.Event.Emit("provider:event", evt)
	}
}

func formatClaudeDesignChoice(result design.ChoiceResult) string {
	if title := strings.TrimSpace(result.Title); title != "" {
		return fmt.Sprintf("The user chose design option %q (ID: %s). Continue from that selection.", title, result.Chosen)
	}
	return fmt.Sprintf("The user chose design option %s. Continue from that selection.", result.Chosen)
}

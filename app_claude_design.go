package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/design"
	"agent-overflow/internal/provider"
)

type providerToolStartMeta struct {
	ToolName string          `json:"toolName"`
	Input    json.RawMessage `json:"input"`
}

// handleClaudeDesignTool intercepts Claude EventToolStart events for the
// design-mode tools and routes them through the unified design reactor. The
// reactor dispatch is identical to the path the Codex MCP server runs — the
// fork between providers is just how the tool call surfaces (event vs. MCP
// HTTP request), not what the call does.
func (a *App) handleClaudeDesignTool(evt provider.ProviderEvent) {
	if evt.Kind != provider.EventToolStart || a.reactor == nil || a.store == nil {
		return
	}

	toolName := strings.TrimSpace(evt.ItemType)
	if toolName != design.ToolRenderDesign && toolName != design.ToolPresentOptions {
		return
	}

	thread, err := a.store.GetThread(evt.ThreadID)
	if err != nil {
		log.Printf("design runtime: load thread %s: %v", evt.ThreadID, err)
		return
	}
	if thread.Provider != string(provider.Claude) || thread.Mode != "design" {
		return
	}

	var meta providerToolStartMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		a.emitErrorToThread(evt.ThreadID, fmt.Sprintf("design tool %s: invalid input: %v", toolName, err))
		return
	}
	if len(meta.Input) == 0 {
		a.emitErrorToThread(evt.ThreadID, fmt.Sprintf("design tool %s: missing input", toolName))
		return
	}

	go a.runClaudeDesignTool(evt.ThreadID, toolName, meta.Input)
}

// runClaudeDesignTool dispatches a Claude design tool call asynchronously. The
// goroutine survives until the reactor returns a result, the user picks an
// option, or teardownDesignThread cancels the pending request. The hardcoded
// 30-minute timeout was removed: TeardownThread is the single in-band cancel
// path keyed by thread id, so a stale goroutine cannot outlive the session.
func (a *App) runClaudeDesignTool(threadID, toolName string, rawInput json.RawMessage) {
	result, err := a.reactor.Dispatch(context.Background(), threadID, toolName, rawInput)
	if err != nil {
		if errors.Is(err, design.ErrDesignSessionEnded) {
			return
		}
		a.emitErrorToThread(threadID, fmt.Sprintf("design tool %s: %v", toolName, err))
		return
	}

	// For present_options we have to feed the user's choice back to Claude so
	// the model continues from the selection. Until the Claude provider grows
	// MCP server support (parity with Codex), the only available channel is a
	// follow-up message via sendMessage. The text format is documented as a
	// known artifact in the transcript — replacing this with a proper
	// tool-result block requires registering the design tools as an MCP
	// server in the Claude session config, which is a separate task.
	if toolName == design.ToolPresentOptions {
		choice, ok := result.Payload.(design.ChoiceResult)
		if !ok {
			return
		}
		if err := a.sendMessage(threadID, formatClaudeDesignChoice(choice), nil); err != nil {
			a.emitErrorToThread(threadID, fmt.Sprintf("design option selection: %v", err))
		}
	}
}

func formatClaudeDesignChoice(result design.ChoiceResult) string {
	if title := strings.TrimSpace(result.Title); title != "" {
		return fmt.Sprintf("The user chose design option %q (ID: %s). Continue from that selection.", title, result.Chosen)
	}
	return fmt.Sprintf("The user chose design option %s. Continue from that selection.", result.Chosen)
}

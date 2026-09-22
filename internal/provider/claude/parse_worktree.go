package claude

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// EnterWorktree / ExitWorktree are the CLI's own mid-session working-directory
// moves (claude-wire.md §E10). The assistant tool_use records the call here;
// the user tool_result reads the structured `tool_use_result` sibling and
// emits EventWorkspaceChanged beside the ordinary EventToolComplete.
//
// Only the top-level agent's moves become workspace changes: a subagent's
// EnterWorktree (or an Agent launched with `isolation: "worktree"`) scopes
// the move to that agent and leaves the session's directory alone.

const (
	enterWorktreeToolName = "EnterWorktree"
	exitWorktreeToolName  = "ExitWorktree"
)

// worktreeToolUse is the assistant-side record of an in-flight worktree
// tool call, keyed by tool_use id until its result arrives.
type worktreeToolUse struct {
	name            string
	parentToolUseID string
}

func isWorktreeToolName(name string) bool {
	switch name {
	case enterWorktreeToolName, exitWorktreeToolName:
		return true
	default:
		return false
	}
}

func (p *Parser) markWorktreeTool(toolUseID, name, parentToolUseID string) {
	if toolUseID == "" {
		return
	}
	if p.worktreeToolUses == nil || len(p.worktreeToolUses) >= parserTaskMapCap {
		p.worktreeToolUses = make(map[string]worktreeToolUse)
	}
	p.worktreeToolUses[toolUseID] = worktreeToolUse{name: name, parentToolUseID: parentToolUseID}
}

// takeWorktreeTool releases the record for toolUseID: a result is a
// one-shot, so the map stays bounded across a long session.
func (p *Parser) takeWorktreeTool(toolUseID string) (worktreeToolUse, bool) {
	if toolUseID == "" || p.worktreeToolUses == nil {
		return worktreeToolUse{}, false
	}
	use, ok := p.worktreeToolUses[toolUseID]
	if ok {
		delete(p.worktreeToolUses, toolUseID)
	}
	return use, ok
}

// enterWorktreeResult mirrors the CLI's EnterWorktree output schema:
// `{worktreePath, worktreeBranch?, message}` (bundle-read 2.1.257; the
// message wording varies: Created / Entered / Resumed / Reused).
type enterWorktreeResult struct {
	WorktreePath   string `json:"worktreePath"`
	WorktreeBranch string `json:"worktreeBranch"`
}

// exitWorktreeResult mirrors the CLI's ExitWorktree output schema:
// `{action, originalCwd, worktreePath, worktreeBranch?, ..., message}`.
type exitWorktreeResult struct {
	Action         string `json:"action"`
	OriginalCwd    string `json:"originalCwd"`
	WorktreePath   string `json:"worktreePath"`
	WorktreeBranch string `json:"worktreeBranch"`
}

// appendWorkspaceChangeEvent turns a completed worktree tool call into the
// session-level EventWorkspaceChanged. A refused call (`is_error`) moved
// nothing. A result whose structured sibling is missing or does not name
// the directory is reported as an EventError rather than guessed from the
// message text: the thread would otherwise silently keep the wrong
// workspace, which is exactly the defect this path exists to close.
func (p *Parser) appendWorkspaceChangeEvent(
	events []provider.ProviderEvent,
	threadID string,
	now time.Time,
	toolUseID string,
	use worktreeToolUse,
	block map[string]json.RawMessage,
	toolUseResultRaw json.RawMessage,
) []provider.ProviderEvent {
	if use.parentToolUseID != "" {
		return events
	}
	var isError bool
	if v, ok := block["is_error"]; ok {
		_ = json.Unmarshal(v, &isError)
	}
	if isError {
		return events
	}
	meta, problem := decodeWorkspaceChange(use.name, toolUseResultRaw)
	if problem != "" {
		return append(events, provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			ItemID:    toolUseID,
			Content:   "Claude changed its working directory with " + use.name + ", but the result did not say where: " + problem + ". The thread still shows its previous workspace.",
			Timestamp: now,
		})
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return append(events, provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			ItemID:    toolUseID,
			Content:   "Claude changed its working directory with " + use.name + ", but AO could not record it: " + err.Error(),
			Timestamp: now,
		})
	}
	return append(events, provider.ProviderEvent{
		Kind:      provider.EventWorkspaceChanged,
		ThreadID:  threadID,
		ItemID:    toolUseID,
		Meta:      encoded,
		Timestamp: now,
	})
}

// decodeWorkspaceChange reads the tool's structured result into the
// provider-neutral meta. The returned problem is non-empty when the result
// cannot name the new working directory.
func decodeWorkspaceChange(tool string, raw json.RawMessage) (provider.WorkspaceChangeMeta, string) {
	if len(raw) == 0 {
		return provider.WorkspaceChangeMeta{}, "no structured tool_use_result on the wire"
	}
	switch tool {
	case enterWorktreeToolName:
		var result enterWorktreeResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return provider.WorkspaceChangeMeta{}, "tool_use_result is not the EnterWorktree shape (" + err.Error() + ")"
		}
		path := strings.TrimSpace(result.WorktreePath)
		if path == "" {
			return provider.WorkspaceChangeMeta{}, "tool_use_result.worktreePath is empty"
		}
		return provider.WorkspaceChangeMeta{
			Tool:         tool,
			Cwd:          path,
			WorktreePath: path,
			Branch:       strings.TrimSpace(result.WorktreeBranch),
		}, ""
	case exitWorktreeToolName:
		var result exitWorktreeResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return provider.WorkspaceChangeMeta{}, "tool_use_result is not the ExitWorktree shape (" + err.Error() + ")"
		}
		cwd := strings.TrimSpace(result.OriginalCwd)
		if cwd == "" {
			return provider.WorkspaceChangeMeta{}, "tool_use_result.originalCwd is empty"
		}
		return provider.WorkspaceChangeMeta{
			Tool:            tool,
			Cwd:             cwd,
			WorktreePath:    strings.TrimSpace(result.WorktreePath),
			Branch:          strings.TrimSpace(result.WorktreeBranch),
			RemovedWorktree: strings.TrimSpace(result.Action) == "remove",
		}, ""
	default:
		return provider.WorkspaceChangeMeta{}, "unknown worktree tool " + tool
	}
}

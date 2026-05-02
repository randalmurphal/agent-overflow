// Tool-path extraction. Triage records the workspace-relative paths
// each agent tool wrote during a turn so the per-turn checkpoint can
// later be reverted in a path-scoped fashion (only paths the agent
// touched, leaving manual edits to unrelated files alone).
//
// The two providers expose tool inputs differently, so each branch
// parses the wire-shaped Meta against the contract documented in
// internal/provider/{claude,codex}/CLAUDE.md. Bash and read-only tools
// (Read, Grep, etc.) are intentionally untracked — they don't write
// files, and Bash side effects are out of scope for path-scoped revert.
//
// Extraction returns raw, as-given paths (often absolute). Workspace
// normalization runs once at checkpoint-capture time so the router's
// hot path doesn't look the thread up on every tool boundary.
//
// On the "Do NOT reach back into provider-specific types" rule in
// internal/triage/CLAUDE.md: the rule is upheld here in spirit — no
// provider-specific Go types are imported. This file inspects
// `json.RawMessage` Meta only, the same pattern used by
// tool_result_file_change.go (which parses the same Codex
// `params.item.changes[]` shape) and codex_background.go (which parses
// Codex's `unifiedExec` envelopes). Promoting `ToolPaths` onto
// `ProviderEvent` would diverge from that pattern and carry an empty
// slice on every event in steady state for one persistence consumer.

package triage

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/provider"
)

// claudeFilePathTools is the subset of Claude SDK tools whose `input.file_path`
// is the workspace path written by the tool. ExitPlanMode and Bash are
// intentionally absent — the former never writes a file, the latter is
// out of scope per the design.
var claudeFilePathTools = map[string]struct{}{
	"Edit":         {},
	"Write":        {},
	"MultiEdit":    {},
	"NotebookEdit": {},
}

// extractToolPaths returns the raw paths the tool will write (or has
// written) based on the EventToolStart Meta payload. Returns nil if the
// tool is out of scope (Bash, Read, etc.) or the payload doesn't carry a
// path. The returned paths can be absolute or relative — workspace
// normalization happens at checkpoint capture time via
// normalizeWorkspaceRelativePath.
func extractToolPaths(evt provider.ProviderEvent) []string {
	if len(evt.Meta) == 0 {
		return nil
	}
	switch {
	case isClaudeFilePathTool(evt.ItemType):
		return claudeToolPaths(evt.Meta)
	case isCodexFileChangeItem(evt.ItemType):
		return codexFileChangePaths(evt.Meta)
	}
	return nil
}

// isClaudeFilePathTool reports whether the given item type is one of
// Claude's file-edit tools (Edit / Write / MultiEdit / NotebookEdit)
// — the same set drives both path tracking and file_change tool_result
// dispatch.
func isClaudeFilePathTool(itemType string) bool {
	_, ok := claudeFilePathTools[itemType]
	return ok
}

func isCodexFileChangeItem(itemType string) bool {
	// Codex emits both `fileChange` (v2) and `file_change` (legacy) — handle
	// both so a wire-format change in upstream doesn't silently drop tracking.
	return itemType == "fileChange" || itemType == "file_change"
}

// isFileChangeItemType is the unified predicate for the file_change
// tool_result extractor. Both providers route through
// persistFileChangeToolResult; this is the gate that determines
// whether to attempt extraction at all. Codex stamps `fileChange` /
// `file_change` directly on EventToolStart's ItemType; Claude stamps
// the tool name (`Edit`, etc.). The dispatcher in tool_result_file_change.go
// uses this predicate AFTER resolving an empty ItemType from the
// persisted launch row's ToolName.
func isFileChangeItemType(itemType string) bool {
	return isCodexFileChangeItem(itemType) || isClaudeFilePathTool(itemType)
}

// claudeToolPaths reads the file_path field from a Claude tool_use Meta.
// The shape is `{"toolName":"Edit","input":{"file_path":"..."}}` per
// internal/provider/claude/parse_assistant.go marshalToolMeta.
func claudeToolPaths(meta json.RawMessage) []string {
	var envelope struct {
		Input struct {
			FilePath string `json:"file_path"`
		} `json:"input"`
	}
	if err := json.Unmarshal(meta, &envelope); err != nil {
		return nil
	}
	if envelope.Input.FilePath == "" {
		return nil
	}
	return []string{envelope.Input.FilePath}
}

// codexFileChangePaths reads the structured `changes[]` array on a Codex
// fileChange item. Wire shape (v2): `{ "item": { "changes": [
//
//	{ "path": "...", "kind": {"type": "add"|"delete"} },
//	{ "path": "...", "kind": {"type": "update", "move_path": "..."|null} },
//	... ] } }`. Update entries with `move_path` set track BOTH the original
//
// path (so the old file is removed) and the move target (so the new file is
// restored).
func codexFileChangePaths(meta json.RawMessage) []string {
	var envelope struct {
		Item struct {
			Changes []struct {
				Path string `json:"path"`
				Kind struct {
					Type     string `json:"type"`
					MovePath string `json:"move_path"`
				} `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
	}
	if err := json.Unmarshal(meta, &envelope); err != nil {
		return nil
	}
	if len(envelope.Item.Changes) == 0 {
		return nil
	}
	out := make([]string, 0, len(envelope.Item.Changes))
	for _, change := range envelope.Item.Changes {
		if change.Path != "" {
			out = append(out, change.Path)
		}
		if change.Kind.Type == "update" && change.Kind.MovePath != "" {
			out = append(out, change.Kind.MovePath)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toolCallSucceeded inspects an EventToolComplete Meta to decide whether the
// tool actually wrote (Claude `is_error: false`, Codex `item_status:
// "completed"`). Failed/rejected tools drop their staged paths so a
// rejected Edit doesn't get reverted later.
func toolCallSucceeded(evt provider.ProviderEvent) bool {
	if len(evt.Meta) == 0 {
		// No meta = no signal to drop on; assume success so we don't lose
		// a tracked path because of a wire-format change.
		return true
	}
	var probe struct {
		IsError    bool   `json:"is_error"`
		ItemStatus string `json:"item_status"`
	}
	if err := json.Unmarshal(evt.Meta, &probe); err != nil {
		return true
	}
	if probe.IsError {
		return false
	}
	switch probe.ItemStatus {
	case "", "completed":
		return true
	default:
		// Codex non-completed statuses include "failed", "cancelled",
		// "incomplete". None of these mean the file was successfully
		// written — drop the staged path.
		return false
	}
}

// normalizeWorkspaceRelativePaths normalizes raw tool paths into the
// deduped workspace-relative slice that gets persisted on the checkpoint
// row. Paths that resolve outside the workspace are dropped; absolute
// paths inside the workspace are converted to relative form. Result order
// is sorted ascending so the persisted JSON is deterministic across
// captures.
func normalizeWorkspaceRelativePaths(raw []string, workspace string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	for _, p := range raw {
		rel := normalizeWorkspaceRelativePath(p, workspace)
		if rel == "" {
			continue
		}
		seen[rel] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// normalizeDisplayPath is the display-friendly path normalizer used
// by the file_change tool_result extractors. Returns:
//   - the workspace-relative form when the path is inside the
//     workspace (e.g. `src/app.ts` rather than the full absolute)
//   - the cleaned absolute path when it's outside the workspace —
//     this lets diffs against test files (e.g. `/tmp/scratch.txt`)
//     still render with their actual location instead of being
//     silently dropped
//   - "" only for empty input or paths with embedded control bytes
//
// Unlike normalizeWorkspaceRelativePath (used for `committedToolPaths`
// + checkpoint revert, which IS workspace-scoped by design), this
// preserves outside-workspace paths because diff display is not.
// The `.git` rejection and pathspec-magic guards from the strict
// variant don't apply here: those exist to defend against a
// malicious agent corrupting THIS repo's git state, which only
// matters for paths inside the workspace.
func normalizeDisplayPath(path, workspace string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	// Same control-byte rejection as the strict variant — NUL would
	// fragment paths in shell-adjacent tooling, other low control
	// bytes are functionally never legitimate filenames.
	for i := 0; i < len(path); i++ {
		if path[i] < 0x20 {
			return ""
		}
	}
	if filepath.IsAbs(path) {
		// Prefer workspace-relative when the path is inside the
		// workspace; otherwise keep it absolute so users see the
		// actual file location.
		if workspace != "" {
			if rel, err := filepath.Rel(workspace, path); err == nil {
				relSlash := filepath.ToSlash(rel)
				if relSlash != ".." && relSlash != "." && !strings.HasPrefix(relSlash, "../") {
					return relSlash
				}
			}
		}
		return filepath.ToSlash(filepath.Clean(path))
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == "" {
		return ""
	}
	return clean
}

func normalizeWorkspaceRelativePath(path, workspace string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	// Reject control bytes (NUL through 0x1F). os/exec rejects argv with
	// embedded NULs, JSON round-trips them as NUL, and ls-tree's -z
	// output is NUL-delimited — a smuggled NUL would silently fragment a
	// path. Other control bytes are functionally never legitimate
	// filenames and would bork shell-adjacent tooling that touches the
	// path later (status displays, log lines).
	for i := 0; i < len(path); i++ {
		if path[i] < 0x20 {
			return ""
		}
	}
	if filepath.IsAbs(path) {
		if workspace == "" {
			return ""
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return ""
		}
		path = rel
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == "" {
		return ""
	}
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return ""
	}
	// Reject `.git` and anything inside it. The provider trust boundary
	// makes a malicious `.git/config` or `.git/hooks/pre-commit` write a
	// real attack: capture stages it via `git add -A`, restore writes it
	// back, RCE on next commit. Defense-in-depth alongside the tool
	// allow-list.
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return ""
	}
	// Reject git pathspec magic prefix `:`. Even with `--`, some git
	// subcommands still parse `:!exclude`, `:(literal)`, etc., which
	// either crash the partition step (ls-tree refuses unknown magic) or
	// silently change the operation's scope.
	if clean[0] == ':' {
		return ""
	}
	return clean
}

// File-tool predicates and path normalizers shared by the file_change
// tool_result dispatch and the command inline-diff pipeline.
//
// The two providers expose tool inputs differently, so each predicate
// matches the wire-shaped ItemType against the contract documented in
// internal/provider/{claude,codex}/CLAUDE.md.
//
// On the "Do NOT reach back into provider-specific types" rule in
// internal/triage/CLAUDE.md: the rule is upheld here in spirit — no
// provider-specific Go types are imported; these helpers inspect item
// types and paths only.

package triage

import (
	"path/filepath"
	"strings"
)

// claudeFilePathTools is the subset of Claude SDK tools whose `input.file_path`
// is the workspace path written by the tool. ExitPlanMode and Bash are
// intentionally absent — the former never writes a file, the latter
// doesn't carry a structured path.
var claudeFilePathTools = map[string]struct{}{
	"Edit":         {},
	"Write":        {},
	"MultiEdit":    {},
	"NotebookEdit": {},
}

// IsClaudeFilePathTool reports whether the given item type is one of
// Claude's file-edit tools (Edit / Write / MultiEdit / NotebookEdit)
// — the set that drives file_change tool_result dispatch.
func IsClaudeFilePathTool(itemType string) bool {
	_, ok := claudeFilePathTools[itemType]
	return ok
}

func isCodexFileChangeItem(itemType string) bool {
	// Codex emits both `fileChange` (v2) and `file_change` (legacy) — handle
	// both so a wire-format change in upstream doesn't silently drop dispatch.
	return itemType == "fileChange" || itemType == "file_change"
}

// IsFileChangeItemType is the unified predicate for the file_change
// tool_result extractor. Both providers route through
// persistFileChangeToolResult; this is the gate that determines
// whether to attempt extraction at all. Codex stamps `fileChange` /
// `file_change` directly on EventToolStart's ItemType; Claude stamps
// the tool name (`Edit`, etc.). The dispatcher in tool_result_file_change.go
// uses this predicate AFTER resolving an empty ItemType from the
// persisted launch row's ToolName.
func IsFileChangeItemType(itemType string) bool {
	return isCodexFileChangeItem(itemType) || IsClaudeFilePathTool(itemType)
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
// Unlike normalizeWorkspaceRelativePath (which IS workspace-scoped by
// design), this preserves outside-workspace paths because diff display
// is not. The `.git` rejection and pathspec-magic guards from the
// strict variant don't apply here: those exist to defend against a
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
	// real attack for any downstream consumer that acts on the path.
	// Defense-in-depth alongside the tool allow-list.
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return ""
	}
	// Reject git pathspec magic prefix `:`. Even with `--`, some git
	// subcommands still parse `:!exclude`, `:(literal)`, etc., which
	// either crash the consumer (ls-tree refuses unknown magic) or
	// silently change the operation's scope.
	if clean[0] == ':' {
		return ""
	}
	return clean
}

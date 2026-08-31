package store

// Accessor methods that together satisfy the provider.ThreadView interface.
// The provider package defines ThreadView so it can project a Thread onto
// SessionOptions without importing internal/store (which would create a
// dependency cycle: provider → store → provider).

// GetProvider returns the provider identifier (claude/codex).
func (t Thread) GetProvider() string { return t.Provider }

// GetModel returns the model string as stored.
func (t Thread) GetModel() string { return t.Model }

// GetWorkspacePath returns the effective cwd the provider should launch
// into. This may differ from project.path when a worktree is active.
func (t Thread) GetWorkspacePath() string { return t.WorkspacePath }

// GetReasoningEffort returns the effort tier as stored. Valid values are
// provider-specific.
func (t Thread) GetReasoningEffort() string { return t.ReasoningEffort }

// GetFastMode returns the fast-mode boolean.
func (t Thread) GetFastMode() bool { return t.FastMode }

// GetContextWindow returns the requested context window size in tokens.
func (t Thread) GetContextWindow() int { return t.ContextWindow }

// GetAutoCompactStandardPercent returns the standard-window compaction
// threshold override. Zero means provider default/inherit.
func (t Thread) GetAutoCompactStandardPercent() int { return t.AutoCompactStandardPercent }

// GetAutoCompactExtendedPercent returns the extended-window compaction
// threshold override. Zero means provider default/inherit.
func (t Thread) GetAutoCompactExtendedPercent() int { return t.AutoCompactExtendedPercent }

// GetMode returns the interaction mode (chat / plan /
// discussion).
func (t Thread) GetMode() string { return t.Mode }

// GetRuntimeMode returns the runtime mode (approval-required /
// auto-accept-edits / full-access).
func (t Thread) GetRuntimeMode() string { return t.RuntimeMode }

// GetSessionRef returns the resume reference the provider session knows
// about (Claude session uuid, Codex thread id). Empty for brand-new
// threads.
func (t Thread) GetSessionRef() string { return t.SessionRef }

// GetPendingForkRef returns the pending-fork resume reference. Claude
// uses this when the next session start should branch off a prior
// session without consuming it as the live resume target.
func (t Thread) GetPendingForkRef() string { return t.PendingForkRef }

// ResolvedSessionRef returns the resume reference Claude's fork path
// should use: prefer the live SessionRef when present, otherwise the
// PendingForkRef captured at fork time. Empty when neither is set
// (brand-new thread).
//
// Codex's fork path doesn't use this — it resumes by thread id alone
// — but the field semantics are general enough that the accessor
// stays on Thread rather than in a Claude-specific helper.
func (t Thread) ResolvedSessionRef() string {
	if t.SessionRef != "" {
		return t.SessionRef
	}
	return t.PendingForkRef
}

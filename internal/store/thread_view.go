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

// GetReasoningEffort returns the effort tier as stored (low / medium /
// high / xhigh / max).
func (t Thread) GetReasoningEffort() string { return t.ReasoningEffort }

// GetFastMode returns the fast-mode boolean.
func (t Thread) GetFastMode() bool { return t.FastMode }

// GetContextWindow returns the requested context window size in tokens
// (200000 or 1000000).
func (t Thread) GetContextWindow() int { return t.ContextWindow }

// GetMode returns the interaction mode (chat / plan / design /
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

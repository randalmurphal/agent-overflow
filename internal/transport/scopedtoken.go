package transport

import "context"

// Caller-scope kinds. An interactive session belongs to a human-driven thread
// (chat, plan, triage, studio); a phase session belongs to an unattended
// workflow phase and carries exactly the grants that phase declared.
const (
	ScopeKindInteractive = "interactive"
	ScopeKindPhase       = "phase"
)

// CallerScope identifies the provider session on whose behalf an in-process
// MCP tool call (ao-remote-tools, ao-workflow-tools) runs. It travels on the
// request context so handlers read the caller's authority instead of trusting
// caller-supplied arguments.
type CallerScope struct {
	// Kind is ScopeKindInteractive or ScopeKindPhase.
	Kind string `json:"kind"`
	// ThreadID is the AO thread whose provider session holds the capability URL.
	ThreadID string `json:"threadId"`
	// ProjectID scopes every run this session may see or act on.
	ProjectID string `json:"projectId"`
	// ItemID and PhaseID identify the phase attempt (phase kind only). They are
	// the effect-ledger key that makes re-entry surface-and-skip.
	ItemID  string `json:"itemId,omitempty"`
	PhaseID string `json:"phaseId,omitempty"`
	// Grants is the frozen phase's grant list (phase kind only).
	Grants []string `json:"grants,omitempty"`
	// Access is the frozen access of the phase, or of the fan-out unit the
	// session runs (phase kind only): `write` or `read-only`.
	Access string `json:"access,omitempty"`
	// Session is the token of the provider session the caller authenticated
	// as, set where the call was authenticated. Work the call starts is that
	// session's alone: it is refused once the session has ended.
	Session string `json:"-"`
}

// HasGrant reports whether the scope carries one named grant.
func (s CallerScope) HasGrant(grant string) bool {
	for _, held := range s.Grants {
		if held == grant {
			return true
		}
	}
	return false
}

// IsPhase reports whether the scope is a workflow phase session.
func (s CallerScope) IsPhase() bool { return s.Kind == ScopeKindPhase }

// IsInteractive reports whether the scope has the only other supported kind.
func (s CallerScope) IsInteractive() bool { return s.Kind == ScopeKindInteractive }

type callerScopeKey struct{}

// WithCallerScope attaches an authenticated scope to a request context.
func WithCallerScope(ctx context.Context, scope CallerScope) context.Context {
	return context.WithValue(ctx, callerScopeKey{}, scope)
}

// CallerScopeFrom returns the scope attached to ctx. Absent for every webview
// and remote-client call, which carry the user's own authority through the
// session token and never a per-session scope.
func CallerScopeFrom(ctx context.Context) (CallerScope, bool) {
	scope, ok := ctx.Value(callerScopeKey{}).(CallerScope)
	return scope, ok
}

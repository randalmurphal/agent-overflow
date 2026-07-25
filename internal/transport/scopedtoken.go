package transport

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Scoped tokens are the credential the `ao` CLI presents (spec §5, D15).
// They are NOT the webview token: a scoped token authenticates one live
// provider session and authorizes only the workflow method set below —
// filtered further, for a phase session, by the grants its workflow froze.
//
// The app owns the registry (a token exists exactly as long as the session it
// was minted for); this package owns the authorization table and the one-shot
// HTTP route the CLI speaks.

// Caller-scope kinds. An interactive session belongs to a human-driven thread
// (chat, plan, design, triage, studio) whose every `ao` invocation is approved
// by the provider's own bash-approval UX; a phase session belongs to an
// unattended workflow phase and carries exactly the grants that phase declared.
const (
	ScopeKindInteractive = "interactive"
	ScopeKindPhase       = "phase"
)

// CallerScope is what a scoped token resolves to. It travels on the request
// context so a bound method reads the caller's authority instead of trusting an
// argument the caller supplied.
type CallerScope struct {
	// Kind is ScopeKindInteractive or ScopeKindPhase.
	Kind string `json:"kind"`
	// ThreadID is the AO thread whose provider session holds this token.
	ThreadID string `json:"threadId"`
	// ProjectID scopes every run this token may see or act on.
	ProjectID string `json:"projectId"`
	// ItemID and PhaseID identify the phase attempt (phase kind only). They are
	// the effect-ledger key that makes re-entry surface-and-skip.
	ItemID  string `json:"itemId,omitempty"`
	PhaseID string `json:"phaseId,omitempty"`
	// Grants is the frozen phase's grant list (phase kind only).
	Grants []string `json:"grants,omitempty"`
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

// ScopedTokens is the narrow app-side registry this package consults. The
// implementation registers a token when a provider session starts and revokes
// it when the session stops, so a resolved scope always names a live session.
type ScopedTokens interface {
	ResolveScopedToken(token string) (CallerScope, bool)
}

type callerScopeKey struct{}

// WithCallerScope attaches an authenticated scope to a request context.
func WithCallerScope(ctx context.Context, scope CallerScope) context.Context {
	return context.WithValue(ctx, callerScopeKey{}, scope)
}

// CallerScopeFrom returns the scope a scoped-token call was authenticated
// with. Absent for every webview / remote-client call: those carry the user's
// own authority through the session token and never a per-session scope.
func CallerScopeFrom(ctx context.Context) (CallerScope, bool) {
	scope, ok := ctx.Value(callerScopeKey{}).(CallerScope)
	return scope, ok
}

// ScopedTokenMethods is the complete set of App methods a scoped token may
// invoke, mapped to the phase grants that admit each one. It is a closed
// allow-list: anything absent — every non-workflow RPC, every LocalOnly method
// outside this table — is refused for a scoped token as method_not_found,
// exactly as an unregistered method would be, so the surface stays
// unenumerable from a compromised agent session.
//
// An interactive scope may call every method named here (the human approves
// each invocation). A phase scope may call one only if it holds at least one of
// the listed grants; the empty case does not exist, because a method no grant
// admits would be unreachable from a phase and misleading to list.
//
// Row-level scoping — "which runs may this phase act on" — is NOT expressed
// here. It cannot be: it depends on the run record, not on the method name. The
// bound methods enforce it from the scope on their context.
var ScopedTokenMethods = map[string][]string{
	// Starting a run, and controlling the runs this phase started.
	"WorkflowAgentStartRun": {"start-run"},
	"WorkflowCancelItem":    {"start-run"},
	"WorkflowPauseItem":     {"start-run"},
	"WorkflowResumeItem":    {"start-run"},
	"WorkflowRerunItem":     {"start-run"},
	"WorkflowRetryUnit":     {"start-run"},
	// Reading run state: project-wide with introspect, own-started-only with
	// start-run alone.
	"WorkflowAgentRunStatus": {"introspect", "start-run"},
	"WorkflowAgentRunOutput": {"introspect", "start-run"},
	"WorkflowAgentListRuns":  {"introspect"},
	// Automations.
	"WorkflowAgentSchedule": {"schedule"},
	"WorkflowAgentGetNotes": {"introspect", "update-notes"},
	"WorkflowAgentSetNotes": {"update-notes"},
}

// ErrCodeGrantRequired is the typed refusal a phase token gets for a method its
// workflow did not grant. It is deliberately distinct from method_not_found:
// the method exists and the caller is authenticated, so hiding the reason would
// only make a misconfigured workflow harder to fix. The route is loopback-only
// and the caller is our own CLI, so naming the grant leaks nothing to a network
// peer.
const ErrCodeGrantRequired = "grant_required"

// AuthorizeScopedMethod decides whether a scoped token may invoke methodName.
// A nil return authorizes the call.
func AuthorizeScopedMethod(scope CallerScope, methodName string) *FrameError {
	grants, listed := ScopedTokenMethods[methodName]
	if !listed {
		return &FrameError{Code: ErrCodeMethodNotFound, Message: "method not registered"}
	}
	if !scope.IsPhase() {
		return nil
	}
	for _, grant := range grants {
		if scope.HasGrant(grant) {
			return nil
		}
	}
	return &FrameError{
		Code: ErrCodeGrantRequired,
		Message: fmt.Sprintf(
			"this phase was not granted %s; add %s to the phase's grants: to allow it",
			describeGrants(grants), describeGrants(grants),
		),
	}
}

// describeGrants renders a grant requirement the way an author would fix it.
func describeGrants(grants []string) string {
	sorted := append([]string(nil), grants...)
	sort.Strings(sorted)
	quoted := make([]string, 0, len(sorted))
	for _, grant := range sorted {
		quoted = append(quoted, fmt.Sprintf("%q", grant))
	}
	switch len(quoted) {
	case 0:
		return "any grant"
	case 1:
		return quoted[0]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
}

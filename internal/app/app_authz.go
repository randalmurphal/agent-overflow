package app

import (
	"context"

	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
)

// Argument-dependent authorization for bound methods
// (docs/specs/remote-access.md §16 phase 3: "the annotation is the FLOOR").
//
// A method's //ao:scope directive is checked once, before dispatch, against
// the method NAME. That is all the generated table can know. Two authorities
// are decided by what the call CARRIES instead: selecting an autonomous
// runtime mode (§5, threads:autonomy) and writing a host-tier settings key
// (§6). Both are rechecked here, inside the method, against the same session.
//
// One helper set rather than a copy per method. Six methods can select a
// runtime mode today and a seventh will be added by somebody who greps for
// how the sixth did it; a per-method copy is how one of them ends up with a
// subtly different rule.
//
// Every helper ADMITS a caller with no session context. That is the
// in-process caller — a background saga, a workflow phase, a test — and the
// launch-credential connection, which names no session and is judged by the
// origin gate exactly as it was before this existed.

// callerGrants reads the scopes the calling connection's session holds right
// now, or the closed-vocabulary reason it holds none.
//
// The third return distinguishes "this call has no session behind it" from
// "the session grants nothing", which are opposite answers: the first
// admits and the second refuses.
func (a *App) callerGrants(ctx context.Context) (granted []string, refusal string, hasSession bool) {
	sessionID := transport.SessionFromContext(ctx)
	if sessionID == "" {
		return nil, "", false
	}
	granted, refusal = SessionScopes(a, sessionID)
	return granted, refusal, true
}

// requireScope refuses when the calling session does not hold scope.
// detail names what asked for it, and rides the refusal message.
func (a *App) requireScope(ctx context.Context, scope transport.Scope, detail string) error {
	granted, refusal, hasSession := a.callerGrants(ctx)
	if !hasSession {
		return nil
	}
	if refusal != "" {
		return transport.AuthRefused(refusal)
	}
	for _, name := range granted {
		if name == string(scope) {
			return nil
		}
	}
	return transport.ScopeRequired(scope, detail)
}

// requireStepUp refuses when the call carries no fresh step-up proof. This
// phase the proof is host presence, resolved at upgrade and read off the
// connection principal rather than re-derived from anything the peer sent
// (transport.stepUpProven has the argument and the phase-5 swap point).
//
// A call with no session behind it is in-process and passes, for the reason
// the file header gives.
func (a *App) requireStepUp(ctx context.Context, detail string) error {
	if transport.SessionFromContext(ctx) == "" {
		return nil
	}
	if transport.HostPresentFromContext(ctx) {
		return nil
	}
	return transport.StepUpRequired(detail)
}

// requireAutonomy refuses when mode hands the agent action without a human
// in the loop and the calling session was not granted threads:autonomy
// (docs/specs/remote-access.md §5).
//
// An EMPTY mode is not a selection and passes: it means "whatever this
// thread or this install already resolves to", which the caller did not
// choose. An unparseable mode also passes, because refusing it here would
// answer scope_required for what is really a bad argument — the method's own
// validator gives the caller the truthful error a moment later.
func (a *App) requireAutonomy(ctx context.Context, mode string) error {
	parsed, present, err := threadmode.ParseOptionalRuntime(mode)
	if err != nil || !present {
		return nil
	}
	if !threadmode.RuntimeNeedsAutonomy(parsed) {
		return nil
	}
	return a.requireScope(ctx, transport.ScopeThreadsAutonomy,
		"selecting the "+string(parsed)+" runtime mode")
}

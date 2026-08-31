package app

import (
	"context"
	"sort"
	"strings"

	"agent-overflow/internal/settings"
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
	if holdsScope(granted, scope) {
		return nil
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
// The mode passed here is the EFFECTIVE one — what the call will actually
// leave the thread running in — never the literal argument. §5 draws the
// boundary by outcome: threads:operate covers driving a thread "in
// read-only or approval-required modes only", so a call whose effective
// mode is autonomous is an autonomy act however it was spelled. An omitted
// argument is not a free pass, because provider.DefaultRuntimeMode is
// full-access and omitting it lands there. Resolving the effective mode is
// the caller's job (requireAutonomyForThread, and the
// AuthorizeRuntimeMode hook on threadapp's create paths); this function
// only judges the answer.
//
// An EMPTY mode passes, which now means "nothing resolved to judge" rather
// than "the caller selected nothing" — no create or drive path can reach
// here with one. An unparseable mode passes too, because refusing it here
// would answer scope_required for what is really a bad argument, and the
// method's own validator gives the caller the truthful error a moment
// later.
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

// requireAutonomyForThread judges the effective mode of a call that DRIVES
// an existing thread — send, steer, queue.
//
// Effective mode is the override when one was selected, and otherwise the
// mode the thread already runs in. That is not a second copy of a
// resolution rule: it is the same answer the method's own code produces,
// which applies an override if given and leaves the thread's mode alone if
// not. Sending into a full-access thread commits the agent to acting
// without approval gates just as surely as selecting full-access does, and
// §5's boundary is the outcome.
//
// The thread read happens only when there is a session to judge AND no
// override was given, so an in-process caller and an explicit selection
// both cost nothing. A thread that cannot be loaded passes: the method's
// own lookup is a step away and answers with the truthful error.
func (a *App) requireAutonomyForThread(ctx context.Context, threadID, selected string) error {
	if transport.SessionFromContext(ctx) == "" {
		return nil
	}
	if strings.TrimSpace(selected) != "" {
		return a.requireAutonomy(ctx, selected)
	}
	if a.store == nil {
		return nil
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil
	}
	return a.requireAutonomy(ctx, thread.RuntimeMode)
}

// requireSettingsTier refuses a settings patch that reaches further than the
// calling session may write. One rule per tier, exactly as
// docs/specs/remote-access.md §6 states them:
//
//   - device tier rides ANY valid session. It is a property of the screen in
//     front of the person, and refusing it would mean a phone cannot set its
//     own font size.
//   - user tier needs settings:write. It is the person's working preference
//     and follows them between machines.
//   - host tier needs a fresh step-up proof. It configures the backend
//     machine itself — the listeners it binds, the binaries it spawns, the
//     authorities it hands a provider session.
//
// An UNCLASSIFIED key is host tier and false from settings.TierForKey, and is
// treated as host here for the same fail-closed reason that answer exists: a
// key nobody assigned a tier to is not one to write from a phone.
//
// The METHOD's own //ao:scope floor is `session` — any live session — so a
// device-tier-only patch reaches this function on session presence alone and
// the three rules above are the whole answer. That floor is what makes the
// first rule true rather than aspirational: while the floor was
// settings:write, a view-only device could not reach this function to set its
// own font size.
func (a *App) requireSettingsTier(ctx context.Context, patch map[string]any) error {
	granted, refusal, hasSession := a.callerGrants(ctx)
	if !hasSession {
		return nil
	}
	if refusal != "" {
		return transport.AuthRefused(refusal)
	}

	// Sorted, so a patch touching several keys always names the same one in
	// its refusal. A message that varies run to run is one nobody can search
	// for in a bug report.
	keys := make([]string, 0, len(patch))
	for key := range patch {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		tier, _ := settings.TierForKey(key)
		switch tier {
		case settings.TierHost:
			if err := a.requireStepUp(ctx, "writing the host-tier setting "+key); err != nil {
				return err
			}
		case settings.TierUser:
			if !holdsScope(granted, transport.ScopeSettingsWrite) {
				return transport.ScopeRequired(transport.ScopeSettingsWrite,
					"writing the user-tier setting "+key)
			}
		case settings.TierDevice:
			// A live session is the whole requirement, and callerGrants just
			// re-read it.
		}
	}
	return nil
}

// holdsScope reports whether a grant set carries scope.
func holdsScope(granted []string, scope transport.Scope) bool {
	for _, name := range granted {
		if name == string(scope) {
			return true
		}
	}
	return false
}

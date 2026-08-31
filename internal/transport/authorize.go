package transport

import (
	"errors"
	"fmt"
)

// Per-RPC authorization for a connection that named a durable session
// (docs/specs/remote-access.md §5).
//
// Two gates are live at once during the migration window, and they answer
// different questions. The ORIGIN gate (LocalOnlyMethods, enforced in
// Dispatcher.ResolveForOrigin) asks "is this peer on this machine"; it is
// what a launch-credential client has always been judged by and it is
// deleted when every client authenticates. The SCOPE gate here asks "was
// this session granted this capability", and it is what remains. A
// connection carrying only the launch credential passes through this file
// untouched — it names no session, so there is nothing to compare a
// method's scope against.
//
// Nothing here caches. The scopes are re-read per call through
// Config.SessionScopes, because a revocation lands after the upgrade that
// admitted the connection and a grant read once at upgrade time would
// outlive it (§4 "Revocation": no RPC authorizes from state cached at
// upgrade time).

// ErrCodeScopeRequired is the typed refusal a session-scoped caller gets
// for a method its grants do not cover. Distinct from method_not_found for
// the reason ErrCodeGrantRequired is: the method exists and the caller is
// authenticated, so the refusal is already attributable and naming the
// missing scope discloses nothing it could not learn by trying every
// method. What it buys is a client that can say WHY a surface is disabled
// instead of showing a dead button (§5 "Frontend capability model").
const ErrCodeScopeRequired = "scope_required"

// ErrCodeStepUpRequired is the refusal for a call in §4's step-up set made
// without a fresh proof. Its own code rather than a shade of
// scope_required, because the remedy is different in kind: no grant can
// satisfy it, and the client's next move is to produce a proof rather than
// to ask for a scope.
const ErrCodeStepUpRequired = "step_up_required"

// methodClassification indexes the generated table by name. Built once —
// the table is static — so the per-call cost is one map lookup.
var methodClassification = func() map[string]MethodMeta {
	index := make(map[string]MethodMeta, len(GeneratedMethods))
	for _, method := range GeneratedMethods {
		index[method.Name] = method
	}
	return index
}()

// classify answers what capability a resolved method exercises.
//
// A name the generated table does not carry answers `host`. That is every
// method on a receiver methodgen does not generate for — the harness's,
// today — and it is fail-closed rather than arbitrary: an unclassified
// method is one nobody decided a remote form for, which is what `host`
// already means. Those receivers register LocalOnly, so the answer also
// matches the reachability they already have.
func classify(methodName string) MethodMeta {
	if meta, ok := methodClassification[methodName]; ok {
		return meta
	}
	return MethodMeta{Name: methodName, Scope: ScopeHost}
}

// stepUpProven reports whether this call carries a fresh proof for §4's
// step-up set ("Step-up (mandatory, not optional)").
//
// THIS PHASE THE PROOF IS HOST PRESENCE. §4 asks for "a per-call fresh
// passkey (or host-presence) proof"; passkeys arrive in phase 5, so the
// host-presence half is what exists, and being on this machine is what it
// means today. That is also the reality the step-up set already lives
// under — every method carrying //ao:stepup derives local-only from its
// scope, so nothing is loosened by naming the rule.
//
// It is a function rather than an inline `isLoopback` so phase 5 swaps the
// PROOF without touching a call site: the passkey assertion lands here,
// beside the argument for what a proof is, and every gate that asks the
// question keeps asking it the same way.
func stepUpProven(hostPresent bool) bool { return hostPresent }

// AuthorizeSessionMethod decides whether a connection carrying a durable
// session may invoke methodName. A nil return authorizes the call.
//
// granted is the session's scope set, re-read for this call. hostPresent
// is whether the caller is on this machine, which is what a step-up proof
// resolves to this phase (see stepUpProven).
//
// The order is the contract. `host` is judged before the grant set because
// no grant can ever satisfy it — telling a caller to acquire a scope no
// session may hold would be a false instruction — and step-up before the
// grant set for the same reason.
func AuthorizeSessionMethod(granted []string, methodName string, hostPresent bool) *FrameError {
	meta := classify(methodName)
	if meta.Scope == ScopeHost && !hostPresent {
		return &FrameError{
			Code: ErrCodeScopeRequired,
			Message: fmt.Sprintf(
				"%s acts on the host desktop and has no remote form", methodName),
			Scope: string(ScopeHost),
		}
	}
	if meta.StepUp && !stepUpProven(hostPresent) {
		return &FrameError{
			Code: ErrCodeStepUpRequired,
			Message: fmt.Sprintf(
				"%s requires a fresh host-presence proof, which a standing grant cannot supply",
				methodName),
		}
	}
	if meta.Scope == ScopeHost {
		// Host presence IS the authorization here, and it was just proven.
		// Looking for `host` in the grant set would refuse the embedded
		// webview — whose session names one and correctly holds no such
		// grant, because none exists.
		return nil
	}
	if !meta.Scope.Valid() {
		// Unreachable through the generated table, which methodgen refuses
		// to emit with an undeclared scope in it. Refuse rather than admit,
		// so a hand-built MethodMeta cannot become an unguarded surface.
		return scopeRefusal(meta.Scope, methodName)
	}
	for _, name := range granted {
		if name == string(meta.Scope) {
			return nil
		}
	}
	return scopeRefusal(meta.Scope, methodName)
}

// scopeRefusal is the one construction of a scope refusal, so the pre-call
// gate and a method's own argument recheck (ScopeRequired) cannot disagree
// about what the wire carries.
func scopeRefusal(scope Scope, methodName string) *FrameError {
	return &FrameError{
		Code:    ErrCodeScopeRequired,
		Message: fmt.Sprintf("%s requires the %s scope, which this session was not granted", methodName, scope),
		// The scope name is a FIELD, not prose to parse: it is what a
		// client branches on to explain a disabled surface, and prose does
		// not survive the wire for a non-loopback caller.
		Scope: string(scope),
	}
}

// authzError is a bound method's OWN authorization refusal, for the case
// the classification table cannot answer: authority that depends on an
// ARGUMENT rather than on the method name (§16 phase 3 — "the annotation
// is the FLOOR"). Selecting an autonomous runtime mode and writing a
// host-tier settings key are the two.
//
// It exists so an in-method refusal reaches the wire as the SAME code and
// message a pre-call refusal would. Without it a method could only return
// an ordinary error, which the dispatcher redacts for a non-loopback
// caller — so the client that most needs to know which scope it lacks is
// the one that would be told "method failed".
type authzError struct{ frame FrameError }

func (e *authzError) Error() string { return e.frame.Message }

// ScopeRequired builds the refusal a bound method returns when its
// ARGUMENTS ask for more than the caller's session holds. The scope name
// rides the same field the pre-call refusal uses; detail says what asked.
func ScopeRequired(scope Scope, detail string) error {
	return &authzError{frame: FrameError{
		Code:    ErrCodeScopeRequired,
		Message: fmt.Sprintf("%s requires the %s scope, which this session was not granted", detail, scope),
		Scope:   string(scope),
	}}
}

// StepUpRequired builds the refusal a bound method returns when an
// argument reaches something §4 puts behind a fresh proof — a host-tier
// settings key, today the only one.
func StepUpRequired(detail string) error {
	return &authzError{frame: FrameError{
		Code:    ErrCodeStepUpRequired,
		Message: fmt.Sprintf("%s requires a fresh host-presence proof, which a standing grant cannot supply", detail),
	}}
}

// authzFrame reports the wire refusal a method error carries, if it is one
// of the two above. The dispatcher asks before its redaction path, because
// these messages are the answer rather than an internal detail.
func authzFrame(err error) (*FrameError, bool) {
	var refusal *authzError
	if errors.As(err, &refusal) {
		frame := refusal.frame
		return &frame, true
	}
	return nil, false
}

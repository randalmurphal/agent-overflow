package transport

import (
	"errors"
	"fmt"
)

// Per-RPC authorization for a connection that named a durable session
// (docs/specs/remote-access.md §5).
//
// This file is the ONE gate on what a named session may call. The
// per-method origin partition that ran beside it during the migration
// window — LocalOnlyMethods, derived from these same scopes and enforced
// in Dispatcher.ResolveForOrigin — is deleted: every off-host connection
// now names a session (the admission rule) whose binding class permits the
// peer presenting it, so "is this peer on this machine" no longer stands
// in for "may this caller do this". What survives on the dispatcher is
// RegisterOptions{LocalOnly}, a whole RECEIVER of host tooling, which is a
// different statement.
//
// A connection carrying only the launch credential passes through this
// file untouched — it names no session, so there is nothing to compare a
// method's scope against, and it is loopback by the admission rule.
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
// already means. Those receivers register RegisterOptions{LocalOnly}, so
// the answer also matches the reachability they already have.
func classify(methodName string) MethodMeta {
	if meta, ok := methodClassification[methodName]; ok {
		return meta
	}
	return MethodMeta{Name: methodName, Scope: ScopeHost}
}

// CallerProof is what ONE CALL proved about the caller, resolved per RPC
// rather than captured at upgrade.
//
// Per call, because step-up is per call: §4 asks for "a per-call fresh
// passkey (or host-presence) proof", and an answer read once when the
// socket opened would be a standing grant wearing the word "fresh". Host
// presence is fixed for a connection and could have been captured, but it
// travels in the same struct so the two answers to one question live in
// one place — a gate that reads one and forgets the other is the failure
// this shape exists to make impossible.
type CallerProof struct {
	// HostPresent is whether the peer is on this machine, from the
	// kernel-reported address the upgrade classified.
	HostPresent bool
	// StepUp is whether this call presented a valid step-up token: a
	// single-use, minutes-lived grant a passkey assertion minted, bound to
	// the session presenting it. Spent by the presentation, whatever the
	// answer (Config.StepUpProof).
	StepUp bool
}

// stepUpProven reports whether this call carries a fresh proof for §4's
// step-up set ("Step-up (mandatory, not optional)").
//
// TWO proofs, and §4 names both: standing at the machine, or a passkey
// assertion this backend verified moments ago. Neither is a standing
// grant — host presence is the kernel's answer about this connection's
// peer, and a step-up token is single-use, expires in minutes, and is
// bound to the session that asked for it.
//
// The second proof does not weaken the first. Before passkeys the
// step-up set was unreachable from ANY remote session, so an owner away
// from their desk could not change their own backend's network binding;
// the passkey path makes the set reachable to the OWNER specifically, by
// demanding a signature from a credential only they hold. A session
// without one is refused exactly as before.
//
// It stays one function so every gate asks the question the same way, and
// a third proof would move no call site.
func stepUpProven(proof CallerProof) bool { return proof.HostPresent || proof.StepUp }

// AuthorizeSessionMethod decides whether a connection carrying a durable
// session may invoke methodName. A nil return authorizes the call.
//
// granted is the session's scope set, re-read for this call. proof is what
// this call proved about its caller, also resolved for this call (see
// CallerProof).
//
// The order is the contract. `host` is judged before the grant set because
// no grant can ever satisfy it — telling a caller to acquire a scope no
// session may hold would be a false instruction — and step-up before the
// grant set for the same reason. `session` joins them for the mirror
// reason: no grant is needed, so no grant is looked for.
func AuthorizeSessionMethod(granted []string, methodName string, proof CallerProof) *FrameError {
	meta := classify(methodName)
	if meta.Scope == ScopeHost && !proof.HostPresent {
		return &FrameError{
			Code: ErrCodeScopeRequired,
			Message: fmt.Sprintf(
				"%s acts on the host desktop and has no remote form", methodName),
			Scope: string(ScopeHost),
		}
	}
	if meta.StepUp && !stepUpProven(proof) {
		return &FrameError{
			Code:    ErrCodeStepUpRequired,
			Message: stepUpMessage(methodName),
		}
	}
	if meta.Scope == ScopeHost {
		// Host presence IS the authorization here, and it was just proven.
		// Looking for `host` in the grant set would refuse the embedded
		// webview — whose session names one and correctly holds no such
		// grant, because none exists.
		return nil
	}
	if meta.Scope == ScopeSession {
		// The floor, and reaching this function is the proof: only a
		// connection that named a live session is judged here, and its
		// liveness was re-read for this call. Looking for `session` in the
		// grant set would refuse every session, because it is not a grant
		// anybody can be given. What the call may actually DO is decided
		// past this point — per key in requireSettingsTier, or by the
		// bucket uiStateScope resolves for this connection and no other.
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
		Message: stepUpMessage(detail),
	}}
}

// stepUpMessage is the one phrasing of a step-up refusal, so the pre-call
// gate and a method's own argument recheck name the same two remedies.
// The client branches on the CODE; this text is what a person reads in a
// log or a loopback error, and it has to say which proofs exist or the
// remedy is unguessable.
func stepUpMessage(detail string) string {
	return fmt.Sprintf(
		"%s requires a fresh proof — host presence or a passkey — which a standing grant cannot supply",
		detail)
}

// AuthRefused builds the refusal a bound method returns when the session
// its connection named stopped admitting work between the pre-call gate
// and the argument recheck — a revocation landing mid-call, which is the
// window §4 "Revocation" says must close on the next answer rather than on
// the next watchdog tick.
//
// Same envelope AuthFailure builds for the pre-call path, wrapped so it
// reaches the wire as itself: reasonCode is identity's closed vocabulary,
// carried uninterpreted for authReason.ts to present.
func AuthRefused(reasonCode string) error {
	return &authzError{frame: *AuthFailure(reasonCode)}
}

// AuthzFrame reports the wire refusal a method error carries, if it is one
// the constructors above built. The dispatcher asks before its redaction
// path, because these messages are the answer rather than an internal
// detail.
//
// Exported because the constructors are: a package that returns one of
// these from a bound method is the package that has to assert what its
// caller will actually receive.
func AuthzFrame(err error) (*FrameError, bool) {
	var refusal *authzError
	if errors.As(err, &refusal) {
		frame := refusal.frame
		return &frame, true
	}
	return nil, false
}

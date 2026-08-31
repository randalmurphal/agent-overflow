package app

import (
	"context"
	"errors"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The argument-dependent rechecks (docs/specs/remote-access.md §16 phase 3).
//
// Each one has three cases and they are the contract: a session holding the
// scope is ADMITTED, a session without it is REFUSED with the typed code,
// and a call with no session behind it — every in-process caller and every
// launch-credential connection — passes untouched.

// pairSessionWithScopes mints a device session holding exactly scopes, so a
// test can name the one grant it is about instead of the whole set.
func pairSessionWithScopes(t *testing.T, app *App, thumbprint string, scopes []identity.Scope) store.Session {
	t.Helper()
	state := app.identityState()
	link, err := state.sessions.MintPairingLink(identity.PairingRequest{
		UserID:       state.owner.ID,
		DeviceClass:  identity.DeviceBrowser,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       scopes,
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	redemption, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token: link.Token, KeyThumbprint: thumbprint, Label: "a browser", Platform: "linux",
	})
	if reason.Refused() {
		t.Fatalf("RedeemPairing: %s", reason.Code())
	}
	if _, err := state.sessions.ConfirmPairing(link.Link.ID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	session, err := app.store.GetSession(redemption.Tokens.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return session
}

// callFrom is the ctx a bound method sees when the transport dispatched it
// on a connection carrying sessionID.
func callFrom(sessionID string, hostPresent bool) context.Context {
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{
		SessionID: sessionID, HostPresent: hostPresent,
	})
	return ctx
}

// wantScopeRefusal asserts err is the typed refusal naming scope.
func wantScopeRefusal(t *testing.T, err error, scope transport.Scope) {
	t.Helper()
	if err == nil {
		t.Fatalf("call was admitted, want a %s refusal", scope)
	}
	frame, ok := transport.AuthzFrame(err)
	if !ok {
		t.Fatalf("err = %v, want a typed authorization refusal", err)
	}
	if frame.Code != transport.ErrCodeScopeRequired {
		t.Errorf("code = %q, want %q", frame.Code, transport.ErrCodeScopeRequired)
	}
	if frame.Scope != string(scope) {
		t.Errorf("scope = %q, want %q", frame.Scope, scope)
	}
}

// autonomyApp is an identity-wired App plus the two sessions every recheck
// test compares: one holding threads:autonomy and one that does not.
func autonomyApp(t *testing.T) (app *App, withScope, withoutScope string) {
	t.Helper()
	app = identityApp(t)
	full := pairSessionWithScopes(t, app, "thumb-full", identity.Scopes)
	limited := pairSessionWithScopes(t, app, "thumb-limited", []identity.Scope{
		identity.ScopeThreadsRead, identity.ScopeThreadsOperate,
	})
	return app, full.ID, limited.ID
}

// TestRequireAutonomyFollowsTheSelectedMode walks the whole runtime-mode
// vocabulary rather than a sample, so a tier added to
// provider.AllRuntimeModes has to be classified here before this passes.
func TestRequireAutonomyFollowsTheSelectedMode(t *testing.T) {
	app, granted, ungranted := autonomyApp(t)

	for _, mode := range provider.AllRuntimeModes {
		needsScope := mode != provider.RuntimeReadOnly && mode != provider.RuntimeApprovalRequired

		if err := app.requireAutonomy(callFrom(granted, false), string(mode)); err != nil {
			t.Errorf("a session holding threads:autonomy selecting %s: %v", mode, err)
		}
		err := app.requireAutonomy(callFrom(ungranted, false), string(mode))
		if needsScope {
			wantScopeRefusal(t, err, transport.ScopeThreadsAutonomy)
		} else if err != nil {
			t.Errorf("selecting %s needs no grant, got %v", mode, err)
		}
	}
}

// TestRequireAutonomyAdmitsACallWithNoSession is the compatibility case: an
// in-process saga, a workflow phase, a test, and every launch-credential
// connection name no session and must keep working exactly as before.
func TestRequireAutonomyAdmitsACallWithNoSession(t *testing.T) {
	app, _, _ := autonomyApp(t)

	for _, ctx := range []context.Context{context.Background(), callFrom("", false)} {
		if err := app.requireAutonomy(ctx, string(provider.RuntimeFullAccess)); err != nil {
			t.Errorf("a call with no session behind it was refused: %v", err)
		}
	}
}

// TestRequireAutonomyIgnoresWhatIsNotASelection: an empty mode is "whatever
// this thread already resolves to", and an unparseable one is a bad argument
// the method's own validator explains. Neither is a capability request, and
// answering scope_required for either would be a false instruction.
func TestRequireAutonomyIgnoresWhatIsNotASelection(t *testing.T) {
	app, _, ungranted := autonomyApp(t)

	for _, mode := range []string{"", "   ", "yolo"} {
		if err := app.requireAutonomy(callFrom(ungranted, false), mode); err != nil {
			t.Errorf("requireAutonomy(%q) = %v, want admitted", mode, err)
		}
	}
}

// TestRequireAutonomyRefusesARevokedSession. The pre-call gate ran on this
// connection microseconds ago; a revocation landing in between must refuse
// on THIS answer rather than on the next watchdog tick (§4 "Revocation").
func TestRequireAutonomyRefusesARevokedSession(t *testing.T) {
	app, granted, _ := autonomyApp(t)
	if _, err := app.identityState().sessions.RevokeSession(granted); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	err := app.requireAutonomy(callFrom(granted, false), string(provider.RuntimeFullAccess))
	frame, ok := transport.AuthzFrame(err)
	if !ok {
		t.Fatalf("err = %v, want a typed authorization refusal", err)
	}
	if frame.Code != transport.ErrCodeAuthFailed {
		t.Errorf("code = %q, want %q — a revoked session is not a missing scope",
			frame.Code, transport.ErrCodeAuthFailed)
	}
	if frame.Reason == "" {
		t.Error("the refusal carries no reason; authReason.ts has nothing to present")
	}
}

// TestBoundMethodsRecheckTheSelectedMode is the inventory in test form: every
// method that can select a runtime mode by ARGUMENT, called by a session
// without the grant, refuses. A method added to that set and not listed here
// is the defect this test exists to catch.
func TestBoundMethodsRecheckTheSelectedMode(t *testing.T) {
	app, _, ungranted := autonomyApp(t)
	ctx := callFrom(ungranted, false)
	auto := string(provider.RuntimeFullAccess)

	// Every call names a thread and a project that do not exist. That is
	// deliberate: the recheck runs BEFORE any store read, so a refusal here
	// cannot be confused with a lookup failure, and a method that moved its
	// recheck below the lookup would fail this test with the wrong error.
	calls := []struct {
		name string
		call func() error
	}{
		{"CreateThread", func() error {
			_, err := app.CreateThread(ctx, CreateThreadOptions{ProjectID: "no-project", RuntimeMode: auto})
			return err
		}},
		{"UpdateThreadRuntimeMode", func() error {
			_, err := app.UpdateThreadRuntimeMode(ctx, "no-thread", auto)
			return err
		}},
		{"SendMessageWithOptions", func() error {
			_, err := app.SendMessageWithOptions(ctx, "no-thread", "hi", SendMessageOptions{RuntimeMode: auto})
			return err
		}},
		{"SteerMessageWithOptions", func() error {
			_, err := app.SteerMessageWithOptions(ctx, "no-thread", "hi", SendMessageOptions{RuntimeMode: auto})
			return err
		}},
		{"RegisterQueueItem", func() error {
			_, err := app.RegisterQueueItem(ctx, "no-thread", "hi", SendMessageOptions{RuntimeMode: auto})
			return err
		}},
		{"UpdateNewThreadDefaults", func() error {
			_, err := app.UpdateNewThreadDefaults(ctx, NewThreadDefaultsUpdate{
				ProjectID: "no-project", RuntimeMode: auto,
			})
			return err
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			wantScopeRefusal(t, c.call(), transport.ScopeThreadsAutonomy)
		})
	}
}

// TestGrantedSessionReachesTheMethodBody is the admitting half: the same
// calls from a session that HOLDS threads:autonomy get past the recheck and
// fail on the missing thread instead, which is the ordinary error.
func TestGrantedSessionReachesTheMethodBody(t *testing.T) {
	app, granted, _ := autonomyApp(t)
	ctx := callFrom(granted, false)
	auto := string(provider.RuntimeFullAccess)

	_, err := app.UpdateThreadRuntimeMode(ctx, "no-thread", auto)
	if err == nil {
		t.Fatal("UpdateThreadRuntimeMode on a missing thread succeeded")
	}
	if _, isAuthz := transport.AuthzFrame(err); isAuthz {
		t.Fatalf("a granted session was refused by the recheck: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation: %v", err)
	}
}

package app

import (
	"context"
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

// threadInMode mints a thread already running in mode, which is what the
// drive paths resolve their effective mode from.
func threadInMode(t *testing.T, app *App, mode provider.RuntimeMode) store.Thread {
	t.Helper()
	thread, err := createTestThread(t, app, "claude", t.TempDir(), "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	updated, err := app.UpdateThreadRuntimeMode(context.Background(), thread.ID, string(mode))
	if err != nil {
		t.Fatalf("UpdateThreadRuntimeMode(%s): %v", mode, err)
	}
	return updated
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

// TestCreateThreadJudgesTheResolvedMode is the headline of the
// resolved-mode rule: the SAME call, with no mode argument at all, is
// refused or admitted depending only on what the install's default
// resolves to. §5 draws the boundary by outcome, so an omitted argument
// that lands in full-access is an autonomy act.
func TestCreateThreadJudgesTheResolvedMode(t *testing.T) {
	app, granted, ungranted := autonomyApp(t)
	project, err := app.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	noModeSelected := CreateThreadOptions{ProjectID: project.ID}

	// The precondition, asserted rather than assumed: with no profile
	// saved, a create with no mode argument lands in full-access
	// (provider.DefaultRuntimeMode). This is the thread a session without
	// threads:autonomy must not be able to mint.
	seeded, err := app.CreateThread(context.Background(), noModeSelected)
	if err != nil {
		t.Fatalf("CreateThread (sessionless): %v", err)
	}
	if seeded.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Fatalf("the unset default resolved to %q, want full-access — this test "+
			"no longer covers what it was written for", seeded.RuntimeMode)
	}

	wantScopeRefusal(t, mustErr(app.CreateThread(callFrom(ungranted, false), noModeSelected)),
		transport.ScopeThreadsAutonomy)
	if _, err := app.CreateThread(callFrom(granted, false), noModeSelected); err != nil {
		t.Fatalf("a session holding threads:autonomy was refused: %v", err)
	}

	// Now move the install default to approval-required. The identical
	// call — still selecting nothing — must be admitted, because the
	// thread it produces no longer acts without a human.
	if _, err := app.UpdateNewThreadDefaults(context.Background(), NewThreadDefaultsUpdate{
		ProjectID:   project.ID,
		RuntimeMode: string(provider.RuntimeApprovalRequired),
	}); err != nil {
		t.Fatalf("UpdateNewThreadDefaults: %v", err)
	}
	created, err := app.CreateThread(callFrom(ungranted, false), noModeSelected)
	if err != nil {
		t.Fatalf("creating an approval-required thread was refused: %v", err)
	}
	if created.RuntimeMode != string(provider.RuntimeApprovalRequired) {
		t.Fatalf("resolved mode = %q, want approval-required", created.RuntimeMode)
	}
}

// TestDrivingAnAutonomousThreadNeedsAutonomy. Send, steer and queue select
// nothing here. Their effective mode is the mode the TARGET already runs
// in, because sending into a full-access thread commits the agent to
// acting without approval gates just as surely as selecting it does.
func TestDrivingAnAutonomousThreadNeedsAutonomy(t *testing.T) {
	app, _, ungranted := autonomyApp(t)
	ctx := callFrom(ungranted, false)
	autonomous := threadInMode(t, app, provider.RuntimeFullAccess)
	gated := threadInMode(t, app, provider.RuntimeApprovalRequired)

	for _, c := range []struct {
		name string
		call func(threadID string) error
	}{
		{"SendMessageWithOptions", func(id string) error {
			_, err := app.SendMessageWithOptions(ctx, id, "hi", SendMessageOptions{})
			return err
		}},
		{"SteerMessageWithOptions", func(id string) error {
			_, err := app.SteerMessageWithOptions(ctx, id, "hi", SendMessageOptions{})
			return err
		}},
		{"RegisterQueueItem", func(id string) error {
			_, err := app.RegisterQueueItem(ctx, id, "hi", SendMessageOptions{})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantScopeRefusal(t, c.call(autonomous.ID), transport.ScopeThreadsAutonomy)
		})
	}

	// The admitting half stops at the helper deliberately. Driving these
	// methods through to a gated thread starts a real provider session,
	// which tests must never do (internal/AGENTS.md, "Testing bar") — and
	// the recheck is the only thing between the entry point and that
	// session, so the helper answers exactly the question being asked.
	if err := app.requireAutonomyForThread(ctx, gated.ID, ""); err != nil {
		t.Errorf("driving an approval-required thread was refused as autonomy: %v", err)
	}
}

// TestSelectingAnAutonomousModeStillNeedsAutonomy is the original
// selection case, which the resolved-mode rule widened rather than
// replaced: an explicit argument is judged as the override it is, on a
// thread whose current mode would have passed on its own.
func TestSelectingAnAutonomousModeStillNeedsAutonomy(t *testing.T) {
	app, _, ungranted := autonomyApp(t)
	ctx := callFrom(ungranted, false)
	gated := threadInMode(t, app, provider.RuntimeApprovalRequired)
	auto := string(provider.RuntimeFullAccess)

	for _, c := range []struct {
		name string
		call func() error
	}{
		{"SendMessageWithOptions", func() error {
			_, err := app.SendMessageWithOptions(ctx, gated.ID, "hi", SendMessageOptions{RuntimeMode: auto})
			return err
		}},
		{"SteerMessageWithOptions", func() error {
			_, err := app.SteerMessageWithOptions(ctx, gated.ID, "hi", SendMessageOptions{RuntimeMode: auto})
			return err
		}},
		{"RegisterQueueItem", func() error {
			_, err := app.RegisterQueueItem(ctx, gated.ID, "hi", SendMessageOptions{RuntimeMode: auto})
			return err
		}},
		{"UpdateThreadRuntimeMode", func() error {
			_, err := app.UpdateThreadRuntimeMode(ctx, gated.ID, auto)
			return err
		}},
		{"UpdateNewThreadDefaults", func() error {
			_, err := app.UpdateNewThreadDefaults(ctx, NewThreadDefaultsUpdate{
				ProjectID: gated.ProjectID, RuntimeMode: auto,
			})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantScopeRefusal(t, c.call(), transport.ScopeThreadsAutonomy)
		})
	}
}

// TestEffectiveModeLeavesSessionlessCallersAlone. Every path above, from a
// caller with no session: the workflow engine drives full-access threads
// constantly and must keep doing so.
func TestEffectiveModeLeavesSessionlessCallersAlone(t *testing.T) {
	app, _, _ := autonomyApp(t)
	autonomous := threadInMode(t, app, provider.RuntimeFullAccess)

	for _, ctx := range []context.Context{context.Background(), callFrom("", false)} {
		if err := app.requireAutonomyForThread(ctx, autonomous.ID, ""); err != nil {
			t.Errorf("driving a full-access thread with no session behind it: %v", err)
		}
		if err := app.requireAutonomy(ctx, string(provider.RuntimeFullAccess)); err != nil {
			t.Errorf("selecting full-access with no session behind it: %v", err)
		}
	}
}

// TestUnloadableThreadDefersToTheMethodsOwnError. A thread that cannot be
// read is a bad argument, not a capability request, and the method's own
// lookup answers it a step later with something true.
func TestUnloadableThreadDefersToTheMethodsOwnError(t *testing.T) {
	app, _, ungranted := autonomyApp(t)

	if err := app.requireAutonomyForThread(callFrom(ungranted, false), "no-such-thread", ""); err != nil {
		t.Errorf("an unreadable thread produced an authorization refusal: %v", err)
	}
}

// The settings tiers (docs/specs/remote-access.md §6). One case per tier,
// plus the two the table itself decides: an unclassified key and a caller
// with no session.

// settingsTierApp pairs an identity-wired App with three callers: a session
// holding everything, one holding no settings grant at all, and the host.
func settingsTierApp(t *testing.T) (app *App, full, limited string) {
	t.Helper()
	app = identityApp(t)
	return app,
		pairSessionWithScopes(t, app, "thumb-settings-full", identity.Scopes).ID,
		pairSessionWithScopes(t, app, "thumb-settings-limited",
			[]identity.Scope{identity.ScopeThreadsRead}).ID
}

// TestDeviceTierSettingRidesAnyValidSession: a font size is a property of the
// screen in front of the person. Refusing it would mean a phone cannot set
// its own.
func TestDeviceTierSettingRidesAnyValidSession(t *testing.T) {
	app, _, limited := settingsTierApp(t)

	patch := map[string]any{"fontSize": 15}
	if err := app.requireSettingsTier(callFrom(limited, false), patch); err != nil {
		t.Fatalf("a device-tier key from a session with no settings grant: %v", err)
	}
}

// TestUserTierSettingNeedsSettingsWrite: a working preference follows the
// person, and writing one is a grant they gave this device.
func TestUserTierSettingNeedsSettingsWrite(t *testing.T) {
	app, full, limited := settingsTierApp(t)
	patch := map[string]any{"confirmDelete": false}

	wantScopeRefusal(t, app.requireSettingsTier(callFrom(limited, false), patch),
		transport.ScopeSettingsWrite)
	if err := app.requireSettingsTier(callFrom(full, false), patch); err != nil {
		t.Fatalf("a user-tier key from a session holding settings:write: %v", err)
	}
}

// TestHostTierSettingNeedsAStepUpProof: no standing grant reaches the backend
// machine's own configuration. The session below holds EVERY scope and is
// still refused off-host, which is the property worth pinning — a host-tier
// write is not a scope anybody can be given.
func TestHostTierSettingNeedsAStepUpProof(t *testing.T) {
	app, full, _ := settingsTierApp(t)
	patch := map[string]any{"retention": map[string]any{}}

	err := app.requireSettingsTier(callFrom(full, false), patch)
	frame, ok := transport.AuthzFrame(err)
	if !ok {
		t.Fatalf("err = %v, want a typed authorization refusal", err)
	}
	if frame.Code != transport.ErrCodeStepUpRequired {
		t.Errorf("code = %q, want %q", frame.Code, transport.ErrCodeStepUpRequired)
	}

	if err := app.requireSettingsTier(callFrom(full, true), patch); err != nil {
		t.Fatalf("a host-tier key with host presence proven: %v", err)
	}
}

// TestUnclassifiedSettingsKeyIsHostTier. settings.TierForKey answers
// (TierHost, false) for a key nobody classified, and this is the enforcement
// half of that fail-closed default.
func TestUnclassifiedSettingsKeyIsHostTier(t *testing.T) {
	app, full, _ := settingsTierApp(t)

	err := app.requireSettingsTier(callFrom(full, false), map[string]any{"noSuchSetting": 1})
	frame, ok := transport.AuthzFrame(err)
	if !ok || frame.Code != transport.ErrCodeStepUpRequired {
		t.Fatalf("err = %v, want step_up_required for an unclassified key", err)
	}
}

// TestSettingsTierAdmitsACallWithNoSession is the compatibility case: the
// launch-credential connection and every in-process caller are unchanged.
func TestSettingsTierAdmitsACallWithNoSession(t *testing.T) {
	app, _, _ := settingsTierApp(t)

	for _, ctx := range []context.Context{context.Background(), callFrom("", false)} {
		if err := app.requireSettingsTier(ctx, map[string]any{"retention": map[string]any{}}); err != nil {
			t.Errorf("a call with no session behind it was refused: %v", err)
		}
	}
}

// TestUpdateSettingsRefusesBeforeItWrites is the wiring test: the gate runs
// ahead of the persist, so a refused patch leaves the file untouched. A gate
// placed after settings.Update would pass every assertion above and still
// have applied the change.
func TestUpdateSettingsRefusesBeforeItWrites(t *testing.T) {
	app, full, _ := settingsTierApp(t)
	before := app.currentSettings()

	_, err := app.UpdateSettings(callFrom(full, false), map[string]any{
		"observabilityTracingEnabled": !before.ObservabilityTracingEnabled,
	})
	if _, ok := transport.AuthzFrame(err); !ok {
		t.Fatalf("UpdateSettings err = %v, want a typed authorization refusal", err)
	}
	if got := app.currentSettings().ObservabilityTracingEnabled; got != before.ObservabilityTracingEnabled {
		t.Errorf("the refused patch was persisted anyway: tracing = %v", got)
	}
}

package app

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
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
		Token: link.Token, Proof: identity.DeviceProof{Value: thumbprint}, Label: "a browser", Platform: "linux",
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
// on a connection carrying sessionID: the connection's principal, plus
// what the transport resolved this ONE call to have proved.
func callFrom(sessionID string, hostPresent bool) context.Context {
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{
		SessionID: sessionID,
	})
	return transport.WithCallerProof(ctx, transport.CallerProof{HostPresent: hostPresent})
}

// callSteppedUp is the same ctx for a call whose step-up was proved by a
// spent passkey token rather than by standing at the machine — the remote
// owner's path, which host presence alone can never produce.
func callSteppedUp(sessionID string) context.Context {
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{
		SessionID: sessionID,
	})
	return transport.WithCallerProof(ctx, transport.CallerProof{StepUp: true})
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// threadDrivingCalls is one call of every bound method that sends to an
// existing thread or runs something in it, each selecting no mode.
// TestEveryThreadDrivingMethodIsListed keeps it the set the code has.
func threadDrivingCalls(app *App) map[string]func(ctx context.Context, threadID string) error {
	return map[string]func(ctx context.Context, threadID string) error{
		"SendMessage": func(ctx context.Context, id string) error {
			return app.SendMessage(ctx, id, "hi", nil)
		},
		"SendMessageWithOptions": func(ctx context.Context, id string) error {
			_, err := app.SendMessageWithOptions(ctx, id, "hi", SendMessageOptions{})
			return err
		},
		"SteerMessageWithOptions": func(ctx context.Context, id string) error {
			_, err := app.SteerMessageWithOptions(ctx, id, "hi", SendMessageOptions{})
			return err
		},
		"RegisterQueueItem": func(ctx context.Context, id string) error {
			_, err := app.RegisterQueueItem(ctx, id, "hi", SendMessageOptions{})
			return err
		},
		"RevertConversationAndResendMessage": func(ctx context.Context, id string) error {
			_, err := app.RevertConversationAndResendMessage(ctx, id, "item", RevertAndResendOptions{Content: "hi"})
			return err
		},
		"SubmitAsyncAnswers": func(ctx context.Context, id string) error {
			return app.SubmitAsyncAnswers(ctx, id, "send", nil)
		},
		"StartCodexReview": func(ctx context.Context, id string) error {
			_, err := app.StartCodexReview(ctx, id, CodexReviewTarget{Kind: "uncommittedChanges"})
			return err
		},
		"SendPlanRevisionComments": func(ctx context.Context, id string) error {
			_, err := app.SendPlanRevisionComments(ctx, id, "plan", []string{"comment"})
			return err
		},
		"SendDiffReviewComments": func(ctx context.Context, id string) error {
			_, err := app.SendDiffReviewComments(ctx, id, "", "", []string{"comment"}, SendDiffReviewCommentsInput{})
			return err
		},
		"SendPRDiffReviewComments": func(ctx context.Context, id string) error {
			_, err := app.SendPRDiffReviewComments(ctx, gitops.PRReference{}, id, "", "", []string{"comment"}, SendDiffReviewCommentsInput{})
			return err
		},
		"RunThreadShellCommand": func(ctx context.Context, id string) error {
			_, err := app.RunThreadShellCommand(ctx, id, ShellCommandRequest{RunID: "run", Command: "true"})
			return err
		},
	}
}

// TestDrivingAnAutonomousThreadNeedsAutonomy. These select nothing here.
// Their effective mode is the mode the TARGET already runs in, because
// sending into a full-access thread commits the agent to acting without
// approval gates just as surely as selecting it does, and a command the
// person runs sends its output to that agent.
func TestDrivingAnAutonomousThreadNeedsAutonomy(t *testing.T) {
	t.Parallel()
	app, _, ungranted := autonomyApp(t)
	ctx := callFrom(ungranted, false)
	autonomous := threadInMode(t, app, provider.RuntimeFullAccess)
	gated := threadInMode(t, app, provider.RuntimeApprovalRequired)

	for name, call := range threadDrivingCalls(app) {
		t.Run(name, func(t *testing.T) {
			wantScopeRefusal(t, call(ctx, autonomous.ID), transport.ScopeThreadsAutonomy)
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

// threadDrivePrimitives are the calls that put a message on a thread or run
// a command in it. shellCommands.Start is matched by its receiver as well.
var threadDrivePrimitives = map[string]bool{
	"sendMessageWithOptions":      true,
	"sendMessageLocked":           true,
	"steerMessageWithOptions":     true,
	"registerQueueItem":           true,
	"queueAgentNotice":            true,
	"dispatchFlush":               true,
	"dispatchFlushWithGeneration": true,
}

// threadDriveDepth is how many calls below a bound method the walk follows.
// The walk matches callees by name, and past this depth names shared by
// unrelated receivers (Start, Cancel, Send) join paths that do not exist.
const threadDriveDepth = 3

// TestEveryThreadDrivingMethodIsListed derives the methods that drive a
// thread from the code, so a new one cannot skip the autonomy check or its
// test. A bound method that reaches a drive primitive must pass
// requireAutonomyForThread on the way, in its own body or in a helper the
// path goes through, unless its floor is threads:autonomy. Every bound
// method that calls the check itself must be in threadDrivingCalls, which
// proves the check refuses.
//
// The walk is by name over internal/app, follows calls threadDriveDepth
// deep, and does not enter function literals below the method itself, where
// a constructor wires callbacks it does not call. A drive that leaves the
// package through a service's callback is beyond it.
func TestEveryThreadDrivingMethodIsListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("internal/app")
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string][]*ast.FuncDecl{}
	var bound []*ast.FuncDecl
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join("internal/app", name), nil,
			parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			decls[fn.Name.Name] = append(decls[fn.Name.Name], fn)
			if isAppMethod(fn) && fn.Name.IsExported() && boundScope(fn) != "" {
				bound = append(bound, fn)
			}
		}
	}

	// drives reports the path from fn to a primitive that no autonomy check
	// interrupts, or nil.
	var drives func(fn *ast.FuncDecl, depth int, visiting map[string]bool) []string
	drives = func(fn *ast.FuncDecl, depth int, visiting map[string]bool) []string {
		if callsAutonomyCheck(fn) || visiting[fn.Name.Name] {
			return nil
		}
		visiting[fn.Name.Name] = true
		defer delete(visiting, fn.Name.Name)
		var path []string
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if path != nil {
				return false
			}
			if _, literal := node.(*ast.FuncLit); literal && depth > 0 {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var callee string
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				callee = fun.Name
			case *ast.SelectorExpr:
				callee = fun.Sel.Name
				if field, ok := fun.X.(*ast.SelectorExpr); ok && field.Sel.Name == "shellCommands" && callee == "Start" {
					path = []string{"shellCommands.Start"}
					return false
				}
			}
			if threadDrivePrimitives[callee] {
				path = []string{callee}
				return false
			}
			if depth == threadDriveDepth {
				return true
			}
			for _, next := range decls[callee] {
				if below := drives(next, depth+1, visiting); below != nil {
					path = append([]string{callee}, below...)
					return false
				}
			}
			return true
		})
		return path
	}

	listed := threadDrivingCalls(nil)
	checking := map[string]bool{}
	for _, fn := range bound {
		name := fn.Name.Name
		if callsAutonomyCheck(fn) {
			checking[name] = true
			if _, ok := listed[name]; !ok {
				t.Errorf("%s checks autonomy; add it to threadDrivingCalls so a test proves the check refuses", name)
			}
			continue
		}
		if boundScope(fn) == string(transport.ScopeThreadsAutonomy) {
			continue
		}
		if path := drives(fn, 0, map[string]bool{}); path != nil {
			t.Errorf("%s drives a thread (%s > %s) without requireAutonomyForThread: a session holding "+
				"threads:operate but not threads:autonomy could drive a full-access thread through it",
				name, name, strings.Join(path, " > "))
		}
	}
	for name := range listed {
		if !checking[name] {
			t.Errorf("threadDrivingCalls lists %s, which is not a bound method that checks autonomy", name)
		}
	}
}

// boundScope is a method's //ao:scope value, or empty for a method that is
// not bound.
func boundScope(fn *ast.FuncDecl) string {
	if fn.Doc == nil {
		return ""
	}
	for _, comment := range fn.Doc.List {
		if scope, ok := strings.CutPrefix(comment.Text, "//ao:scope "); ok {
			return strings.TrimSpace(scope)
		}
	}
	return ""
}

// callsAutonomyCheck reports whether fn calls requireAutonomyForThread.
func callsAutonomyCheck(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "requireAutonomyForThread" {
				found = true
			}
		}
		return !found
	})
	return found
}

// TestSelectingAnAutonomousModeStillNeedsAutonomy is the original
// selection case, which the resolved-mode rule widened rather than
// replaced: an explicit argument is judged as the override it is, on a
// thread whose current mode would have passed on its own.
func TestSelectingAnAutonomousModeStillNeedsAutonomy(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	app, _, limited := settingsTierApp(t)

	patch := map[string]any{"fontSize": 15}
	if err := app.requireSettingsTier(callFrom(limited, false), patch); err != nil {
		t.Fatalf("a device-tier key from a session with no settings grant: %v", err)
	}
}

// TestUserTierSettingNeedsSettingsWrite: a working preference follows the
// person, and writing one is a grant they gave this device.
func TestUserTierSettingNeedsSettingsWrite(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

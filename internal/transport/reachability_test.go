package transport

import "testing"

// What reaches an off-host client is decided by ONE gate now
// (docs/specs/remote-access.md §5). The origin partition this file used
// to freeze — `LocalOnlyMethods`, its `transitionalReachability`
// overrides, and the `preScopeTableLocalOnly` list they were held
// against — is deleted: every off-host connection names a session whose
// binding class permits it (wave 6d1 plus internal/app's
// bindingAdmitsPeer), and `AuthorizeSessionMethod` compares that
// session's grants against the method's scope on every call.
//
// The cases below pin what that gate answers for the two groups the
// override map used to hold, because those are the reachability CHANGES
// the deletion performs rather than incidental consequences of it.

// grantsFor is the scope set a session holding exactly these names has,
// in the shape Config.SessionScopes answers.
func grantsFor(scopes ...Scope) []string {
	granted := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		granted = append(granted, string(scope))
	}
	return granted
}

// workspaceContentMethods is the old override map's group 3: workspace
// content the spec places in `files:read` (observe) that this tree locked
// to loopback in 2026-05, before the scope table existed. Unlocking them
// for a session granted the scope is what `files:read` is FOR, and it is
// the deliberate reachability change the origin gate's deletion performs.
//
// Three of them read attachment bytes or per-thread review notes and
// carry `threads:read` instead; they moved with the diffs for the same
// reason and are listed with the scope each actually declares.
var workspaceContentMethods = map[string]Scope{
	"GetBranchBaseDiff":         ScopeFilesRead,
	"GetWorkingTreeDiff":        ScopeFilesRead,
	"GetWorkspaceCurrentDiff":   ScopeFilesRead,
	"GetCommitDiff":             ScopeFilesRead,
	"GetDiffContextLines":       ScopeFilesRead,
	"VerifyEditDiffs":           ScopeFilesRead,
	"HighlightPatchWithContext": ScopeFilesRead,
	"SearchWorkspaceFiles":      ScopeFilesRead,
	"GetLocalImageData":         ScopeFilesRead,
	"ListDiffReviewComments":    ScopeThreadsRead,
	"GetAttachmentData":         ScopeThreadsRead,
	"GetAttachmentThumbnail":    ScopeThreadsRead,
}

// bookkeepingMutations is the old override map's group 1: thread,
// project and discussion bookkeeping that is execute-tier by any honest
// reading and was wire-reachable before the scope table existed. The
// overrides kept them reachable while "execute implies local-only" was
// the rule; with that rule gone, `threads:operate` is what carries them.
var bookkeepingMutations = []string{
	"ArchiveProject", "UnarchiveProject", "DeleteProject", "RenameProject",
	"UpdateProjectSortPositions", "ArchiveThread", "UnarchiveThread",
	"DeleteThread", "RenameThread", "PinThread", "UnpinThread",
	"SetThreadPinGroup", "MarkThreadRead", "MarkThreadUnread", "SwitchThread",
	"CreateDiscussion", "UpdateDiscussion", "DeleteDiscussion",
	"CreateProposedPlanComment", "UpdateProposedPlanComment",
	"DeleteProposedPlanComment",
}

// TestWorkspaceContentAnswersASessionGrantedTheScope is the unlock,
// asserted at the gate that now decides it. A session holding the scope
// passes; one holding every OTHER declared scope is refused with the
// typed `scope_required` naming what it lacks, so the refusal is the
// grant's absence rather than the caller's address.
//
// hostPresent is false throughout: these are exactly the calls that had
// no answer for an off-host caller before this wave.
func TestWorkspaceContentAnswersASessionGrantedTheScope(t *testing.T) {
	for name, scope := range workspaceContentMethods {
		t.Run(name, func(t *testing.T) {
			if got := classify(name).Scope; got != scope {
				t.Fatalf("scope = %q, want %q; this list records what each method declares", got, scope)
			}
			if fe := AuthorizeSessionMethod(grantsFor(scope), name, false); fe != nil {
				t.Fatalf("a session granted %q was refused: %+v", scope, fe)
			}

			var without []Scope
			for _, other := range Scopes {
				if other != scope {
					without = append(without, other)
				}
			}
			fe := AuthorizeSessionMethod(grantsFor(without...), name, false)
			if fe == nil {
				t.Fatalf("a session holding every scope EXCEPT %q was admitted", scope)
			}
			if fe.Code != ErrCodeScopeRequired || fe.Scope != string(scope) {
				t.Fatalf("refusal = {%s, %s}, want {%s, %s}", fe.Code, fe.Scope, ErrCodeScopeRequired, scope)
			}
		})
	}
}

// TestBookkeepingMutationsRideThreadsOperate is the same statement for
// the other group: the reachability these methods had is now a grant a
// session either holds or does not.
func TestBookkeepingMutationsRideThreadsOperate(t *testing.T) {
	for _, name := range bookkeepingMutations {
		t.Run(name, func(t *testing.T) {
			if got := classify(name).Scope; got != ScopeThreadsOperate {
				t.Fatalf("scope = %q, want %q", got, ScopeThreadsOperate)
			}
			if fe := AuthorizeSessionMethod(grantsFor(ScopeThreadsOperate), name, false); fe != nil {
				t.Fatalf("a session granted %q was refused: %+v", ScopeThreadsOperate, fe)
			}
			fe := AuthorizeSessionMethod(grantsFor(ScopeThreadsRead), name, false)
			if fe == nil || fe.Code != ErrCodeScopeRequired || fe.Scope != string(ScopeThreadsOperate) {
				t.Fatalf("a read-only session got %+v, want %s naming %s", fe, ErrCodeScopeRequired, ScopeThreadsOperate)
			}
		})
	}
}

// TestHostScopedMethodsStayRefusedForEveryOffHostSession is the floor the
// deletion must not lower. `host` names a call with no remote form, no
// session may be granted it, and holding every OTHER scope changes
// nothing — otherwise deleting the origin gate would have opened the
// host surface rather than moved its key.
func TestHostScopedMethodsStayRefusedForEveryOffHostSession(t *testing.T) {
	everything := grantsFor(Scopes...)
	checked := 0
	for _, method := range GeneratedMethods {
		if method.Scope != ScopeHost {
			continue
		}
		checked++
		fe := AuthorizeSessionMethod(everything, method.Name, false)
		if fe == nil {
			t.Errorf("%s is host-scoped and answered a session that is not on this machine", method.Name)
			continue
		}
		if fe.Code != ErrCodeScopeRequired || fe.Scope != string(ScopeHost) {
			t.Errorf("%s refusal = {%s, %s}, want {%s, %s}", method.Name, fe.Code, fe.Scope, ErrCodeScopeRequired, ScopeHost)
		}
	}
	if checked == 0 {
		t.Fatal("no host-scoped method in the generated table; this gate is asserting nothing")
	}
}

// TestStepUpMethodsRefuseAnOffHostSession — §4 puts these calls behind a
// per-call proof, and this phase the proof is host presence. They were
// local-only by derivation before; nothing about the deletion may turn a
// mandatory proof into a standing grant.
//
// A step-up method that is ALSO `host`-scoped is refused for the scope
// first, which is the stricter of the two answers and equally final, so
// both codes count as "refused" here and the case records which.
func TestStepUpMethodsRefuseAnOffHostSession(t *testing.T) {
	everything := grantsFor(Scopes...)
	for _, method := range GeneratedMethods {
		if !method.StepUp {
			continue
		}
		want := ErrCodeStepUpRequired
		if method.Scope == ScopeHost {
			want = ErrCodeScopeRequired
		}
		fe := AuthorizeSessionMethod(everything, method.Name, false)
		if fe == nil || fe.Code != want {
			t.Errorf("%s is //ao:stepup and answered %+v for an off-host session, want %s", method.Name, fe, want)
		}
	}
}

// TestEveryGeneratedMethodCarriesADeclaredScope is the runtime-side
// half of the generator's completeness gate. methodgen refuses to emit
// an unannotated method, so this cannot fail from a missing annotation
// — what it catches is a scope that the tier table does not place,
// which would derive as local-only for a reason nobody wrote down.
func TestEveryGeneratedMethodCarriesADeclaredScope(t *testing.T) {
	for _, method := range GeneratedMethods {
		if !method.Scope.Valid() {
			t.Errorf("%s carries scope %q, which internal/transport/scopes.go does not declare", method.Name, method.Scope)
		}
	}
}

// TestStepUpMethodsAreTheSpecSet pins the step-up annotations to §4's
// list. Step-up is mandatory-or-nothing there: these are the calls that
// re-key the system or re-route every prompt, so one dropped //ao:stepup
// turns a required per-call proof into an ambient standing grant, and
// nothing else in the tree would notice.
func TestStepUpMethodsAreTheSpecSet(t *testing.T) {
	want := map[string]string{
		"MintDevicePairing":            "minting a pairing link",
		"SetNetworkSettings":           "network bind / exposure change",
		"SetProviderCustomEnvVar":      "provider custom-env write",
		"DeleteProviderCustomEnvVar":   "provider custom-env write",
		"SetThreadMcpServerEnabled":    "MCP config write",
		"SetWorkspaceMcpServerEnabled": "MCP config write",
		"SetWSLDistroPreference":       "WSL distro preference",
		"SetProjectWorktreeSetup":      "worktree-setup recipe write: stores argv that runs unattended on every worktree cut",
	}
	got := map[string]bool{}
	for _, method := range GeneratedMethods {
		if method.StepUp {
			got[method.Name] = true
		}
	}
	for name, why := range want {
		if !got[name] {
			t.Errorf("%s lost its //ao:stepup annotation (%s, docs/specs/remote-access.md §4)", name, why)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is annotated //ao:stepup and §4 does not list it; widen the spec set here deliberately or drop the directive", name)
		}
	}
}

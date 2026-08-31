package transport

// InternalServiceMethods names every method on the App receiver that
// the dispatcher must NEVER expose on the wire, regardless of the
// AllowList. The set has two sources:
//
//  1. Wails framework lifecycle hooks (`ServiceName`, `ServiceStartup`,
//     `ServiceShutdown`, `ServeHTTP`) — Wails' own bindings package
//     filters these at framework level; we mirror that behavior so a
//     change in Wails doesn't accidentally expose them through us.
//
//  2. App-level lifecycle hooks marked //wails:ignore in source. These
//     get stripped from the binding generator's output AND skipped at
//     runtime by methodgen + the dispatcher. We also list them here so
//     a developer who accidentally drops the //wails:ignore directive
//     can't reach them from the wire — defense-in-depth alongside the
//     codegen filter.
//
// The codegen tool reads this list (via go-source AST) and the runtime
// dispatcher reads it directly. Single source of truth — change here
// when an App method should disappear from the wire surface.
var InternalServiceMethods = map[string]bool{
	// Wails framework lifecycle.
	"ServiceName":     true,
	"ServiceStartup":  true,
	"ServiceShutdown": true,
	"ServeHTTP":       true,

	// App-level wiring hooks. Marked //wails:ignore in source, but
	// listed here so the dispatcher refuses to expose them even if a
	// developer drops the directive on a future edit.
	"SetEventBus":        true,
	"SetTransportServer": true,
	"Shutdown":           true,
}

// LocalOnlyMethods names every App method that must refuse calls from
// non-loopback peers when the server is bound to a LAN interface.
//
// DERIVED from the generated scope table (methods_gen.go): a method is
// local-only when the tier of its `//ao:scope` is not observe, or when
// it requires step-up. One hand-edited source, which is the point —
// before this, four overlapping classifications each had to be updated
// by hand and none of them referenced the others
// (docs/specs/remote-access.md §5). The origin gate this set feeds is
// itself temporary: it disappears once every client authenticates, and
// the scopes it is derived from are what remains.
//
// It stays map[string]bool because that is what the dispatcher's hot
// path wants (one lookup, no comparison against a typed zero) and what
// every existing caller and gate reads.
//
// The dispatcher rejects local-only calls from non-loopback peers with
// ErrCodeMethodNotFound rather than a distinct "forbidden" code: the
// wire shape is indistinguishable from a probe of an unregistered
// method, so a LAN scanner cannot fingerprint which methods are
// privileged vs simply absent. The bookkeeping cost is one map lookup
// per call.
var LocalOnlyMethods = func() map[string]bool {
	set := make(map[string]bool, len(GeneratedMethods))
	for _, method := range GeneratedMethods {
		localOnly := derivedLocalOnly(method)
		if override, ok := transitionalReachability[method.Name]; ok {
			localOnly = override.LocalOnly
		}
		if localOnly {
			set[method.Name] = true
		}
	}
	return set
}()

// derivedLocalOnly is the rule, with no overrides applied: privileged
// scope implies local-only. Named separately so the reachability test
// can ask what the scopes alone say and fail an override that no longer
// changes the answer.
func derivedLocalOnly(method MethodMeta) bool {
	return method.Scope.Tier() != TierObserve || method.StepUp
}

// transitionalOverride records one method whose reachability is pinned
// against the derivation, and why.
type transitionalOverride struct {
	// LocalOnly is the reachability this method keeps, in place of the
	// one its scope implies.
	LocalOnly bool
	// Reason is why the honest annotation and today's reachability
	// disagree. One line, and it is the whole argument for the entry.
	Reason string
}

// transitionalReachability is MIGRATION SCAFFOLDING and is expected to
// empty out.
//
// The annotation wave that introduced the scope table was required to
// change no method's reachability, so every honest annotation that
// disagreed with the partition it inherited landed here instead of
// quietly moving a method on or off the LAN. Each entry is therefore a
// live question — "is the scope wrong, or is today's cut wrong?" — for
// the phase-3 enforcement work to answer, one at a time, by DELETING
// the entry once the two agree.
//
// The map only ever shrinks: TestTransitionalOverridesAreLoadBearing
// fails on an entry that no longer contradicts the derivation, so a
// scope change that resolves a question cannot leave its scaffolding
// standing. Nothing new belongs here — a genuinely new method takes the
// reachability its scope implies.
//
// The entries fall into three groups, which is the useful shape of the
// disagreement:
//
//  1. Bookkeeping mutations that are execute-tier by any honest reading
//     (rename a thread, archive a project) and are wire-reachable today.
//     "Execute implies local-only" is what puts them here; whether that
//     rule or that reachability is wrong is the adjudication.
//  2. The settings and preferences surface. The getters left when
//     `settings:read` (observe) was added; the ui_state WRITES remain,
//     because §6 puts device-tier writes on "a valid session" and the
//     vocabulary has no name for that floor.
//  3. Workspace content reads the spec places in `files:read` (observe)
//     that this tree deliberately locked to loopback in 2026-05, before
//     the scope table existed.
var transitionalReachability = map[string]transitionalOverride{
	// 1. Thread, project, and discussion bookkeeping: execute-tier
	// mutations that reach no provider session, no process, and no path.
	"ArchiveProject":             {Reason: "project bookkeeping mutation; wire-reachable since the LAN bind shipped"},
	"UnarchiveProject":           {Reason: "archive's inverse, same surface"},
	"DeleteProject":              {Reason: "deletes no branch, so it destroys nothing git cannot still reach (D25)"},
	"RenameProject":              {Reason: "renames a row; the project's path and repo are untouched"},
	"UpdateProjectSortPositions": {Reason: "sidebar ordering, the emptiest write in the set"},
	"ArchiveThread":              {Reason: "closes that thread's own session, which starts nothing and can run nothing"},
	"UnarchiveThread":            {Reason: "archive's inverse, same surface"},
	"DeleteThread":               {Reason: "has closed the thread's session for as long as it has existed"},
	"RenameThread":               {Reason: "renames a row"},
	"PinThread":                  {Reason: "sidebar placement"},
	"UnpinThread":                {Reason: "pin's inverse"},
	"SetThreadPinGroup":          {Reason: "sidebar placement"},
	"MarkThreadRead":             {Reason: "read-state flag; the side effect of having looked"},
	"MarkThreadUnread":           {Reason: "read-state flag"},
	"SwitchThread":               {Reason: "records which thread has focus"},
	"CreateDiscussion":           {Reason: "deliberation CRUD; the turn-driving half is PostChannelMessage, which is not here"},
	"UpdateDiscussion":           {Reason: "deliberation CRUD"},
	"DeleteDiscussion":           {Reason: "deliberation CRUD"},
	"CreateProposedPlanComment":  {Reason: "plan comments are notes until SendPlanRevisionComments hands them to a provider"},
	"UpdateProposedPlanComment":  {Reason: "plan comment CRUD"},
	"DeleteProposedPlanComment":  {Reason: "plan comment CRUD"},

	// 2. What is left of the settings and preferences surface. The eight
	// GETTERS that sat here are gone: `settings:read` (observe) now names
	// what they do, so the derivation and today's reachability agree and
	// the overrides had nothing left to pin. These two are WRITES, and
	// they stay because §6 puts device-tier writes on "a valid session"
	// while the vocabulary can only spell `settings:write` — a floor no
	// session minted today lacks, so nothing is narrowed by waiting.
	"SetUIState":    {Reason: "the bucket is derived from the connection, so there is no argument in which to ask for another device's"},
	"DeleteUIState": {Reason: "same bucket, same derivation"},

	// 3. Workspace content the spec calls observe and this tree locked
	// to loopback before the scope table existed. Unlocking them is the
	// point of `files:read`; it is a deliberate reachability change and
	// belongs to the enforcement wave, not to an annotation pass.
	"GetBranchBaseDiff":         {LocalOnly: true, Reason: "diff bytes: uncommitted and unpushed work in one call"},
	"GetWorkingTreeDiff":        {LocalOnly: true, Reason: "diff bytes, including uncommitted edits"},
	"GetWorkspaceCurrentDiff":   {LocalOnly: true, Reason: "diff bytes, including uncommitted edits"},
	"GetCommitDiff":             {LocalOnly: true, Reason: "diff bytes for committed-but-unpushed work"},
	"GetDiffContextLines":       {LocalOnly: true, Reason: "reads workspace or ref file content by line range"},
	"VerifyEditDiffs":           {LocalOnly: true, Reason: "runs the same content resolution to report servability"},
	"HighlightPatchWithContext": {LocalOnly: true, Reason: "resolves workspace or ref file content by path to prime spans"},
	"SearchWorkspaceFiles":      {LocalOnly: true, Reason: "shells `git ls-files` in a cwd the thread record supplies"},
	"GetLocalImageData":         {LocalOnly: true, Reason: "its arguments select a host file"},
	"ListDiffReviewComments":    {LocalOnly: true, Reason: "rides the diff surface it annotates; moves when the diffs do"},
	"GetAttachmentData":         {LocalOnly: true, Reason: "attachment bytes off the host's disk; reads ride payload auth, which is not built yet"},
	"GetAttachmentThumbnail":    {LocalOnly: true, Reason: "attachment bytes off the host's disk"},
}

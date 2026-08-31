package transport

import "strconv"

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

// LocalOnlyCategory is the closed set of reasons a method refuses a
// non-loopback caller. Every entry in localOnlyCategories carries one,
// and TestLocalOnlyCategorySetIsClosed fails on a value outside this
// list — so the categories are a type the compiler checks rather than a
// numbering in a comment that a new block can quietly extend.
//
// The list was ten numbered blocks in prose before it was a type, and
// the header above the map claimed six. That gap is the whole argument:
// a category set nothing enforces drifts from the set the entries
// actually use, and the drift is invisible because both halves read
// fine on their own.
//
// Ordinals are stable and start at 1 so the zero value is "untagged"
// rather than a real category — an entry written without a tag fails
// rather than silently joining category 1. They match the numbered
// blocks in the map below, which are the same categories with their
// worked reasoning attached.
type LocalOnlyCategory uint8

const (
	// CategoryLocalExecution is execution-equivalent access to the
	// host: terminal spawn and IO, opening files in editors, directory
	// browsing, writing files into the workspace, git operations that
	// mutate the local repo or invoke `gh`, spawning a provider CLI to
	// answer a question, and saving payload bytes to disk. A caller
	// reaching one of these from a LAN peer has effective shell access.
	// Bulk local-content reads are here too — a diff is a file read
	// with a nicer wire shape.
	CategoryLocalExecution LocalOnlyCategory = iota + 1

	// CategorySessionControl is starting, stopping, interrupting,
	// steering, or answering a provider session. Each of those spawns
	// or drives a CLI subprocess that executes on the host, so a
	// LAN-attached caller driving one would be running prompts and tool
	// calls in the user's shell context.
	CategorySessionControl

	// CategorySettingsMutation is anything that reconfigures the
	// running server: the LAN-bind toggle, the remote-endpoint store,
	// editor preferences, keybindings, appearance, provider
	// environment, a generic settings patch. A peer that can flip
	// BindAll or rewrite the origin allow-list has undone the opt-in
	// model it is attached through.
	CategorySettingsMutation

	// CategoryAttachmentPayload is the attachment surface on both
	// sides. Writes put user-chosen bytes on disk under the config
	// dir; reads pull them back, and the read side is here for the
	// same reason the diffs are — an attachment id is opaque on the
	// wire, but a caller enumerating thread ids collects every
	// uploaded file in one pass.
	CategoryAttachmentPayload

	// CategoryLocalFSBookkeeping is app-managed files under the user's
	// config directory: the UI render trace, the frontend error log,
	// and the paths that name them. Rotation-capped or not, a remote
	// peer has no business writing this host's disk or learning its
	// directory layout.
	CategoryLocalFSBookkeeping

	// CategoryCredentialEnumeration is disclosure of credentials or of
	// what accounts and endpoints exist. A stored token handed back
	// verbatim is a leak in one call; an endpoint or account listing is
	// the map for the next one, which is why it stays here even with
	// the secret fields stripped.
	CategoryCredentialEnumeration

	// CategoryWSLInventory is the Windows launcher's distro inventory
	// and persisted preference. Listing spawns wsl.exe and reading the
	// preference reads the launcher's own config file, so it is
	// execution and host fingerprinting on one small surface, paired
	// with its mutation companion in CategorySettingsMutation.
	CategoryWSLInventory

	// CategoryMCPState is the MCP server surface: status reads that
	// spawn the provider's CLI under the user's bearer tokens, toggles
	// that rewrite provider-native config or drive a live session, and
	// listings that enumerate what is installed. The whole surface goes
	// together — conservatively and consistently — rather than being
	// split by which half looks like a read.
	CategoryMCPState

	// CategoryDesktopHostControl is control over THIS desktop: in-app
	// self-update (network fetch, binary written to disk, swap helper
	// spawned, host process quit), the launcher's install-status
	// report that settles an in-flight update, and the renderer memory
	// trim. A remote peer's state says nothing about the desktop
	// session, and letting one drive these would be driving somebody
	// else's machine.
	CategoryDesktopHostControl

	// CategorySessionImport is reading the user's provider homes
	// (~/.claude, ~/.codex) and ingesting what is there. No process is
	// spawned, but the answers are absolute paths, workspace paths, and
	// prompt text — a directory listing of every conversation on the
	// host, which is the widest local-FS read in the set.
	CategorySessionImport

	// CategoryDeviceAccess is the device-access surface: minting a
	// pairing link, confirming or cancelling one, the device and session
	// inventory, the credential audit log, and revocation.
	//
	// Not folded into CategoryCredentialEnumeration, which covers
	// DISCLOSURE. Minting is issuance — one call turns into a credential
	// a new device holds — and revocation withdraws every credential a
	// device has. A surface that can add and remove the peers of a
	// backend is the surface that decides who reaches it at all, so it is
	// answered only where the owner already is.
	CategoryDeviceAccess
)

// localOnlyCategoryNames is the reverse map, used to name a category in
// a failure message. It is also the declared list: the closedness test
// reads it, so a constant added above without a name here fails rather
// than passing as an unnamed ordinal.
var localOnlyCategoryNames = map[LocalOnlyCategory]string{
	CategoryLocalExecution:        "local execution",
	CategorySessionControl:        "session control",
	CategorySettingsMutation:      "settings mutation",
	CategoryAttachmentPayload:     "attachment payload",
	CategoryLocalFSBookkeeping:    "local FS bookkeeping",
	CategoryCredentialEnumeration: "credential and account enumeration",
	CategoryWSLInventory:          "WSL inventory",
	CategoryMCPState:              "MCP state",
	CategoryDesktopHostControl:    "desktop host control",
	CategorySessionImport:         "session import",
	CategoryDeviceAccess:          "device access",
}

// String names the category, or reports the ordinal when it has no name
// — which the closedness test then fails on.
func (c LocalOnlyCategory) String() string {
	if name, ok := localOnlyCategoryNames[c]; ok {
		return name
	}
	return "LocalOnlyCategory(" + strconv.Itoa(int(c)) + ")"
}

// LocalOnlyMethods names every App method that must refuse calls from
// non-loopback peers when the server is bound to a LAN interface. It is
// derived from localOnlyCategories below — the authored half, where each
// name carries the category that put it there — so the two cannot
// disagree and there is nowhere to add a method without saying why.
//
// It stays map[string]bool because that is what the dispatcher's hot
// path wants (one lookup, no comparison against a typed zero) and what
// every existing caller and gate reads. Callers that want the reason
// ask LocalOnlyCategoryOf.
//
// The dispatcher rejects local-only calls from non-loopback peers with
// ErrCodeMethodNotFound rather than a distinct "forbidden" code: the
// wire shape is indistinguishable from a probe of an unregistered
// method, so a LAN scanner can't fingerprint which methods are
// privileged vs simply absent. The bookkeeping cost is one map lookup
// per call.
//
// Intentionally NOT in this set: GetKeybindings and its theme
// counterpart GetThemeFiles. The frontend's
// keybindings loader has no client-side defaults — a method_not_found
// refusal zeros every keyboard shortcut for remote-browser users. The
// returned data is UI preferences (chord → command), not credentials,
// and the mutation companions (UpdateKeybindings / ResetKeybindings)
// stay in CategorySettingsMutation so a LAN-attached peer still cannot
// rewrite the user's bindings. A future reviewer running this same
// exercise: don't silently flip it without revisiting the
// remote-browser UX cost. GetThemeFiles reads the same way: theme files
// are opaque UI preference text, the frontend degrades to built-in
// themes without them, and its writers stay in
// CategorySettingsMutation. GetSpinnerFiles joins them on identical
// grounds — the user's custom working-indicator sprites out of a
// sibling directory of the same config dir, with no write companion to
// classify at all (sprites are authored by dropping files in).
//
// Updates to this set must keep the names in sync with the App-side
// declarations. methods_gen_test.go gates that contract by failing if
// any LocalOnlyMethods entry is missing from GeneratedMethods.
var LocalOnlyMethods = func() map[string]bool {
	set := make(map[string]bool, len(localOnlyCategories))
	for name := range localOnlyCategories {
		set[name] = true
	}
	return set
}()

// LocalOnlyCategoryOf reports which class put a method in the set, and
// whether it is in the set at all. The dispatcher does not need it —
// membership is the whole decision there — but a refusal that can say
// what kind of thing it refused is worth more to whoever reads the
// failure than a bare "not found".
func LocalOnlyCategoryOf(method string) (LocalOnlyCategory, bool) {
	category, ok := localOnlyCategories[method]
	return category, ok
}

// localOnlyCategories is the authored half of LocalOnlyMethods: every
// method that must refuse a non-loopback caller, each tagged with the
// LocalOnlyCategory that put it there. The numbered blocks below carry
// the worked reasoning for each class and, where an entry does not
// follow obviously from its block, for the entry.
//
// A block heading and a tag are not the same thing. The tag is what the
// gate reads, so an entry may sit in the block where its neighbours are
// while carrying the category it actually belongs to —
// GetCodexAccountUsage, which used to be numbered 8b, is the live
// example, and its comment says so.
var localOnlyCategories = map[string]LocalOnlyCategory{
	// 1. CategoryLocalExecution — RCE-equivalent (FS / process touching).
	"OpenTerminal":         CategoryLocalExecution,
	"ListTerminals":        CategoryLocalExecution,
	"GetTerminalReplay":    CategoryLocalExecution,
	"WriteTerminal":        CategoryLocalExecution,
	"RestartTerminal":      CategoryLocalExecution,
	"CloseTerminal":        CategoryLocalExecution,
	"CloseThreadTerminals": CategoryLocalExecution,
	"ResizeTerminal":       CategoryLocalExecution,
	"RefreshTerminal":      CategoryLocalExecution,
	"MoveThreadTerminals":  CategoryLocalExecution,
	// RetryThreadWorktreeSetup executes the project's argv setup commands in
	// the thread's worktree; GetThreadWorktreeSetup returns their captured
	// output. RCE and the transcript of it, same pairing as
	// RestartTerminal / GetTerminalReplay.
	"RetryThreadWorktreeSetup": CategoryLocalExecution,
	"GetThreadWorktreeSetup":   CategoryLocalExecution,

	"OpenInEditor":                CategoryLocalExecution,
	"OpenExternalURL":             CategoryLocalExecution,
	"BrowseDirectory":             CategoryLocalExecution,
	"SavePayloadToFile":           CategoryLocalExecution,
	"ClearBrowserSiteData":        CategoryLocalExecution,
	"BrowserCompanionSubscribe":   CategoryLocalExecution,
	"BrowserCompanionNextFrame":   CategoryLocalExecution,
	"BrowserCompanionUnsubscribe": CategoryLocalExecution,
	"BrowserCompanionResize":      CategoryLocalExecution,
	"BrowserCompanionDo":          CategoryLocalExecution,
	"BrowserCompanionInput":       CategoryLocalExecution,
	"WriteThreadWorkspaceFile":    CategoryLocalExecution,
	"GitPush":                     CategoryLocalExecution,
	"GitStatusSubscribe":          CategoryLocalExecution,
	"GitStatusUnsubscribe":        CategoryLocalExecution,
	"GetGitStatus":                CategoryLocalExecution,
	// GetWorkspaceActivity answers two integer counters, but it takes a
	// caller-supplied path and resolves it through EvalSymlinks — a
	// filesystem probe, and therefore an existence-and-shape oracle for a
	// LAN token-holder over any path they care to name. It sits with the
	// rest of the workspace-path surface for that reason, not for what it
	// returns.
	"GetWorkspaceActivity":           CategoryLocalExecution,
	"GetGitStatusFastForProject":     CategoryLocalExecution,
	"GitCheckout":                    CategoryLocalExecution,
	"GitCheckoutForProject":          CategoryLocalExecution,
	"GitCreateBranch":                CategoryLocalExecution,
	"GitCreateBranchFrom":            CategoryLocalExecution,
	"GitCreateWorktree":              CategoryLocalExecution,
	"GitRemoveWorktree":              CategoryLocalExecution,
	"RemoveOtherWorktree":            CategoryLocalExecution,
	"RemoveOtherWorktreeForProject":  CategoryLocalExecution,
	"GitWorktreeStatus":              CategoryLocalExecution,
	"GitWorktreeStatusForProject":    CategoryLocalExecution,
	"GitListBranches":                CategoryLocalExecution,
	"GitListBranchesForProject":      CategoryLocalExecution,
	"GitListWorktrees":               CategoryLocalExecution,
	"GitListWorktreesForProject":     CategoryLocalExecution,
	"GitCommit":                      CategoryLocalExecution,
	"GitPull":                        CategoryLocalExecution,
	"GitStageAll":                    CategoryLocalExecution,
	"GitMaybeFetchRemotes":           CategoryLocalExecution,
	"GitMaybeFetchRemotesForProject": CategoryLocalExecution,
	"GitListBranchPruneCandidates":   CategoryLocalExecution,
	"GitPruneBranches":               CategoryLocalExecution,
	"GitSyncBranch":                  CategoryLocalExecution,
	"GitSyncBranchForProject":        CategoryLocalExecution,
	// GitCreatePR shells out to `gh` — same RCE-equivalent class as
	// the rest of the git/external-CLI surface.
	"GitCreatePR": CategoryLocalExecution,
	// PR review APIs shell out to gh/glab and expose remote review state
	// tied to local credentials; keep them with the forge CLI surface.
	"GetPRDetail":          CategoryLocalExecution,
	"GetPRDiff":            CategoryLocalExecution,
	"ListPRReviewThreads":  CategoryLocalExecution,
	"SubmitPRReview":       CategoryLocalExecution,
	"ReplyToPRThread":      CategoryLocalExecution,
	"SubscribePRUpdates":   CategoryLocalExecution,
	"UnsubscribePRUpdates": CategoryLocalExecution,
	"SetPRUpdatesActive":   CategoryLocalExecution,
	"GetPRMergeConflicts":  CategoryLocalExecution,
	"GetMergeConflictFile": CategoryLocalExecution,
	// CI surface: shells out to gh/glab; SavePRCIJobLog additionally
	// writes into the local ci-logs directory.
	"GetPRCIJobs":    CategoryLocalExecution,
	"GetPRCIJobLog":  CategoryLocalExecution,
	"SavePRCIJobLog": CategoryLocalExecution,
	// PrepareThreadWorktree creates a git worktree on disk; same
	// class as the Git* mutators above.
	"PrepareThreadWorktree": CategoryLocalExecution,
	// AttachThreadWorktree creates a worktree pointing at an existing
	// branch; same class as PrepareThreadWorktree.
	"AttachThreadWorktree": CategoryLocalExecution,
	// Diff-returning bindings expose bulk file content in a single wire
	// call. The threat shape matches the credential / endpoint
	// enumeration class below: a token-holder gets the user's
	// agent-edited code, in-progress work, and any non-gitignored config
	// (e.g. a forgotten `secrets.local`, an `.env.example` the agent
	// touched while iterating) in one call. Working-tree and
	// workspace-current diffs include uncommitted edits; commit diffs
	// expose committed-but-unpushed work.
	//
	// Lock the diff surface down loopback-only. UX cost is
	// "diff panels don't render from a remote browser"; that's a feature
	// nobody currently depends on, and locking down later (after a remote
	// workflow grows to need them) would be a breaking change.
	"GetBranchBaseDiff":       CategoryLocalExecution,
	"GetWorkingTreeDiff":      CategoryLocalExecution,
	"GetWorkspaceCurrentDiff": CategoryLocalExecution,
	"ListBranchCommits":       CategoryLocalExecution,
	"ListRecentCommits":       CategoryLocalExecution,
	"GetCommitDiff":           CategoryLocalExecution,
	"ListPRCommits":           CategoryLocalExecution,
	"GetPRCommitDiff":         CategoryLocalExecution,
	// GetDiffContextLines reads arbitrary workspace/ref file content by
	// line range (review hunk-gap expansion) — same bulk-content class.
	// VerifyEditDiffs runs the same content resolution (it only reports
	// servability, but the resolution reads workspace files by path).
	"GetDiffContextLines": CategoryLocalExecution,
	"VerifyEditDiffs":     CategoryLocalExecution,
	// HighlightPatchWithContext resolves workspace/ref file content by
	// path to prime span parsing — same class. The wire-safe
	// HighlightCode / HighlightPatch / HighlightClassNames RPCs are
	// pure text-in/metadata-out and deliberately NOT in this set.
	"HighlightPatchWithContext":  CategoryLocalExecution,
	"ListDiffReviewComments":     CategoryLocalExecution,
	"CreateDiffReviewComment":    CategoryLocalExecution,
	"UpdateDiffReviewComment":    CategoryLocalExecution,
	"DeleteDiffReviewComment":    CategoryLocalExecution,
	"MarkDiffReviewCommentsSent": CategoryLocalExecution,
	"SendDiffReviewComments":     CategoryLocalExecution,
	// Codex model discovery spawns the configured `codex app-server`
	// subprocess. It looks like a catalog read, but the local process
	// execution makes it loopback-only.
	"GetModelsForProvider": CategoryLocalExecution,
	// GetCodexSkills is the same shape as GetModelsForProvider one line up:
	// it rides a live `codex app-server` connection when one exists and
	// spawns a short-lived one otherwise, and the answer it returns names
	// absolute SKILL.md paths under the user's home and repo. Local process
	// execution plus host-path disclosure — loopback-only on both counts.
	"GetCodexSkills": CategoryLocalExecution,
	// GetClaudeSkills reads the user's ~/.claude/skills, the workspace's
	// .claude/skills, and enabled plugins' installation directories.
	// Pure filesystem reads (no spawn), but the answer enumerates what
	// is installed on the host — loopback-only like GetCodexSkills.
	"GetClaudeSkills":      CategoryLocalExecution,
	"CreateProject":        CategoryLocalExecution,
	"ListAvailableEditors": CategoryLocalExecution,
	// GenerateCommitMessage runs `claude` / `codex` in the workspace
	// cwd with --dangerously-skip-permissions and the model emits a
	// commit message from the staged diff. That's a local-process
	// invocation under user-attacker control (the workspace path is
	// derived from the thread); same class as the Git*/CLI surface.
	"GenerateCommitMessage": CategoryLocalExecution,
	// RegenerateThreadTitle runs `claude` / `codex` in the thread's
	// workspace cwd to re-title it from its own history. Same
	// local-process invocation under user-attacker control as
	// GenerateCommitMessage — the workspace path comes off the thread
	// record — so it takes the same classification.
	"RegenerateThreadTitle": CategoryLocalExecution,
	// SearchWorkspaceFiles shells `git ls-files` inside the thread's
	// workspace cwd. The argv is fixed but the cwd is user-supplied
	// through the thread record — keep with the rest of the local-CLI
	// invocations for doctrine consistency.
	"SearchWorkspaceFiles": CategoryLocalExecution,
	// Payload reads (GetPayloadPreview, GetPayloadChunk,
	// GetPayloadData) moved to wireSafeMethods — remote clients need
	// tool-call output, command results, and thinking blocks to render
	// the timeline. Authorization is enforced by getThreadPayloadMeta's
	// (threadID, payloadID) linkage check; the token is the security
	// boundary, matching every other wire-safe method.
	// SavePayloadToFile stays here: it writes to the host filesystem.

	// 2. CategorySessionControl (provider subprocess spawn / steer).
	"StartSession":             CategorySessionControl,
	"AutoResumeThread":         CategorySessionControl,
	"StopSession":              CategorySessionControl,
	"ReconnectSession":         CategorySessionControl,
	"SendMessage":              CategorySessionControl,
	"SendMessageWithOptions":   CategorySessionControl,
	"SteerMessageWithOptions":  CategorySessionControl,
	"SendPlanRevisionComments": CategorySessionControl,
	// NotificationActivated synthesizes desktop navigation after a native
	// notification click. Only the loopback Windows launcher may drive it;
	// a LAN-attached browser must not steer another client's pane focus.
	"NotificationActivated": CategorySessionControl,
	// Flush-queue surface: RegisterQueueItem stages a user message
	// and the dispatcher writes it to the local provider's stdin /
	// JSON-RPC as soon as possible. GetQueueState and GetThreadLiveState
	// return per-thread snapshots — disclosure of pending user-typed text
	// + attachment IDs + plan refs is the same threat shape as the
	// diff-returning bindings (category 6 below): in-progress drafted work
	// shouldn't be readable by a LAN token-holder. These bindings are
	// loopback-only.
	"RegisterQueueItem":      CategorySessionControl,
	"GetQueueState":          CategorySessionControl,
	"GetThreadLiveState":     CategorySessionControl,
	"SaveDraft":              CategorySessionControl,
	"GetDraft":               CategorySessionControl,
	"ClearDraft":             CategorySessionControl,
	"DeleteEmptyDraftThread": CategorySessionControl,
	"StartDiscussion":        CategorySessionControl,
	"StartDiscussionByID":    CategorySessionControl,
	// PostChannelMessage now has a side-effecting path: a human post
	// can arm the next participant's turn prompt (promptDiscussionSpeakerAsync),
	// which dispatches into that participant's live provider session
	// via the same SendMessage path above. What used to be a plain
	// data write became session control the moment turn-driving
	// landed — see the comment this displaces in
	// methods_gen_test.go's wireSafeMethods, which called out exactly
	// this re-audit trigger.
	"PostChannelMessage": CategorySessionControl,
	// Workflow mutations drive autonomous full-access provider sessions or
	// persist local settings state. Keep that control plane loopback-only.
	// The pure workflow reads live in methods_gen_test.go's wireSafeMethods
	// so remote workflow surfaces can render without gaining mutation access.
	"WorkflowStartRun":                     CategorySessionControl,
	"WorkflowCancelItem":                   CategorySessionControl,
	"WorkflowResumeItem":                   CategorySessionControl,
	"WorkflowScheduleResume":               CategorySessionControl,
	"WorkflowAnswerQuestion":               CategorySessionControl,
	"WorkflowResolveGate":                  CategorySessionControl,
	"WorkflowSetGlobalPause":               CategorySessionControl,
	"WorkflowCompleteTakeover":             CategorySessionControl,
	"WorkflowMergeItem":                    CategorySessionControl,
	"WorkflowCreateItemPR":                 CategorySessionControl,
	"WorkflowDiscardItem":                  CategorySessionControl,
	"WorkflowFetchPRReviewComments":        CategorySessionControl,
	"WorkflowSendPRReviewCommentsToThread": CategorySessionControl,
	"WorkflowDiscussPR":                    CategorySessionControl,
	"WorkflowSetJobNotes":                  CategorySessionControl,
	"WorkflowRerunItem":                    CategorySessionControl,
	"WorkflowPauseItem":                    CategorySessionControl,
	// A soft stop starts nothing by itself, but it decides whether the next wave
	// of autonomous sessions runs at all — the same control plane pause is on,
	// reached one boundary later.
	"WorkflowRequestSoftStop": CategorySessionControl,
	// Automation CRUD is the same control plane one step removed: an automation
	// is a standing instruction to start autonomous full-access provider sessions
	// on a schedule, so arming, editing, disabling, or deleting one is session
	// control even though no session starts inside the call. Run now starts one
	// outright. The pure read (WorkflowListAutomations) is wire-safe.
	"WorkflowCreateAutomation":     CategorySessionControl,
	"WorkflowUpdateAutomation":     CategorySessionControl,
	"WorkflowDeleteAutomation":     CategorySessionControl,
	"WorkflowSetAutomationEnabled": CategorySessionControl,
	"WorkflowRunAutomationNow":     CategorySessionControl,
	// Thread binding wires a run's results into a local provider session: a
	// bound run injects user turns into that thread from a background
	// goroutine.
	"WorkflowBindThread":   CategorySessionControl,
	"WorkflowUnbindThread": CategorySessionControl,
	// Discard preview reads local checkouts and repository history — dirty
	// paths and unmerged commit subjects — which is the same local-disclosure
	// class as the diff bindings. ProjectDeletionPreview reads the same local
	// checkouts across every run tree a project owns, so it inherits the
	// reasoning. DeleteProject itself stays wire reachable: it deletes no
	// branch, so it destroys nothing git cannot still reach (D25).
	"WorkflowDiscardPreview": CategorySessionControl,
	"ProjectDeletionPreview": CategorySessionControl,
	// Fan-out unit recovery is the same control plane one unit down: a retry
	// starts a provider session or a local command, a drop rewrites what the
	// join consolidates, and a takeover restarts a session schema-less so a
	// human can steer it.
	"WorkflowRetryUnit":        CategorySessionControl,
	"WorkflowRetryFailedUnits": CategorySessionControl,
	"WorkflowDropUnit":         CategorySessionControl,
	"WorkflowTakeOverUnit":     CategorySessionControl,
	// The `ao` CLI surface. Every one of these is reachable only with a scoped
	// token minted for a local provider session (see scopedtoken.go), and the
	// scoped route is loopback-only in its own right — but they are classified
	// here too, because the classification is what governs the WebSocket, and a
	// remote peer must not reach the agent surface just because the SPA can.
	// The reads are as privileged as the writes here: they name a project's runs
	// and a workflow's outputs, and they exist for a process on this machine.
	// InspectRun and RunNarrative go further still — one names local worktree
	// paths and the other reads a file out of the app-managed run directory — so
	// they belong here on the plain FS rule as much as on the agent-surface one.
	"WorkflowAgentStartRun":     CategorySessionControl,
	"WorkflowAgentRunStatus":    CategorySessionControl,
	"WorkflowAgentRunOutput":    CategorySessionControl,
	"WorkflowAgentInspectRun":   CategorySessionControl,
	"WorkflowAgentRunNarrative": CategorySessionControl,
	"WorkflowAgentListRuns":     CategorySessionControl,
	"WorkflowAgentWatchRun":     CategorySessionControl,
	"WorkflowAgentAmendSeeds":   CategorySessionControl,
	"WorkflowAgentGuideRun":     CategorySessionControl,
	"WorkflowAgentSchedule":     CategorySessionControl,
	"WorkflowAgentGetNotes":     CategorySessionControl,
	"WorkflowAgentSetNotes":     CategorySessionControl,
	// Campaign memory reads and appends a file under the app-managed config
	// root, so it lands here on the plain FS rule as well as the agent-surface
	// one — the same pair of reasons WorkflowAgentRunNarrative does.
	"WorkflowAgentAddMemory":  CategorySessionControl,
	"WorkflowAgentListMemory": CategorySessionControl,
	// ConcludeDiscussion is lifecycle control over the deliberation's
	// provider-session turn loop — same class as PostChannelMessage: it
	// removes the in-memory FSM (a.deliberations) and can race an
	// in-flight participant turn, the same coordination surface
	// PostChannelMessage's turn-driving path touches.
	"ConcludeDiscussion":             CategorySessionControl,
	"UpdateThreadMode":               CategorySessionControl,
	"UpdateThreadProvider":           CategorySessionControl,
	"UpdateThreadModel":              CategorySessionControl,
	"UpdateThreadModelSelection":     CategorySessionControl,
	"UpdateThreadReasoningEffort":    CategorySessionControl,
	"UpdateThreadFastMode":           CategorySessionControl,
	"UpdateThreadContextWindow":      CategorySessionControl,
	"UpdateThreadContextSettings":    CategorySessionControl,
	"UpdateThreadRuntimeMode":        CategorySessionControl,
	"UpdateNewThreadDefaults":        CategorySessionControl,
	"UpdateThreadBranch":             CategorySessionControl,
	"UpdateThreadWorkspace":          CategorySessionControl,
	"InterruptTurn":                  CategorySessionControl,
	"InterruptAndRevertIfClean":      CategorySessionControl,
	"ListPendingInteractiveRequests": CategorySessionControl,
	"RespondToApproval":              CategorySessionControl,
	"RespondToUserInput":             CategorySessionControl,
	// Thread creation can spawn a worktree / probe the provider; the
	// branch fork variant runs git ops, and the PR variant shells `gh`.
	// GetThreadDefaults reads project FS to detect the current git
	// branch, so it sits in the same FS-touching bucket. StartTerminal
	// resolves the host home directory and persists a terminal-mode
	// thread whose workspace is a local path the frontend then spawns a
	// PTY in — same FS-touching thread-creation class.
	"CreateThread":          CategorySessionControl,
	"CreateThreadFromPR":    CategorySessionControl,
	"GetThreadDefaults":     CategorySessionControl,
	"StartTerminal":         CategorySessionControl,
	"ForkThread":            CategorySessionControl,
	"ForkThreadFromMessage": CategorySessionControl,
	// RevertConversationAndResendMessage cuts the provider session (Claude
	// JSONL slice / Codex thread/revert or thread/fork) and truncates
	// SQLite in place, then
	// dispatches the edited replacement on the same session — the fork
	// variants' session-control + FS class plus SendMessage's.
	"RevertConversationAndResendMessage": CategorySessionControl,
	// Background-task control terminates host subprocesses. All four are
	// the same class: a PTY / task the model launched on this machine dies
	// when the call lands. TerminateCodexBackgroundTerminal is the per-row
	// sibling of the thread-wide clean.
	//
	// StopThreadBackgroundWork is the provider-neutral thread-wide form:
	// it routes each of that thread's live tasks through the very RPCs
	// above, so it terminates host subprocesses by definition and takes
	// their classification. Its read companion, ListRunningBackgroundWork,
	// is deliberately NOT here — see the wireSafeMethods note on it. The
	// remote-access spec wants an attached client to hold these stop
	// controls; that arrives with the labelled step-up tiers, not by
	// making one termination RPC the exception.
	"StopClaudeTask":                   CategorySessionControl,
	"CleanCodexBackgroundTerminals":    CategorySessionControl,
	"TerminateCodexBackgroundTerminal": CategorySessionControl,
	"StopCodexSubagent":                CategorySessionControl,
	"StopThreadBackgroundWork":         CategorySessionControl,
	// BackgroundClaudeTask drives the same live Claude stdio control
	// channel in the opposite direction: it detaches a running subagent /
	// Bash from the foreground turn rather than killing it. Session
	// control over a local subprocess either way.
	"BackgroundClaudeTask": CategorySessionControl,
	// StartCodexReview and CompactCodexThread each start a real, billed,
	// non-steerable turn on the thread's live provider subprocess — the
	// review one that reads the user's working tree or git history and runs
	// tools in it. Same session-control class as SendMessage, just reached
	// through a purpose-built RPC instead of a prompt.
	"StartCodexReview":   CategorySessionControl,
	"CompactCodexThread": CategorySessionControl,
	// GetThreadContextUsage drives a live Claude session over its stdio
	// control channel (a get_context_usage control_request). It reads
	// rather than mutates, but it is still traffic on the local provider
	// subprocess, and its answer names the memory files, skills, and
	// agents loaded from the host — same session-control class as the
	// stop primitives above.
	"GetThreadContextUsage":       CategorySessionControl,
	"GetProviderStatuses":         CategorySessionControl,
	"ProbeClaudeAccount":          CategorySessionControl,
	"ProbeCodexAccount":           CategorySessionControl,
	"RecheckClaudeAccount":        CategorySessionControl,
	"RecheckCodexAccount":         CategorySessionControl,
	"ListProviderAccounts":        CategorySessionControl,
	"LoginProviderAccount":        CategorySessionControl,
	"SwitchProviderAccount":       CategorySessionControl,
	"RemoveProviderAccount":       CategorySessionControl,
	"RefreshProviderAccountUsage": CategorySessionControl,

	// claude-tui take-control: Attach arms raw-output fan-out and Replay
	// returns the PTY frame buffer; Input/Resize/Refresh/SetControl steer the
	// host PTY of a live provider subprocess, and Detach tears the attach down.
	// Session-control + host-PTY class — never reachable from a LAN peer.
	"ProviderTerminalAttach":     CategorySessionControl,
	"ProviderTerminalDetach":     CategorySessionControl,
	"ProviderTerminalReplay":     CategorySessionControl,
	"ProviderTerminalInput":      CategorySessionControl,
	"ProviderTerminalResize":     CategorySessionControl,
	"ProviderTerminalRefresh":    CategorySessionControl,
	"ProviderTerminalSetControl": CategorySessionControl,

	// 3. CategorySettingsMutation. A LAN-attached token-holder must not be
	// able to reconfigure the server they're attached to.
	"UpdateSettings":               CategorySettingsMutation,
	"UpdateContextSettingsProfile": CategorySettingsMutation,
	"SetNetworkSettings":           CategorySettingsMutation,
	"AddRemoteEndpoint":            CategorySettingsMutation,
	"UpdateRemoteEndpoint":         CategorySettingsMutation,
	"DeleteRemoteEndpoint":         CategorySettingsMutation,
	"TouchRemoteEndpoint":          CategorySettingsMutation,
	"SetEditorSettings":            CategorySettingsMutation,
	"UpdateKeybindings":            CategorySettingsMutation,
	"ResetKeybindings":             CategorySettingsMutation,
	// SetAppearance writes <configDir>/themes/appearance.json on the
	// host — a settings mutation whose file happens to live outside
	// settings.json. SetWindowBackgroundColor repaints THIS machine's
	// native window chrome, which no remote peer has any business
	// driving: the desktop user would watch their window change color
	// on a stranger's call. Their read companion, GetThemeFiles, is
	// deliberately NOT here — see the GetKeybindings note above.
	"SetAppearance":            CategorySettingsMutation,
	"SetWindowBackgroundColor": CategorySettingsMutation,
	"SetChatBarFavorite":       CategorySettingsMutation,
	// Custom provider environment. Settings mutation (category 3) AND
	// credential-shaped input (category 6): the value a caller supplies is
	// injected verbatim into every provider subprocess for that provider —
	// a LAN peer able to set ANTHROPIC_BASE_URL would silently reroute the
	// user's turns, and the values themselves are the kind of material the
	// sensitive flag exists to keep off the wire.
	"SetProviderCustomEnvVar":    CategorySettingsMutation,
	"DeleteProviderCustomEnvVar": CategorySettingsMutation,
	// Per-project worktree setup. RCE-equivalent (category 1) as much as
	// settings mutation: the argv commands a caller stores here are executed
	// unattended, with the user's environment, every time that project cuts a
	// worktree. The reader is classified with the writer because the recipe
	// names local paths and can carry credential-shaped material (.env globs).
	"GetProjectWorktreeSetup": CategorySettingsMutation,
	"SetProjectWorktreeSetup": CategorySettingsMutation,
	// SetWSLDistroPreference rewrites the Windows launcher's
	// wsl.json — the next launch will boot whatever distro a LAN
	// peer talked the user's backend into saving. Same threat shape
	// as the rest of the settings-mutation block: a token leak must
	// not let a remote peer reconfigure the local user's launcher.
	"SetWSLDistroPreference": CategorySettingsMutation,
	// ReconfigureObservability reconciles the live observability stack
	// against a Settings snapshot the caller supplies. Toggling the
	// replay writer flips on-disk NDJSON capture under the config dir;
	// tracing changes feed the user's restart-required state. Same
	// settings-mutation threat shape.
	"ReconfigureObservability": CategorySettingsMutation,

	// 4. CategoryAttachmentPayload — local-FS surface (writes + reads).
	"UploadAttachment":       CategoryAttachmentPayload,
	"DeleteAttachment":       CategoryAttachmentPayload,
	"GetAttachmentData":      CategoryAttachmentPayload,
	"GetAttachmentThumbnail": CategoryAttachmentPayload,
	"GetLocalImageData":      CategoryAttachmentPayload,
	// 5. CategoryLocalFSBookkeeping.
	"AppendUIRenderTraceBatch": CategoryLocalFSBookkeeping,
	"BookmarkUIRenderTrace":    CategoryLocalFSBookkeeping,
	// ReportFrontendErrorBatch writes JSONL into the user's config
	// directory like the render-trace writer above. The embedded webview
	// is the diagnostic surface that matters; a LAN peer should not be
	// able to write the host's disk, even rotation-capped.
	"ReportFrontendErrorBatch": CategoryLocalFSBookkeeping,
	// GetUIRenderTracePath returns the absolute path to the trace JSONL
	// under the user config dir. The trace is a dev-only debug
	// surface; a remote browser has no reason to know the backend's
	// config-dir layout, and a LAN-attached token-holder fingerprinting
	// the host filesystem layout is the threat we lock the writer
	// companions down for.
	"GetUIRenderTracePath": CategoryLocalFSBookkeeping,

	// 6. CategoryCredentialEnumeration. Plaintext token
	// retrieval is a single-call credential leak; bulk listing reveals
	// the saved-endpoint set even after the Token field is stripped from
	// the wire shape. Defense-in-depth: keep both off the LAN.
	"GetRemoteEndpointToken": CategoryCredentialEnumeration,
	"ListRemoteEndpoints":    CategoryCredentialEnumeration,
	// GetNetworkSettings returns network.Settings, which carries the
	// current ephemeral auth token verbatim (the user can copy the
	// share URL with token in the query string). A LAN-attached
	// token-holder calling this hands the next attacker the same
	// token — single-call credential leak, same class as
	// GetRemoteEndpointToken above. SetNetworkSettings is already
	// loopback-only in category 3 (settings mutation); locking the
	// read companion keeps the token off the LAN regardless of which
	// direction the call comes from.
	"GetNetworkSettings": CategoryCredentialEnumeration,
	// ProbeDevServerURL TCP-dials a loopback port on the backend host to
	// gate the dev-server chip. Wire-reachable it would be a loopback
	// port-scan oracle (which local services exist, one call per port) —
	// the same host-fingerprinting shape as the enumeration entries
	// above. The UX cost is nil: a remote viewer's localhost is not this
	// machine, so a chip it cannot verify is a chip that would open the
	// wrong host.
	"ProbeDevServerURL": CategoryCredentialEnumeration,

	// 7. CategoryWSLInventory / preference. ListWSLDistros spawns wsl.exe per
	// invocation — that's an external-process invocation under category 1
	// even though the argv is fixed. GetWSLDistroPreference reads the
	// launcher's wsl.json from disk (local-FS read under the user config
	// dir). A LAN-attached token-holder shouldn't be able to fingerprint
	// the host's WSL inventory or its persisted distro choice; both pair
	// with the SetWSLDistroPreference mutation on a single host surface.
	"ListWSLDistros":         CategoryWSLInventory,
	"GetWSLDistroPreference": CategoryWSLInventory,

	// 8. CategoryMCPState — per-thread state and status. The whole surface is
	// local-only:
	//   - GetMcpServerStatus / RefreshMcpServerStatus spawn the
	//     provider's own CLI (`claude mcp list`, `codex app-server`)
	//     as a short-lived subprocess to read the live server list
	//     using the user's env-var bearer tokens — external-process
	//     invocation (category 1).
	//   - SetThreadMcpServerEnabled / SetWorkspaceMcpServerEnabled
	//     toggle the provider-native disable state — a live-session
	//     `mcp_toggle` RPC or a direct ~/.claude.json /
	//     ~/.codex/config.toml write (categories 2 + 3) — and reshape
	//     what tools the provider can call. ReconnectMcpServer drives
	//     the live session's reconnect the same way (category 2).
	//   - TriggerMcpAuth starts a session if needed, while
	//     TriggerWorkspaceMcpAuth starts a temporary provider process
	//     (category 2). Both emit an authorization URL the desktop user
	//     opens locally. A LAN peer opening the URL would land on the AO
	//     backend's loopback OAuth callback, not their own browser.
	//   - ListThreadMcpServers / ListWorkspaceMcpServers /
	//     ListMcpServerStatuses disclose server names, scopes, and tool
	//     inventory (never args/env) — the same enumeration shape
	//     category 6 locks down, and the thread listing can drive a
	//     live-session control RPC. Conservative + consistent:
	//     everything goes loopback-only.
	"ListThreadMcpServers":         CategoryMCPState,
	"ListWorkspaceMcpServers":      CategoryMCPState,
	"SetThreadMcpServerEnabled":    CategoryMCPState,
	"SetWorkspaceMcpServerEnabled": CategoryMCPState,
	"ReconnectMcpServer":           CategoryMCPState,
	"GetMcpServerStatus":           CategoryMCPState,
	"ListMcpServerStatuses":        CategoryMCPState,
	"RefreshMcpServerStatus":       CategoryMCPState,
	"TriggerMcpAuth":               CategoryMCPState,
	"TriggerWorkspaceMcpAuth":      CategoryMCPState,

	// 8b. Codex account-level usage, tagged CategoryCredentialEnumeration
	// rather than getting a category of its own: it sits here because this is
	// where it was added, and the tag is what the gate reads.
	// GetCodexAccountUsage drives the user's
	// own `codex` CLI — a live session's app-server when one is open, a
	// short-lived subprocess otherwise (category 1) — under the pinned
	// provider credentials, and returns account-scoped figures for the
	// signed-in ChatGPT login: lifetime tokens, peak day, streaks, and a
	// per-day history covering every client that account has ever used, not
	// just this app. That is the same account-enumeration shape category 6
	// locks down. Note this is NOT GetUsageStats, which stays wire-reachable:
	// that one reads AO's own SQLite ledger and discloses nothing about the
	// login.
	"GetCodexAccountUsage": CategoryCredentialEnumeration,

	// 9. CategoryDesktopHostControl — in-app self-update. CheckForUpdate /
	// ListReleases / DownloadUpdate
	// reach out to the GitHub releases API and stream a binary to disk
	// (network + local-FS writes, category 1); RestartToUpdate spawns the swap
	// helper and quits the host process (external-process + lifecycle control,
	// category 1/2). Kept loopback-only with the rest of the surface —
	// self-update is a desktop-host control, not something a LAN-attached
	// --connect peer should drive. A remote client still sees the backend
	// version via App.Version (not local-only).
	"CheckForUpdate":  CategoryDesktopHostControl,
	"ListReleases":    CategoryDesktopHostControl,
	"DownloadUpdate":  CategoryDesktopHostControl,
	"RestartToUpdate": CategoryDesktopHostControl,

	// 9b. The Windows launcher's answer to an updater:install directive.
	// ReportUpdateInstallStatus settles the in-flight install: it clears the
	// on-disk update marker and releases the updater's busy fence (category 5's
	// app-managed FS bookkeeping, wrapped around category 2's lifecycle
	// control). The only legitimate caller is the launcher process on this
	// host, which reaches the backend over the WSL localhost relay and is
	// therefore loopback by construction. A LAN-attached peer forging "failed"
	// would cancel a real install; forging "proceeding" would strand the app
	// with the fence held until the launcher's own report was refused as stale.
	"ReportUpdateInstallStatus": CategoryDesktopHostControl,

	// 9c. Desktop renderer memory trim. RequestWebviewMemoryTrim emits the
	// webview:trim directive the Windows launcher answers with a forced
	// renderer GC — a desktop-host control like the rest of category 9. The
	// caller is the embedded webview reporting its own input idleness; a
	// LAN-attached peer's idleness says nothing about the desktop session,
	// and letting one command GC pauses into it would be a jank lever.
	"RequestWebviewMemoryTrim": CategoryDesktopHostControl,

	// 10. CategorySessionImport. Every method here reads the user's provider homes
	// (~/.claude, ~/.codex) off the local filesystem and hands back what it
	// finds: absolute session-file paths, workspace paths, and the prompt text
	// the session titles are derived from — a directory listing of the user's
	// entire conversation history, which is category 1's local-FS read surface
	// at its widest. ImportSessions additionally creates threads and project
	// rows from those files, and the progress frames it pushes carry the same
	// paths (which is why the channel is loopback-only too). The two refresh
	// methods read one named session file and append to an existing thread.
	// None of them spawn a process — import is a file read and a SQLite write
	// — but "a LAN peer can enumerate and ingest every conversation on this
	// host" is exactly the disclosure the local-only cut exists to prevent.
	"ListImportableSessions":   CategorySessionImport,
	"ImportSessions":           CategorySessionImport,
	"CancelSessionImport":      CategorySessionImport,
	"CheckThreadImportUpdates": CategorySessionImport,
	"ImportThreadUpdates":      CategorySessionImport,

	// 11. CategoryDeviceAccess. The surface that decides which devices
	// hold credentials on this backend (docs/specs/remote-access.md §4).
	// MintDevicePairing ISSUES one: the link it returns is redeemable by
	// the next device to present it with a key, so a wire-reachable mint
	// would let a credential-holder enroll further devices of its own
	// choosing — the one call that grows the trusted set. Confirm and
	// Cancel are the owner's half of that same exchange, and the whole
	// point of the verification number is that it is matched by a person
	// at the machine rather than by whoever happens to hold a session.
	// The two revocations run the other way and are just as decisive: one
	// call ends every credential a device holds and closes its live
	// sockets.
	//
	// GetAccessOverview is a read, and locked down with the writers on
	// category 6's grounds: it lists every device, its platform, its live
	// sessions with their connection counts, and the credential audit log
	// — the complete map of what reaches this backend and when, which is
	// exactly what a caller would consult before calling anything else
	// here. It also carries the verification number of a pending pairing,
	// and a number readable off the wire is one the owner is no longer
	// the only party comparing.
	//
	// DevicePairingStatus is the polling companion of the mint and
	// carries the same number, so it is answered in the same place.
	"GetAccessOverview":    CategoryDeviceAccess,
	"MintDevicePairing":    CategoryDeviceAccess,
	"DevicePairingStatus":  CategoryDeviceAccess,
	"ConfirmDevicePairing": CategoryDeviceAccess,
	"CancelDevicePairing":  CategoryDeviceAccess,
	"RevokeAccessDevice":   CategoryDeviceAccess,
	"RevokeAccessSession":  CategoryDeviceAccess,
}

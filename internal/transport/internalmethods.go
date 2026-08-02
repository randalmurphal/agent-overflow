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
// non-loopback peers when the server is bound to a LAN interface. The
// set covers six threat classes:
//
//  1. RCE-equivalent: terminal spawn/IO, opening files in editors,
//     directory browsing, writing files into the workspace, git
//     operations that mutate the local repo or invoke `gh`, and saving
//     payload bytes to disk. A token-holder hitting one of these from
//     a LAN peer would gain effective shell access on the host.
//  2. Session control: starting / stopping / interrupting / responding
//     to a provider session spawns or steers a CLI subprocess that
//     executes on the host. A LAN-attached caller mustn't be able to
//     drive the local provider — that would let them run arbitrary
//     prompts (and tool calls) in the user's shell context.
//  3. Settings mutation: anything that lets a remote peer reconfigure
//     the running server (LAN-bind toggle, remote-endpoint store,
//     editor preference, keybindings, generic settings patch). Letting
//     a LAN peer flip BindAll, mutate the origin allow-list, or rewrite
//     editor / keybinding state defeats the whole opt-in security model.
//  4. Attachment / payload local-FS surface. UploadAttachment writes
//     user-chosen bytes to disk under the config dir; DeleteAttachment
//     removes filesystem entries; GetAttachmentData / GetAttachmentThumbnail
//     read those bytes back. Reads stay loopback-only on the same doctrine
//     as the diff-returning bindings (category 1): the attachment id is
//     opaque to the wire but a LAN-attached token-holder enumerating
//     thread ids could pull every uploaded image / file in one pass.
//  5. Local-FS bookkeeping: terminal-process control (CloseTerminal,
//     ResizeTerminal — they steer host PTY children) and the UI render
//     trace writer (writes JSONL into the user's config directory).
//     Background-task termination is in the same category — it stops
//     local provider subprocesses.
//  6. Credential retrieval / endpoint enumeration: GetRemoteEndpointToken
//     hands back a plaintext stored token — that's a credential leak in
//     one call. ListRemoteEndpoints reveals which remote backends the
//     local user has saved; even with the Token field stripped, the
//     enumeration aids an attacker pivoting to other systems on the
//     user's network. Both stay loopback-only.
//
// The dispatcher rejects local-only calls from non-loopback peers with
// ErrCodeMethodNotFound rather than a distinct "forbidden" code: the
// wire shape is indistinguishable from a probe of an unregistered
// method, so a LAN scanner can't fingerprint which methods are
// privileged vs simply absent. The bookkeeping cost is one map lookup
// per call.
//
// Intentionally NOT in this set: GetKeybindings. The frontend's
// keybindings loader has no client-side defaults — a method_not_found
// refusal zeros every keyboard shortcut for remote-browser users. The
// returned data is UI preferences (chord → command), not credentials,
// and the mutation companions (UpdateKeybindings / ResetKeybindings)
// stay in category 3 so a LAN-attached peer still cannot rewrite the
// user's bindings. A future reviewer running this same exercise: don't
// silently flip it without revisiting the remote-browser UX cost.
//
// Updates to this set must keep the names in sync with the App-side
// declarations. methods_gen_test.go gates that contract by failing if
// any LocalOnlyMethods entry is missing from GeneratedMethods.
var LocalOnlyMethods = map[string]bool{
	// 1. RCE-equivalent (FS / process touching).
	"OpenTerminal":                   true,
	"ListTerminals":                  true,
	"GetTerminalReplay":              true,
	"WriteTerminal":                  true,
	"RestartTerminal":                true,
	"CloseTerminal":                  true,
	"CloseThreadTerminals":           true,
	"ResizeTerminal":                 true,
	"RefreshTerminal":                true,
	"MoveThreadTerminals":            true,
	"OpenInEditor":                   true,
	"OpenExternalURL":                true,
	"BrowseDirectory":                true,
	"SavePayloadToFile":              true,
	"WriteThreadWorkspaceFile":       true,
	"GitPush":                        true,
	"GitStatusSubscribe":             true,
	"GitStatusUnsubscribe":           true,
	"GetGitStatus":                   true,
	"GetGitStatusFast":               true,
	"GetGitStatusFastForProject":     true,
	"GitCheckout":                    true,
	"GitCheckoutForProject":          true,
	"GitCreateBranch":                true,
	"GitCreateBranchFrom":            true,
	"GitCreateWorktree":              true,
	"GitRemoveWorktree":              true,
	"RemoveOtherWorktree":            true,
	"RemoveOtherWorktreeForProject":  true,
	"GitWorktreeStatus":              true,
	"GitWorktreeStatusForProject":    true,
	"GitListBranches":                true,
	"GitListBranchesForProject":      true,
	"GitListWorktrees":               true,
	"GitListWorktreesForProject":     true,
	"GitCommit":                      true,
	"GitPull":                        true,
	"GitStageAll":                    true,
	"GitMaybeFetchRemotes":           true,
	"GitMaybeFetchRemotesForProject": true,
	"GitListBranchPruneCandidates":   true,
	"GitPruneBranches":               true,
	"GitSyncBranch":                  true,
	"GitSyncBranchForProject":        true,
	// GitCreatePR shells out to `gh` — same RCE-equivalent class as
	// the rest of the git/external-CLI surface.
	"GitCreatePR": true,
	// PR review APIs shell out to gh/glab and expose remote review state
	// tied to local credentials; keep them with the forge CLI surface.
	"GetPRDetail":          true,
	"GetPRDiff":            true,
	"ListPRReviewThreads":  true,
	"SubmitPRReview":       true,
	"ReplyToPRThread":      true,
	"SubscribePRUpdates":   true,
	"UnsubscribePRUpdates": true,
	"SetPRUpdatesActive":   true,
	"GetPRMergeConflicts":  true,
	"GetMergeConflictFile": true,
	// CI surface: shells out to gh/glab; SavePRCIJobLog additionally
	// writes into the local ci-logs directory.
	"GetPRCIJobs":    true,
	"GetPRCIJobLog":  true,
	"SavePRCIJobLog": true,
	// PrepareThreadWorktree creates a git worktree on disk; same
	// class as the Git* mutators above.
	"PrepareThreadWorktree": true,
	// AttachThreadWorktree creates a worktree pointing at an existing
	// branch; same class as PrepareThreadWorktree.
	"AttachThreadWorktree": true,
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
	"GetBranchBaseDiff":       true,
	"GetWorkingTreeDiff":      true,
	"GetWorkspaceCurrentDiff": true,
	"ListBranchCommits":       true,
	"GetCommitDiff":           true,
	"ListPRCommits":           true,
	"GetPRCommitDiff":         true,
	// GetDiffContextLines reads arbitrary workspace/ref file content by
	// line range (review hunk-gap expansion) — same bulk-content class.
	// VerifyEditDiffs runs the same content resolution (it only reports
	// servability, but the resolution reads workspace files by path).
	"GetDiffContextLines": true,
	"VerifyEditDiffs":     true,
	// HighlightPatchWithContext resolves workspace/ref file content by
	// path to prime span parsing — same class. The wire-safe
	// HighlightCode / HighlightPatch / HighlightClassNames RPCs are
	// pure text-in/metadata-out and deliberately NOT in this set.
	"HighlightPatchWithContext":  true,
	"ListDiffReviewComments":     true,
	"CreateDiffReviewComment":    true,
	"UpdateDiffReviewComment":    true,
	"DeleteDiffReviewComment":    true,
	"MarkDiffReviewCommentsSent": true,
	"SendDiffReviewComments":     true,
	// Codex model discovery spawns the configured `codex app-server`
	// subprocess. It looks like a catalog read, but the local process
	// execution makes it loopback-only.
	"GetModelsForProvider": true,
	"CreateProject":        true,
	"ListAvailableEditors": true,
	// GenerateCommitMessage runs `claude` / `codex` in the workspace
	// cwd with --dangerously-skip-permissions and the model emits a
	// commit message from the staged diff. That's a local-process
	// invocation under user-attacker control (the workspace path is
	// derived from the thread); same class as the Git*/CLI surface.
	"GenerateCommitMessage": true,
	// SearchWorkspaceFiles shells `git ls-files` inside the thread's
	// workspace cwd. The argv is fixed but the cwd is user-supplied
	// through the thread record — keep with the rest of the local-CLI
	// invocations for doctrine consistency.
	"SearchWorkspaceFiles": true,
	// Payload reads (GetPayloadPreview, GetPayloadChunk,
	// GetPayloadData) moved to wireSafeMethods — remote clients need
	// tool-call output, command results, and thinking blocks to render
	// the timeline. Authorization is enforced by getThreadPayloadMeta's
	// (threadID, payloadID) linkage check; the token is the security
	// boundary, matching every other wire-safe method.
	// SavePayloadToFile stays here: it writes to the host filesystem.

	// 2. Session control (provider subprocess spawn / steer).
	"StartSession":             true,
	"AutoResumeThread":         true,
	"StopSession":              true,
	"ReconnectSession":         true,
	"SendMessage":              true,
	"SendMessageWithOptions":   true,
	"SteerMessageWithOptions":  true,
	"SendPlanRevisionComments": true,
	// NotificationActivated synthesizes desktop navigation after a native
	// notification click. Only the loopback Windows launcher may drive it;
	// a LAN-attached browser must not steer another client's pane focus.
	"NotificationActivated": true,
	// Flush-queue surface: RegisterQueueItem stages a user message
	// and the dispatcher writes it to the local provider's stdin /
	// JSON-RPC as soon as possible. GetQueueState and GetThreadLiveState
	// return per-thread snapshots — disclosure of pending user-typed text
	// + attachment IDs + plan refs is the same threat shape as the
	// diff-returning bindings (category 6 below): in-progress drafted work
	// shouldn't be readable by a LAN token-holder. These bindings are
	// loopback-only.
	"RegisterQueueItem":      true,
	"GetQueueState":          true,
	"GetThreadLiveState":     true,
	"SaveDraft":              true,
	"GetDraft":               true,
	"ClearDraft":             true,
	"DeleteEmptyDraftThread": true,
	"StartDiscussion":        true,
	"StartDiscussionByID":    true,
	// PostChannelMessage now has a side-effecting path: a human post
	// can arm the next participant's turn prompt (promptDiscussionSpeakerAsync),
	// which dispatches into that participant's live provider session
	// via the same SendMessage path above. What used to be a plain
	// data write became session control the moment turn-driving
	// landed — see the comment this displaces in
	// methods_gen_test.go's wireSafeMethods, which called out exactly
	// this re-audit trigger.
	"PostChannelMessage": true,
	// Workflow mutations drive autonomous full-access provider sessions or
	// persist local settings state. Keep that control plane loopback-only.
	// The pure workflow reads live in methods_gen_test.go's wireSafeMethods
	// so remote workflow surfaces can render without gaining mutation access.
	"WorkflowStartRun":                     true,
	"WorkflowCancelItem":                   true,
	"WorkflowResumeItem":                   true,
	"WorkflowAnswerQuestion":               true,
	"WorkflowResolveGate":                  true,
	"WorkflowSetGlobalPause":               true,
	"WorkflowCompleteTakeover":             true,
	"WorkflowMergeItem":                    true,
	"WorkflowCreateItemPR":                 true,
	"WorkflowDiscardItem":                  true,
	"WorkflowFetchPRReviewComments":        true,
	"WorkflowSendPRReviewCommentsToThread": true,
	"WorkflowDiscussPR":                    true,
	"WorkflowSetJobNotes":                  true,
	"WorkflowRerunItem":                    true,
	"WorkflowPauseItem":                    true,
	// A soft stop starts nothing by itself, but it decides whether the next wave
	// of autonomous sessions runs at all — the same control plane pause is on,
	// reached one boundary later.
	"WorkflowRequestSoftStop": true,
	// Automation CRUD is the same control plane one step removed: an automation
	// is a standing instruction to start autonomous full-access provider sessions
	// on a schedule, so arming, editing, disabling, or deleting one is session
	// control even though no session starts inside the call. Run now starts one
	// outright. The pure read (WorkflowListAutomations) is wire-safe.
	"WorkflowCreateAutomation":     true,
	"WorkflowUpdateAutomation":     true,
	"WorkflowDeleteAutomation":     true,
	"WorkflowSetAutomationEnabled": true,
	"WorkflowRunAutomationNow":     true,
	// Thread binding wires a run's results into a local provider session: a
	// bound run injects user turns into that thread from a background
	// goroutine.
	"WorkflowBindThread":   true,
	"WorkflowUnbindThread": true,
	// Discard preview reads local checkouts and repository history — dirty
	// paths and unmerged commit subjects — which is the same local-disclosure
	// class as the diff bindings. ProjectDeletionPreview reads the same local
	// checkouts across every run tree a project owns, so it inherits the
	// reasoning. DeleteProject itself stays wire reachable: it deletes no
	// branch, so it destroys nothing git cannot still reach (D25).
	"WorkflowDiscardPreview": true,
	"ProjectDeletionPreview": true,
	// Fan-out unit recovery is the same control plane one unit down: a retry
	// starts a provider session or a local command, a drop rewrites what the
	// join consolidates, and a takeover restarts a session schema-less so a
	// human can steer it.
	"WorkflowRetryUnit":        true,
	"WorkflowRetryFailedUnits": true,
	"WorkflowDropUnit":         true,
	"WorkflowTakeOverUnit":     true,
	// The `ao` CLI surface. Every one of these is reachable only with a scoped
	// token minted for a local provider session (see scopedtoken.go), and the
	// scoped route is loopback-only in its own right — but they are classified
	// here too, because the classification is what governs the WebSocket, and a
	// remote peer must not reach the agent surface just because the SPA can.
	// The reads are as privileged as the writes here: they name a project's runs
	// and a workflow's outputs, and they exist for a process on this machine.
	"WorkflowAgentStartRun":  true,
	"WorkflowAgentRunStatus": true,
	"WorkflowAgentRunOutput": true,
	"WorkflowAgentListRuns":  true,
	"WorkflowAgentSchedule":  true,
	"WorkflowAgentGetNotes":  true,
	"WorkflowAgentSetNotes":  true,
	// ConcludeDiscussion is lifecycle control over the deliberation's
	// provider-session turn loop — same class as PostChannelMessage: it
	// removes the in-memory FSM (a.deliberations) and can race an
	// in-flight participant turn, the same coordination surface
	// PostChannelMessage's turn-driving path touches.
	"ConcludeDiscussion":             true,
	"UpdateThreadMode":               true,
	"UpdateThreadProvider":           true,
	"UpdateThreadModel":              true,
	"UpdateThreadModelSelection":     true,
	"UpdateThreadReasoningEffort":    true,
	"UpdateThreadFastMode":           true,
	"UpdateThreadContextWindow":      true,
	"UpdateThreadContextSettings":    true,
	"UpdateThreadRuntimeMode":        true,
	"UpdateNewThreadDefaults":        true,
	"UpdateThreadBranch":             true,
	"UpdateThreadWorkspace":          true,
	"InterruptTurn":                  true,
	"InterruptAndRevertIfClean":      true,
	"ListPendingInteractiveRequests": true,
	"RespondToApproval":              true,
	"RespondToUserInput":             true,
	// Thread creation can spawn a worktree / probe the provider; the
	// branch fork variant runs git ops, and the PR variant shells `gh`.
	// GetThreadDefaults reads project FS to detect the current git
	// branch, so it sits in the same FS-touching bucket. StartTerminal
	// resolves the host home directory and persists a terminal-mode
	// thread whose workspace is a local path the frontend then spawns a
	// PTY in — same FS-touching thread-creation class.
	"CreateThread":          true,
	"CreateThreadFromPR":    true,
	"GetThreadDefaults":     true,
	"StartTerminal":         true,
	"ForkThread":            true,
	"ForkThreadFromMessage": true,
	// RevertConversationToMessage cuts the provider session (Claude JSONL
	// slice / Codex thread/fork) and truncates SQLite in place — same
	// session-control + FS class as the fork variants above.
	"RevertConversationToMessage": true,
	// Background-task control terminates host subprocesses.
	"StopClaudeTask":                true,
	"CleanCodexBackgroundTerminals": true,
	"GetProviderStatuses":           true,
	"ProbeClaudeAccount":            true,
	"ProbeCodexAccount":             true,
	"RecheckClaudeAccount":          true,
	"RecheckCodexAccount":           true,
	"ListProviderAccounts":          true,
	"LoginProviderAccount":          true,
	"SwitchProviderAccount":         true,
	"RemoveProviderAccount":         true,
	"RefreshProviderAccountUsage":   true,

	// claude-tui take-control: Attach arms raw-output fan-out and Replay
	// returns the PTY frame buffer; Input/Resize/Refresh/SetControl steer the
	// host PTY of a live provider subprocess, and Detach tears the attach down.
	// Session-control + host-PTY class — never reachable from a LAN peer.
	"ProviderTerminalAttach":     true,
	"ProviderTerminalDetach":     true,
	"ProviderTerminalReplay":     true,
	"ProviderTerminalInput":      true,
	"ProviderTerminalResize":     true,
	"ProviderTerminalRefresh":    true,
	"ProviderTerminalSetControl": true,

	// 3. Settings mutation. A LAN-attached token-holder must not be
	// able to reconfigure the server they're attached to.
	"UpdateSettings":               true,
	"UpdateContextSettingsProfile": true,
	"SetNetworkSettings":           true,
	"AddRemoteEndpoint":            true,
	"UpdateRemoteEndpoint":         true,
	"DeleteRemoteEndpoint":         true,
	"TouchRemoteEndpoint":          true,
	"SetEditorSettings":            true,
	"UpdateKeybindings":            true,
	"ResetKeybindings":             true,
	"SetChatBarFavorite":           true,
	// Custom provider environment. Settings mutation (category 3) AND
	// credential-shaped input (category 6): the value a caller supplies is
	// injected verbatim into every provider subprocess for that provider —
	// a LAN peer able to set ANTHROPIC_BASE_URL would silently reroute the
	// user's turns, and the values themselves are the kind of material the
	// sensitive flag exists to keep off the wire.
	"SetProviderCustomEnvVar":    true,
	"DeleteProviderCustomEnvVar": true,
	// SetWSLDistroPreference rewrites the Windows launcher's
	// wsl.json — the next launch will boot whatever distro a LAN
	// peer talked the user's backend into saving. Same threat shape
	// as the rest of the settings-mutation block: a token leak must
	// not let a remote peer reconfigure the local user's launcher.
	"SetWSLDistroPreference": true,
	// ReconfigureObservability reconciles the live observability stack
	// against a Settings snapshot the caller supplies. Toggling the
	// replay writer flips on-disk NDJSON capture under the config dir;
	// tracing changes feed the user's restart-required state. Same
	// settings-mutation threat shape.
	"ReconfigureObservability": true,

	// 4. Attachment / payload local-FS surface (writes + reads).
	"UploadAttachment":       true,
	"DeleteAttachment":       true,
	"GetAttachmentData":      true,
	"GetAttachmentThumbnail": true,
	// Design-mode local-FS + coordination surface. The frontend posts
	// iframe-captured diagnostics into the per-thread ring and
	// reads/writes the per-thread design workdir (option-set
	// bookkeeping, layout enumeration). All loopback-only — these
	// expose either RCE-equivalent surface (workdir mutation) or
	// filesystem-layout disclosure (the absolute main path + manifest
	// in GetDesignWorkdirInfo). The agent's read_screenshot path is
	// backend-driven and never touches the wire.
	"IngestDiagnosticBatch":  true,
	"EnsureDesignWorkdir":    true,
	"DismissDesignOptionSet": true,
	"LatestDesignOptionSet":  true,
	"GetDesignWorkdirInfo":   true,

	// 5. Local-FS bookkeeping.
	"AppendUIRenderTraceBatch": true,
	"BookmarkUIRenderTrace":    true,
	// ReportFrontendErrorBatch writes JSONL into the user's config
	// directory like the render-trace writer above. The embedded webview
	// is the diagnostic surface that matters; a LAN peer should not be
	// able to write the host's disk, even rotation-capped.
	"ReportFrontendErrorBatch": true,
	// GetUIRenderTracePath returns the absolute path to the trace JSONL
	// under the user config dir — same path-disclosure shape as
	// GetDesignWorkdirInfo in category 4. The trace is a dev-only debug
	// surface; a remote browser has no reason to know the backend's
	// config-dir layout, and a LAN-attached token-holder fingerprinting
	// the host filesystem layout is the threat we lock the writer
	// companions down for.
	"GetUIRenderTracePath": true,

	// 6. Credential retrieval / endpoint enumeration. Plaintext token
	// retrieval is a single-call credential leak; bulk listing reveals
	// the saved-endpoint set even after the Token field is stripped from
	// the wire shape. Defense-in-depth: keep both off the LAN.
	"GetRemoteEndpointToken": true,
	"ListRemoteEndpoints":    true,
	// GetNetworkSettings returns network.Settings, which carries the
	// current ephemeral auth token verbatim (the user can copy the
	// share URL with token in the query string). A LAN-attached
	// token-holder calling this hands the next attacker the same
	// token — single-call credential leak, same class as
	// GetRemoteEndpointToken above. SetNetworkSettings is already
	// loopback-only in category 3 (settings mutation); locking the
	// read companion keeps the token off the LAN regardless of which
	// direction the call comes from.
	"GetNetworkSettings": true,

	// 7. WSL inventory / preference. ListWSLDistros spawns wsl.exe per
	// invocation — that's an external-process invocation under category 1
	// even though the argv is fixed. GetWSLDistroPreference reads the
	// launcher's wsl.json from disk (local-FS read under the user config
	// dir). A LAN-attached token-holder shouldn't be able to fingerprint
	// the host's WSL inventory or its persisted distro choice; both pair
	// with the SetWSLDistroPreference mutation on a single host surface.
	"ListWSLDistros":         true,
	"GetWSLDistroPreference": true,

	// 8. MCP library / per-thread config and status. The whole surface
	// is local-only:
	//   - GetMcpServerStatus / RefreshMcpServerStatus spawn the
	//     provider's own CLI (`claude mcp list`, `codex app-server`)
	//     as a short-lived subprocess to read the live server list
	//     using the user's env-var bearer tokens — external-process
	//     invocation (category 1).
	//   - CreateMcpServer / UpdateMcpServer / DeleteMcpServer mutate
	//     ~/.claude.json or ~/.codex/config.toml (category 3) and
	//     reshape what tools the provider can call.
	//   - SetMcpServerEnabled toggles the provider-native disable list
	//     and live-reconciles the affected provider session (category 2);
	//     Claude diff-reconciles in-process, Codex hot-reloads, either
	//     way the subprocess sees a new tool surface.
	//   - TriggerMcpAuth starts a session if needed (category 2) and
	//     emits the authorization URL the desktop user opens locally — a
	//     LAN peer opening the URL would land on the AO backend's
	//     loopback OAuth callback, not their own browser.
	//   - ListMcpServers / ListMcpServerStatuses disclose URLs, env-var
	//     bearer references, and tool inventory — the same enumeration
	//     shape category 6 locks down. Conservative + consistent:
	//     everything goes loopback-only.
	"ListMcpServers":               true,
	"ListMcpServersForThread":      true,
	"ListMcpServersForNewThread":   true,
	"CreateMcpServer":              true,
	"UpdateMcpServer":              true,
	"DeleteMcpServer":              true,
	"SetMcpServerEnabled":          true,
	"SetNewThreadMcpServerEnabled": true,
	"GetMcpServerStatus":           true,
	"ListMcpServerStatuses":        true,
	"RefreshMcpServerStatus":       true,
	"TriggerMcpAuth":               true,

	// 9. In-app self-update. CheckForUpdate / ListReleases / DownloadUpdate
	// reach out to the GitHub releases API and stream a binary to disk
	// (network + local-FS writes, category 1); RestartToUpdate spawns the swap
	// helper and quits the host process (external-process + lifecycle control,
	// category 1/2). Kept loopback-only with the rest of the surface —
	// self-update is a desktop-host control, not something a LAN-attached
	// --connect peer should drive. A remote client still sees the backend
	// version via App.Version (not local-only).
	"CheckForUpdate":  true,
	"ListReleases":    true,
	"DownloadUpdate":  true,
	"RestartToUpdate": true,
}

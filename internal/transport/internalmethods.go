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
//  4. Attachment / payload writes: UploadAttachment writes user-chosen
//     bytes to disk under the config dir; DeleteAttachment removes
//     filesystem entries. Both are local-FS mutation under user-supplied
//     keys and must stay loopback-only.
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
// Updates to this set must keep the names in sync with the App-side
// declarations. methods_gen_test.go gates that contract by failing if
// any LocalOnlyMethods entry is missing from GeneratedMethods.
var LocalOnlyMethods = map[string]bool{
	// 1. RCE-equivalent (FS / process touching).
	"OpenTerminal":             true,
	"ListTerminals":            true,
	"GetTerminalReplay":        true,
	"WriteTerminal":            true,
	"RestartTerminal":          true,
	"CloseTerminal":            true,
	"ResizeTerminal":           true,
	"OpenInEditor":             true,
	"OpenExternalURL":          true,
	"BrowseDirectory":          true,
	"SavePayloadToFile":        true,
	"WriteThreadWorkspaceFile": true,
	"GitPush":                  true,
	"GitStatusSubscribe":       true,
	"GitStatusUnsubscribe":     true,
	"GetGitStatus":             true,
	"GitCheckout":              true,
	"GitCreateBranch":          true,
	"GitCreateBranchFrom":      true,
	"GitCreateWorktree":        true,
	"GitRemoveWorktree":        true,
	"RemoveOtherWorktree":      true,
	"GitWorktreeStatus":        true,
	"GitListBranches":          true,
	"GitListWorktrees":         true,
	"GitCommit":                true,
	"GitPull":                  true,
	"GitStageAll":              true,
	"GitMaybeFetchRemotes":     true,
	"GitPruneRemotes":          true,
	"GitSyncBranch":            true,
	// GitCreatePR shells out to `gh` — same RCE-equivalent class as
	// the rest of the git/external-CLI surface.
	"GitCreatePR": true,
	// PrepareThreadWorktree creates a git worktree on disk; same
	// class as the Git* mutators above.
	"PrepareThreadWorktree": true,
	// AttachThreadWorktree creates a worktree pointing at an existing
	// branch; same class as PrepareThreadWorktree.
	"AttachThreadWorktree": true,
	// RevertToMessageCheckpoint mutates the local working tree (git restore
	// checkout into the workspace). Same class.
	"RevertToMessageCheckpoint":            true,
	"RevertToMessageCheckpointWithOptions": true,
	// Diff-returning bindings expose bulk file content in a single wire
	// call. The threat shape matches the credential / endpoint
	// enumeration class below: a token-holder gets the user's
	// agent-edited code, in-progress work, and any non-gitignored config
	// (e.g. a forgotten `secrets.local`, an `.env.example` the agent
	// touched while iterating) in one call.
	//
	// Storage in a hidden git ref is NOT a security boundary — checkpoint
	// captures stage everything tracked-at-HEAD plus untracked-not-ignored,
	// so anything not gitignored ends up in the ref namespace. Working-tree
	// and workspace-current diffs additionally include uncommitted edits.
	//
	// Lock the checkpoint/diff surface down loopback-only. UX cost is
	// "diff panels don't render from a remote browser"; that's a feature
	// nobody currently depends on, and locking down later (after a remote
	// workflow grows to need them) would be a breaking change.
	"GetMessageCheckpointDiff":       true,
	"GetMessageCheckpointRevertDiff": true,
	"GetSessionAgentDiff":            true,
	"ListThreadCheckpoints":          true,
	"GetWorkingTreeDiff":             true,
	"GetWorkspaceCurrentDiff":        true,
	"ListDiffReviewComments":         true,
	"CreateDiffReviewComment":        true,
	"UpdateDiffReviewComment":        true,
	"DeleteDiffReviewComment":        true,
	"SendDiffReviewComments":         true,
	// Codex model discovery spawns the configured `codex app-server`
	// subprocess. It looks like a catalog read, but the local process
	// execution makes it loopback-only.
	"GetModelsForProvider": true,
	"CreateProject":        true,
	"ListAvailableEditors": true,

	// 2. Session control (provider subprocess spawn / steer).
	"StartSession":             true,
	"AutoResumeThread":         true,
	"StopSession":              true,
	"ReconnectSession":         true,
	"SendMessage":              true,
	"SendMessageWithOptions":   true,
	"SteerMessageWithOptions":  true,
	"SendPlanRevisionComments": true,
	// Flush-queue surface: RegisterQueueItem stages a user message
	// and the dispatcher writes it to the local provider's stdin /
	// JSON-RPC as soon as possible. GetQueueState and GetThreadLiveState
	// return per-thread snapshots — disclosure of pending user-typed text
	// + attachment IDs + plan refs is the same threat shape as the
	// diff-returning bindings (category 6 below): in-progress drafted work
	// shouldn't be readable by a LAN token-holder. These bindings are
	// loopback-only.
	"RegisterQueueItem":              true,
	"GetQueueState":                  true,
	"GetThreadLiveState":             true,
	"SaveDraft":                      true,
	"GetDraft":                       true,
	"ClearDraft":                     true,
	"StartDiscussion":                true,
	"StartDiscussionByID":            true,
	"UpdateThreadMode":               true,
	"UpdateThreadProvider":           true,
	"UpdateThreadModel":              true,
	"UpdateThreadModelSelection":     true,
	"UpdateThreadReasoningEffort":    true,
	"UpdateThreadFastMode":           true,
	"UpdateThreadContextWindow":      true,
	"UpdateThreadContextSettings":    true,
	"UpdateThreadRuntimeMode":        true,
	"UpdateThreadBranch":             true,
	"UpdateThreadWorkspace":          true,
	"InterruptTurn":                  true,
	"InterruptAndRevertIfClean":      true,
	"ListPendingInteractiveRequests": true,
	"RespondToApproval":              true,
	"RespondToUserInput":             true,
	// Thread creation can spawn a worktree / probe the provider; the
	// branch fork variant runs git ops, and the PR variant shells `gh`.
	"CreateThread":          true,
	"CreateThreadFromPR":    true,
	"ForkThread":            true,
	"ForkThreadFromMessage": true,
	// Background-task control terminates host subprocesses.
	"StopClaudeTask":                true,
	"CleanCodexBackgroundTerminals": true,
	"GetProviderStatuses":           true,
	"ProbeClaudeAccount":            true,
	"ProbeCodexAccount":             true,
	"RecheckClaudeAccount":          true,
	"RecheckCodexAccount":           true,

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
	"SetThreadRuntimeMode":         true,
	// SetWSLDistroPreference rewrites the Windows launcher's
	// wsl.json — the next launch will boot whatever distro a LAN
	// peer talked the user's backend into saving. Same threat shape
	// as the rest of the settings-mutation block: a token leak must
	// not let a remote peer reconfigure the local user's launcher.
	"SetWSLDistroPreference": true,

	// 4. Attachment / payload writes (local-FS mutation).
	"UploadAttachment": true,
	"DeleteAttachment": true,
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

	// 6. Credential retrieval / endpoint enumeration. Plaintext token
	// retrieval is a single-call credential leak; bulk listing reveals
	// the saved-endpoint set even after the Token field is stripped from
	// the wire shape. Defense-in-depth: keep both off the LAN.
	"GetRemoteEndpointToken": true,
	"ListRemoteEndpoints":    true,

	// 7. WSL inventory / preference. ListWSLDistros spawns wsl.exe per
	// invocation — that's an external-process invocation under category 1
	// even though the argv is fixed. GetWSLDistroPreference reads the
	// launcher's wsl.json from disk (local-FS read under the user config
	// dir). A LAN-attached token-holder shouldn't be able to fingerprint
	// the host's WSL inventory or its persisted distro choice; both pair
	// with the SetWSLDistroPreference mutation on a single host surface.
	"ListWSLDistros":         true,
	"GetWSLDistroPreference": true,

	// 8. MCP library / per-thread config and probes. The whole surface
	// is local-only:
	//   - ProbeMcpServer spawns stdio MCP subprocesses and dials HTTP
	//     servers using the user's env-var bearer tokens, so it is an
	//     external-process invocation (category 1).
	//   - CreateMcpServer / UpdateMcpServer / DeleteMcpServer mutate
	//     persistent settings (category 3) and reshape what tools the
	//     provider can call.
	//   - UpdateThreadMcpServers reconciles the live provider session
	//     (category 2): Claude diff-reconciles in-process, Codex
	//     reconnects, either way the subprocess sees a new tool surface.
	//   - TriggerMcpAuth writes to ~/.codex/config.toml (category 3),
	//     starts a session if needed (category 2), and emits the
	//     authorization URL the desktop user opens locally — a LAN peer
	//     opening the URL would land on the AO backend's loopback OAuth
	//     callback, not their own browser.
	//   - ListMcpServers / GetThreadMcpServers / GetMcpThreadProfile /
	//     GetMcpProbeSnapshot disclose URLs, env-var bearer references,
	//     and tool inventory — the same enumeration shape category 6
	//     locks down. Conservative + consistent: everything goes
	//     loopback-only.
	"ListMcpServers":         true,
	"CreateMcpServer":        true,
	"UpdateMcpServer":        true,
	"DeleteMcpServer":        true,
	"GetThreadMcpServers":    true,
	"UpdateThreadMcpServers": true,
	"ProbeMcpServer":         true,
	"GetMcpProbeSnapshot":    true,
	"TriggerMcpAuth":         true,
	"GetMcpThreadProfile":    true,
}

# Phase 1 Refactor — `app_*.go` Inventory

Complete classification of 66 non-test `app_*.go` files at `/home/rmurphy/repos/agent-overflow/` (~308 KB total, ~13.5 K source LOC). Read-only research; no code modified.

## 1. Methodology + caveats

- **Corpus.** Every non-test `app_*.go` at repo root. `app.go` cross-referenced but not classified — it stays in `package main`.
- **Inputs.** Every file read in full. Helper dependencies confirmed with grep across the corpus.
- **Classification:**
  - **A** = thin shim, methods already delegate to an injected collaborator. Mechanical move.
  - **B** = self-contained, clear destination package, helpers private to the concern. Migration mechanical but non-trivial.
  - **C** = participates in cross-cutting infrastructure (mutex registry, send pipeline, session map, central emit, settings DI, provider-restart fan-out). Needs a seam designed first.
- **Method/signature invariant preserved.** Every exported `*App` method keeps its name AND signature; methodgen FNV-IDs must re-emit identical. Only the *implementation* moves.
- **`LocalOnlyMethods` invariant preserved.** Every privileged method (FS / process / settings / attachments / credentials) keeps its membership in `internal/transport/internalmethods.go`.
- **No Claude/Codex merger.** Provider-specific helpers move into `internal/provider/claude` or `internal/provider/codex`.
- **Borderline files.** Some small files sit in C because of one cross-cutting helper; some large files sit in B because every method delegates. Borderlines called out in §3.

## 2. Summary table

| File (bytes) | Bucket | Exported App methods | App fields touched | Helper deps | Proposed destination | Migration notes |
|---|---|---|---|---|---|---|
| `app_attachment.go` (3 240) | A | `UploadAttachment`, `ListAttachments`, `DeleteAttachment`, `OpenAttachment` | `a.attachments`, `a.attachmentRoot` | none | `internal/attachment` | Local-only. |
| `app_external_url.go` (469) | A | `OpenExternalURL` | none | none | `internal/externalurl` | Local-only. |
| `app_logging.go` (657) | A | (helper only) | `a.logger`, `a.settings` | provides `newProviderEventLogger` (used by `app.go`) | `internal/logging` | Constructor wiring. |
| `app_workspace_files.go` (1 433) | A | `SearchWorkspaceFiles` | `a.workspaceFiles` | none | `internal/workspacefiles` | One-line delegation. |
| `app_turns.go` (732) | A | `ListRecentTurns` | `a.store` | none | `internal/store` (extend) | Pure passthrough. |
| `app_editor.go` (4 016) | A | `ListAvailableEditors`, `OpenInEditor` | `a.editorCatalog` | uses `editor.RefreshEditors` | `internal/editor` | Local-only. |
| `app_editor_settings.go` (1 832) | A | `GetPreferredEditorID`, `SetPreferredEditorID`, `GetCachedEditors`, `SetCachedEditors` | `a.settings`, `a.editorCatalog` | uses `editor.RefreshEditors` | `internal/editor` | Local-only. |
| `app_remote.go` (5 438) | A | `ListRemoteEndpoints`, `AddRemoteEndpoint`, `UpdateRemoteEndpoint`, `RemoveRemoteEndpoint`, `SetLastRemoteEndpoint`, `GetLastRemoteEndpoint` | `a.settings` | none | `internal/settings` (extend) | Pure CRUD over the remote endpoints slice. |
| `app_search.go` (1 026) | A | `SearchThreadMessages` | `a.store` | none | `internal/store` (extend) | Pure store wrapper. |
| `app_account_probe.go` (1 434) | B | (bootstrap) | `a.settings` | provides `probeStartupAccountInfo` | `internal/provider` boot | Fans out to claude+codex probes. |
| `app_claude_probe.go` (4 013) | B | `RefreshClaudeAccountInfo`, `ClaudeAccountInfo` | `a.settings` | package-level cache (`probeMu`, `probeFlight`) | `internal/provider/claude` (extend) | Local-only. Emit becomes injected `Emitter`. |
| `app_codex_probe.go` (3 094) | B | `RefreshCodexAccountInfo`, `CodexAccountInfo` | `a.settings` | mirrors claude probe pattern | `internal/provider/codex` (extend) | Local-only. |
| `app_claude_stop.go` (2 124) | B | `StopClaudeTask` | `a.sessions`, `a.mu` | reads live Claude session | `internal/provider/claude` | Local-only. Needs `SessionManager.Live(threadID)`. |
| `app_codex_background.go` (2 351) | B | `CleanCodexBackgroundTerminals` | `a.sessions`, `a.mu` | reads live Codex backend | `internal/provider/codex` | Local-only. Same seam as `claude_stop`. |
| `app_context_settings.go` (5 977) | B | `GetThreadContextWindow`, `SetThreadContextWindow`, `ClearThreadContextWindow`, `ListContextWindowPresets` | `a.store`, via threads-CRUD | consumes `restartSessionIfAffected`, `rememberChatModelProfile` | `internal/contextsettings` (new) or fold into threads | Depends on threads seam. |
| `app_design.go` (14 697) | B | `EnsureDesignThreadFromMessage`, `GetThreadDesignThread`, `EnsureDesignWorkdir`, `ListDesignFiles`, `OpenDesignFile`, `WriteDesignFile`, `DeleteDesignFile`, `SaveDesignSnapshot`, `ResetDesignWorkdir`, `RunDesignCheck` | `a.design`, `a.sessions`, `a.mu` | uses `teardownDesignThread` | `internal/design` (extend) | Local-only. |
| `app_diff_review_comments.go` (4 996) | B | `AddDiffReviewComment`, `EditDiffReviewComment`, `DeleteDiffReviewComment`, `SendDiffReviewToProvider` | `a.store` | one call to `sendMessageWithOptions` | `internal/diffreview` (new) | Depends on send seam. |
| `app_directory.go` (8 770) | B | `BrowseDirectory`, `BrowseDirectoryEntries`, `ResolveHomeDirectory` | none | none | `internal/dirbrowse` (new) | Local-only. Zero dependencies — early migration. |
| `app_discussion.go` (4 446) | B | `ListThreadDiscussion`, `SetThreadDiscussionConfig`, `GetThreadDiscussionConfig`, `SaveDiscussionTurn` | `a.discussion`, `a.store` | uses `a.discussion.*` | `internal/discussion` (extend) | Mostly delegation. |
| `app_discussion_runtime.go` (2 345) | B | (helper only) | `a.discussion`, `a.sessions` | provides `syncDiscussionTurn` (consumed by `sessionEventHandler`) | `internal/discussion` (extend) | Moves with provider_events seam. |
| `app_draft.go` (8 279) | B | `LoadDraft`, `SaveDraft`, `DeleteDraft`, `CloneDraftAttachments` | `a.store`, `a.attachments` | none | `internal/draft` (new) or fold into store | Tied to attachments for clone. |
| `app_git.go` (14 617) | B | many `Git*` methods (status, branches, commit, push, PR helpers) | `a.git`, `a.store` | provides `resolveGitPaths`, `gitCore` (consumed by 9 files); one `sendThreadMuRegistry` path | `internal/git` (extend) | Local-only. **Lift the two private helpers into `internal/git` exports first** so other migrations consume the package, not App. |
| `app_gitwatch.go` (6 113) | B | `SubscribeWorkspaceGitStatus`, `UnsubscribeWorkspaceGitStatus` | `a.gitwatch`, `a.transportServer` | transport `ConnState` cleanup hook | `internal/gitwatch` (extend) | Cleanup hook becomes registered callback. |
| `app_keybindings.go` (12 536) | B | `ListKeybindings`, `UpdateKeybinding`, `ResetKeybindings`, `ImportKeybindings`, `ExportKeybindings` | `a.settings` | none | `internal/keybindings` (new) | Self-contained. Early migration. |
| `app_live_state.go` (3 419) | B | `LiveState`, `LiveSessionForThread` | `a.triage`, `a.sessions`, `a.mu` | none | `internal/triage` (extend) or `internal/livestate` | Pure read-only projection. |
| `app_network.go` (12 693) | B | `GetNetworkSettings`, `SetNetworkSettings`, `ListLocalNetworkInterfaces`, `LANDiscoveryStart`, `LANDiscoveryStop`, `LANDiscoveryPeers` | `a.settings`, `a.transportServer` | none | `internal/network` (new) | Local-only. Transport reconfigure as callback. |
| `app_observability.go` (3 136) | B | `ObservabilityState`, `SetObservabilityEnabled` | `a.observability`, `a.settings` | provides `threadIDFromEvent` | `internal/observability` (extend) | |
| `app_paging.go` (6 749) | B | `LoadThreadHead`, `LoadOlderThreadMessages`, `LoadNewerThreadMessages` | `a.store` | none | `internal/store` (extend) | Pure store paging. |
| `app_payloads.go` (5 894) | B | `LoadPayload`, `SavePayload`, `DownloadPayloadToFile` | `a.store` | uses `runtime` save dialog | split: store CRUD + thin App shim | Local-only. The OS dialog call stays in `package main`. |
| `app_projects.go` (5 565) | B | `ListProjects`, `CreateProject`, `RenameProject`, `DeleteProject`, `MoveProject` | `a.store` | provides `ensureProjectForWorkspace` (consumed by 5 C files) | `internal/project` (new) or fold into store | **Lift `ensureProjectForWorkspace` early** — many C migrations need it. |
| `app_proposed_plans.go` (7 494) | B | `AddProposedPlan`, `EditProposedPlan`, `DeleteProposedPlan`, `SendProposedPlanToProvider` | `a.store` | one call to `sendMessageWithOptions` | `internal/proposedplans` (new) | Depends on send seam. Mirror of diff_review. |
| `app_provider_status.go` (6 527) | B | `ProviderStatusForThread`, `ProviderStatusByID` | `a.sessions`, `a.mu`, `a.triage` | none | `internal/provider` (extend) | Read-only projection. |
| `app_session_prompts.go` (1 036) | B | `GetSessionSystemPrompt`, `SetSessionSystemPrompt` | `a.systemPrompts`, `a.mu` | none | `internal/session` (new) | Tied to session lifecycle. |
| `app_session_start.go` (1 253) | B | (helper only) | `a.startingSessions`, `a.mu` | provides `runSessionStart` | `internal/session` (new) | Dedup primitive. Move with session pkg. |
| `app_text_generation.go` (6 385) | B | (helper) | `a.settings` | provides `runTextGeneration` (consumed by commit_message, thread_title) | `internal/textgen` (new) | Local-only. **Migrate first within textgen cluster.** |
| `app_thread_from_pr.go` (9 766) | B | `CreateThreadFromPR` | `a.store`, `a.git`, `a.threads` | consumes `ensureProjectForWorkspace`, `seedChatModelProfile`, `gitCore` | `internal/threadfrompr` (new) or fold into threads | Local-only. Migrate after the three helpers lift. |
| `app_thread_title.go` (8 911) | B | `GenerateThreadTitle`, `SetThreadTitle` | `a.store`, `a.threads` | consumes `runTextGeneration` | `internal/threadtitle` (new) | Depends on textgen seam. |
| `app_ui_trace.go` (3 491) | B | `RecordUITrace`, `ListUITraces`, `ClearUITraces` | `a.uiTrace` | none | `internal/uitrace` (new) | Self-contained. Early migration. |
| `app_workspace.go` (1 952) | B | `WriteThreadWorkspaceFile` | `a.threads`, `a.store` | none | `internal/workspacefiles` (extend) | Local-only. |
| `app_worktree_branch.go` (2 230) | B | `GenerateWorktreeBranchName` | `a.git` | none | `internal/git` (extend) | Pure logic. |
| `app_wsl.go` (3 270) | B | `ListWSLDistros`, `SetSelectedWSLDistro` | `a.settings` | none | `internal/wsldistro` (extend) | Already wraps the pkg. |
| `app_commit_message.go` (12 999) | B | `GenerateCommitMessage` | `a.git`, `a.store` | consumes `runTextGeneration` | `internal/commitmessage` (new) or fold into `internal/git` | Depends on textgen + git seams. |
| `app_approval.go` (5 543) | C | `RespondToApproval`, `UserInput` | `a.sessions`, `a.mu`, `a.triage` | consumes `recordSessionActivity`, `emitEvent`, `closeSession` | Session+emit seam | Local-only. |
| `app_chat_bar.go` (8 064) | C | `LoadChatBarState`, `SaveChatBarState` | `a.store` | provides `seedChatModelProfile`, `rememberChatModelProfile`, `chatModelProfileFor` | `internal/chatmodel` (new) | Profile helpers consumed by threads, thread_model, context_settings, thread_from_pr. |
| `app_checkpoint.go` (17 983) | C | `RestoreCheckpointForMessage`, `RestoreCheckpointAndRollback`, diff bindings | `a.checkpoints`, `a.sessions`, `a.mu`, `a.threads` | provides `captureMessageCheckpoint`, `claudeSourceSessionRef`, `revertProviderConversationToMessage` | `internal/checkpoint` (extend) | Local-only. **Migrate with `thread_fork.go` as pair**. |
| `app_claude_ratelimits.go` (3 951) | C | `RefreshClaudeRateLimits`, `ClaudeRateLimits` | `a.sessions`, `a.mu` | provides `probeClaudeRateLimits` (called by sessionEventHandler) | `internal/provider/claude` | Local-only. Needs emit + session seams. |
| `app_codex_reconcile.go` (9 560) | C | `ReconcileCodexBackgroundTerminals` | `a.sessions`, `a.mu`, `a.store` | uses Codex backend + store | `internal/provider/codex` | Local-only. Needs session seam. |
| `app_emit.go` (5 119) | C | (central plumbing) | `a.transportServer`, `a.eventBus` | provides `emit`, `emitToThread`, `emitErrorToThread`, `closeSessionsParallel` (consumed by ~14 callers) | **Stays in `package main`; introduce `Emitter` interface** | Phase-0 cornerstone — do not move the impl. |
| `app_errors.go` (1 608) | C | (none) | none | provides `emitErrorToThread`, `emitError`, `emitErrorEvent` | Emit seam | Lifts with emit seam. |
| `app_flush_queue.go` (34 674) | C | `LoadFlushQueue`, `FlushQueuedMessage`, `RemoveQueuedMessage`, `ReorderFlushQueue` | `a.flushQueues`, `a.mu`, `a.store`, `a.sessions` | consumes `sendThreadMuRegistry`, `sendMessageWithOptions`, `captureMessageCheckpoint`, `emit` | Send seam | Largest file. Cannot move independently of send. |
| `app_provider_events.go` (4 733) | C | (handler) | `a.sessions`, `a.mu`, `a.triage`, `a.discussion` | provides `sessionEventHandler`, `recordSessionActivity` | Session+triage seam | Central event plumbing. |
| `app_runtime_mode.go` (5 377) | C | (helper) | `a.startingSessions`, `a.sessions`, `a.mu`, `a.threads` | provides `applyRuntimeMode` (consumed by send, steer) | Send seam | Mode-switch on first message. |
| `app_send.go` (22 907) | C | `SendMessage` (+ variants) | `a.sendThreadMuRegistry`, `a.sessions`, `a.flushQueues`, `a.triage`, `a.store`, `a.checkpoints`, `a.mu` | provides `sendThreadMuRegistry`, `sendMessageWithOptions`, `sendToProvider`, `userMessageMeta`, `recordSendFailureAndCompleteTurn` | Send seam (`internal/send`) | Local-only. **Cornerstone of bucket C.** |
| `app_session.go` (14 891) | C | `EnsureSession`, `StartSession`, `StopSession`, `CloseSession`, `RestartSession` | `a.sessions`, `a.startingSessions`, `a.mu`, `a.systemPrompts`, `a.design` | provides `startSession`, `stopSession`, `closeSession`, `restartSession`, `teardownDesignThread` | Session seam | Local-only. Core lifecycle. |
| `app_session_bindings.go` (3 213) | C | `SwitchThread`, `ReconnectSession` | `a.sessions`, `a.mu` | consumes `startSession`, `stopSession` | Session seam | Moves with `app_session.go`. |
| `app_session_reaper.go` (6 903) | C | (bg loop) | `a.sessions`, `a.mu`, `a.store` | consumes `closeSession`, `recordSessionActivity` | Session seam | Idle reaper. |
| `app_settings.go` (7 238) | C | `LoadSettings`, `SaveSettings`, `ProviderBinaryPath`, codex catalog bindings | `a.settings` + package-level catalog cache (sync.Mutex + singleflight) | provides `providerBinaryPath`, `currentSettings` | **Split**: `internal/settings` (adapter) + `internal/provider/codex` (catalog) | Local-only. Two responsibilities, share no state. |
| `app_steer.go` (11 544) | C | `SteerMessage`, `CompleteImplementModeSwitch` | `a.sendThreadMuRegistry`, `a.sessions`, `a.triage`, `a.mu` | consumes `sendThreadMuRegistry`, `applyRuntimeMode`, `sendMessageWithOptions`, `recordSendFailureAndCompleteTurn` | Send seam | Local-only. Mirror of `app_send.go`. |
| `app_discussion_start.go` (8 042) | C | `StartDiscussion`, `StopDiscussion` | `a.discussion`, `a.sessions`, `a.systemPrompts`, `a.mu` | consumes session helpers | Discussion + session seam | Coordinates child-thread creation. |
| `app_thread_delete.go` (8 364) | C | `DeleteThread` | `a.sessions`, `a.terminals`, `a.checkpoints`, `a.attachments`, `a.design`, `a.gitwatch`, `a.mu`, `a.store`, `a.flushQueues` | consumes `sendThreadMuRegistry`, `closeSession`, `teardownDesignThread`, `closeFlushQueue` | Threads pkg | Local-only. Cascading delete; migrate last in C. |
| `app_thread_fork.go` (21 118) | C | `ForkThread` (+ variants) | `a.sessions`, `a.checkpoints`, `a.store`, `a.mu` | consumes `sendThreadMuRegistry`, `captureMessageCheckpoint`, `revertProviderConversationToMessage`, `rollbackCodexThread`, `claudeSourceSessionRef` | Threads + checkpoint | Local-only. Migrate paired with checkpoint. |
| `app_thread_interaction_mode.go` (5 271) | C | `SetThreadInteractionMode` | `a.sessions`, `a.threads`, `a.mu` | consumes `sendMessageWithOptions`, Claude `SetInteractionMode` | Threads pkg | Send seam dependent. |
| `app_thread_model.go` (3 521) | C | `SetThreadModel`, `SetThreadEffort`, `SetThreadFastMode` | `a.threads`, `a.store`, `a.sessions` | consumes `restartSessionIfAffected`, `rememberChatModelProfile` | Threads pkg | Thin once helpers lift. |
| `app_threads.go` (23 144) | C | `ListThreads`, `CreateThread`, `RenameThread`, many `SetThread*`, `ResolveWorkspace` | `a.store`, `a.threads`, `a.sessions`, `a.mu` | provides `restartSessionIfAffected`, `sessionAffectingFields`; consumes `seedChatModelProfile`, `closeSession`, `startSession` | Threads pkg | Local-only. The `restartSessionIfAffected` infrastructure is the real coupling, not the CRUD. |
| `app_worktree.go` (18 710) | C | `CreateWorktree`, `DeleteWorktree`, `SwitchThreadWorkspace`, multi-thread restart helpers | `a.git`, `a.store`, `a.sessions`, `a.threads`, `a.mu` | consumes `sendThreadMuRegistry`, `restartSessionIfAffected`, `ensureProjectForWorkspace`, `closeSession`, `startSession` | Worktree (or extend `internal/git`) + threads | Local-only. Heavy fan-out. |
| `app_terminal.go` (5 021) | C | `OpenTerminal`, `WriteTerminal`, `ResizeTerminal`, `CloseTerminal` | `a.terminals` | provides callbacks that emit | Terminal pkg + emit seam | Local-only. |

## 3. Detailed per-file notes (buckets B and C)

### Bucket B

- **`app_account_probe.go`** → `internal/provider`: bootstrap fan-out from `app.go`'s `New` to claude+codex probes. No state.
- **`app_claude_probe.go`** → `internal/provider/claude` (extend): owns the package-level `probeMu` + `probeFlight` dedup primitive plus emit at the end. Local-only.
- **`app_codex_probe.go`** → `internal/provider/codex` (extend): mirror of claude probe. Same migration shape. Local-only.
- **`app_claude_stop.go`** → `internal/provider/claude` (extend): single method that injects STOP into the live Claude session. Blocker is the sessions-map lookup under `a.mu` — needs `SessionManager.Live(threadID)`. Local-only.
- **`app_codex_background.go`** → `internal/provider/codex` (extend): wraps `codex thread/backgroundTerminals/clean`. Same sessions-map seam. Per-process kill blocked by upstream constraint (`docs/references/codex.md`). Local-only.
- **`app_context_settings.go`** → `internal/contextsettings` (new) or fold into threads: CRUD around per-thread context-window override + restartSessionIfAffected call. Depends on threads seam.
- **`app_design.go`** → `internal/design` (extend): bulk of methods already delegate to `a.design`. `teardownDesignThread` is wired through session teardown. Local-only.
- **`app_diff_review_comments.go`** → `internal/diffreview` (new): store-backed CRUD + one `sendMessageWithOptions` call.
- **`app_directory.go`** → `internal/dirbrowse` (new): WSL-aware fs browser, ignore rules, depth limits. Zero deps. Local-only. **Recommended early migration.**
- **`app_discussion.go`** → `internal/discussion` (extend): mostly delegation.
- **`app_discussion_runtime.go`** → `internal/discussion` (extend): `syncDiscussionTurn` called from `sessionEventHandler`. Moves with events seam.
- **`app_draft.go`** → `internal/draft` (new) or fold into store: persistence + attachment cloning.
- **`app_git.go`** → `internal/git` (extend): bodies wrap `internal/git`. Two private helpers (`resolveGitPaths`, `gitCore`) consumed by 9 files — **lift to exported `git.Resolve` and `git.Core` first**. Local-only.
- **`app_gitwatch.go`** → `internal/gitwatch` (extend): subscription bookkeeping + transport-disconnect cleanup. Cleanup becomes injected callback.
- **`app_keybindings.go`** → `internal/keybindings` (new): defaults + overrides + import/export JSON. **Recommended early migration.**
- **`app_live_state.go`** → `internal/triage` (extend) or `internal/livestate`: read-only projection.
- **`app_network.go`** → `internal/network` (new): LAN bind toggle + discovery. Transport reconfigure via callback. Local-only.
- **`app_observability.go`** → `internal/observability` (extend): + `threadIDFromEvent` classifier.
- **`app_paging.go`** → `internal/store` (extend): pure paging.
- **`app_payloads.go`** → split: store + thin shim for `runtime` save dialog. Local-only.
- **`app_projects.go`** → `internal/project` (new) or fold into store: **`ensureProjectForWorkspace` lift is the prerequisite for 5 C files.**
- **`app_proposed_plans.go`** → `internal/proposedplans` (new): mirror of `diff_review_comments`.
- **`app_provider_status.go`** → `internal/provider` (extend): read-only projection.
- **`app_session_prompts.go`** → `internal/session` (new): tiny map under `a.mu`; move with session pkg.
- **`app_session_start.go`** → `internal/session` (new): `runSessionStart` dedup primitive; move with session pkg.
- **`app_text_generation.go`** → `internal/textgen` (new): CLI executor for `claude -p` / `codex exec --ephemeral`. **Migrate first** — unblocks commit_message + thread_title. Local-only.
- **`app_thread_from_pr.go`** → `internal/threadfrompr` (new) or fold into threads: needs project + chat-model + git seams. Local-only.
- **`app_thread_title.go`** → `internal/threadtitle` (new): depends on textgen seam.
- **`app_ui_trace.go`** → `internal/uitrace` (new): self-contained. **Recommended early migration.**
- **`app_workspace.go`** → `internal/workspacefiles` (extend): single `WriteThreadWorkspaceFile`. Local-only.
- **`app_worktree_branch.go`** → `internal/git` (extend): one method.
- **`app_wsl.go`** → `internal/wsldistro` (extend): already wraps the pkg.
- **`app_commit_message.go`** → `internal/commitmessage` (new) or fold into git: depends on textgen lift.

### Bucket C

- **`app_approval.go`** → Session+emit seam: `RespondToApproval` + `UserInput` touch sessions, triage, emit. Local-only.
- **`app_chat_bar.go`** → `internal/chatmodel` (new): the real surface is the chat-model profile helpers (`seedChatModelProfile`, `rememberChatModelProfile`, `chatModelProfileFor`) consumed by 4 other files. Persistence is incidental.
- **`app_checkpoint.go`** → `internal/checkpoint` (extend): `captureMessageCheckpoint` + `revertProviderConversationToMessage` shared with `thread_fork.go`. Provider branches (Claude JSONL slicing + Codex thread/rollback). **Move with `thread_fork.go` as pair.** Local-only.
- **`app_claude_ratelimits.go`** → `internal/provider/claude`: bg loop + emit + sessions-map probe. Needs emit + session seams. Local-only.
- **`app_codex_reconcile.go`** → `internal/provider/codex`: ghost-row reconciliation for `unifiedExecStartup` terminals. Needs session seam. Local-only.
- **`app_emit.go`** → **Stays in `package main`; introduce `Emitter` interface in `internal/emit`** (or `internal/transport/emit`). Phase-0 work threads the interface through migrated packages. Don't move the impl.
- **`app_errors.go`** → Emit seam: tiny but consumed everywhere; lifts with emit.
- **`app_flush_queue.go`** → Send seam (`internal/send`): largest file. Per-thread worker pool drains queue via send seam. Cannot move independently of `app_send.go`.
- **`app_provider_events.go`** → Session+triage seam: `sessionEventHandler` and `recordSessionActivity` are the chokepoint every event-driven helper consumes.
- **`app_runtime_mode.go`** → Send seam: `applyRuntimeMode` consumed only by send + steer.
- **`app_send.go`** → Send seam (`internal/send`): owns `sendThreadMuRegistry`, `sendMessageWithOptions`, `sendToProvider`, `userMessageMeta`, `recordSendFailureAndCompleteTurn`. **First migration within bucket C.** Local-only.
- **`app_session.go`** → Session seam (`internal/session`): core lifecycle + design wiring + token rotation. Defines `SessionManager` collaborator. Local-only.
- **`app_session_bindings.go`** → Session seam: moves with `app_session.go`.
- **`app_session_reaper.go`** → Session seam: idle reaper.
- **`app_settings.go`** → **Split**: `internal/settings` (adapter; `LoadSettings`/`SaveSettings`/`ProviderBinaryPath`) + `internal/provider/codex` (TTL cache + singleflight model catalog). The two halves share no state. Local-only.
- **`app_steer.go`** → Send seam: mirror of `app_send.go` for Codex steer + Claude implement-mode switch. Local-only.
- **`app_discussion_start.go`** → Discussion + session seam: coordinates participant child threads.
- **`app_thread_delete.go`** → Threads pkg: cascades through every subsystem. **Migrate last in C.** Local-only.
- **`app_thread_fork.go`** → Threads + checkpoint: provider-branched fork orchestration. Pair with `app_checkpoint.go`. Local-only.
- **`app_thread_interaction_mode.go`** → Threads pkg: synthetic-message send for mode switch. Depends on send seam.
- **`app_thread_model.go`** → Threads pkg: thin once `restartSessionIfAffected` lifts.
- **`app_threads.go`** → Threads pkg (or extend internal/threads if created): the `restartSessionIfAffected` + `sessionAffectingFields` infra is the real coupling. Local-only.
- **`app_worktree.go`** → Worktree (or extend `internal/git`) + threads: fans out to many subsystems. Local-only.
- **`app_terminal.go`** → Terminal pkg (extend): callbacks that emit. Move once emit seam exists. Local-only.

## 4. Dependency graph

Arrows point **from consumer to provider**. Files at the bottom of each column block migration of consumers above.

```
                     app_emit.go ──────┐
                     app_errors.go ────┤
                            ▲           │  consumed by ~14 callers
                            │           │  (everything that emits)
                  [EMIT SEAM CORNERSTONE — Phase 0]

  app_provider_events.go (sessionEventHandler, recordSessionActivity)
       ▲ ▲ ▲ ▲
       │ │ │ └── app_session_reaper.go
       │ │ └──── app_claude_ratelimits.go
       │ └────── app_approval.go
       └──────── app_session.go (close path)

  app_session.go (startSession, stopSession, closeSession,
       ▲ ▲ ▲ ▲ ▲   restartSession, teardownDesignThread)
       │ │ │ │ └── app_thread_delete.go
       │ │ │ └──── app_worktree.go
       │ │ └────── app_threads.go (restartSessionIfAffected)
       │ └──────── app_discussion_start.go
       └────────── app_session_bindings.go

  app_send.go (sendThreadMuRegistry, sendMessageWithOptions,
       ▲ ▲ ▲ ▲ ▲   sendToProvider, userMessageMeta,
       │ │ │ │ │   recordSendFailureAndCompleteTurn)
       │ │ │ │ └── app_steer.go
       │ │ │ └──── app_flush_queue.go
       │ │ └────── app_diff_review_comments.go (SendDiffReview…)
       │ └──────── app_proposed_plans.go (SendProposedPlan…)
       └────────── app_thread_interaction_mode.go (mode-switch send)

  app_checkpoint.go (captureMessageCheckpoint,
       ▲ ▲ ▲             revertProviderConversationToMessage,
       │ │ │             claudeSourceSessionRef)
       │ │ └── app_thread_fork.go (revert + provider branch)
       │ └──── app_flush_queue.go (capture before drain)
       └────── app_send.go (capture before send)

  app_runtime_mode.go (applyRuntimeMode)
       ▲ ▲
       │ └── app_steer.go
       └──── app_send.go

  app_threads.go (restartSessionIfAffected, sessionAffectingFields)
       ▲ ▲ ▲
       │ │ └── app_worktree.go
       │ └──── app_context_settings.go
       └────── app_thread_model.go

  app_chat_bar.go (seedChatModelProfile, rememberChatModelProfile)
       ▲ ▲ ▲ ▲
       │ │ │ └── app_thread_from_pr.go
       │ │ └──── app_context_settings.go
       │ └────── app_thread_model.go
       └──────── app_threads.go

  app_projects.go (ensureProjectForWorkspace)
       ▲ ▲ ▲ ▲ ▲
       │ │ │ │ └── app_thread_fork.go
       │ │ │ └──── app_discussion_start.go
       │ │ └────── app_thread_from_pr.go
       │ └──────── app_worktree.go
       └────────── app_threads.go (CreateThread)

  app_git.go (resolveGitPaths, gitCore)
       ▲ ▲ ▲ ▲ ▲ ▲ ▲ ▲ ▲
       │ │ │ │ │ │ │ │ └── app_send.go (one path)
       │ │ │ │ │ │ │ └──── app_worktree.go
       │ │ │ │ │ │ └────── app_thread_from_pr.go
       │ │ │ │ │ └──────── app_commit_message.go
       │ │ │ │ └────────── app_worktree_branch.go
       │ │ │ └──────────── app_diff_review_comments.go
       │ │ └────────────── app_proposed_plans.go
       │ └──────────────── app_checkpoint.go (diff)
       └────────────────── app_thread_delete.go (worktree cleanup)

  app_text_generation.go (runTextGeneration)
       ▲ ▲
       │ └── app_thread_title.go
       └──── app_commit_message.go
```

Chokepoints (most incoming arrows): `app_emit.go`, `app_session.go`, `app_send.go`, `app_provider_events.go`, `app_projects.go`, `app_git.go`. Lift these first.

## 5. Recommended migration order — bucket B (topological)

**Phase 0 — Prerequisite seams (not B work, but required before any B file touches them).**
1. **Emit seam.** Introduce `Emitter` interface; App implements it; migrated pkgs take the interface in their constructor. No state moves.
2. **`ensureProjectForWorkspace` → `internal/project`.** Lift the single helper.
3. **`resolveGitPaths` + `gitCore` → `internal/git` exports.** Required for git, worktree_branch, commit_message, thread_from_pr.

**Phase 1 — Zero-dependency B leaves (any order).**
4. `app_ui_trace.go` → `internal/uitrace` (new).
5. `app_directory.go` → `internal/dirbrowse` (new).
6. `app_keybindings.go` → `internal/keybindings` (new).
7. `app_network.go` → `internal/network` (new).

**Phase 2 — Settings adapters + WSL.**
8. `app_remote.go` → `internal/settings`.
9. `app_editor.go` + `app_editor_settings.go` → `internal/editor`.
10. `app_wsl.go` → `internal/wsldistro`.

**Phase 3 — Store extensions.**
11. `app_search.go`, `app_turns.go`, `app_paging.go` → `internal/store`.
12. `app_payloads.go` → split (store + App save-dialog shim).
13. `app_workspace.go`, `app_workspace_files.go` → `internal/workspacefiles`.

**Phase 4 — Attachments + drafts.**
14. `app_attachment.go` → `internal/attachment`.
15. `app_draft.go` → `internal/draft` or fold into store.

**Phase 5 — Observability + live state.** (Requires emit seam.)
16. `app_observability.go` → `internal/observability`.
17. `app_live_state.go` → `internal/triage` or `internal/livestate`.
18. `app_provider_status.go` → `internal/provider`.
19. `app_gitwatch.go` → `internal/gitwatch`.

**Phase 6 — Git surface.**
20. `app_worktree_branch.go` → `internal/git`.
21. `app_git.go` → `internal/git` (after `resolveGitPaths` + `gitCore` exports).

**Phase 7 — Provider probes + small provider files.** (Requires emit + session seams.)
22. `app_account_probe.go`, `app_claude_probe.go`, `app_codex_probe.go` → `internal/provider` boot + provider pkgs.
23. `app_claude_stop.go` → `internal/provider/claude`.
24. `app_codex_background.go` → `internal/provider/codex`.

**Phase 8 — Design + discussion + diff review + proposed plans.** (Requires send + session + events seams.)
25. `app_design.go` → `internal/design`.
26. `app_discussion.go` + `app_discussion_runtime.go` → `internal/discussion`.
27. `app_diff_review_comments.go` → `internal/diffreview`.
28. `app_proposed_plans.go` → `internal/proposedplans`.

**Phase 9 — Text generation cluster (this order).**
29. `app_text_generation.go` → `internal/textgen`.
30. `app_commit_message.go` → `internal/commitmessage` (or `internal/git`).
31. `app_thread_title.go` → `internal/threadtitle` (or `internal/textgen`).

**Phase 10 — Threads boundary (last of B; bridge into C).**
32. `app_context_settings.go` → `internal/contextsettings` or fold into threads.
33. `app_thread_from_pr.go` → `internal/threadfrompr` or fold into threads.

## 6. Bucket C — why these need design first

### Send seam (`internal/send`) — `app_send.go`, `app_steer.go`, `app_flush_queue.go`, `app_runtime_mode.go`
Owns `sendThreadMuRegistry` (per-thread mutex serializing send/steer/flush/revert/fork) and `userMessageMeta` (user-item-id sequencing). Every caller mutating session state during a send takes the same mutex. Design: `Sender` with `WithThreadLock(threadID, fn)` + `Send(...)`. Until that exists, none of the four can move.

### Session seam (`internal/session`) — `app_session.go`, `app_session_bindings.go`, `app_session_reaper.go`, `app_session_prompts.go` (B), `app_session_start.go` (B), `app_approval.go`, `app_claude_stop.go` (B), `app_codex_background.go` (B), `app_codex_reconcile.go`, `app_claude_ratelimits.go`
Sessions map + starting-sessions guard are App's most cross-cutting state. `SessionManager` with `Live(threadID)`, `Start/Stop/Close`, `RestartIfAffected(threadID, fields)`, `ForEach(...)`. Reaper, approval, ratelimits, codex reconcile all become consumers.

### Provider-events seam — `app_provider_events.go` + downstream consumers
`sessionEventHandler` and `recordSessionActivity`. Live on the session manager (closures' lifetime is bounded by the session, which owns the liveness counters).

### Emit seam — `app_emit.go`, `app_errors.go`, ~14 consumers
Smallest seam, most consumers. `Emitter` interface threaded through pkg constructors. App keeps the concrete impl. No state moves; only callers do.

### Checkpoint seam (`internal/checkpoint`) — `app_checkpoint.go`, `app_thread_fork.go`
`captureMessageCheckpoint`, `revertProviderConversationToMessage`, `claudeSourceSessionRef` — provider-branched (Claude JSONL slicing + Codex thread/rollback). Lift as exported pkg functions; checkpoint + fork both consume.

### Threads seam — `app_threads.go`, `app_thread_model.go`, `app_thread_interaction_mode.go`, `app_thread_delete.go`, `app_thread_fork.go`, `app_worktree.go`, `app_context_settings.go` (B)
`restartSessionIfAffected` + `sessionAffectingFields` belong on the SessionManager (it's `RestartIfAffected`). Once lifted, `app_threads.go` flattens to thin CRUD.

### Chat-model profile seam (`internal/chatmodel`) — `app_chat_bar.go` provider, 4 consumers
`Seed(threadID)`, `Remember(threadID, profile)`, `For(threadID) Profile`. Persistence of the bar's UI state is separate; can fold into store or stay in an App-side adapter.

### Settings split — `app_settings.go`
Adapter (load/save/providerBinaryPath) → `internal/settings`. Codex model catalog (TTL cache + singleflight) → `internal/provider/codex/catalog.go`. Halves share no state.

### Provider-specific files behind session+emit — `app_codex_reconcile.go`, `app_claude_ratelimits.go`
Move into `internal/provider/{codex,claude}` once session + emit seams exist; become bg workers owned by the provider pkg.

### Terminal — `app_terminal.go`
Mostly delegation, but callbacks emit. Move once emit seam exists; callback becomes injected `Emitter`.

## 7. Headline findings

1. **Bucket counts: A=9, B=33, C=24.** About half the surface is straightforward (A + early B). The other half is tangled by ~7 cross-cutting seams; each seam unblocks a cluster of 3–7 files.

2. **The send pipeline is one 67 KB unit.** `app_send.go` (22.9 KB) + `app_steer.go` (11.5 KB) + `app_flush_queue.go` (34.7 KB) share `sendThreadMuRegistry` and `sendMessageWithOptions`. They cannot be migrated independently. Any plan that ships them as three PRs hits a wall at the mutex registry.

3. **`app_threads.go` is heavier than it looks.** 23 KB of mostly thin CRUD, but the `restartSessionIfAffected` infrastructure inside it is the connective tissue that turns model/effort/fastMode/contextWindow/workspace mutations into session restarts. Until that lifts into a SessionManager method, four other files can't move cleanly.

4. **`app_chat_bar.go` is misnamed.** The persistent-state surface is small; the real role is the cross-cutting chat-model **profile** consumed by every thread-creating and model-mutating binding. The package is `chatmodel`, not `chatbar`.

5. **Two responsibilities collide in `app_settings.go`.** Settings adapter already maps to `internal/settings`. The Codex model catalog — with `sync.Mutex`, TTL cache, singleflight — is a substantial subsystem belonging in `internal/provider/codex`. The split is the only blocker.

6. **The emit seam is the cheapest unlock.** ~6.7 KB across `app_emit.go` + `app_errors.go`, consumed by ~14 callers. Lifting an `Emitter` interface — without moving the implementation — unblocks every B and C migration that emits. Phase 0 of the plan.

7. **`app_provider_events.go` is the chokepoint that doesn't look like one.** 4.7 KB, but every event-driven helper in C consumes `sessionEventHandler` or `recordSessionActivity`. Until the session seam absorbs both, approval, ratelimits, reaper, discussion-runtime, and design teardown all stay in `package main`.

8. **`app_thread_delete.go` is the last file to move.** Cascades through sessions, terminals, checkpoints, attachments, design, gitwatch, flush queues — every C seam + the design/gitwatch B-migrations.

9. **`app_directory.go`, `app_keybindings.go`, `app_network.go`, `app_ui_trace.go` are the recommended warm-ups.** All four are self-contained, large enough to justify a package, zero downstream impact on the harder migrations.

10. **Provider isolation holds.** Nothing in the corpus pushes toward a unified Claude/Codex package; the natural split is already drawn along provider lines.

11. **`LocalOnlyMethods` membership must be re-verified on every C migration.** Every C file contains methods that touch local FS, spawn processes, mutate settings, or write attachments. The `methods_gen_test.go` integrity check must continue to pass.

12. **No fundamentally new architecture is required.** Every destination is an existing `internal/` package or a thin new package matching the one-responsibility convention. The seams (`Emitter`, `SessionManager`, `Sender`) are interfaces, not new subsystems.

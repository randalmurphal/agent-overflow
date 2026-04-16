# UI Loop Progress

## Status: Review Phase

All work items from Phases 0-6 are implemented and passing quality gate.

## Known Issues

### KI-1: ChangedFilesTree not wired (WI-2.3)
- **Severity**: High
- **Description**: `ChangedFilesTree.svelte` is fully implemented but not rendered in `MessageTimeline.svelte`. The spec says it should show "all files changed in a turn" grouped by directory.
- **Fix**: Wire into MessageTimeline at turn boundaries — collect diff items per turn and render ChangedFilesTree as a summary before the next user message or at the end.

### KI-2: ModelPicker selectModel is no-op (WI-3.1)
- **Severity**: Medium
- **Description**: `ModelPicker.svelte` renders the dropdown but `selectModel()` does nothing — comments say "no SetModel binding available." However, `UpdateSettings` binding exists and can persist model preference.
- **Fix**: Use `UpdateSettings` to persist the selected model as `defaultModel`, and if the thread is new (no turns yet), recreate or update via available bindings.

### KI-3: Missing bindings exports (WI-0.3)
- **Severity**: Medium
- **Description**: `stores/bindings.ts` is missing exports for: `SwitchThread`, `StopSession`, `GetThread`, `GetWorkingTreeDiff`, `GitCreatePR`, `GitListWorktrees`, `GitRemoveWorktree`. These all exist in the generated `app.js`.
- **Fix**: Add missing exports to `stores/bindings.ts`.

### KI-4: Worktree management incomplete (WI-6.4)
- **Severity**: Low
- **Description**: Only worktree creation exists (in Sidebar new-thread form). No UI to list or remove worktrees. Bindings `GitListWorktrees` and `GitRemoveWorktree` exist but aren't exported or used.
- **Fix**: Add worktree list/remove to BranchToolbar or GitActionsControl dropdown.

## Completed Phases

### Phase 0: Code Reorganization
- [x] WI-0.1: Frontend directory restructure
- [x] WI-0.2: New type files
- [x] WI-0.3: Bindings expansion (partial - see KI-3)

### Phase 1: Shared Components
- [x] WI-1.1: Toast notification system
- [x] WI-1.2: CopyButton component
- [x] WI-1.3: CodeBlock with shiki syntax highlighting
- [x] WI-1.4: ConfirmDialog

### Phase 2: Chat Improvements
- [x] WI-2.1: StreamingMessage component
- [x] WI-2.2: ThinkingBlock expansion
- [x] WI-2.3: ChangedFilesTree (partial - see KI-1)
- [x] WI-2.4: ContextWindowMeter
- [x] WI-2.5: RateLimitsMeter
- [x] WI-2.6: ProviderStatusBanner
- [x] WI-2.7: Event router updates
- [x] WI-2.8: Session-scoped approval tracking

### Phase 3: Composer Improvements
- [x] WI-3.1: ModelPicker (partial - see KI-2)
- [x] WI-3.2: ProviderPicker
- [x] WI-3.3: WorkspacePicker
- [x] WI-3.4: Enhanced ApprovalPrompt

### Phase 4: Sidebar & Thread Management
- [x] WI-4.1: Thread rename
- [x] WI-4.2: Thread delete
- [x] WI-4.3: Archive confirmation

### Phase 5: Settings Panel
- [x] WI-5.1: Settings store
- [x] WI-5.2: SettingsView
- [x] WI-5.3: GeneralSettings
- [x] WI-5.4: ProviderSettings
- [x] WI-5.5: ArchivedThreads
- [x] WI-5.6: Theme system

### Phase 6: Git UI
- [x] WI-6.1: BranchToolbar
- [x] WI-6.2: GitActionsControl
- [x] WI-6.3: CommitDialog
- [x] WI-6.4: Worktree UI (partial - see KI-4)

## Review Log

| Iteration | Category | Findings | Fixed |
|-----------|----------|----------|-------|
| R1 | Known Issues | KI-1 through KI-4 | Pending |

# UI Loop Progress

## Status: Review Phase

All work items from Phases 0-6 are implemented and passing quality gate.

## Known Issues

- **KI-6**: ArchivedThreads "Unarchive" button calls `ArchiveThread` which only sets `archived = 1` (no toggle). Backend has no `UnarchiveThread` binding. The UI appears to work (thread removed from local list) but the thread remains archived in the database. Requires a backend fix (Loop 1 scope) to add an `UnarchiveThread` or toggle-based archive binding.

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
| R1 | Known Issues | KI-1: ChangedFilesTree unwired, KI-2: ModelPicker no-op, KI-3: Missing binding exports, KI-4: Worktree management incomplete | All 4 fixed |
| R2 | Spec Compliance | Types/imports/event routing vs IMPLEMENTATION-PARITY.md | Fixed |
| R3 | Code Consistency | Unify types, imports, error handling across components | All fixed |
| R4 | Dead Code | Unused components, settings, error paths | All wired |
| R5 | Accessibility | ARIA labels, dialog focus management, dropdown patterns | All fixed |
| R6 | Known Issues | Dead code wiring, missing feature completions | All fixed |
| R7 | Spec Compliance | 8 findings: wrong ApprovalRequest.questions shape, untyped permissions, missing types (UserInputQuestion, PermissionProfile, ApprovalResponse, DeliberationState), model_rerouted not updating store, tool_progress using wrong mutation, Thread.interactionMode unnarrowed, ApprovalPrompt missing user-input/permission kind rendering | All 8 fixed |
| R8 | Error Handling | 8 fixes: shiki initPromise .catch()+retry on failure, shiki/clipboard empty catch blocks now log, CodeBlock falls back to raw code on highlight failure, CopyButton toasts on copy failure, GitActionsControl shows error state+retry on status load failure, thread.svelte.ts toasts on payload meta load failure, settings.svelte.ts rolls back optimistic update on failure, ModelPicker wraps selectModel in try/catch | All 8 fixed |
| R9 | Visual Polish | 27 files: Svelte transitions on toasts/dialogs/collapsible panels/banner/icon swap; focus-visible rings on all interactive elements; semantic theme colors (success/error/warning) replacing hard-coded green/red/amber/yellow across codebase; fixed yellow vs amber inconsistency; cursor-help on ContextWindowMeter; disabled:cursor-not-allowed on menu items; flexible header truncation replacing fixed max-w; increased branch/model name max-w; max-h on command preview overflow; StatusBar token usage truncation; title attr for truncated errors; empty state for ModelPicker | All fixed |
| R10 | Integration Wiring | 6 findings: (1) SwitchThread binding never called -- backend not notified on thread switch; (2) StopSession binding never called -- no session cleanup on archive/delete; (3) timestampFormat setting stored but never consumed by relativeTime(); (4) streamingEnabled setting stored but never checked by MessageTimeline; (5) thread.svelte.ts and threads.svelte.ts imported bindings directly from Wails instead of centralized stores/bindings.ts; (6) types/events.ts exported internal-only types (PermissionProfile, ApprovalKind, UserInputQuestion, etc.) creating dead exports and duplication with Wails binding classes | All 6 fixed |
| R11 | Code Consistency | 7 findings: (1) ApprovalPrompt imported PermissionProfile directly from Wails bindings, bypassing stores/bindings.ts; (2) GitActionsControl missing console.error() before 5 pane.setError() calls; (3) ComposerControls used raw bg-green-400/bg-red-400 instead of semantic bg-success/bg-error; (4) ConfirmDialog destructive button used raw text-red-100/hover:bg-red-600 instead of semantic tokens; (5) WorkEntryData interface exported from WorkEntry.svelte instead of types/models.ts; (6) Theme type in utils/theme.ts duplicated Settings.theme union; (7) ProviderPicker had svelte import (onMount) last instead of first | All 7 fixed |
| R12 | Dead Code | 7 findings: (1) types/design.ts entirely unused -- no backend bindings, no frontend imports; (2) types/discussion.ts entirely unused -- same; (3) ThinkingMeta interface exported from types/models.ts but never imported (thinking payloads rendered as raw strings); (4) Toast interface exported from toast store but never imported externally; (5) EventKind type exported from types/events.ts but never imported externally; (6) GetThread, GetWorkingTreeDiff, GitListWorktrees re-exported from stores/bindings.ts but never called; (7) ComposerControls component rendered provider/model/status identically to StatusBar (which also shows token usage) -- redundant | All 7 fixed |
| R13 | Known Issues + Accessibility | KI-5: Added defaultProvider select to GeneralSettings. Accessibility sweep (20+ findings): 3 critical (aria-live on StreamingMessage, ProviderStatusBanner, ApprovalPrompt), 11 moderate (aria-expanded on 5 expand/collapse components, unique ConfirmDialog IDs, focus-visible rings on 6 buttons, ARIA tab pattern in SettingsView with arrow-key nav, role=status on StatusBar/WorkEntry, role=meter on RateLimitsMeter), 6 minor (aria-hidden on decorative SVGs in 8 components, aria-label on rename input) | All fixed |
| R14 | Visual Polish | 17 files, 8 categories: (1) semantic color tokens -- added provider-codex and overlay to app.css, replaced raw orange in ThreadRow+ArchivedThreads, raw black/60 in modal backdrops; (2) z-index hierarchy -- popovers z-30, dropdowns z-40/z-50, modals z-[60], toasts z-[80]; (3) dropdown transitions -- fly/fade on ModelPicker, BranchToolbar, GitActionsControl, WorkspacePicker, ContextWindowMeter; (4) ThreadRow button opacity transition-opacity for smooth hover reveal; (5) focus-visible:ring-2 + transition-colors on 13 inputs/textareas/selects across 8 components; (6) focus-visible rings on 5 buttons missing them (Sidebar Create/Cancel, Browse, branch create, git chevron); (7) overflow -- toast line-clamp-3, banner line-clamp-2, commit error break-words, provider status break-words; (8) pending message max-w-[80%] to match UserMessage | All fixed |
| R15 | Spec Compliance | 4 findings: (1) settings.svelte.ts DEFAULT_SETTINGS had 5 fields diverging from spec §3.5 -- confirmArchive was false (spec: true), defaultModelClaude/defaultModelCodex were empty (spec: 'claude-sonnet-4-6'/'gpt-5.4'), claudeBinaryPath/codexBinaryPath were empty (spec: 'claude'/'codex'); (2) thread_renamed event updated threads list but not the active pane's thread object -- ChatView header showed stale title; (3) model_rerouted event updated threads list but not the active pane's thread object -- ChatView header/StatusBar showed stale model; (4) ArchivedThreads "Unarchive" calls ArchiveThread which only sets archived=1 (no toggle) -- backend has no UnarchiveThread binding, noted as KI-6 (backend scope) | 3 fixed, 1 noted as KI-6 |
| R16 | Error Handling | 4 findings: (1) ThreadRow StopSession .catch(() => {}) silently swallowed errors on archive/delete -- now logs; (2) MessageTimeline JSON.parse catch had no error variable or logging -- now logs payloadId + error for diagnostics; (3) thread.svelte.ts SwitchThread catch only console.error'd -- user never told backend wasn't notified, now shows warning toast; (4) GitActionsControl refreshStatus used addToast but not pane.setError, inconsistent with all other git error paths in same file -- now uses pane.setError | All 4 fixed |
| R17 | Code Consistency | 12 findings across 9 files: (1-8) Import ordering violations in BranchToolbar, GitActionsControl, Sidebar, ArchivedThreads, ProviderSettings, ThreadRow, MessageTimeline (svelte/onMount not first, stores/types/components interleaved instead of grouped by category); (9) main.ts used double quotes while entire codebase uses single; (10) BranchToolbar used addToast for 2 pane-scoped git errors (status fetch + branch list) instead of pane.setError -- removed dead addToast import; (11) GitActionsControl dropdown used rounded while ModelPicker/BranchToolbar dropdowns use rounded-lg; (12) CommitDialog body textarea missing focus-visible:ring-2 focus-visible:ring-accent/50 that every other input/textarea has | All 12 fixed |
| R18 | Dead Code | 3 findings: (1) WorkEntry.svelte typeLabel switch used lowercase names (file_read, command, bash) that matched neither Claude tools (Bash, Read, Write, Edit, Grep) nor Codex items (command_execution, file_change) -- all entries fell to default [T], making [F] and [C] branches dead; now uses case-insensitive matching covering both providers; (2) ChangedFile.kind typed as string but sourced exclusively from DiffMeta.changeKind (4-value union) -- kindBadge default branch unreachable; narrowed type to DiffMeta['changeKind'], removed default branch and redundant type cast; (3) main.ts exported default app variable never imported by any module (Vite entry point loaded via script tag) -- removed unused export | All 3 fixed |
| R19 | Accessibility | 21 findings across 19 files: (1) CRITICAL: ApprovalPrompt alertdialog had no focus management -- focus now moves into panel when approvals appear, restores on dismiss; (2-4) CommandOutput/DiffPreview/ThinkingBlock missing aria-controls linking toggle buttons to content panels; (5-7) same 3 components: loading states missing role=status, error states missing role=alert; (8) ThinkingBlock scrollable region not keyboard-accessible -- added tabindex=0 + role=region; (9-11) ModelPicker/BranchToolbar/GitActionsControl dropdowns lacked focus management on open/close; (12) GitActionsControl menu had no arrow-key navigation -- added ArrowUp/ArrowDown/Escape; (13) CommitDialog error not announced -- added role=alert; (14-15) ThreadList/ArchivedThreads collections had no list semantics -- added role=list/listitem; (16) ArchivedThreads Unarchive/Delete buttons lacked contextual aria-labels; (17) ProviderPicker toggle buttons lacked radio group semantics -- added role=radiogroup/radio/aria-checked + status in aria-label; (18) ChangedFilesTree file buttons missing aria-label; (19) ContextWindowMeter SVG/percentage not aria-hidden, popover missing role=tooltip; (20) WorkEntry type labels/StreamingMessage cursors/StatusBar pipes/ThreadRow provider badge not aria-hidden; (21) ThreadRow had no keyboard rename trigger -- added F2; MessageTimeline scroll container missing role=log, loading state missing role=status | All 21 fixed |
| R20 | Visual Polish | 15 fixes across 14 files: (1) ContextWindowMeter missing >95% error tier -- added stroke-error matching RateLimitsMeter 3-tier system; (2-3) CommandOutput + DiffPreview collapse transitions broken -- content gated on always-truthy displayText/displayLines instead of `expanded`, slide transition never fired on collapse; (4-5) UserMessage max-w-[80%] and pending message inconsistent with AssistantMessage/StreamingMessage max-w-[85%] -- unified to 85%; (6) MessageTimeline "Thinking..." fallback bg-surface-1 mismatched StreamingMessage bg-surface-2; (7) CopyButton checkmark stroke-width 2.5 inconsistent with clipboard icon 2; (8) Composer Send/Stop buttons used different hover patterns (opacity vs bg-shift) and lacked transition-colors; (9) Markdown headings rendered raw text, not inline formatting -- now parseInline() applied to h1/h2/h3; (10) ProviderSettings no empty state when models list empty; (11) StatusBar cost span truncated before token counts -- added shrink-0; (12) ApprovalPrompt redundant overflow-x-auto with whitespace-pre-wrap; approval buttons now flex-wrap + transition-colors; (13) GitActionsControl menu items missing active:bg state + transition-colors; (14) ThreadRow delete/archive icons undersized w-3 bumped to w-3.5, added stroke-linejoin; (15) ChatView header gap-y for flex-wrap, workspace path max-w-[200px], empty state icon | All 15 fixed |

# PROMPT: agent-overflow UI (Loop 2)

## Housekeeping

- Ignore `ralph-*.log`, `node_modules/`, `dist/`, `coverage/`
- Pre-existing uncommitted changes are not the agent's problem

## Prime Directive

This loop builds ALL frontend UI for feature parity with Forge. Backend work was done in Loop 1 -- all Wails bindings are available. This loop touches ONLY the `frontend/` directory.

Deliverables:
- **Shared components**: syntax highlighting (shiki), copy buttons, confirm dialogs, toast notifications
- **Chat improvements**: streaming markdown, thinking block expansion, changed files tree, context/rate meters, provider status banner
- **Composer improvements**: model/provider/workspace pickers, enhanced approvals with session-scoped + structured responses
- **Sidebar improvements**: inline rename, delete with confirmations
- **Settings panel**: general settings, provider settings, archived threads, theme system
- **Git UI**: branch toolbar, git actions control, commit dialog, worktree UI
- **Frontend code reorganization**: component subdirectories per IMPLEMENTATION-PARITY.md section 1.3

### Authority Hierarchy

1. ARCHITECTURE.md -- behavioral authority
2. IMPLEMENTATION-PARITY.md -- implementation authority
3. This PROMPT file -- work items and rules

### Mission

Build a polished, fully-functional frontend where:
- All components use the ThreadPane factory pattern (props: pane)
- Wails bindings handle all backend communication (RPC via auto-generated bindings, push via EventsOn)
- Heavy payloads load on-demand via GetPayloadData (not inlined in events)
- Settings persist via GetSettings/UpdateSettings bindings (backend handles JSON file)
- Git operations go through thread-scoped bindings (GitCommit, GitPush, etc.)
- The app is usable end-to-end with both Claude and Codex providers

## Rules of Engagement

### Non-Negotiable

1. Read progress-ui.md first every iteration
2. Read the relevant IMPLEMENTATION-PARITY.md section before implementing each work item
3. Read referenced Forge component files before implementing their agent-overflow equivalents
4. Use Svelte 5 runes exclusively ($state, $derived, $effect, $props) -- no Svelte 4 patterns (no `$:`, no `export let`, no `on:click`)
5. Tailwind CSS 4 with @theme directives -- no tailwind.config.js
6. All components must work with the ThreadPane factory pattern -- receive `pane: ThreadPane` as prop where applicable
7. Use Wails `runtime.EventsOn` for push events, auto-generated bindings from `wailsjs/go/main/App` for RPC
8. Max ~300 lines per Svelte component. Split into named sub-components when exceeding.
9. Use Playwright MCP to test UI changes in the browser when `wails dev` is running
10. Run quality gate (`npm run build && npm run check`) before every commit
11. No Go backend changes -- all backend work was completed in Loop 1
12. No new npm dependencies without verifying the feature cannot be done in <30 lines. Exception: shiki (syntax highlighting) is explicitly approved.
13. One work item per iteration

### Prohibited

- React patterns (useState, useEffect, useMemo, useCallback, JSX)
- Svelte 4 patterns (`$:` reactive statements, `export let` props, `on:click` directives)
- Modifying Go files or anything outside `frontend/`
- Adding TODO/FIXME/HACK comments
- Empty catch blocks -- always log or surface the error via `pane.setError()`
- God components exceeding 300 lines
- Barrel exports (index.ts re-exporting everything)
- Console.log for error handling -- use `pane.setError()` for user-facing errors, `console.error()` for developer diagnostics
- Global mutable state -- all thread state lives on the ThreadPane instance
- Guessing at Wails binding signatures -- check `wailsjs/go/main/App.d.ts`

## Environment

- Framework: Svelte 5, Vite 8, Tailwind CSS 4, TypeScript
- Working directory: `/Users/randy/repos/agent-overflow/frontend`
- Wails bindings: `frontend/wailsjs/go/main/App.{js,d.ts}` (auto-generated from Go)
- Wails runtime: `frontend/wailsjs/runtime/runtime.js` (EventsOn, EventsOff, etc.)
- Existing stores: `stores/thread.svelte.ts` (ThreadPane factory), `stores/panes.svelte.ts`, `stores/threads.svelte.ts`, `stores/events.ts`, `stores/bindings.ts`
- Existing types: `types/events.ts`, `types/models.ts`
- Existing components: `ChatView.svelte`, `MessageTimeline.svelte`, `Composer.svelte`, `Sidebar.svelte`, `Markdown.svelte`, etc.
- Spec files: `ARCHITECTURE.md`, `IMPLEMENTATION-PARITY.md`
- Reference: Forge UI at `~/repos/forge/apps/web/src/` -- use for UI patterns, not architecture

## Quality Gate

```bash
cd /Users/randy/repos/agent-overflow/frontend && npm run build && npm run check
```

0 errors required. Run before every commit.

## Workflow Per Iteration

1. Read progress-ui.md -- check Known Issues and resolve ALL first
2. Pick the next incomplete work item (phases are ordered, items within a phase are ordered)
3. Read the relevant IMPLEMENTATION-PARITY.md sections cited in the work item
4. Read referenced Forge component files cited in the work item
5. Implement the component/feature
6. Run quality gate
7. If `wails dev` is running, test in browser with Playwright MCP
8. Commit with descriptive message
9. Update progress-ui.md
10. Repeat

## Work Items

### Phase 0: Code Reorganization

#### WI-0.1: Frontend directory restructure

Move existing components into subdirectories per IMPLEMENTATION-PARITY.md section 1.3:
- `components/ChatView.svelte` -> `components/chat/ChatView.svelte`
- `components/MessageTimeline.svelte` -> `components/chat/MessageTimeline.svelte`
- `components/UserMessage.svelte` -> `components/chat/UserMessage.svelte`
- `components/AssistantMessage.svelte` -> `components/chat/AssistantMessage.svelte`
- `components/WorkEntry.svelte` -> `components/chat/WorkEntry.svelte`
- `components/DiffPreview.svelte` -> `components/chat/DiffPreview.svelte`
- `components/CommandOutput.svelte` -> `components/chat/CommandOutput.svelte`
- `components/Composer.svelte` -> `components/composer/Composer.svelte`
- `components/ComposerControls.svelte` -> `components/composer/ComposerControls.svelte`
- `components/ApprovalPrompt.svelte` -> `components/composer/ApprovalPrompt.svelte`
- `components/Sidebar.svelte` -> `components/sidebar/Sidebar.svelte`
- `components/ThreadList.svelte` -> `components/sidebar/ThreadList.svelte`
- `components/ThreadRow.svelte` -> `components/sidebar/ThreadRow.svelte`
- `components/Markdown.svelte` -> `components/shared/Markdown.svelte`
- `components/StatusBar.svelte` -> `components/shared/StatusBar.svelte`
- `components/BackgroundTray.svelte` -> `components/shared/BackgroundTray.svelte`

Update ALL imports in `App.svelte`, ChatView, MessageTimeline, Sidebar, and any other files that reference moved components.

**Spec refs**: IMPLEMENTATION-PARITY.md section 1.3
**Tests**: `npm run build` passes, `npm run check` passes, no broken imports
**Done when**: all components in correct subdirectories, quality gate passes

#### WI-0.2: New type files

Create type files with all types from IMPLEMENTATION-PARITY.md section 4:
- `types/settings.ts` -- Settings, ProviderStatus, ModelInfo
- `types/discussion.ts` -- DiscussionParticipant, DiscussionDefinition, Channel, ChannelMessage
- `types/design.ts` -- DesignArtifact, DesignOption, DesignOptionsRequest
- `types/git.ts` -- GitStatus, GitBranch, GitActionResult

Expand `types/events.ts` with new EventKind values: `tool_progress`, `compact_boundary`, `rate_limits`, `model_rerouted`, `thread_renamed`.

**Spec refs**: IMPLEMENTATION-PARITY.md sections 4.1-4.5
**Tests**: TypeScript compiles, `npm run check` passes
**Done when**: all new types defined and exported

#### WI-0.3: Bindings expansion

Export ALL new Wails bindings from `stores/bindings.ts`:
- Settings: `GetSettings`, `UpdateSettings`
- Provider detection: `GetProviderStatuses`, `GetModelsForProvider`
- Thread management: `SwitchThread`, `ReconnectSession`, `RenameThread`, `DeleteThread`, `StopSession`
- Git operations: `GetGitStatus`, `GetWorkingTreeDiff`, `GitListBranches`, `GitCommit`, `GitPush`, `GitPull`, `GitCheckout`, `GitCreateBranch`, `GitCreatePR`, `GitCreateWorktree`, `GitRemoveWorktree`, `GitListWorktrees`
- Discussion: `ListDiscussions`, `GetDiscussion`, `CreateDiscussion`, `UpdateDiscussion`, `DeleteDiscussion`, `StartDiscussion`, `GetChannelMessages`, `PostChannelMessage`
- Design: `ListDesignArtifacts`, `GetDesignArtifactHTML`, `ChooseDesignOption`

All imported from `wailsjs/go/main/App`.

**Spec refs**: IMPLEMENTATION-PARITY.md sections 7.3, 9.5, 10.4, 11.4
**Tests**: TypeScript compiles
**Done when**: all bindings accessible from `stores/bindings.ts`

### Phase 1: Shared Components

#### WI-1.1: Toast notification system

Create `stores/toast.svelte.ts`:
- Toast state: array of `{ id, type, message, duration }` with $state rune
- `addToast(type: 'success'|'error'|'warning'|'info', message: string, duration?: number)` -- generates unique ID, pushes to array, sets auto-dismiss timer (default 5s)
- `removeToast(id: string)` -- removes from array
- Export singleton instance (this is app-level state, not per-pane)

Create `components/shared/Toast.svelte`:
- Renders toast stack in fixed position (bottom-right)
- Each toast: icon by type, message text, dismiss button
- Enter/exit transitions
- Color-coded by type: success=green, error=red, warning=amber, info=blue

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.1
**Done when**: toast system works, usable from any component via store import

#### WI-1.2: CopyButton component

Create `utils/clipboard.ts`:
- `copyToClipboard(text: string): Promise<boolean>` -- uses `navigator.clipboard.writeText`, returns success/failure

Create `components/shared/CopyButton.svelte`:
- Props: `text: string`, `label?: string`
- Shows copy icon, transitions to checkmark for 2s on successful copy
- Uses `copyToClipboard` utility
- Compact size for embedding in code blocks

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.1
**Done when**: copy button works with visual feedback

#### WI-1.3: CodeBlock with shiki syntax highlighting

Install shiki: `npm install shiki`

Create `utils/shiki.ts`:
- Singleton highlighter: lazy-initialized on first use
- `getHighlighter(): Promise<Highlighter>` -- creates highlighter with common languages (typescript, javascript, python, go, rust, bash, json, html, css, svelte, sql, markdown, diff)
- `highlightCode(code: string, lang: string): Promise<string>` -- returns highlighted HTML string
- Fallback: if language not loaded or highlighting fails, return escaped HTML

Create `components/shared/CodeBlock.svelte`:
- Props: `code: string`, `lang: string`
- Renders syntax-highlighted code via shiki
- CopyButton in top-right corner
- Language label in header bar
- Falls back to plain `<pre><code>` while loading or on failure

Update `components/shared/Markdown.svelte`:
- Replace plain `<pre><code>` rendering with CodeBlock component for code blocks

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.1
**Done when**: code blocks render with syntax highlighting and copy button in Markdown

#### WI-1.4: ConfirmDialog

Create `components/shared/ConfirmDialog.svelte`:
- Props: `open: boolean`, `title: string`, `description: string`, `confirmLabel?: string`, `cancelLabel?: string`, `destructive?: boolean`, `onConfirm: () => void`, `onCancel: () => void`
- Modal overlay with backdrop
- Focus trap: tab cycles within dialog, auto-focus confirm button
- Escape key closes (calls onCancel)
- Click outside closes (calls onCancel)
- Destructive variant: red confirm button
- Accessible: role="dialog", aria-labelledby, aria-describedby

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.1
**Done when**: dialog works, destructive variant styled differently, keyboard accessible

### Phase 2: Chat Improvements

#### WI-2.1: StreamingMessage component

Create `components/chat/StreamingMessage.svelte`:
- Props: `content: string` (the raw streaming text)
- Splits content into "completed" and "in-progress" regions:
  - Completed: text ending with a double newline (paragraph boundary) -- rendered through Markdown component
  - In-progress: remaining tail text -- rendered as plain text with cursor animation
- Uses the existing Markdown component for completed blocks
- Cursor: blinking block at the end of in-progress text

Update `components/chat/MessageTimeline.svelte`:
- Replace the plain streaming content `<p>` with StreamingMessage component

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.2
**Done when**: streaming content renders markdown for completed blocks, plain text for tail

#### WI-2.2: ThinkingBlock expansion

Create `components/chat/ThinkingBlock.svelte`:
- Props: `item: Item` (kind === 'thinking', has payloadId)
- Collapsed state: shows preview (first 200 chars from item.summary), "Show thinking" toggle
- Expanded state: loads full content via `GetPayloadData(item.payloadId)`, renders as plain text
- Animated height transition on expand/collapse
- Loading state while fetching payload data
- Error handling: show error message if payload fetch fails

Update `components/chat/MessageTimeline.svelte`:
- Replace the inline thinking `<div>` with ThinkingBlock component

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.2
**Done when**: thinking blocks expand/collapse with lazy-loaded full content

#### WI-2.3: ChangedFilesTree

Create `components/chat/ChangedFilesTree.svelte`:
- Props: `files: Array<{ path: string, insertions: number, deletions: number, kind: string, payloadId: string }>`
- Groups files by directory (extract directory from file path)
- Each file shows:
  - File name (last path segment)
  - Colored +/- badges (green for insertions, red for deletions)
  - Change kind indicator (added/modified/deleted/renamed)
- Click a file to expand and show its DiffPreview inline

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.2
**Reference**: `~/repos/forge/apps/web/src/components/chat/ChangedFilesTree.tsx`
**Done when**: file tree renders with stats, click expands diff preview

#### WI-2.4: ContextWindowMeter

Create `components/chat/ContextWindowMeter.svelte`:
- Props: `usedTokens: number`, `maxTokens?: number`, `usedPercentage?: number`, `totalProcessed?: number`
- Circular SVG ring (radius ~10, stroke-width 3)
- Shows percentage number in center
- Background ring at low opacity, filled ring proportional to usage
- Hover popover showing:
  - "Context window" header
  - Percentage and token counts
  - Total processed if different from used
- Color shifts toward warning at high usage (>80%)

Add context window state to ThreadPane (`stores/thread.svelte.ts`):
- New $state field: `contextWindow: ContextWindow | null`
- New mutation: `setContextWindow(data: ContextWindow)`

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.2
**Reference**: `~/repos/forge/apps/web/src/components/chat/ContextWindowMeter.tsx`
**Done when**: meter renders from context window data, popover shows details

#### WI-2.5: RateLimitsMeter

Create `components/chat/RateLimitsMeter.svelte`:
- Props: `limits: RateLimitEntry[]` (from types/events.ts)
- Shows rate limit entries grouped by window (5h, 7d)
- Each entry: limit name, usage bar, percentage
- Warning color (amber) when > 80%, danger (red) when > 95%
- Compact display for header bar integration

Add rate limits state to ThreadPane (`stores/thread.svelte.ts`):
- New $state field: `rateLimits: RateLimitEntry[]`
- New mutation: `setRateLimits(limits: RateLimitEntry[])`

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.2
**Done when**: rate limits display from provider events, warning colors correct

#### WI-2.6: ProviderStatusBanner

Create `components/chat/ProviderStatusBanner.svelte`:
- Props: `pane: ThreadPane`
- Shows alert banner when sessionStatus is 'error' or 'disconnected'
- Different messages/colors for each status:
  - `error`: red banner with error message from `pane.error`
  - `disconnected`: amber banner with "Session disconnected"
  - `retrying`: amber banner with "Reconnecting..."
- "Reconnect" button that calls `ReconnectSession(pane.threadId)`
- "Dismiss" button that calls `pane.clearError()`
- Slides in/out with transition

Update `components/chat/ChatView.svelte`:
- Replace the inline error div with ProviderStatusBanner component

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.2
**Done when**: banner shows on disconnect/error, reconnect button works

#### WI-2.7: Event router updates

Update `stores/events.ts`:
- Add routing for new event kinds: `tool_progress`, `compact_boundary`, `rate_limits`, `model_rerouted`, `thread_renamed`
- `tool_progress`: update active tool call meta with progress data
- `compact_boundary`: call `pane.setContextWindow()` with parsed meta
- `rate_limits`: call `pane.setRateLimits()` with parsed limits array
- `model_rerouted`: emit toast notification with new model name
- `thread_renamed`: update thread title in pane state, update threads list

Update `stores/thread.svelte.ts`:
- Add `contextWindow` and `rateLimits` $state fields
- Add `setContextWindow()` and `setRateLimits()` mutations
- Add `updateToolProgress()` mutation to merge progress into activeToolCalls

**Spec refs**: IMPLEMENTATION-PARITY.md sections 4.5, 5.3
**Target files**: `stores/events.ts`, `stores/thread.svelte.ts`
**Done when**: all new events routed to correct pane state, no unhandled event kinds

#### WI-2.8: Session-scoped approval tracking

Update `stores/thread.svelte.ts`:
- Add `sessionApprovedTools: Set<string>` as $state field (cleared on switchThread)
- Add `addSessionApprovedTool(toolName: string)` mutation
- Add `isToolSessionApproved(toolName: string): boolean` getter

Update `stores/events.ts`:
- On `approval_request`: check `pane.isToolSessionApproved(toolName)` first
  - If yes: auto-resolve by calling `RespondToApproval` with allow decision, do NOT show prompt
  - If no: show prompt as normal

Update approval UI to include "Allow for Session" button (WI-3.4 handles the full UI, but the state tracking wires here).

**Spec refs**: IMPLEMENTATION-PARITY.md section 5.1
**Target files**: `stores/thread.svelte.ts`, `stores/events.ts`
**Done when**: session-approved tools auto-resolve without prompting user

### Phase 3: Composer Improvements

#### WI-3.1: ModelPicker

Create `components/composer/ModelPicker.svelte`:
- Props: `pane: ThreadPane`
- Dropdown trigger showing current model name
- Dropdown content:
  - List of known models from `GetModelsForProvider(pane.thread.provider)` (fetched on open)
  - Selected model highlighted
  - Divider
  - Custom model input: text field with "Use custom model" label
- On selection: update thread model (may need a SetModel binding or include in thread update)
- Closes on selection or click outside / Escape

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.3
**Done when**: model picker works with known models and custom input

#### WI-3.2: ProviderPicker

Create `components/composer/ProviderPicker.svelte`:
- Props: `currentProvider: 'claude' | 'codex'`, `onSelect: (provider: string) => void`
- Toggle buttons for Claude and Codex
- Each shows provider name + status dot (green=ready, red=error, gray=not_found)
- Disabled providers grayed out with reduced opacity
- Fetches status via `GetProviderStatuses()` on mount

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.3
**Done when**: provider switching works, status dots reflect actual provider state

#### WI-3.3: WorkspacePicker

Create `components/sidebar/WorkspacePicker.svelte`:
- Props: `value: string`, `onSelect: (path: string) => void`, `recentWorkspaces: string[]`
- Text input for manual path entry
- "Browse" button: calls Wails `runtime.OpenDirectoryDialog()` for native folder picker
- Dropdown of recent workspaces (from settings) shown on focus
- Click a recent workspace to select it

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.3
**Done when**: native dialog opens, recent workspaces shown, manual input works

#### WI-3.4: Enhanced ApprovalPrompt

Update `components/composer/ApprovalPrompt.svelte`:
- Show full tool input details:
  - For command tools: command text in a code block
  - For file tools: file path and content preview
- Three response buttons: "Allow" (primary), "Allow for Session" (secondary), "Deny" (destructive)
  - "Allow for Session" calls `pane.addSessionApprovedTool(toolName)` then resolves
- For user-input kind (from ApprovalRequest.kind === 'user-input'):
  - Render each question from `approval.questions[]`
  - Text input or option selector per question
  - Submit collects answers into `{ answers: { [questionId]: value } }`
- For permission kind (from ApprovalRequest.kind === 'permission'):
  - Show permission details (network, filesystem paths)
  - Scope selector: "This turn only" vs "This session"
  - Submit includes permissions and scope

Call `RespondToApproval` binding with the structured `ApprovalResponse`.

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.3
**Reference**: `~/repos/forge/apps/web/src/components/chat/ComposerPendingApprovalPanel.tsx`
**Done when**: all three approval types render correctly with appropriate controls

### Phase 4: Sidebar & Thread Management

#### WI-4.1: Thread rename

Update `components/sidebar/ThreadRow.svelte`:
- Double-click title text to enter inline edit mode
- Edit mode: replace title span with a text input, pre-filled with current title
- Enter: save by calling `RenameThread(thread.id, newTitle)`, update thread in list
- Escape: cancel, restore original title
- Click outside: save
- Loading state during save

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.4
**Done when**: inline rename works with Enter/Escape/click-outside handling

#### WI-4.2: Thread delete

Update `components/sidebar/ThreadRow.svelte`:
- Add delete button (trash icon) on hover, next to existing archive button
- On click:
  - If `confirmDelete` setting is true: show ConfirmDialog (destructive variant)
  - If false: delete immediately
- Call `DeleteThread(thread.id)`, remove from threads list

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.4
**Done when**: delete with optional confirmation works, thread removed from sidebar

#### WI-4.3: Archive confirmation

Update `components/sidebar/ThreadRow.svelte` (or wherever archive action lives):
- On archive click:
  - If `confirmArchive` setting is true: show ConfirmDialog before archiving
  - If false: archive immediately
- Check setting value from settings store

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.4
**Done when**: archive respects confirmArchive setting from settings store

### Phase 5: Settings Panel

#### WI-5.1: Settings store

Create `stores/settings.svelte.ts`:
- `settings` $state field initialized to default values
- `loadSettings()`: calls `GetSettings()`, updates reactive state
- `updateSetting(key, value)`: calls `UpdateSettings({ [key]: value })`, updates reactive state
- `loaded` boolean $state flag
- Export singleton instance (settings are app-level, not per-pane)
- Load on app mount (in App.svelte)

**Spec refs**: IMPLEMENTATION-PARITY.md sections 3.5, 7.3
**Target files**: `stores/settings.svelte.ts`
**Done when**: settings state reactive and persistent via Wails bindings

#### WI-5.2: SettingsView

Create `components/settings/SettingsView.svelte`:
- Full-height container replacing ChatView when settings are open
- Left sidebar navigation: General, Providers, Archived
- Active section highlighted
- Content area renders the selected section component
- Close button (X) to return to chat view

Add settings toggle to App.svelte:
- State for `showSettings: boolean`
- When true: render SettingsView instead of ChatView
- Gear icon in sidebar header to toggle

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.5
**Reference**: `~/repos/forge/apps/web/src/components/settings/SettingsPanels.tsx`
**Done when**: settings panel navigable with section switching

#### WI-5.3: GeneralSettings

Create `components/settings/GeneralSettings.svelte`:
- Theme picker: radio group or select for system/light/dark
- Timestamp format: select for locale/12-hour/24-hour
- Diff word wrap: toggle switch
- Streaming enabled: toggle switch
- Confirm before archive: toggle switch
- Confirm before delete: toggle switch
- Each setting calls `updateSetting()` from settings store on change

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.5
**Done when**: all general settings functional, changes persist

#### WI-5.4: ProviderSettings

Create `components/settings/ProviderSettings.svelte`:
- Renders settings for both Claude and Codex providers
- Per provider:
  - Enable/disable toggle
  - Binary path input
  - Version display (from `GetProviderStatuses()`)
  - Status indicator (ready/not_found/error with colored dot)
  - Known models list (read-only, from `GetModelsForProvider()`)
  - Custom model add/remove (edits settings `customModels` if applicable)
- Provider status refreshes on mount and when binary path changes

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.5
**Reference**: `~/repos/forge/apps/web/src/components/settings/SettingsPanels.tsx` (PROVIDER_SETTINGS, PROVIDER_STATUS_STYLES)
**Done when**: provider settings work for both providers, status reflects reality

#### WI-5.5: ArchivedThreads

Create `components/settings/ArchivedThreads.svelte`:
- Lists all archived threads (fetched via `ListThreads` with archived filter, or a dedicated binding)
- Each row: thread title, provider icon, archived date
- Actions per thread: "Unarchive" button, "Delete" button (with confirmation if enabled)
- Empty state: "No archived threads"

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.5
**Done when**: archived threads listed with unarchive/delete actions

#### WI-5.6: Theme system

Implement system/light/dark theme support:
- CSS custom properties for all colors (text, surface, border, accent) -- likely already partially in place via Tailwind @theme
- `utils/theme.ts`:
  - `applyTheme(theme: 'system'|'light'|'dark')`: sets `class` on `<html>` element (dark/light), handles system preference via `matchMedia('(prefers-color-scheme: dark)')`
  - System theme: listen to `matchMedia` change events
- Apply theme on app mount from settings store
- Re-apply on settings change

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.5
**Reference**: `~/repos/forge/apps/web/src/lib/appearance.ts`
**Done when**: theme switching works (system/light/dark), persists across reloads

### Phase 6: Git UI

#### WI-6.1: BranchToolbar

Create `components/git/BranchToolbar.svelte`:
- Props: `pane: ThreadPane`
- Shows current branch name from git status (fetched via `GetGitStatus(pane.threadId)`)
- Branch icon prefix
- Click opens branch picker dropdown:
  - Search/filter input
  - Branch list from `GitListBranches(pane.threadId)`
  - Current branch marked
  - "Create new branch" option at bottom (text input, calls `GitCreateBranch`)
- Selecting a branch calls `GitCheckout(pane.threadId, branch)`
- Closes on selection or Escape

Integrate into `components/chat/ChatView.svelte` header area.

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.6
**Reference**: `~/repos/forge/apps/web/src/components/BranchToolbar.logic.ts`
**Done when**: branch display and switching works in chat header

#### WI-6.2: GitActionsControl

Create `components/git/GitActionsControl.svelte`:
- Props: `pane: ThreadPane`
- Context-aware primary action button based on git status:
  - Has changes -> "Commit" (opens CommitDialog)
  - Has commits ahead -> "Push" (calls `GitPush`)
  - Has upstream, no PR -> "Create PR" (opens PR creation flow)
  - Behind upstream -> "Pull" (calls `GitPull`)
  - Up to date -> disabled "Commit" with tooltip
- Split button: primary action + dropdown with all actions
- Disabled states with explanatory tooltips
- Loading/progress state during git operations

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.6
**Reference**: `~/repos/forge/apps/web/src/components/GitActionsControl.logic.ts` (buildMenuItems, resolveQuickAction)
**Done when**: git actions work based on status, disabled states correct

#### WI-6.3: CommitDialog

Create `components/git/CommitDialog.svelte`:
- Uses ConfirmDialog pattern (modal with backdrop)
- Props: `pane: ThreadPane`, `open: boolean`, `onClose: () => void`
- Subject input (single line, max 72 chars)
- Body textarea (multi-line, optional)
- Shows changed file count and +/- stats from git status
- Confirm button: calls `GitCommit(pane.threadId, subject, body)`
- Success: close dialog, show toast, refresh git status
- Error: show error in dialog

**Spec refs**: IMPLEMENTATION-PARITY.md section 12.6
**Done when**: commit dialog works, refreshes git status after commit

#### WI-6.4: Worktree UI

Worktree mode for thread creation:
- Update `components/sidebar/Sidebar.svelte` new thread form:
  - Add "Mode" toggle: Local vs Worktree
  - When Worktree: call `GitCreateWorktree(threadId, branch)` after thread creation
  - Set thread workspace to the returned worktree path
- Visual indicator in `components/sidebar/ThreadRow.svelte` for worktree threads:
  - Small branch-split icon or "WT" badge if `thread.worktreePath` is set
- Worktree cleanup: when deleting a worktree thread, call `GitRemoveWorktree` if applicable

**Spec refs**: IMPLEMENTATION-PARITY.md section 2.7, ARCHITECTURE.md section 9
**Reference**: `~/repos/forge/apps/web/src/components/BranchToolbar.logic.ts` (env mode handling)
**Done when**: worktree threads can be created and identified visually

## Review Phase

When all work items are complete, enter the Review Phase. This is NOT optional. You NEVER write "Loop Complete" or "Loop Done" in the progress file. The human decides when the loop is done.

### Review Iteration Workflow

1. Read progress-ui.md -- check "Known Issues" and "Review Log"
2. If known issues exist, fix ALL known issues (highest severity first)
3. If no known issues, perform a FULL SWEEP of one category:
   a. Scan thoroughly -- search every relevant file, grep for patterns, compare against spec
   b. Collect ALL findings for this category before fixing anything
   c. Fix everything you found
   d. Write/fix tests for all changes
4. Run quality gate
5. Commit all fixes with descriptive message
6. Update progress-ui.md

### Review Categories (cycle through)

1. **Spec Compliance** -- Compare all types, interfaces, event routing against IMPLEMENTATION-PARITY.md
2. **Error Handling** -- Find every unhandled promise rejection, empty catch, console.log-as-error-handler
3. **Code Consistency** -- Same patterns across all components (props destructuring, state management, naming)
4. **Dead Code** -- Unused exports, unreferenced components, implemented-but-unwired features
5. **Accessibility** -- Focus management, keyboard navigation, aria labels, screen reader support
6. **Visual Polish** -- Spacing, colors, transitions, responsive behavior, empty states
7. **Integration Wiring** -- Every component connected, every event routed, every binding called

### Review Rules

- Known Issues are #1 priority
- One category per iteration, sweep completely
- Be adversarial -- zero findings means you didn't look hard enough
- Every finding must have a spec citation or concrete UX impact
- "Noted but not fixed" IS a defect
- Dead code is a defect
- NEVER mark the loop as complete

## Reminders

- **Svelte 5 runes exclusively.** `$state`, `$derived`, `$effect`, `$props`. No `$:`, no `export let`, no `on:click`.
- **ThreadPane factory pattern is the state model.** Components receive `pane: ThreadPane` as a prop. They do NOT import global pane getters. The event router fans out to all panes. This enables future tiling.
- **Events arrive via Wails EventsOn** on channels: `provider:event`, `provider:meta`, `provider:error`. Route by event kind in `stores/events.ts`.
- **Heavy payloads are NOT in events.** Load via `GetPayloadData(payloadId)` binding on user expand. Meta previews arrive with items.
- **Settings are a JSON file** (backend handles persistence). Frontend calls `GetSettings`/`UpdateSettings` bindings only.
- **Project path is not workspace path** (ARCHITECTURE.md principle 9). Worktree threads have different workspace and project paths.
- **shiki is the ONLY approved new dependency** for syntax highlighting. Everything else must be hand-rolled.
- **Read progress-ui.md.** Every iteration. No exceptions.
- **Known Issues in progress-ui.md are your #1 priority.**
- **If you build it, wire it.** No dead code.
- **NEVER write "Loop Complete" in the progress file.**

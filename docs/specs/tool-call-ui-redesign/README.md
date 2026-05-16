# Tool-Call UI Redesign

Specification for the ground-up redesign of how tool calls are presented
in the chat surface, for both Claude and Codex providers.

Artifacts in this directory:

- `README.md` — this file. Intent, decisions, three-phase execution plan.
- `design-mockup.html` — the original design mockup. Open it in a
  browser to see the target visual language for the rail layout, row
  geometry, indicator states, error sub-lines, diff caps, and agent /
  spawn / waiting / waited / closed row variants.

## Intent

The current tool-call UI grew organically. Renderers accumulated
provider quirks, the visual language is inconsistent across tool
kinds, success badges add noise to the common case, and the catch-all
`GenericToolCallRow.svelte` accretes special cases beyond its
component-size budget. The data layer is sound — both providers
already normalize into a single `Item` shape and one dispatcher
(`ToolCallCard.svelte`) routes to specialized renderers — but the
visual / component layer is overdue for a unification pass.

This redesign:

- Keeps the existing **semantics** intact. What information is shown
  for each tool call (command, path, diff, output, error, agent name,
  model, status) does not change. Wire shapes, provider parsers, the
  `Item` model, and payload meta stay as-is.
- Replaces the **presentation**. Uniform row geometry (chevron + icon
  + lowercase-gutter label + body + stats + indicator + timestamp),
  no success badges, color-coded icons, a continuous indented rail
  under each assistant message, error sub-lines aligned to the body
  column, and a 15-line inline diff cap with fade-to-side-panel.
- Diverges in protocol where it makes sense. Provider-specific
  variants (Codex `spawn` / `waiting` / `waited` / `closed`, Claude
  inline subagent with latest-tool preview, Codex `apply_patch` labeled
  as `patch`) keep their semantics; they share the same row primitive.
- Keeps proper separation between providers where it makes sense.
  `CollabToolRow.svelte` stays Codex-only, `AgentRow.svelte` stays
  Claude-only. They consume the same primitives but their bodies
  remain provider-specific.

## Settled decisions

| Topic | Decision |
|---|---|
| Subagent rows | Keep inline "latest tool" preview under the agent row (Claude inline subagents only). Apply new design styles to the preview. |
| Multi-file edits | One row per file (current behavior). MultiEdit / apply_patch already explodes into N `DiffFileBlock`s via `DiffFileStack`. |
| Diff cap | 15 lines + fade + "open in side panel" button. |
| Phase labels on stamps | Out of scope. |
| Tool output bodies | Collapsed by default with chevron-to-expand (matches current; matches design). |
| Assistant prose | Untouched. |
| MCP rows | label = `mcp`, body = `server.tool(args)`. |
| AskUserQuestion / ProposedPlan | Keep current behavior; restyle chrome to match the new visual language. |
| Rail | Continuous left-border under consecutive tool / think rows via per-row `border-l` with internal padding (no row margin). |
| Font | Keep Geist Mono. |
| Width | Keep 992px (`max-w-[62rem]`). |
| Success | No success badge or text anywhere. Indicator dot only for running / backgrounded / error / declined. |
| Icons | Same lucide icons; colors fit the project palette (cooler tones, harmonized with the cobalt-violet accent). |
| Errors | `RowError` sub-line under the row, aligned to the body column. No text-badge pills. |

## What's in scope

- Tool-call rendering surface: every renderer under
  `frontend/src/lib/components/chat/` that handles `tool_call` /
  `tool_completion` / `thinking` items.
- Primitives that those renderers compose
  (`TranscriptDisclosureHeader`, `ToolHeaderMeta`, `ToolKindIcon`,
  `CompletionBadge` removal).
- The rail layout in `MessageTimeline.svelte` (per-row outer wrapper
  only — no virtua / scroll / row-contract changes).
- Tokens for per-tool-kind icon colors.

## What's explicitly out of scope

- Assistant prose (`AssistantMessage.svelte`, `ChatMarkdown.svelte`,
  Streamdown wiring).
- Composer, sidebar, activity rail, settings, palette, terminal.
- Wire shapes (`Item` type, payload meta, provider parsers).
- Scroll architecture (virtua, `useStickToBottom`, the row contract).
- TodoWrite remains in the activity rail
  (`ActivityRailTodosBody`), not the timeline.
- Phase labels on stamps ("planning · 9:42 PM" etc.) — fragile to
  infer; defer.
- Subagent transcripts moving to a side panel or new thread view —
  the current inline "latest tool" preview stays.

## Architecture context (current state)

The data layer is already provider-unified. Both Claude and Codex
normalize into a single `Item` shape (`frontend/src/lib/types/models.ts`)
with `kind`-discriminated rows (`tool_call`, `tool_completion`,
`thinking`, `assistant_text`, `user_text`, `terminal_interaction`,
`notification`, `api_retry`, `api_error`, `error`, `compaction`).
There is no `claude/` or `codex/` Svelte directory — everything routes
through one timeline.

**Dispatcher chain:**

```
ChatView.svelte
 └─ MessageTimeline.svelte                  (virtua scroll surface, turn dividers)
     └─ TimelineLeaf.svelte                 (kind-based discrimination)
         ├─ AssistantMessage / UserMessage  (prose; out of scope)
         ├─ ThinkingBlock                   (think rows)
         ├─ ToolCallCard.svelte             (dispatcher for tool_call / tool_completion)
         │   ├─ AskUserQuestionCard
         │   ├─ CollabToolRow               (Codex collab family)
         │   ├─ ProposedPlanCard
         │   ├─ DiffFileBlock               (single-file diff)
         │   ├─ DiffFileStack               (multi-file → N DiffFileBlocks)
         │   ├─ ToolResultCard              (legacy non-diff tool_result)
         │   ├─ CommandOutput               (bash / shell / exec_command)
         │   └─ GenericToolCallRow          (catch-all: Read/Grep/Glob/Web/Plan/Agent/MCP/...)
         ├─ TerminalInteractionRow          (Codex bg-PTY)
         ├─ NotificationRow / SessionDiedNotification
         ├─ APIRetryRow / APIErrorRow
         └─ (compaction marker)
```

**Provider edge is small** (~6 branch points): `toolPresentation.ts`,
`MessageTimeline.svelte`, `ToolCallCard.svelte`, `backgroundTray.ts`,
`BackgroundTaskTrayRow.svelte`, plus the tool-name sets in
`commandDisplay.ts` and `codexCollabControls.ts`. Everything from
`ToolCallCard.svelte` down operates on the unified `Item` shape.

**The structure already matches the design at most places:**

- Tool outputs are collapsed by default with chevron-to-expand
  (`CommandOutput.svelte:219`, `GenericToolCallRow.svelte:337`).
- Multi-file edits already render one row per file via `DiffFileStack`
  → N `DiffFileBlock`s.
- Inline diffs already cap and fade with a side-panel CTA
  (`DiffFileBlock.svelte:307-314`), just at a different line count
  (30 today, 15 target).
- The chevron primitive (`TranscriptDisclosureHeader.svelte`) is
  already in place and rotates on expand.
- Thinking is already italic and clamped to 3 lines.
- Diff stats slot (`+N` / `-N`) already exists.

This means the work is concentrated on **visual presentation** —
labels, colors, the rail, indicator dots, error sub-lines — not on
structural / data-layer changes. Risk profile is correspondingly
small.

## Row geometry

Every tool / think row composes the same column structure. Widths /
spacing align so consecutive rows form a clean visual grid.

```
┌──────┬──────┬──────────┬─────────────────────┬───────┬─────────┬─────────┐
│ chev │ icon │ label    │ body                │ stats │ indic.  │ time    │
│      │      │ (48px)   │ (flex)              │       │         │         │
└──────┴──────┴──────────┴─────────────────────┴───────┴─────────┴─────────┘
  8px    14px   48px       1fr (truncate)        ~3rem   ~3rem     ~4rem
```

- **chev** — chevron (rotates 90° when expanded). From
  `TranscriptDisclosureHeader.svelte`.
- **icon** — colored SVG via `ToolKindIcon.svelte`. Color per tool
  kind from `--ico-*` tokens.
- **label** — fixed-width gutter, lowercase, `fg-hint`,
  no tracking. Examples: `bash`, `read`, `edit`, `write`, `patch`,
  `grep`, `glob`, `fetch`, `search`, `think`, `agent`, `spawn`,
  `waiting`, `waited`, `closed`, `terminal`, `mcp`, `tool`,
  `notebook`, `plan`, `ask`.
- **body** — flexes; truncates with ellipsis on overflow. Holds
  the human-readable invocation (command, path, diff filename,
  query, agent description).
- **stats** — fixed slot for `+N` / `−N` magnitude indicator on
  edit / write / patch rows. Empty otherwise.
- **indicator** — text-free state dot (see table below). Empty for
  success / idle.
- **time** — absolute HH:MM, tabular numerals, `fg-hint`.

Rows that have an `errored` / `killed` / `declined` state add a
`RowError` sub-line below the row, padding-aligned to the body
column (clears chev + icon + label gutters).

## Indicator states

`Indicator.svelte` is the single source of truth for state on each
row. Success / idle renders nothing — the absence of a dot is the
positive signal.

| State | Visual | Trigger |
|---|---|---|
| `null` (idle / success) | Empty slot | `status === 'completed'` without `is_error` / non-zero exit / `meta.isError`. Default. |
| `running` | Pulsing accent dot, ~6px, `var(--accent)`, 1.5s ease-in-out | `status === 'running'` or `'streaming'`, not backgrounded. |
| `backgrounded` | 3 staggered animated accent dots, ~3.5px each, 1.4s with 0.2s + 0.4s offsets | `status === 'running'` AND `isBackground === true` on the launch row. |
| `error` | Static red dot, ~6px, `var(--error)` | `status === 'errored'` / `'killed'`, or completed with `is_error` / non-zero exit / `meta.isError`. Pairs with `RowError`. |
| `declined` | Static amber dot, ~6px, `var(--warning)` | `status === 'declined'`. Pairs with `RowError`. |

No status text labels (no "RUNNING", "Failed", "Stopped"). No success
pill. No exit-code chip on the row itself — exit code goes in
`RowError`'s `code` slot when present.

## Tool inventory — current → target

Coverage check so the implementation hits every tool currently
rendered. Per-tool labels are categorical (the lowercase gutter
label), not the raw tool name.

| Tool name | Provider | Current renderer | Target gutter label | Notes |
|---|---|---|---|---|
| `Bash` | claude | `CommandOutput` | `bash` | |
| `command_execution`, `commandExecution`, `exec_command`, `shell` | codex | `CommandOutput` | `bash` | All normalize to the same renderer |
| `Edit` | claude | `DiffFileStack` / `DiffFileBlock` | `edit` | |
| `MultiEdit` | claude | `DiffFileStack` | `edit` | Explodes to N file rows |
| `Write` | claude | `DiffFileStack` / `DiffFileBlock` | `write` | |
| `NotebookEdit` | claude | `DiffFileStack` | `notebook` | |
| `apply_patch` | codex | `DiffFileStack` | `patch` | Multi-file → N rows |
| `Read` | claude | `GenericToolCallRow` | `read` | Collapsed by default |
| `Grep` | claude | `GenericToolCallRow` | `grep` | |
| `Glob` | claude | `GenericToolCallRow` | `glob` | |
| `WebFetch` | claude | `GenericToolCallRow` | `fetch` | |
| `WebSearch`, `web_search`, `webSearch` | both | `GenericToolCallRow` | `search` | |
| `ViewImage`, `ImageGeneration` | claude | `GenericToolCallRow` | `image` | |
| `Plan`, `ExitPlanMode` | claude | `GenericToolCallRow` | `plan` | |
| `Agent`, `Task` | claude | `GenericToolCallRow` + `SubagentGroup` | `agent` | New `AgentRow.svelte` extracted in Phase 2 |
| `collab_agent` | codex | `CollabToolRow` | `spawn` | |
| `send_input` | codex | `CollabToolRow` | `send` | |
| `wait_agent` | codex | `CollabToolRow` | `waiting` | |
| `close_agent` | codex | `CollabToolRow` | `closed` | |
| `resume_agent` | codex | `CollabToolRow` | `resume` | |
| `MCP/<name>` | both | `GenericToolCallRow` | `mcp` | Body: `server.tool(args)` |
| `AskUserQuestion` | claude | `AskUserQuestionCard` | `ask` | Card chrome; behavior unchanged |
| `request_user_input` | codex | `AskUserQuestionCard` | `ask` | Same |
| `TaskOutput` | claude | `GenericToolCallRow` (body suppressed) | `output` | Header-only |
| terminal_interaction | codex | `TerminalInteractionRow` | `terminal` | |
| `Thinking` | both | `ThinkingBlock` | `think` | |
| Proposed plan | both | `ProposedPlanCard` | (own card) | Card chrome restyle |
| anything else | both | `GenericToolCallRow` | `tool` | Generic fallback |

`TodoWrite` (Claude) and `update_plan` (Codex) are deliberately not in
this table — they don't render as timeline rows. They drive the
activity rail (`composer/ActivityRailTodosBody.svelte`).

## Rejected alternatives

Decisions we considered and didn't pick — captured so reviewers don't
relitigate.

| Considered | Rejected because |
|---|---|
| Subagent transcript opens in side panel / new thread view | Loses the at-a-glance "what is the subagent doing right now" affordance. Keep inline "latest tool" preview, restyled. |
| Multi-file edits as one row with stacked diff | Each file deserves its own row + diff for individual inspection; current `DiffFileStack` already does this. |
| Group consecutive tool rows in `MessageTimeline.svelte`'s `groupedNodes` to draw the rail | Adds a new `TimelineNode` variant, complicates row-index math + auto-load-older trigger, risks the row-contract stability rule. Per-row `border-l` with tight spacing yields the same visual outcome. |
| Render full diff inline (no cap) | Conflicts with the compact aesthetic and produces long virtua rows that fight `bufferSize`. Cap at 15 + side-panel CTA. |
| Phase labels on stamps (`planning · 9:42 PM`, `implementing`, `testing`) | No wire signal exists for phase; inferring from prose is fragile. Out of scope for this redesign. |
| Adopt JetBrains Mono | Geist Mono is already shipping and looks comparable. Avoids a web-font swap that ripples across composer / sidebar / settings. |
| Narrow to 880px | 992px gives long bash commands and paths room without aggressive truncation. The compactness goal is met by row density, not container width. |
| Use the design's exact icon palette | Cobalt-violet accent project; design uses warmer greens / ambers. Pick a project-aligned palette under new `--ico-*` tokens. |
| Adopt the design's prose treatment | Out of scope — this redesign is the tool-call surface only. |
| Force interactive cards (AskUserQuestion, ProposedPlan) into the single-row format | They have buttons, multi-select, plan checklists. Restyle their chrome; keep their structural shape. |
| Header-then-revealable-body for interactive cards | Two-click interaction for high-attention UI. Keep the cards' first-paint interactive. |

## Implementation constraints

Project rules that bound this work — pointers, not duplication.

- **`frontend/CLAUDE.md` § Raw-content rendering** — assistant prose
  goes through `ChatMarkdown` → `<Streamdown>`. Don't touch.
- **`frontend/src/lib/components/chat/CLAUDE.md` § Row contract** —
  every row inside `<Virtualizer>` keeps a stable outer shell. No
  late chevron insertion, no static-to-button swaps, no
  completion-time history appendages. Row state lives in per-pane
  registries (`pane.expansionStateFor`, `pane.attachmentCacheFor`,
  `pane.isSubagentGroupExpanded`), not local `let foo = $state(false)`.
  Payload bytes route through `utils/payloadDataCache.ts`.
- **`frontend/CLAUDE.md` § Scroll architecture** — `MessageTimeline`
  owns scroll, `useStickToBottom` owns intent. Don't touch. The rail
  per-row `border-l` approach is chosen specifically to avoid
  changing `groupedNodes` / row geometry.
- **`frontend/CLAUDE.md` § Anti-patterns** — no `.svelte` past ~300
  lines (currently `GenericToolCallRow.svelte` at 402 is over;
  `AskUserQuestionCard.svelte` at 408 is over). The
  `GenericToolCallRow` split into `+ AgentRow.svelte` resolves the
  largest violation.
- **`CLAUDE.md` (root) § Permanent invariants** — transport boundary
  stays clean; `.claude/` and `.playwright-mcp/` stay excluded from
  the dev watcher. Not relevant to this redesign but worth flagging
  if any helper script ends up running in dev mode.

## File-level deltas

### New files (Phase 1)

| Path | Purpose |
|---|---|
| `frontend/src/lib/components/chat/Indicator.svelte` | Text-free state dot. Props: `state: 'running' \| 'backgrounded' \| 'error' \| 'declined' \| null`. Renders nothing when `null` (success / idle). |
| `frontend/src/lib/components/chat/RowError.svelte` | Sub-line under errored / killed / declined rows. Props: `code?: string`, `msg: string`, `tone: 'error' \| 'declined'`. Padding-aligned to the body column. |
| `frontend/src/lib/components/chat/AgentRow.svelte` | Extracted from `GenericToolCallRow.svelte` — Claude `Agent` rendering + latest-tool inline preview. |
| `frontend/src/styles/tokens.css` additions | `--ico-bash`, `--ico-read`, `--ico-edit`, `--ico-write`, `--ico-grep`, `--ico-web`, `--ico-agent`, `--ico-think`, `--ico-mcp`, `--ico-terminal`, `--ico-notebook`. Project-aligned palette (cooler tones, harmonized with `--accent` cobalt-violet). |

### Existing files modified

| File | Phase | Change |
|---|---|---|
| `TranscriptDisclosureHeader.svelte` | 1 | Internal layout becomes `chev \| icon \| label-48px-gutter \| body-flex \| trailing-slot`. Slot API unchanged so existing callers still work. |
| `ToolHeaderMeta.svelte` | 1 | Status slot expects `Indicator` (or nothing). Duration + timestamp slots unchanged. |
| `ToolKindIcon.svelte` | 1 | Per-`kind` switch now emits matching color class via the new `--ico-*` tokens. |
| `MessageTimeline.svelte` | 1 | Per-row outer wrapper: tool / think rows get `border-l border-border-subtle ml-[14px] pl-[18px]` and shed `mb-1.5` in favor of internal `py-`. Assistant_text rows don't get the rail border and break the line. No grouping changes; row-shell stability rule preserved. |
| `CommandOutput.svelte` | 2 | Drop `Bash` label literal (gutter handles it). Drop `CompletionBadge`. Backgrounded `…` → `Indicator`. Inline `errorLine` → `RowError`. Lowercase `bash` gutter label. |
| `GenericToolCallRow.svelte` | 2 | Split. Claude `Agent` paths (`isClaudeAgent` branch, `subagentTranscriptEntries`, latest-tool inline preview, agent label / model / description helpers) move to `AgentRow.svelte`. Remaining covers Read / Grep / Glob / WebFetch / WebSearch / Plan / TaskOutput / MCP and stays under 250 lines. Drop running-text label; `Indicator` carries the state. Drop `CompletionBadge`. Convert label to lowercase gutter. |
| `ToolCallCard.svelte` | 2 | Add dispatch branch for `presentation.kind === 'agent'` → `AgentRow.svelte`. |
| `toolPresentation.ts` | 2 | Add `agent` presentation kind for `toolName === 'Agent'`. |
| `DiffFileBlock.svelte` | 2 | Label becomes lowercase category (`edit` / `write` / `patch`), not uppercase tool name. Stats slot moves to fixed `+N` / `-N` slot to the right of body. Fade gradient restyled to match design (accent on hover). |
| `inlineThreshold.ts` | 2 | `INLINE_DIFF_PREVIEW_LINE_COUNT` from `30` → `15`. |
| `ThinkingBlock.svelte` | 2 | `think` lowercase label in 48px gutter. Body stays italic + 3-line clamp. |
| `CollabToolRow.svelte` | 2 | Per-variant lowercase labels (`spawn` / `waiting` / `agent` / `closed`). Body holds agent name + meta-tag. Drop status text, use `Indicator`. |
| `TerminalInteractionRow.svelte` | 2 | `terminal` lowercase label. |
| `ToolResultCard.svelte` | 2 | Same shell treatment as `GenericToolCallRow`. |
| `AskUserQuestionCard.svelte` | 2 | Restyle chrome (borders, fonts, drop success badge). Behavior unchanged. |
| `ProposedPlanCard.svelte` | 2 | Restyle chrome only. |
| `BackgroundTaskTrayRow.svelte` | 2 | Tray surface re-uses the same renderers; verify `surface: 'tray'` modes still render correctly after `CompletionBadge` removal. |
| `toolCardHeader.ts` | 3 | `label` / `displayName` fields trimmed — gutter labels are now by *category* (`bash`, `read`, `edit`, `agent`, `mcp`). `icon` and `isSubagent` remain. |
| `subagentLaunch.ts`, `claudeSubagentLabel.ts`, `claudeSubagentTranscript.ts` | 3 | Audit; fold what only `AgentRow` consumes into `AgentRow.svelte`. |
| `commandDisplay.ts` | 3 | `commandErrorLineForItem` becomes a thin helper producing `{ code, msg }` for `RowError`. |
| `CompletionBadge.svelte` | 3 | Component stays for any non-tool surface that still uses it; if no callers remain after Phase 2, delete. |

## Phase 1 — Primitives and rail (no behavior change)

**Goal:** the screen looks ~70% redesigned (icons colored, labels
lowercase, no success pills, rail visible, indicators dot-only)
without changing what each row does. Tests for individual renderers
keep passing.

**Order of work:**

1. Add `--ico-*` color tokens to `tokens.css`.
2. Add `Indicator.svelte` + `RowError.svelte`.
3. Update `ToolKindIcon.svelte` to emit per-`kind` colors.
4. Reshape `TranscriptDisclosureHeader.svelte` internals; keep slot
   API.
5. Update `ToolHeaderMeta.svelte` status slot.
6. Add rail border + tight spacing to `TimelineLeaf.svelte`'s outer
   wrapper (the per-row outer in `MessageTimeline.svelte`) for tool /
   think rows.
7. Run `pnpm run check` + `pnpm test` + visual sweep against
   `design-mockup.html`.

**Risk to flag during implementation:** rail-border continuity needs
to play well with virtua row geometry. Each tool row is a separate
virtua row; the border-left paints per row. As long as we drop `mb-`
and use internal `py-` for breathing room, the line is continuous.
Verify in dev with multiple consecutive bash / read / edit rows —
both light and dark themes.

## Phase 2 — Per-renderer rewrites

**Goal:** each renderer ports onto the new primitives, with the
lowercase-gutter label / no-success-badge / `Indicator`-dot /
`RowError`-sub-line conventions applied uniformly. Each renderer ends
up smaller and easier to read.

**Order of work:**

1. `CommandOutput.svelte` — smallest surface, validates the pattern.
2. Split `GenericToolCallRow.svelte` → `GenericToolCallRow.svelte`
   (slim) + new `AgentRow.svelte`. Wire dispatch in
   `ToolCallCard.svelte` + `toolPresentation.ts`.
3. `DiffFileBlock.svelte` — restyle, move stats slot, change cap to
   15.
4. `ThinkingBlock.svelte` — label change only.
5. `CollabToolRow.svelte` — restyle.
6. `TerminalInteractionRow.svelte` — restyle.
7. `ToolResultCard.svelte` — restyle.
8. `AskUserQuestionCard.svelte`, `ProposedPlanCard.svelte` — chrome
   restyle.
9. Tray surface (`BackgroundTaskTrayRow.svelte`) verification.
10. Run `pnpm run check` + `pnpm test` + visual sweep.

Each PR-sized chunk in Phase 2 is one renderer with its own tests.
Sequence keeps the screen working between commits.

## Phase 3 — Cleanup

**Goal:** remove dead code, validate, run review.

1. Trim `toolCardHeader.ts` (drop now-unused `label` / `displayName`
   fields).
2. Audit subagent utilities (`subagentLaunch.ts`,
   `claudeSubagentLabel.ts`, `claudeSubagentTranscript.ts`); fold
   what only `AgentRow.svelte` consumes.
3. `commandDisplay.ts` → split into `{ code, msg }` shape for
   `RowError`.
4. Delete `CompletionBadge.svelte` if no callers remain.
5. Update tests that asserted on uppercase labels, `CompletionBadge`
   presence, success-badge text, etc.
6. Run `post-task-review` skill — parallel review across performance
   / memory, code quality, architecture, testing, security, codebase
   consistency.
7. Fix validated findings; surface architectural red flags for
   discussion before applying.
8. `make go-build`, `make go-test`, `pnpm run check`, `pnpm run
   build`, `pnpm test` all pass.

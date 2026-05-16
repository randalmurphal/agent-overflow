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

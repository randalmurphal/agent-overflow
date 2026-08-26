# frontend/

Svelte 5 + Vite 8 (Rolldown) + Tailwind 4 + TypeScript.

## Commands

- `pnpm run check` — Svelte + TypeScript type check. Must pass.
- `pnpm run build` — production build. Must pass.
- `pnpm test` — Vitest unit tests.

## Layout

- `src/lib/stores/` — runes-based reactive stores. `thread.svelte.ts`
  is the composition root for the per-thread `ThreadPane` factory: it
  owns the pane's `$state` fields, the timeline mutation chokepoints
  (`writeItemAt`, `commitTimelineItems`/`commitUpsertResult`,
  `replaceTimelineItems`, `dropTimelineItems` and the dispose/index
  bookkeeping behind them) and the returned API object, and composes
  everything else from its `thread*` sub-factory modules —
  `threadSwitchLoad.svelte.ts` (switch / window-sync / replica
  pipeline, including `switchThread` and `refreshFromBackend`),
  `threadItemStreamApply.ts` (the streaming upsert/delta/meta/patch
  machine), `threadTimelineWindow.svelte.ts`,
  `threadStreamingReveal.svelte.ts`, `threadSubagentMemory.ts`,
  `threadChannelState.svelte.ts`, …. Each takes a ctx object of getters
  and callbacks, so item-array assignment and revision bumps stay at
  the pane's own chokepoints; `events.ts` is a thin composition root
  that fans backend events
  out to the `events*` domain modules (`eventsItemStream.ts`,
  `eventsProvider.ts`, `eventsDiscussion.ts`, …); `bindings.ts` wraps
  generated Wails calls.
- `src/lib/components/panes/` — pane host/layout surfaces. This is the
  only place that should translate layout items into mounted chat panes.
- `src/lib/components/chat/` — timeline rendering. Kind-based
  discrimination; no role/content matching. See its local guide before
  editing rows, virtualized scrolling, markdown, or review/companion
  pane affordances.
- `src/lib/components/composer/` — message composer, mode / effort /
  model pickers. `ComposerInputSurface.svelte` is the reusable editing
  core (text entry, mentions, `/` commands, attachments) with two hosts:
  `Composer.svelte` and chat's `UserMessageEditor.svelte`. Everything
  send-, thread- or prompt-shaped stays with the host — the surface edits
  text, it does not decide what happens to it.
- `src/lib/components/sidebar/` — projects + thread list.
- `src/lib/components/primitives/` — reusable Menu / Popover / Modal /
  dropdown shells. Pickers compose these rather than rolling their own
  positioning, focus-trap, or keyboard behavior. Popovers fit, clip, and
  auto-close at the nearest `data-popover-clip-boundary` ancestor of
  their anchor (the pane strip declares it; fixed surfaces inside such a
  subtree opt out with `="none"`) — contract in
  `utils/popoverOwnership.ts`, math in `utils/popoverGeometry.ts`; a
  close's focus restore is reason-gated through
  `panes/paneComposerFocus.ts#restorePickerFocus`.
- `src/lib/components/workflows/` — the workflows overlay. See "Workflows
  Overlay" below before touching it.
- `src/lib/components/{accounts,design,discussion,git,palette,settings,terminal,usage,shared}/` —
  per-feature component groups. `accounts/` is the account-switcher picker;
  it and Settings → Providers both drive
  `stores/providerAccounts.svelte.ts` — the one load / login / switch /
  refresh / remove path — so neither surface owns account logic of its own.
- `src/lib/types/` — shared TypeScript types.
- `src/lib/utils/` — pure helpers.
- `src/lib/transport/` — WebSocket client + `@wailsio/runtime` shim.
  Feature code should go through `stores/bindings.ts`, not this package.
- `src/lib/harness/` — the agent-harness UI bridge: the semantic viewport
  snapshot, the element/globals probes and the in-page perf meters
  (`docs/architecture/agent-harness.md` § Frontend bridge and perf).
  Reached ONLY through a dynamic import in `stores/harnessBridge.ts`,
  which arms on the `harness` bootstrap flag — so an ordinary boot never
  fetches the chunk, and nothing in here may be statically imported from
  app code. It has no wire surface by design: the subscription and the
  reply RPC live in the store, because `architecture.test.ts` rule 2
  keeps event subscriptions inside `stores/`.
- `bindings/` — Wails-generated TypeScript. Never edit by hand.
- `vendor/` — in-repo third-party packages, linked in as pnpm workspace
  packages. Currently `svelte-streamdown`; edit it like any other source
  in the tree, but read its `VENDOR.md` and `DIVERGENCE.md` first.

## State Boundaries

`ThreadPane` is the sole owner of per-thread runtime UI state: visible
items, streaming flags, approvals, design artifacts, channel messages,
token usage, checkpoint bookkeeping, terminal placement, and scroll
controller registration. Companion pane layout/open state lives in the
pane-layout/companion stores. Do not add a parallel streaming or timeline
state slice next to it.

Pane layout and pane runtime state are separate. Layout stores own
placement/order/min-size metadata; `ThreadPane` owns the chat/discussion
runtime state mounted into that slot. Command palette actions must resolve
against an explicit target pane because enablement can change while the
palette is open.

The visible-thread memory budget is load-bearing. Heavy payloads (diffs,
command output, thinking, attachments) load on demand through bindings.
Thread switch is a bounded-window load or cache restore, not a full-history
hydrate. Settled subagent children are evicted from pane memory into a
per-anchor fold (`utils/subagentFold.ts`) and re-hydrate on card
expansion; see "Live Window Bounds" in
[`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md).

### State ownership

State is keyed by its ENTITY, never by its consumer. Before adding
`$state`, name the entity the value describes:

- **app** — chat-bar favorites, Codex MCP rows (that backend flag is
  global), settings, provider accounts.
- **project** — new-thread defaults.
- **workspace**, i.e. the worktree cwd — git status, branch, dirty bit,
  the open-PR reference, the Claude MCP LISTING (membership is walked
  from the cwd out — `.mcp.json` files plus plugin manifests — so two
  worktrees of one project can legitimately differ). The Claude toggle
  that listing renders is project-scoped and Codex's is global, so a
  toggle changes what a SIBLING workspace should show; that fan-out is
  a push (the `mcp:status` sentinel re-lists every key carrying the
  server), not a reason to widen the key.
  Also the workspace-change lock, keyed by DIRECTORY and answering two
  questions off one fetch. `locked` is the directory view: removing a
  worktree or creating a branch in place mutates a checkout every thread
  in it shares, so any busy thread locks it (open turns and live
  background tasks, aggregated backend-side over every thread that
  references the path, matching the refusal `removeProjectWorktree`
  issues). `threadLocked` is the thread view: the env picker and the
  new-worktree confirm MOVE the pane's thread to another checkout, which
  rewrites only that thread's row, so only that thread's own activity
  locks it (from the same payload's `busyThreads`, matching the backend's
  `ensureWorkspaceChangeAllowed(threadID)`). Gating a move on the
  directory view pinned every idle thread at the project root while any
  sibling responded. A pane's OWN turn is read from local state on top of
  both, purely so the lock closes without waiting for an event round
  trip.
- **PR** — detail, review threads, live head SHA, CI pipeline, merge
  conflicts.
- **thread** — items, streaming, approvals.
- **pane** — view concerns and nothing else: scroll, expansion,
  selection, drafts, and the head a loaded diff was computed AT (PR
  staleness is derived from that against the store's live head; a shared
  `stale` flag would lie to one of two panes).

If the entity outlives or spans the component, it lives in a shared
entity-keyed store. There are TWO primitives for that and the choice is
not stylistic — it follows from what backs the key:

- **`stores/entityStore.svelte.ts`** — the key is backed by a BACKEND
  RESOURCE that has to be acquired, released, and re-acquired across a
  transport reconnect (a subscription, a watcher, a poll pump, an RPC
  whose answer goes stale). Refcounted `attach`, one `apply` chokepoint,
  a retry curve, and the transport edge come with it. Reference
  migrations: `gitStatusStore.svelte.ts`, `prReviewStore.svelte.ts`,
  `mcpServers.svelte.ts`, `chatBarFavorites.svelte.ts`.
- **`stores/keyedSignalRegistry.svelte.ts`** — the key is PUSH-FED or a
  lazy per-key cache with nothing to acquire: events arrive and are
  written, and a reader of key K must not be woken by a write to key J.
  One `$state.raw` box per key, no refcount, no source, no transport
  edge. Users: `threadStatuses`, `sendQueue`, `compactingState`,
  `fastModeState`, `claudeSkills`, `codexSkills`, `providerCommands`,
  `worktreeSetup`, and the thread pane's per-row item boxes behind
  `pane.getItemById` (`pane.items` itself is `$state.raw`; a row's
  reader wakes only when its own row is written — see the `items`
  declaration in `thread.svelte.ts`). The split is load-bearing: a
  reactive scope takes MEMBERSHIP and order from `pane.items` and reads
  a row's FIELDS (status, summary, meta) through `getItemById`. A field
  patch is written in place and the array signal stays silent for it, so
  a scope that scans the array and then reads `.status` off the hit never
  wakes (the nav-rail hover preview and the agent-scope turn pill both did
  this before 2026-08-23; `agentScopeView.svelte.test.ts` pins the fix).

The deciding question is "is there something to release?", not "is it
keyed?". Both module headers carry the full rationale — read the one you
are about to build on.

Every consumer `$derived`s from whichever store owns the entity. What
the entity-store primitive requires:

- `apply()` is the single write chokepoint. Event push, RPC result and
  post-action refresh all land there, so reconciliation (persisting an
  observed branch) happens once and every consumer heals together. Do not
  add a `set()` beside it.
- `attach()` is refcounted per key and the last release tears the backend
  resource down. The transport edge is the primitive's — it suspends on
  disconnect and `resetAll()`s on reconnect for every store built on it, so
  a store neither re-subscribes per consumer nor wires the edge itself.
- Key an attach `$effect` on the ENTITY KEY alone, never on the consumer's
  id. A pane switching threads inside one workspace is still the same
  workspace: tracking the thread id there released and re-attached, dropping
  the shared resource to refcount zero — the fs watcher torn down and rebuilt
  for a change the entity never saw. Values the source needs but the key does
  not (a thread id for the subscribe RPC) go in the ctx as GETTERS, so a
  re-source runs against what the consumer holds now.
- A `source()` run gets an `AbortSignal` that fires the moment it is
  superseded (release, invalidate, suspend, resetAll, retry). `apply`/`fail`
  from a superseded run are dropped for free, so a single-RPC source can
  ignore it — but anything a source does AFTER an await must check it, or a
  superseded run doubles the side effect (mcpServers chains a health check
  that spawns `claude mcp list`).
- Wire events are entity-keyed, never subscription-keyed. See
  [`internal/transport/AGENTS.md`](../internal/transport/AGENTS.md) §
  "Events Are Entity-Keyed".

One thread is mounted in at most one pane, and that is structural rather
than a habit of the callers: `stores/panes.svelte.ts` keeps
`replaceThreadInPane` private and exports `mountThreadInPane` as the only
door into the registry. It reveals the pane already showing the thread
before mounting anything, and a duplicate reaching the registry through
either door — mount or layout restore — is reported to the console AND to
`reportFrontendDiagnostic`, so a breach in the field lands in
`ui-trace/frontend-errors.jsonl` instead of needing devtools open. Two panes
on one WORKSPACE are common and first-class; two panes on one THREAD are a
bug.

`src/lib/architecture.test.ts` enforces the mechanical half: entity-owned
RPCs stay inside `stores/`, and `wailsEventOn` does not appear outside it.
Both rules carry shrink-only allowlists — an exception that has been fixed
must be deleted from the list, not left to grandfather the next one.

`src/lib/themeTokens.test.ts` enforces the token layer the same way and
with the same allowlist discipline. Its rules cover the whole of `src/`:
no raw Tailwind palette classes (the blue-500 text scale and kin), no raw
black/white utilities (a black scrim at 45%), no default Tailwind shadow
scale (the small/medium/large sizes), no arbitrary-value color functions,
no dead token utilities, and no hex literals outside the theme layer
itself. (Class names are deliberately not spelled verbatim here:
Tailwind's source scanner reads this file and compiles a quoted utility
into the production bundle as a dead rule.) Its raw-class
allowlist is EMPTY, which is the claim that every leak is closed. The fix for a failure is adding or using a TOKEN — never
extending the list; if the role you need does not exist yet, define it
(see [`docs/specs/theme-system.md`](../docs/specs/theme-system.md) for
the vocabulary and the file-placement rule: app.css owns colors,
`styles/tokens.css` owns non-color scales and derived roles). Both
directions are checked, so an entry that no longer offends fails too —
delete it when you fix its file.

## Workflows Overlay

Spec: [`docs/specs/workflows-system-ui/UI-SPEC.md`](../docs/specs/workflows-system-ui/UI-SPEC.md)
(§12 maps every section to the file that implements it).

The overlay is a **sibling of `<PaneHost>`** in `App.svelte`, mounted through
`LazyOverlay`. It is never a pane kind and never a surface that replaces the
pane strip: the pane tree stays mounted underneath, so opening and closing
rebuild nothing. Opening a thread is the only action that leaves the overlay,
and it closes it.

Settings mounts the same way, through the same frame primitive
(`primitives/OverlayShell.svelte` — scrim, card, focus trap; Esc is
keybinding-driven, never handled inside the shell). The two are mutually
exclusive, and both directions live in the stores: `openSettingsOverlay`
calls `closeWorkflowsOverlay`, and `openWorkflowsOverlay` — the one writer
of the workflows store's `open` — runs a closer that
`stores/settingsOverlay.svelte.ts` arms at MODULE INIT via
`setWorkflowsOverlayExclusion`. Importing the settings store is the whole
wiring; nothing has to be registered in a particular order, and no test
reset disarms it. That closer is also the one settings-close path (X
button, scrim, `settings.close`, workflows opening), so its
blur-before-unmount — settings fields commit on blur — cannot be
bypassed; it early-returns when settings is already closed so a workflows
open never steals focus from elsewhere.

State ownership:

- `stores/settingsOverlay.svelte.ts` — settings open state and last-used
  section. Deep links (`ContextWindowMeter`, `/config`, the account
  switcher) call `openSettingsOverlay` directly rather than routing a
  window event through App.
- `stores/workflowsOverlay.svelte.ts` — navigation only (stack, project
  filter, sweep cursor, armed confirm, open dialog). Persisted through
  `appStorage`, so it survives a restart; `open` deliberately is not.
  It also holds the run map's expansion state, per run id and LRU-bounded:
  THREE sets — waves, compositions, lanes — because the three things a
  reader can open are keyed differently (`waveItemId`, a called run's
  `itemId`, a branch key) and one merged set would let a lane's key open a
  wave. They survive a detail remount (navigating away and back is not a
  decision to re-fold) but nothing narrower does: which done group or which
  cause a reader opened is per-visit component state, deliberately not
  lifted here.
- `stores/workflowRuns.svelte.ts` — the reactive cache (runs, catalogs,
  automations, costs, the focused run's detail, session receipts). Run detail
  is ROOT-ONLY: it is loaded for the one run the overlay is looking at and
  dropped the moment it looks at another, so overlay memory stays bounded by
  what is on screen. Nothing here walks a run's children — a run's SHAPE
  belongs to the run-map store below. `retainWorkflowDetails` runs inside an
  `$effect` that reads the same cache: it must not write when nothing is
  dropped, or the effect re-enters forever, so its guard is "is the cache
  already exactly the root", never a size comparison.
- `stores/workflowRunMap.svelte.ts` (+ `stores/workflowRunMapPatch.ts`) — the
  run detail's structure surface, and the workflows area's `createEntityStore`
  exemplar: keyed by THE ID THE OVERLAY ASKED FOR (the nav-stack run id, which
  is what a caller can state before any answer exists — the tree root is
  resolved server-side), while the answer covers that run's whole tree
  whichever member was named. `source()` is one `WorkflowGetRunMap`, `apply()`
  is the single write chokepoint that the `workflow:phase-state` /
  `item-state` / `soft-stop` patchers land on, and any event the patcher cannot
  place precisely falls back to `invalidate` rather than guessing. Patches are
  an optimization; correctness is the refetch — a patch that lands but cannot
  be COMPLETE (a `running` frame, whose thread the runner attaches after the
  emit with no event of its own) still schedules the debounced refetch behind
  itself. Values are `rawValue` entries: a run tree is replaced wholesale on
  every write, so deep proxying it would walk thousands of objects to buy
  per-field tracking nothing subscribes to. Read
  `stores/entityStore.svelte.ts`'s header before touching it.
- `stores/workflowData.ts` and `utils/workflow*.ts` — pure. Grouping, sweep
  math, action rows, the run-map projection (`workflowRunMap`, `…Index`,
  `…Frontier`, `…Types`, `…Style`), signal mapping, intake validation and
  envelope reads live there so they are testable without a Svelte runtime.
  Duration/countdown formatting is `utils/format.ts` — a formatter is not a
  store concern, and the map's pure modules must not import upward into
  `stores/` for one. Keep new logic in these, not in components: the map's
  components render a model, they never derive one — `buildRunMap` is ONE walk
  per tick for the whole surface (expansion sets go in, `wave.segments` comes
  out), and `runMapPosition` is the one narrow read, for the header's label.
  A wave's OPENNESS is the model's too: `segments === null` is what "closed"
  means, read through `runMapWaveIsOpen`. Do not add a second `open` flag
  beside it — a prop or a field that can disagree renders "Nothing recorded in
  this wave yet." over a wave full of records. The same convention runs all
  the way down: a collapsed composition's laps carry `segments: null` and a
  folded fan lane carries `chain: []`. Collapsed means NOT BUILT, everywhere.
  What decides it is never depth — it is whether the node is on the frontier
  path (RUN-MAP §1, §3): only the live path is open, at every level. The
  reader's clicks are the other opener, and a click answers with CONTENT,
  not another fold (RUN-MAP §6): a lane's sole child composition arrives
  open with the lane click (`merged`, guarded — never for a failed child or
  under an actionable lane), and an opened settled multi-lap chain defaults
  its FINAL lap — the tail leaf — open, with a click inverting any settled
  lap's default. One PRESENTATION fact rides the walk, not depth: a fan
  renders columns until the walk enters a lane (`RunMapFan.layout`); every
  fan inside one stacks, and labels on this surface wrap — CSS ellipsis is
  banned.
  `utils/workflowRunMapStyle.ts` is the map's presentation vocabulary and is
  deliberately consumed OFF the map too (the evidence block's checks strip): a
  completed check and a completed node are the same statement, and a second
  glyph/tone table starts identical and drifts on the first tuning change.
- `stores/workflowResolve.ts` — the ONE resolution path (dispatch → receipt →
  toast → sweep auto-advance). The action row and the discard dialog both go
  through it; do not add a second.
- `components/workflows/overlayScroller.ts` — the run map writes `scrollTop` on
  a scroller it does not own (RUN-MAP §9.9: one scroller serves every level of
  the overlay). `WorkflowsOverlay.svelte` provides it through context and the
  map REQUIRES it, throwing when absent. Do not replace that with a walk up the
  DOM for something that happens to scroll: the walk picks up whatever
  `overflow-y` a future wrapper introduces, and answers `null` — silently
  disabling placement, follow, jump and compensation at once — in exactly the
  case that should be loud. `WorkflowRunMapFold.svelte` requires the same
  scroller for a related reason: §9.8's "an off-screen fold applies instantly"
  is a question about the OVERLAY's viewport, not the window's.

  Same rule inside the controller: `attach()` never returns a dead
  installation. A scroller the getter cannot answer for yet is retried for a
  few frames and then LATCHES the controller shut — no writes, disengaged, no
  chip — before it throws. The write chokepoint reaches the element through
  that same getter whether or not a listener was ever installed, so a throw
  that changed no state left follow able to glide with nothing listening: that
  is follow running with no way for the reader to escape it (RUN-MAP §9.2),
  not a missing feature. A later successful attach clears the latch.

Rules that are not stylistic:

- **R1, two hues.** Amber (`--warning`) means a human is blocked; red
  (`--error`) means failed. Everything else is neutral, including a done run
  awaiting disposition. `utils/workflowRunSignal.ts` is the only place that
  decides — run state, node signal and their tones — and
  `utils/workflowRunMapStyle.ts` is the map's vocabulary over it (glyph,
  glyph tone, border, fill, glow, spinner). Never inline a colour in a
  component. Two amended clarity hints (RUN-MAP §13 fourth pass,
  user-approved 2026-08-15) sit on top without touching the attention hues:
  the done GLYPH is `--success` — the glyph only, never the label, an
  exactly-one rule pinned in `workflowRunMapStyle.test.ts` — and the run
  map's `now ▸` marker plus its row tint are the surface's one `--accent`
  use, marking POSITION, not status.
- **R2, no internals.** No envelopes, JSON, schemas, gate traces or the word
  "variables" on any surface. A workflow's typed inputs render as plain form
  fields named after the field.
- **§10, remote posture.** Every mutating affordance disables with a
  `Local only` title in a view-only session (`isViewOnlySession()`), and the
  §8 key target refuses too — the guard is not decoration.
- **§4.5, preview is consent.** Discard opens the loss preview; nothing
  destructive fires from a row. Other destructive actions arm a confirm that
  Esc disarms. `WorkflowDiscardDialog.svelte` owns its own rows (per-row
  wording in `utils/workflowLoss.ts`) and the sidebar's project-delete dialog
  owns its own (`utils/projectCleanup.ts`) — deliberately unshared. Discard
  deletes branches, so its rows describe a loss; project deletion (D25) is
  cleanup that keeps every branch, so its dialog describes what will be removed
  and which checkouts git will leave behind. One renderer serving both would
  have to lie to one of them.

## Thread Switch And Scroll

The durable contracts for cache restore, tail-only initial load, lazy
older paging, scroll intent, the windowing engine, and scroll-regression
diagnostics live in
[`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md).
Read that before touching:

- `src/lib/stores/thread.svelte.ts` +
  `src/lib/stores/threadSwitchLoad.svelte.ts` (the switch / sync /
  replica pipeline itself)
- `src/lib/stores/threadItemCache.ts`
- `src/lib/stores/threadScrollSnapshots.ts`
- `src/lib/components/chat/MessageTimeline.svelte`
- `src/lib/components/chat/timeline{Restore,SizePriors,WindowAnchor,RowProjection}.svelte.ts`
  and `timeline{Paging,Diagnostics,RowUiPrune}.ts` (the scroll-session
  modules extracted from MessageTimeline)
- `src/lib/components/virtual/TimelineVirtualizer.svelte`
- `src/lib/components/discussion/ChannelView.svelte`
- `src/lib/components/chat/ActivityRun.svelte` + `utils/activityRun*.ts`
  (the one nested scroller running the pane's physics; see
  [`docs/architecture/activity-runs.md`](../docs/architecture/activity-runs.md))
- `src/lib/utils/scroll/` (`index.svelte.ts` controller + resolver/intent/spring/observers)
- `src/lib/utils/virtual/` (windowing engine + per-thread size priors)

Short version: `MessageTimeline` owns the scroll container, the bespoke
virtualizer (`TimelineVirtualizer.svelte` over the pure engine in
`utils/virtual/`) owns row geometry and never writes `scrollTop`, and
the scroll controller (`utils/scroll/`) owns scroll intent and every
programmatic `scrollTop` write — inside the package the pure resolver
decides, the controller's `writeScrollTop` chokepoint writes. The
virtualizer's `scrollToIndex`, compensation observations, and
content-geometry samples all route through the controller
(`applyScrollTarget` / `applyEngineCompensation` /
`deliverContentGeometry` — chat runs no contentEl ResizeObserver);
`scrollToIndex` is instant-only by design.

## Rendering

Raw content is canonical. Go sends raw item summaries, channel message
content, and payload data; the frontend renders them as viewport-local
projections. Do not add server-rendered chat HTML or a global DOM
observer.

Assistant text, discussion messages, and proposed plans render through
`ChatMarkdown.svelte` and `svelte-streamdown` — which is vendored in
`vendor/svelte-streamdown/`, not installed from npm (see
[`vendor/svelte-streamdown/DIVERGENCE.md`](vendor/svelte-streamdown/DIVERGENCE.md)).
Path linkification happens
inside marked parsing using server-validated `PathRef[]` metadata and a
per-page-load nonce; explicit markdown-link hrefs are additionally
rewritten into the same editor scheme on workspace-carrying surfaces
only (click-time gate: `editor.ResolvePath`); click/copy behavior is
delegated by `markdownEnhance.ts`.

ANSI-like payloads render through `AnsiText.svelte`, which diffs into a
stable `<pre>` with Idiomorph so selection survives streaming updates.

## Theme System

Spec: [`docs/specs/theme-system.md`](../docs/specs/theme-system.md)
(§7 is the contract, §9 records the as-built deviations). Two
independent axes — a UI theme and a code theme — over a light/dark mode,
selected per CLIENT and applied entirely in the frontend. Go never
parses a theme file; it lists `<configDir>/themes/*.json` as opaque raw
strings, owns `appearance.json`, and watches the directory.

Where things live:

- `lib/theme/tokenRegistry.ts` is THE token vocabulary — section, JSON
  key, CSS var, axis, description. `lib/theme/tokenRegistry.test.ts`
  parses the three stylesheets and fails on drift in either direction
  (`themeTokens.test.ts` is the separate leak tripwire: raw classes and
  hex literals outside the theme layer). A role that does not exist yet
  gets DEFINED here; it never becomes a literal at a call site.
- `lib/theme/` is pure and RPC-free: parse (structural only — whether a
  value is a color needs a browser), resolve (selection + files +
  built-ins → declarations + palette identity + warnings), apply.
- `lib/stores/appearance.svelte.ts` owns all three theme RPCs and the
  reactive selection. It degrades along THREE independent facts, not one
  flag, because they fail independently and only two of them are
  structural:
  - `readAvailable` (`isThemeDirectoryAvailable()`) latches false only on
    `method_not_found` from `GetThemeFiles` — a session with no themes
    directory at all, which is a posture to render, not an error.
  - `writesRefused` (`isAppearanceWritable()`) latches on a refused
    `SetAppearance` or a refused read, and a view-only session is
    write-blocked up front. A write-blocked session still TAKES the
    wire's themes, directory and warnings — it just never adopts the
    wire's SELECTION, which stays local (`localStorage`). The theme is a
    property of the client machine; a remote browser must not be
    repainted by whoever is at the desktop.
  - `loaded` / `loadError` (`getAppearanceLoadError()`) is the transient
    lane: a failed read surfaces, keeps writes enabled, and keeps the
    themes already loaded. Nothing latches here.

Rules that are not stylistic:

- **The mode-class stamp and the applier are both `$effect.pre`, in that
  order — class first, then the style rewrite.** Svelte flushes every
  render effect before any user effect, and the resolved MODE is a
  resolver INPUT, so splitting them across the two passes leaves the whole
  render pass reading a palette that does not match the class on `<html>`.
  Cascade-READING work (the window-ground probe, the boot stamp, the
  native-window RPC) goes in a plain `$effect` after them. §9.1 has the
  full ordering story.
- **`lib/theme/` being "pure" stops at `applyTheme`.** Beyond rewriting
  the style element it records what landed — css text, warnings, resolved
  refs, palette identity — into module-level `$state.raw`, read back
  through `getAppliedTheme()` / `getThemePaletteIdentity()`. That is
  app-global reactive state, and it is what Settings → Appearance renders
  the per-token rejections from and what every palette-keyed cache
  invalidates on. `applyTheme` is its ONE writer; it is not a function to
  call speculatively to "check" a resolution.
- **One `<style id="user-theme">`, rewritten wholesale.** Never
  `setProperty` per token on the root: each such write is a whole-document
  style invalidation (~13ms at 5k nodes, ~90ms at 30k), and a theme
  carries up to 85 tokens. A token the theme does not mention keeps the
  app's own declaration by cascade, so there is no reset value to emit.
- **One palette identity.** Anything that caches rendered output keyed on
  the palette — the mermaid config memo and its `{#key}`, the xterm
  bridge — widens `getThemePaletteIdentity()` (`uiTheme|codeTheme|revision`)
  and pairs it with the resolved mode itself. A second key that can
  disagree means either a stale render or a remount on every tick.
- **A bad value costs one token, never the theme.** Three layers each
  answer the question they can: the parser caps length and shape, the
  resolver refuses anything a declaration cannot carry, and the applier
  runs `CSS.supports('color', …)` per token. Every rejection is a
  user-facing warning with the theme id and the path, rendered in
  Settings → Appearance.
- **Values reach non-CSS consumers through `utils/cssColorProbe.ts`.**
  `getComputedStyle` serializes in the DECLARED color space, so the
  palette comes back as `oklch()`/`oklab()`; the canvas readback is what
  produces something xterm's or mermaid's parser can read. Do not hand a
  raw token value to either.

Tests: pure core under `lib/theme/*.test.ts`; anything needing a real
cascade, canvas, or `CSS.supports` is a `*.browser.test.ts`
(`themeApply.browser.test.ts`, `terminalTheme.browser.test.ts`) because
happy-dom answers `false` to every color probe and would assert on code
paths that never run in production.

## Anti-Patterns

- Do NOT create legacy stores. Runes only: `$state`, `$derived`,
  `$effect`, `$props`. No `export let`, no `$:`.
- Do NOT discriminate timeline items by role or content substring.
  Discriminate via `kind`.
- Do NOT re-order items during render. Upsert by `(turnIndex, itemIndex)`
  and let the store stay sorted.
- Do NOT implement count-based slicing for virtualization. Heavy content
  is expand-to-load, not preload.
- Do NOT stretch a `.svelte` file past roughly 300 lines when a clear
  component split exists.
- Do NOT put business logic in templates. Derive in `<script>`, render in
  the template.
- Do NOT call `window.runtime` directly. Use `stores/bindings.ts`.
- Do NOT preload heavy payloads.
- Do NOT statically import conditional feature surfaces from eager code.
  Settings, review/plan/design companion panels, discussion mode, the
  terminal surfaces, and the git/usage overlays all mount lazily
  (`{#await import(...)}` at replace-surface branches such as a pane
  body;
  `primitives/LazyOverlay.svelte` for modals/drawers with exit
  transitions) so their chunks stay out of the startup graph. One static
  import from an eagerly-loaded module silently drags the whole feature
  chunk back into startup — check `dist/index.html`'s modulepreload list
  when touching these boundaries. An `{#await}` input must be a promise
  with STABLE identity — never a reactive expression that constructs one
  (`{#await loaders[kind]()}`): the block re-runs on ANY dependency
  invalidation with no promise-identity cutoff, so unrelated churn (e.g.
  per-frame layout-item replacement during a divider drag) re-pends the
  block and remounts the mounted surface. Capture the promise once at
  init (see `CompanionPane.svelte`); regression:
  `PaneHost.test.ts` "divider width churn".
- Do NOT add visible in-app explanatory text for internal mechanics,
  shortcuts, or implementation details.

## Testing

Store logic: unit-test with Vitest under `src/lib/stores/*.test.ts`.
Component behavior: add a component test when changing rendering or
interaction. Scroll behavior has dedicated coverage in
`src/lib/utils/scroll/index.svelte.test.ts` (controller
choreography), `src/lib/utils/scroll/resolver.test.ts` (the pure
decision core, exhaustive over its state × observation matrix),
`src/lib/utils/scroll/scrollInterleavings.test.ts` (viewport ops ×
starting states, frame-level physics invariants across the drain), and
`src/lib/components/chat/scroll.test.ts`.

`vi.mock` a shared store with an `importOriginal` spread, never a
whole-module factory. A factory that lists only the exports a test drives
turns every LATER export of that module into `undefined` for it, and the
failure lands in an unrelated file (adding `isMethodUnavailableError` to
`transportStatus.svelte.ts` broke five suites that only wanted to pin
`getTransportStatus`).

`vi.mock` does not reliably reach importers that are `.svelte.ts`
modules: a mocked dependency was observed replacing the binding seen by
plain `.ts` importers (`timelineQuietWork.ts`) while `.svelte.ts`
importers of the same module (`threadActivityRuns.svelte.ts`) silently
kept the real one — the mock is ignored with no error, and the assertion
fails with no indication why. Until the vitest/vite-plugin-svelte
transform-order root cause is run down, don't mock a dependency of a
`.svelte.ts` module; assert one layer deeper against the real pipeline
instead (e.g. the diagnostics guards assert via
`setBindingMock('ReportFrontendErrorBatch', …)` rather than mocking
`utils/frontendErrorCapture`).

Operator-run investigation drivers are named `*.manual.ts` and collected only
by the `manual` vitest project (`pnpm test:manual`), never by a gate:
`markdown/freezeReplay.manual.ts` replays a recorded streaming corpus through
the production markdown path (`markdown/freezeReplayHarness.ts` is the reusable
rig), and a genuine finding WEDGES the worker rather than failing. Its corpus is
a real-session capture that is gitignored and must never be committed; generate
one locally with `scripts/generate-freeze-replay-fixture.mjs`, and the driver
skips its corpus suites when none is present.

`pnpm run check` and `pnpm run build` are blockers. `pnpm test` is the
frontend unit-test gate.

## Vendor Patches

Third-party divergence lives in one of two places, and which one is a
decision about the upstream, not about the change:

- `patches/` holds pnpm patches, keyed to exact versions in
  `pnpm-workspace.yaml` (`patchedDependencies`) — a version bump without
  re-rolling the patch fails `pnpm install`. Use
  `pnpm patch <pkg>@<version> --edit-dir <dir>` + `pnpm patch-commit`.
  This is right when the upstream is alive: the patch is small, it
  re-rolls cheaply, and it is meant to be dropped when the fix lands.
- `vendor/` holds in-repo packages, wired in as pnpm workspace packages
  (`packages: [vendor/*]` + `"workspace:*"`). This is right when the
  upstream is dormant and the divergence is permanent — a patch you will
  re-roll by hand forever is pure overhead. Each vendored package carries
  a `VENDOR.md` (upstream URL, baseline version, how to diff a future
  release) and its divergence is documented as a ledger below.

Never edit `node_modules` directly; packages are hardlinked from the pnpm
store, so direct edits corrupt every project on the machine. That is also
why vendored packages are `workspace:` and never `file:` — pnpm resolves a
`file:` directory through the same store and hardlinks the tree in, so the
`vendor/` files and the ones the build reads would share inodes.

- `svelte@5.56.8.patch` — five hunks with different drop rules:
  1. **ownerless-roots** — `$effect.root` no longer inherits the
     creating component's context/parent, so store-level roots
     (threadRowUiState's expansion registry) don't pin dead row
     instances. Deliberate divergence, no upstream issue — carry it
     forward and re-evaluate on every version bump. Regression suite:
     `svelte-patch-ownerless-roots.test.ts`.
  2. **event-slot-release** — svelte's delegated-event dispatcher pins
     every event in a module slot (`last_propagated_event`, a Firefox
     GC workaround) and never clears it, so `event.target` anchors the
     last-clicked component's detached subtree — a whole closed pane —
     until the next delegated event. The hunk schedules a macrotask
     clear after each dispatch (strictly after propagation settles, so
     the Firefox window is preserved). Upstream PR
     [#18569](https://github.com/sveltejs/svelte/pull/18569) (open) is
     this hunk verbatim. Drop when
     `svelte-patch-event-slot.test.ts` passes on an unpatched release;
     `chatview-dom-retention.test.ts` also relies on the clear.
  3. **destroy-pass-errors** — a throwing user `$effect` teardown aborts
     the sibling-destroy loop (keyed `{#each}` reconcile, branch
     teardown, unmount), so the effects still queued for destruction stay
     subscribed and retain detached DOM for the parent's lifetime.
     Teardown errors are collected into an array threaded through the
     destroy call chain; `destroy_effect`'s single catch site pushes and
     completes all structural cleanup, and the entry point that created
     the array rethrows the first error in a `try`/`finally` once its
     pass ends (`flush_destroy_errors(errors, completed)`). Touches
     `reactivity/{effects,deriveds}.js` and
     `dom/blocks/{boundary,branches,each}.js` +
     `dom/elements/attributes.js`. Upstream PR
     [#18566](https://github.com/sveltejs/svelte/pull/18566) (open) for
     [#18415](https://github.com/sveltejs/svelte/issues/18415); this
     hunk is that PR's source diff. Drop when
     `svelte-patch-destroy-pass.test.ts` passes on an unpatched release.
  4. **flush-loop-caps** — svelte's two synchronous flush loops are
     unbounded, and a cycle in either one is an unreportable renderer
     freeze: one core pegged, no paint, no error, nothing in any log
     (incident 2026-08-07 — WebView2 wedged 8+ minutes in a single
     never-completing task). `flushSync`'s outer
     `while (true) { flush_tasks(); …; batch.flush(); }` is invisible to
     `Batch.#process`'s own `flush_count > 1000` guard, because
     `Batch.flush`'s `finally` resets the count — so a cycle producing
     exactly ONE batch per lap (an `$effect` whose re-dirtying write is
     deferred into a `queue_micro_task`, which is what nine streaming hot
     paths' `tick()` looks like under load) never accumulates a count.
     `flush_tasks`'s `while (micro_tasks.length > 0)` drain has no counter
     at all, and while `is_flushing_sync` `queue_micro_task` appends
     straight to that array, so a self-re-queueing task never lets it
     finish. The hunk caps the first at 1000 laps and the second at 50k
     tasks per drain (both ~3 orders of magnitude above anything real —
     tripping one is a bug report, not a tuning knob) and ABORTS: a
     wedged flush must not keep spinning. The error is svelte-shaped
     (`errors.js#effect_update_depth_exceeded`'s construction, same
     `name`) and names the loop, so `utils/frontendErrorCapture.ts`
     persists it to `ui-trace/frontend-errors.jsonl`; unlike svelte's own
     errors it keeps its message in production, because there is no
     `svelte.dev/e/` page to look it up in. The `flush_tasks` cap
     abandons the queue before throwing — the queue IS the cycle, so
     leaving it would re-wedge the very next drain. Third piece, same
     concern: `invoke_error_boundary` returned SILENTLY when the anchor
     effect was DESTROYED, so even when `infinite_loop_guard()` fired its
     error could vanish and `#process` carried on. It now reports whether
     a boundary took the error and the guard rethrows when none did;
     ordinary error-boundary semantics are untouched (every pre-existing
     caller ignores the return value). Touches
     `flush-caps.js` (new), `dom/task.js`, `reactivity/batch.js` and
     `error-handling.js`. No upstream issue filed — all three pieces are
     upstream-PR candidates. Drop when
     `svelte-patch-flush-caps.test.ts` passes on an unpatched release.
  5. **reconnect-dedupe** — `get()` on a derived that was DISCONNECTED
     (lost its last reaction earlier), has run before, and is dirty
     registers it twice in one call: `update_derived` runs with
     `CONNECTED` pre-set, so `update_reaction` pushes the derived into
     every dep past `skipped_deps` (the deps new or re-ordered that run),
     and then `reconnect()` pushes it into EVERY dep again. A dep that was
     new that run holds the derived twice; when the derived later loses
     its last reader, `remove_reaction` pops one copy and the other keeps
     that dep — and everything upstream of it — connected for the life of
     the app. Live shape (2026-08-23 heap snapshot): ComposerToolbar's
     `sessionUsesSelectedAccount` reads the session account's proxied
     fields before `selectedAccount`; the ring popover is its only
     reader. Hover, leave, a re-announced session account (new object,
     new proxied sources), hover again — and `selectedAccount` stayed in
     the global `accounts` signal's reactions after the pane closed,
     retaining the pane's whole DOM (3.4k detached nodes) through the
     derived's closure context. The hunk makes `reconnect()` skip a dep
     that already holds the derived (`runtime.js` only). Upstream `main`
     has the same code as of 2026-08-23; no issue filed yet — PR
     candidate. Drop when `svelte-patch-reconnect-dedupe.test.ts` passes
     on an unpatched release; `chatview-dom-retention.test.ts` carries the
     component-level tripwire (the ring re-hover case).

  Dropped on the 5.56.3 → 5.56.8 re-roll: the **zombie-mint fix** and its
  **probe** (`src/lib/utils/zombieMintProbe.ts`, deleted). Upstream fixed
  the same class of leak in 5.56.5 via
  [#18517](https://github.com/sveltejs/svelte/pull/18517) — `update_effect`
  now leaves `is_updating_effect` false for branch/root effects, so
  init-time prop reads no longer force-connect what they read.
  `svelte-patch-zombie-leak.test.ts` passes unpatched on 5.56.8 and stays
  as the tripwire if that regresses.

- `@lucide__svelte@1.28.0.patch` — **mask-icons**, one hunk: `dist/Icon.svelte`
  renders a CSS-mask `<span>` (`--mask-icon: url(#ao-lucide-N)` into the
  patch's own hidden sprite `<svg>` of `<mask>` elements, inline px box)
  instead of an inline `<svg>` root. Two Blink costs drive the shape, both
  measured: an `<svg>` rendered at a size other than its viewBox is a scaled
  replaced-content transform node (at ~400 mounted lucide icons,
  `GeometryMapperTransformCache::PlaneRootTransform` re-allocation was 72%
  of Oilpan churn while scrolling, 2026-08-24; every root also joined the
  per-frame SMIL time-container walk), and the first replacement — a
  data-URI `mask-image` per icon — cost an isolated SVG document (internal
  page + LocalDOMWindow + singleton roster) per DISTINCT URI, ~57 documents
  whose tiny long-lived singletons pinned hundreds of near-empty 128KB
  Oilpan pages (the renderer floor ratchet, 2026-08-25; sprite fragments
  don't dedupe, so `url(file.svg#frag)` was no fix). Masks are
  `mask-type:alpha`, objectBoundingBox units, 24-unit content scaled to the
  unit square — pixel-identical to the old `mask-size: contain` rendering
  on these square spans (spritecheck3/4 probes). The span's
  color/`mask-mode` styling lives in `app.css`
  (`.lucide-icon`/`.mask-icon`, including the `forced-colors: active` →
  `CanvasText` fallback without which icons vanish in Windows High
  Contrast); the patch owns only shape and box size. The sprite registry is
  duplicated from `utils/maskSprite.ts` (which the app-side siblings use)
  because a vendor patch cannot import app code. Deliberately dropped from
  upstream's contract: the `color` prop no longer stamps a stroke
  (currentColor covers every call site) and `children` snippets are not
  rendered (no call site passes one; audited — see
  `primitives/__tests__/Icon.test.ts`, which pins the patched contract and
  fails against an unpatched release). Deliberate divergence, not an
  upstream bug — carry it forward and re-roll on every version bump; the
  app-side siblings (`ToolKindIcon.svelte`, `primitives/brand/*`) register
  through `utils/maskSprite.ts` and use the same `.mask-icon` rule.

### Vendored svelte-streamdown

`vendor/svelte-streamdown/` is `svelte-streamdown@3.1.2` with 18 permanent
in-tree fixes. The per-entry rationale, drop rules and regression-test names
live in
[`vendor/svelte-streamdown/DIVERGENCE.md`](vendor/svelte-streamdown/DIVERGENCE.md);
wiring, build-config couplings and the upstream-diff recipe live in
[`vendor/svelte-streamdown/VENDOR.md`](vendor/svelte-streamdown/VENDOR.md).
Fix markdown-pipeline bugs in that tree and record them in that ledger.

The asymmetry is deliberate, not an oversight: a vendored package has a
DIRECTORY of its own, so its ledger belongs beside the code it describes,
where anyone editing the code will find it. A pnpm patch has no such place —
`patches/svelte@5.56.8.patch` is one file, and a `patches/svelte.md` next to
it would be a second thing to keep in sync with no code to sit beside. Patch
hunks therefore stay documented above, in this file.

## References

- [`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md) —
  chat/discussion scroll architecture and diagnostics.
- [`docs/architecture/data-flow.md`](../docs/architecture/data-flow.md) —
  provider → triage → store → frontend pipeline.
- [`docs/architecture/how-to.md`](../docs/architecture/how-to.md) —
  extension playbooks.
- [`docs/references/spike-policy.md`](../docs/references/spike-policy.md) —
  when Wails or provider behavior is unclear.
- `/Users/randy/repos/forge/apps/web/src/` — UX reference for ambiguous
  decisions.

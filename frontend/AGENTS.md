# frontend/

Svelte 5 (runes only), Vite 8 (Rolldown), Tailwind 4, TypeScript.
`pnpm run check` and `pnpm run build` are blockers, `pnpm test` is the
unit gate (`test:browser` and `test:manual` are separate vitest
projects). They live here, in `frontend/`, but the repo root carries a
`package.json` of forwarders so running one from there does the right
thing instead of failing on a missing manifest.

`pnpm run check:file <file.ts> …` is the tight loop: `tsc` over exactly
those files and what they import, ~2s against the full check's ~20s. It
covers no `.svelte` file and no Svelte component's props, and prints
that on every run. svelte-check has no per-file mode and its narrowest
scope, `--workspace <dir>`, measured SLOWER than checking everything
(23s vs 20s) because the cost is building the TypeScript program, not
running the diagnostics — so there is no scoped `.svelte` check to have.
`pnpm run check` stays the thing you run before calling it done.

There is no formatter, and that is deliberate. No prettier, no eslint,
no `.editorconfig`, no commit hook: `pnpm exec prettier` fails because
the dependency does not exist, and adding it would rewrite files nobody
asked to have rewritten. Match the file you are editing.

Area guides: [`stores/`](src/lib/stores/AGENTS.md),
[`transport/`](src/lib/transport/AGENTS.md), and under
`src/lib/components/`: `chat/`, `composer/`, `panes/`, `review/`,
`settings/`, `sidebar/`, `terminal/`, `virtual/`, `workflows/`.

`src/lib/native/` is the phone shell's side of this app: one seam per
platform capability, each inert in a browser build behind an
`isNativeShell()` check. It is documented with the container it serves,
[`mobile/AGENTS.md`](../mobile/AGENTS.md). Nothing outside that directory
should branch on being on a phone — the layout is chosen from the
viewport (§ Compact), never from the client.

## Reuse before reimplementing

Two surfaces that are conceptually the same surface share one component.
Parallel reimplementations of the pane host and the composer are the
known failure class here, so extend the existing one instead.

- `components/composer/ComposerInputSurface.svelte` is the editing core,
  hosted by both `Composer.svelte` and chat's `UserMessageEditor.svelte`.
- Menus, popovers, modals and dropdown shells come from
  `components/primitives/`. Compose them rather than authoring new
  positioning, focus-trap or keyboard behavior. The cross-surface
  ownership and clip-boundary contract is `utils/popoverOwnership.ts`.
- `components/panes/` is the only place that turns a layout item into a
  mounted pane.
- Split a `.svelte` file once it passes roughly 300 lines and a clear
  component boundary exists. Derive in `<script>`, render in the template.

## State ownership

State is keyed by its ENTITY, never by its consumer. Before adding
`$state`, name the entity the value describes:

- **app**: settings, provider accounts, chat-bar favorites, Codex MCP
  rows (that backend flag is global).
- **project**: new-thread defaults.
- **workspace**, meaning the worktree cwd: git status, branch, dirty bit,
  the open-PR reference, the Claude MCP listing (walked from the cwd out,
  so two worktrees of one project can legitimately differ), and the
  workspace-change lock, which answers two questions off one fetch.
  `locked` blocks removing the CHECKOUT (any busy thread in it),
  `threadLocked` blocks MOVING one thread (only its own activity).
  Every workspace-scoped git RPC names the checkout with a
  `WorkspaceRef` (`{projectId, workspacePath}`), never a thread id, so a
  DRAFT PLACEHOLDER — a pane with a project and a directory and no thread
  row — runs the same status subscription, the same split-button actions
  and the same review scopes as a real thread. `pane.workspace` is the
  one ref a component reads, and `utils/workspaceKey.ts`
  (`workspaceRefForThread` / `workspaceRefForProject`) is the one place
  that builds one: never assemble the object literal at a call site. A
  null `pane.workspace` (a terminal-only thread names no project) means
  the control DOES NOT RENDER — never renders and swallows the click.
- **PR**: detail, review threads, live head SHA, CI pipeline, conflicts.
- **thread**: items, streaming, approvals.
- **pane**: view concerns only, including the head a loaded diff was
  computed AT. Staleness derives from that against the store's live head,
  because a shared `stale` flag would lie to one of two panes.

An entity that outlives or spans the component lives in a shared
entity-keyed store, and every consumer `$derived`s from it. Which
primitive to build on, and its attach/apply contract:
[`stores/AGENTS.md`](src/lib/stores/AGENTS.md).

`ThreadPane` (`stores/thread.svelte.ts`) is the sole owner of per-thread
runtime UI state, from visible items and streaming flags through
approvals, channel messages, checkpoints and scroll-controller
registration. It is a composition root over `thread*` helper modules that
are constructed once per pane and are pieces of that owner, not sibling
stores ([`stores/AGENTS.md`](src/lib/stores/AGENTS.md) § The ThreadPane
modules). Add to it rather than beside it. Layout stores own
placement, order and min-size. A command-palette action resolves against
an explicit target pane, because enablement can change while the palette
is open.

Frontend memory is bounded by the visible thread: heavy payloads load on
demand through `stores/bindings.ts`, a thread switch is a bounded-window
load or a cache restore rather than a full-history hydrate, and settled
subagent children evict into a per-anchor fold (`utils/subagentFold.ts`)
that rehydrates on expansion. See "Live Window Bounds" in
[`frontend-scroll.md`](../docs/architecture/frontend-scroll.md).

Two panes on one WORKSPACE are first-class, two panes on one THREAD are a
bug ([`stores/AGENTS.md`](src/lib/stores/AGENTS.md)).

## Compact is a layout mode, not a second app

`stores/layoutMode.svelte.ts` picks `compact` from the viewport alone
(narrow AND coarse pointer), stamps `layout-compact` on `<html>`, and
that class is what the `compact:` Tailwind variant in `app.css` keys on.
The phone shell, a phone-sized browser tab and Playwright's compact
project all reach the same layout by that one door; nothing reads the
run mode or the device class to decide it. Every component renders in
both modes, so a surface is done only when it holds in both.

What compact changes, and where:

- **Two screens, both mounted.** The sidebar is the root screen and the
  pane strip is the thread screen (`compact-screen-list` /
  `compact-screen-thread`); the inactive one is `visibility: hidden` and
  `inert`, never unmounted, so a trip back to the list keeps the
  timeline's observers and scroll position. `revealPane` flips to the
  thread screen because every "show me this pane" path already passes
  through it; the chat header's back button flips to the list, and so
  does the last pane leaving (`destroyPane`), since compact has no close
  control and an empty thread screen has no back button.
  Because the swap is INHERITED visibility, an inline
  `visibility: visible` anywhere under a screen punches through it and
  paints over the other screen — the timeline's warm-up gate did exactly
  that on a real phone (2026-09-04). A style that means "not hidden"
  must clear the property (`undefined` / `''`), never set `visible`;
  the compact back-navigation spec fails on any element that still
  computes visible inside the hidden screen.
- **Settings is stacked screens** (`SettingsView`): the rail is a
  full-width screen, picking a section drills into the page, and the
  page header's back button returns — the desktop two-pane spread
  clipped the panel's controls off a phone's right edge. The compact
  spec's Settings case asserts the drill-in and that no control on a
  page extends past the viewport.
- **One pane per screen.** `PaneHost` sizes every pane to the strip and
  drops the dividers; companions still open and the existing reveal
  glide is the switch between a thread and its companion. No pane close
  control, no resizer, no rail.
- **Popovers are bottom sheets** (`Popover`'s `sheet` prop, on by
  default); the composer's mention and slash lists opt out because they
  belong on the caret. Overlays fill the screen.
- **Return inserts a newline** (`enterSends` in `composerKeyboard.ts`) and
  the Send button is the way to send.
- **A long press is the right-click.** `utils/longPressContextMenu.ts` is
  one window-level detector, live only under compact, that turns a held
  touch into a synthetic `contextmenu` at the pressed element, so every
  existing `oncontextmenu` site (rows, the project header, terminal tabs,
  the pane title's rename, the delegated link and diagram hosts) opens on
  the phone with no per-site wiring, and the e2e compact project drives the
  same path a device does. Exactly one `contextmenu` reaches handlers per
  press, a handled press swallows the compatibility mouse sequence the
  engine sends on release, and an unhandled one (chat prose, the composer)
  is left to the engine. Do not add a per-component long-press handler.
- **Menus are sheets from both primitives.** `ContextMenu` renders as a
  bottom sheet under compact (`data-placement="sheet"`), the shape
  `Popover` already takes; `MenuItem` rows grow to 40px there.
- **A hidden gesture is not an affordance.** Every sidebar row carries
  `SidebarRowMenuButton` (`hidden compact:flex`), the visible door into the
  same handler right-click runs; the project header shows its New Thread
  control and moves New Terminal into its menu, compact only. Rows are
  36px (`compact:h-9`) and `compact:select-none`, and nothing drags:
  `ThreadRow`, the project header and the pane title all set `draggable`
  false there, because Android starts a drag on a held draggable and that
  fights the long press.
- **The chat header is a title and one button.** The desktop's action
  cluster (review, PR, terminal, runs, take control, browser, editor,
  git split button) rolls into a menu behind `chat-header-more`
  (`ChatHeaderActions`), so the title keeps its width. That menu is
  the one compact popover that is NOT a bottom sheet (`sheet={false}`,
  `bottom-end`): a control at the top of the screen answers where the
  finger is, and the bottom is where the composer's sheets rise from
  (owner ruling, 2026-09-04). No command palette button: the phone has
  no chords for one to stand in for. The git split button's popover and
  dialogs stay mounted outside the menu (`GitActionsControl`'s
  `trigger={false}` + `openMenu`).
- **The composer's `minimal` rung keeps the model and the meters.**
  Below the width where even icon-only pickers fit beside the
  rate-limit and context meters, every picker but the model folds into
  `composer-pickers-rollup` (`ComposerPickersRollup`), whose rows open
  the same registry handles the chords do; the pickers stay mounted
  under it. The model and the meters are what a phone reader reads
  before sending (owner ruling, 2026-09-04), so they never yield.
  `composerToolbarDensity.ts` documents the ladder. The picker box the
  rung hides is `shrink-0` on purpose: a box allowed below its content
  width let the pickers paint over the meters while the toolbar's
  `scrollWidth` still read as fitting, so the ladder never reached the
  rung it was built for (first phone session, 2026-09-04).
  The model trigger must also resist shrinking until the other pickers
  roll up; only the minimal rung may ellipsize its label. Otherwise a
  shrinkable model silently loses all its text while the toolbar still
  measures as fitting. The meters, Send, and roll-up never shrink.
  `compact-model-label.spec.ts` checks actual label width for both providers
  across phone sizes and a resize back from desktop, not just a visible button.
- **The workspace footer stays on one line.** Compact uses one workspace
  trigger for branch/worktree, with the branch name shown once and the checkout
  kind indicated by its icon. Its sheet hands off to the existing pickers and
  contains new-branch naming. Project/workspace labels may ellipsize; tokens
  and cost stay together and visible. Desktop retains the separate controls.
- **The hardware back is one stack** (`native/lifecycle.ts`
  `answerBackPress`): an open sheet or overlay (a synthetic Escape,
  which also steps Settings' page back to its rail through
  `escapeSettingsOverlay`), then a terminal drawer over the chat, then
  the companion on screen (closed, its thread revealed), then the thread
  screen back to the list, then the app exits. A focused terminal's
  Escape is dispatched at the pane, not at xterm, which would type ESC
  into the shell.
  The event is marked as surface dismissal. Global dispatch may run only an
  enabled command with `dismissesSurface`, independently of its keyboard
  binding; ordinary shortcuts must never run from Android Back. Unmarked
  Escape retains its keyboard meaning, including interrupting a turn.
- **Keyboard chrome is shown while a modifier is held.** The sidebar
  search's chord pill uses `subscribeJumpHints` / `getJumpHintsVisible`
  exactly as the thread rows' jump numbers do, so a phone — which never
  holds one — never sees it.
- **A paired session is never `host`**, even on a loopback peer
  (`transport/scopes.ts`): a phone over `adb reverse` or a tunnel is
  still a phone, and the editor-open surfaces stay off it.

Host-only surfaces already hide by scope, so compact adds no second set
of gates. Do not fork a component for the phone; add a `compact:` class
or read `isCompactLayout()` at the one branch that differs.

## Rules with structural tests

`src/lib/architecture.test.ts` enforces seven. Each carries a shrink-only
allowlist: an exception that has been fixed must be deleted, and an entry
that no longer offends fails too.

1. Entity-owned RPCs are store-only, and an entity store may import the
   RPCs it owns and no others. A new entity store registers its RPCs in
   that registry, which is the rule's whole job.
2. Wire subscriptions live in `stores/`. `wailsEventOn` and
   `@wailsio/runtime`'s `Events.On` are the same door, and components use
   neither. The allowlisted exceptions all consume-and-drop their payload.
3. Authored layer promotion is prohibited app-wide: never a conditional
   `will-change`, never a promote/demote lease. A second paint position
   outside the `scrollTop` chokepoint left WebView2 presenting stale
   pixels while state, DOM and input stayed live. The one carve-out is
   `will-change: scroll-position` on `.pane-scroll-surface`, where the
   scroll offset IS the chokepoint's value.
4. `lib/harness/` is reachable only through the dynamic import in
   `stores/harnessBridge.ts`. A static import from anywhere silently
   pulls the MutationObserver, the rAF meters and the globals probe into
   the startup chunk.
5. An event-driven refresh uses `utils/refreshScheduler.ts`. A trailing
   debounce is postponed forever while events arrive closer together than
   its delay, and two of the three offenders gated workspace mutation.
6. Random identifiers come from `utils/randomId.ts`. `crypto.randomUUID`
   is SECURE-CONTEXT ONLY and a plain-HTTP LAN page is a shipped context,
   where the property is absent and the call throws — in `wsClient`'s
   `generateId` that was a blank page on the first RPC of the boot
   fan-out, not a degraded feature.
7. A send is built by `utils/sendOptions.ts#buildSendOptions`, the one
   place its idempotency id is minted. A module reaching
   `SendMessageWithOptions` or `RegisterQueueItem` either builds the
   options or takes built `OutgoingSendOptions`; an inline object literal
   is the offence, and it made a retried frame look like a second message
   instead of the same one arriving twice.

`src/lib/themeTokens.test.ts` enforces the token layer over all of
`src/`: no raw Tailwind palette or black/white utilities, no default
shadow scale, no arbitrary-value color functions, no dead token
utilities, no hex literals outside the theme layer. Its raw-class
allowlist is EMPTY, so fix a failure by using or defining a TOKEN.
Offending class names are not spelled out here, because Tailwind's
scanner would compile a quoted utility in.

`src/lib/styleInvalidation.test.ts` keeps every compound in the GLOBAL
stylesheets keyed on a class, id, tag or attribute. A featureless
compound (a bare `:last-child`, a `*` right of a sibling combinator)
lands in Blink's universal invalidation sets, so any DOM mutation anywhere
schedules it: two cost 175 whole-subtree invalidations per 15s of
two-pane streaming.

## Motion, layers and scroll

The transcript renders like print: exactly two motion owners exist inside
the timeline scroller and nothing else moves. Mechanism in
[`frontend-scroll.md`](../docs/architecture/frontend-scroll.md) § The
Print Doctrine, directory rules in
[`chat/AGENTS.md`](src/lib/components/chat/AGENTS.md).

- A standing animation is STEPPED. Smoothly interpolated and repeating
  forever, it pins GPU frame production to panel refresh for as long as
  it is on screen: one 6px pulsing dot was a standing 165 presents/sec
  client that stuttered other applications (2026-07-04). Use
  `primitives/SteppedSpinner.svelte` or step a sprite strip. Guard:
  `timelineKeyframeAnimations.test.ts` rule 1, run over all of `app.css`
  because the hazard is document-wide.
- Auto-scroll is INTENTIONAL: never move the viewport somewhere the user
  did not ask to go. Escape is event-sourced, never geometry-inferred,
  and re-engagement needs explicit consent. `utils/scroll/` owns intent
  and every programmatic `scrollTop` write for the timeline
  (frontend-scroll.md § Intent And Programmatic Writes). The run map
  carries the same contract separately
  ([`workflow-run-map.md`](../docs/architecture/workflow-run-map.md) §9).
- Scroll motion uses the engine's measured write lattice, never DPR or an
  integer/fractional readback as a proxy. Browser zoom and flooring engines
  require separate quantum and write-offset calibration. Exercise scale
  transitions and refused one-grid-pixel writes at high refresh rates.
- Work is never shed because something is off-view. Prepaint or tile
  shedding, `animation-play-state` pausing, conditional unmounting and
  conditional layer demotion are all banned: the common case is a reader
  bouncing between panes. Make the always-on unit cheaper instead.
- A suspended ambient ticker wakes only for mutations that introduce its
  consumer. Unrelated streamed text or classes must not restart periodic
  document scans (`ambientTicker.test.ts`).

## Rendering

Raw content is canonical. Go sends raw item summaries, channel message
content and payload data, and the frontend renders them as viewport-local
projections. Add no server-rendered chat HTML and no global DOM observer.
Markdown, ANSI, path linkification and the code and diagram hosts:
[`chat/AGENTS.md`](src/lib/components/chat/AGENTS.md).

## Theme system

Spec: [`theme-system.md`](../docs/architecture/theme-system.md) (§7 the
contract, §9 as-built deviations). Two independent axes, a UI theme and a
code theme, over a light/dark mode, resolved and applied entirely in the
frontend from files Go only lists. The selection is a property of the
CLIENT MACHINE, so a write-blocked session keeps its own locally (§9.6).

- `lib/theme/tokenRegistry.ts` is THE token vocabulary, and
  `tokenRegistry.test.ts` fails on drift against the three stylesheets in
  either direction. A role that does not exist yet gets DEFINED there.
- The mode-class stamp and the applier are both `$effect.pre`, class
  first (§9.1). Svelte flushes render effects before user effects and the
  resolved MODE is a resolver input, so splitting them across the two
  passes leaves the render pass reading a palette that does not match the
  class on `<html>`. Cascade-READING work goes in a plain `$effect`.
- One `<style id="user-theme">`, rewritten wholesale by `applyTheme`,
  which is also the one writer of the applied-theme state read back
  through `getAppliedTheme()`. Never `setProperty` per token on the root:
  each such write is a whole-document style invalidation (~13ms at 5k
  nodes, ~90ms at 30k) and a theme carries up to 85 tokens.
- One palette identity. Anything caching rendered output keyed on the
  palette (the mermaid config memo and its `{#key}`, the xterm bridge)
  widens `getThemePaletteIdentity()` and pairs it with the resolved mode.
  A second key that can disagree means a stale render or a remount per tick.
- Non-CSS consumers read values through `utils/cssColorProbe.ts`, because
  `getComputedStyle` serializes in the DECLARED color space and the
  canvas readback is what xterm's and mermaid's parsers can read.

## Anti-patterns

- Discriminate timeline items by `kind`, never by role or content
  substring.
- Upsert by `(turnIndex, itemIndex)` and let the store stay sorted. Do
  not re-order items during render.
- Heavy content is expand-to-load: no count-based slicing for
  virtualization, no preloading heavy payloads.
- Call Go through `stores/bindings.ts`, never `window.runtime`.
- Every template expression inside a nullable-guarded `{#if}` branch must
  be TOTAL: optional-chained with a neutral default, or one `$derived`
  folding the other inputs with the null check inside. An expression
  carrying a SECOND reactive dep re-runs on that dep and can render after
  the guarded value went null but before the branch tears down, and
  TypeScript's narrowing hides it. Guard:
  `nullableGuardTotality.test.ts`, a SOURCE rule because no component
  test can stage the class.
- Conditional feature surfaces mount lazily and never take a static
  import from eager code: `{#await import(...)}` at replace-surface
  branches, `primitives/LazyOverlay.svelte` for modals and drawers with
  exit transitions. One static import from an eager module drags the
  chunk back into startup, so check `dist/index.html`'s modulepreload
  list at these boundaries. The `{#await}` input must be a promise with
  STABLE identity captured once at init (see `CompanionPane.svelte`). A
  reactive expression that constructs one re-pends on ANY dependency
  invalidation and remounts the surface (`PaneHost.test.ts`).
- No visible in-app explanatory text for internal mechanics, shortcuts,
  or implementation details.
- A render throw is contained, never page-wide. Uncaught, a throw inside
  an update flush aborts the whole batch and every region the traversal
  had not reached keeps its stale DOM for good: a composer that will not
  clear after its send went through, a reveal stopped mid-message
  (2026-08-29, 2026-09-04). Every pane body (`panes/PaneHost.svelte`),
  the thread list (`sidebar/Sidebar.svelte`) and the review pane sit in
  `shared/RenderBoundary.svelte`, which renders the failure in place
  with a Retry and records it through `reportFrontendDiagnostic`
  (the boundary keeps it from `window.onerror`). A new top-level region
  gets the same wrapper. The most common throw of the class, a repeated
  key in a keyed `{#each}`, no longer throws at all: the `each-key-repair`
  vendor hunk below repairs the key and reports it, so a duplicate is a
  logged bug instead of a frozen page. `utils/uniqueEachKeys.ts` stays
  the tool for lists whose repeats are LEGITIMATE (model-authored option
  rows): those must not be reported as bugs on every render.

## Testing

Store logic gets a Vitest unit test beside it, rendering or interaction
changes get a component test. Scroll behavior is covered in
`utils/scroll/` (`index.svelte.test.ts`, the exhaustive `resolver.test.ts`
and the frame-level `scrollInterleavings.test.ts`) plus
`components/chat/scroll.test.ts`.

A globally suppressed engine warning is a defect-ledger entry, not a
config setting. "ResizeObserver loop completed with undelivered
notifications" was filtered out of the browser suite's error sink as
benign noise, and it hid a user-visible stale-frame paint bug for the
whole 2026-08-28 session: an undelivered notification means the
observer's write slid past the frame it belonged to, which is precisely
a row painting last frame's geometry. Suppress at the narrowest scope
that unblocks the test, name the defect it stands for, and pair it with
an assertion that the suppressed condition does not occur where it
matters — never a suite-wide filter. The two real instances are
documented at their sites: `MessageTimeline.svelte`'s
`observeScrollSurfaceContentWidth` (ancestor resolved before row
observers) and `TimelineVirtualizer.svelte`'s
`deferNewRowObservationUntilNextFrame` (overscan rows registered next
frame, outside the painted window).

A stateful door gets a transition test, not just an on-state assertion.
`test/helpers/transitions.ts` drives on→off→on, teardown twice, a second
engagement, and teardown-mid-flight, comparing the state you name after
every lap. The leaks the 2026-08 perf session found by hand all lived in
the SECOND lap — a re-register that duplicated a sink, a toggle that kept
a stale checkpoint, a cache that carried the previous mode.

A timer that hands work to rAF owns BOTH handles. Cancel both on
supersession and detach, and invalidate already-dispatched callbacks by
generation. The scroll observer's resize-clear used to outlive detach
and erase a new attachment's identical stamp (`scroll/observers.test.ts`).

A deterministic CPU sweep gets a contention-sized budget, never the
5s default. Vitest fans files across one fork per core, so a sweep's
wall time scales with whatever else the gate is running — three
markdown sweeps sitting at 48-62% of their budgets on an idle core all
failed the same 2026-08-30 full run at ~1.6x contention, green in
isolation every time. Size the budget as a wedged-runtime tripwire
(10x+ idle cost, stated in a comment at the timeout), and put the real
hang guard in the loop itself: a bounded corpus, or an explicit
iteration cap that throws.

A wait that gives up must FAIL, and its budget is wall-clock, never a
count of loop turns. Both halves came from the same 2026-08-30 flake:
`harnessBridge.test.ts`'s poll helper spent 500 `setTimeout(0)` hops and
then RETURNED, so a cold dynamic import that outran them left the case
asserting against state that had not arrived — and the arrival then
landed inside the NEXT case, past the `afterEach` that had just zeroed
the counters. One slow import, two failing tests, neither naming the
wait. The event loop spins hops happily while a starved worker gets no
CPU, so hops measure the fast machine rather than the work.

Do not quantize a continuous measurement in an assertion. The same
session's second flake compared which 125ms slot two aligned animations
floored into, when what the aligner promises is that their PHASES agree
to within a frame — a pair a millisecond either side of a slot boundary
failed a mechanism that was working. Assert the distance, with the
tolerance the mechanism actually claims.

`vi.mock` a shared store with an `importOriginal` spread, never a
whole-module factory. A factory listing only the exports one test drives
turns every LATER export of that module into `undefined` for it, and the
failure lands in an unrelated file (adding `isMethodUnavailableError` to
`transportStatus.svelte.ts` broke five suites that only wanted
`getTransportStatus`).

`vi.mock` also does not reliably reach `.svelte.ts` importers: a mock was
observed replacing the binding seen by plain `.ts` importers while
`.svelte.ts` importers of the same module silently kept the real one,
with no error. Assert one layer deeper against the real pipeline instead
(`setBindingMock('ReportFrontendErrorBatch', …)` rather than mocking
`utils/frontendErrorCapture`).

A new binding called by a PASSIVE LOAD needs its mock in every suite
that renders the component, not just the one that asserts on it. The
mock dispatcher throws SYNCHRONOUSLY for an unmocked name, so the
`try/catch` around the load runs its `addToast` inside the `$effect`
flush, and Svelte reports `effect_update_depth_exceeded` instead of the
name that was missing. Adding the phone-push status read to
`NotificationsSection` failed four unrelated settings suites that way
(2026-09-02); the fix is one `setBindingMock` per suite, and
`test/integration/_helpers.ts#installAppDefaults` is where the whole-App
ones belong.

Operator-run drivers are named `*.manual.ts` and run only under
`pnpm test:manual`, never in a gate. `markdown/freezeReplay.manual.ts`
replays a recorded streaming corpus through the production markdown path
and WEDGES the worker on a real finding rather than failing. Its corpus
is a gitignored real-session capture that must never be committed.
Generate one with `scripts/generate-freeze-replay-fixture.mjs`.

`src/test/mocks/bindings-app.ts` lists its exports explicitly. A new binding
must be added there or the unit project resolves it as `undefined` and the
first call throws inside whatever effect made it. Binding mocks are plain
objects, so they cannot reproduce a wire shape whose keys are OWN
`undefined` properties (the generated Settings model class does that for
omitted keys); only the harness e2e sees that class. happy-dom drops clicks
on disabled buttons.

## Vendor patches

Third-party divergence has two shapes, and the choice is about the
upstream, not the change. A live upstream whose fix is meant to be
dropped gets a pnpm patch: `patches/` is keyed to exact versions in
`pnpm-workspace.yaml`, so a bump without re-rolling fails `pnpm install`
(`pnpm patch <pkg>@<version> --edit-dir <dir>` then `pnpm patch-commit`).
A patch has nowhere to keep its rationale, so each one's hunks are
documented below. A dormant upstream and permanent divergence gets
ADOPTED into `src/` as ordinary first-party code carrying the upstream
LICENSE — there is no `vendor/` tree and no divergence ledger. Never edit
`node_modules`: packages are hardlinked from the pnpm store, so an edit
corrupts every project on the machine.

The markdown pipeline is the one adoption so far, at
[`src/lib/markdown/`](src/lib/markdown/AGENTS.md) — `svelte-streamdown`
(formerly `vendor/svelte-streamdown/`) plus marked's lexing half in
`parser/engine/`, which replaced both the `marked` dependency and its
pnpm patch. Fix parser bugs there, never duplicating the fix in
`markdownEnhance.ts` or the host wrappers. Its area guide owns the parser
map, the host seams, the path-relative URL security boundary and the test
map.

`patches/svelte@5.57.0.patch` has six hunks, each dropping when its
suite passes against an unpatched release (the two deliberate
divergences never will; their rows say so).
`svelte-patch-zombie-leak.test.ts` and
`svelte-patch-event-slot.test.ts` are two more suites with no hunk
left, guarding leak classes upstream fixed in 5.56.5 and 5.57.0; both
must keep passing UNPATCHED.

| Hunk | What it fixes | Suite |
|---|---|---|
| ownerless-roots | `$effect.root` inherited the creating component's context and parent, so store-level roots pinned dead row instances. Deliberate divergence, no upstream issue: carry forward, re-evaluate every bump. | `svelte-patch-ownerless-roots.test.ts` |
| destroy-pass-errors | A throwing user `$effect` teardown aborted the sibling-destroy loop, leaving queued effects subscribed and detached DOM retained for the parent's lifetime. Upstream PR [#18566](https://github.com/sveltejs/svelte/pull/18566). | `svelte-patch-destroy-pass.test.ts` |
| flush-loop-caps | Both synchronous flush loops were unbounded, so a cycle was an unreportable renderer freeze (2026-08-07: WebView2 wedged 8+ minutes, no paint, no error, nothing in any log). The caps abort and throw a svelte-shaped error that `utils/frontendErrorCapture.ts` persists, message kept in production. PR candidate. | `svelte-patch-flush-caps.test.ts` |
| each-key-repair | A repeated key in a keyed `{#each}` threw `each_key_duplicate` from inside the flush, aborting the batch and freezing every region it had not reached (2026-08-29: 400+ hits behind the pane-freeze incident; 2026-09-04: a composer that kept its sent text). The hunk repairs the repeat to a unique key (`key\u0000#n`, stable across runs for string and number keys; a fresh Symbol otherwise) and reports it once per block through `reportError`, so `utils/frontendErrorCapture.ts` records the key value where the throw used to land. Deliberate divergence: upstream throws by design. | `svelte-patch-each-key-repair.test.ts` |
| reconnect-dedupe | `get()` on a disconnected, dirty, previously-run derived registered it twice in one dep, so losing its last reader left that dep and everything upstream connected for the app's life (2026-08-23 heap snapshot: a closed pane's 3.4k detached nodes). PR candidate. | `svelte-patch-reconnect-dedupe.test.ts`, `chatview-dom-retention.test.ts` |
| flip-phases | An animated keyed-each reorder interleaved abort / read / create per item, forcing up to N style-layout passes in one microtask (34.6ms of gBCR self-time in a sidebar-reorder burst, 2026-08-26). Three phased loops instead: identical geometry, one forced pass. PR candidate. | `svelte-patch-flip-phases.test.ts` |

`patches/@lucide__svelte@1.28.0.patch`, mask-icons: `dist/Icon.svelte`
renders a CSS-mask `<span>` against the patch's own hidden `<mask>`
sprite rather than an inline `<svg>` root, which measured as a scaled
replaced-content transform node costing 72% of Oilpan churn while
scrolling at ~400 icons (2026-08-24). A data-URI `mask-image` per icon,
the obvious alternative, costs an isolated SVG document per DISTINCT URI.
The patch owns only shape and box size. Color and `mask-mode` live in
`app.css` (`.lucide-icon` / `.mask-icon`, with the `forced-colors:
active` fallback without which icons vanish in Windows High Contrast),
and the sprite registry is duplicated from `utils/maskSprite.ts` because
a vendor patch cannot import app code. Upstream's `color` prop and
`children` snippet are deliberately not rendered, pinned by
`primitives/__tests__/Icon.test.ts`. Re-roll on every bump.

## References

`bindings/` is Wails-generated. Regenerate with `wails3 generate bindings
-ts` rather than editing it. `src/lib/generated/` is generated from Go too
and is equally not hand-edited: `settingsDefaults.ts` comes from
`internal/settings.DefaultSettings` via `go generate ./internal/settings`,
and a Go test fails on a stale copy. Backend to frontend flow:
[`data-flow.md`](../docs/architecture/data-flow.md). Extension playbooks:
[`how-to.md`](../docs/architecture/how-to.md). When Wails or provider
behavior is unclear, spike outside the repo
([`spike-policy.md`](../docs/references/spike-policy.md)).

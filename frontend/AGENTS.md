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
`virtual/`, `workflows/`.

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
registration. Add to it rather than beside it. Layout stores own
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

## Rules with structural tests

`src/lib/architecture.test.ts` enforces five. Each carries a shrink-only
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
- Work is never shed because something is off-view. Prepaint or tile
  shedding, `animation-play-state` pausing, conditional unmounting and
  conditional layer demotion are all banned: the common case is a reader
  bouncing between panes. Make the always-on unit cheaper instead.

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

Operator-run drivers are named `*.manual.ts` and run only under
`pnpm test:manual`, never in a gate. `markdown/freezeReplay.manual.ts`
replays a recorded streaming corpus through the production markdown path
and WEDGES the worker on a real finding rather than failing. Its corpus
is a gitignored real-session capture that must never be committed.
Generate one with `scripts/generate-freeze-replay-fixture.mjs`.

## Vendor patches

Third-party divergence lives in one of two places, and the choice is
about the upstream, not the change. `patches/` holds pnpm patches keyed
to exact versions in `pnpm-workspace.yaml`, so a bump without re-rolling
fails `pnpm install` (`pnpm patch <pkg>@<version> --edit-dir <dir>` then
`pnpm patch-commit`). Use it while the upstream is alive and the fix is
meant to be dropped. `vendor/` holds in-repo pnpm workspace packages, for
a dormant upstream and permanent divergence. Each vendored package keeps
a `VENDOR.md` (upstream URL, baseline, how to diff a release) and a
`DIVERGENCE.md` ledger beside its code, which a one-file patch has
nowhere to keep, so patch hunks are listed below instead. Never edit
`node_modules`: packages are hardlinked from the pnpm store, so an edit
corrupts every project on the machine, and that is why vendored packages
are `workspace:` rather than `file:`.

`vendor/svelte-streamdown/` is the markdown pipeline. Fix parser bugs
there and record each in
[`DIVERGENCE.md`](vendor/svelte-streamdown/DIVERGENCE.md) (per-entry
rationale, drop rule, regression test), never duplicating the fix in
`markdownEnhance.ts` or the host wrappers.

`patches/svelte@5.56.8.patch` has six hunks, each dropping when its suite
passes against an unpatched release. `svelte-patch-zombie-leak.test.ts`
is a seventh suite with no hunk left, guarding a leak class upstream
fixed in 5.56.5.

| Hunk | What it fixes | Suite |
|---|---|---|
| ownerless-roots | `$effect.root` inherited the creating component's context and parent, so store-level roots pinned dead row instances. Deliberate divergence, no upstream issue: carry forward, re-evaluate every bump. | `svelte-patch-ownerless-roots.test.ts` |
| event-slot-release | The delegated-event dispatcher never cleared `last_propagated_event`, so `event.target` anchored a closed pane's detached subtree until the next event. Upstream PR [#18569](https://github.com/sveltejs/svelte/pull/18569). | `svelte-patch-event-slot.test.ts`, `chatview-dom-retention.test.ts` |
| destroy-pass-errors | A throwing user `$effect` teardown aborted the sibling-destroy loop, leaving queued effects subscribed and detached DOM retained for the parent's lifetime. Upstream PR [#18566](https://github.com/sveltejs/svelte/pull/18566). | `svelte-patch-destroy-pass.test.ts` |
| flush-loop-caps | Both synchronous flush loops were unbounded, so a cycle was an unreportable renderer freeze (2026-08-07: WebView2 wedged 8+ minutes, no paint, no error, nothing in any log). The caps abort and throw a svelte-shaped error that `utils/frontendErrorCapture.ts` persists, message kept in production. PR candidate. | `svelte-patch-flush-caps.test.ts` |
| reconnect-dedupe | `get()` on a disconnected, dirty, previously-run derived registered it twice in one dep, so losing its last reader left that dep and everything upstream connected for the app's life (2026-08-23 heap snapshot: a closed pane's 3.4k detached nodes). PR candidate. | `svelte-patch-reconnect-dedupe.test.ts`, `chatview-dom-retention.test.ts` |
| flip-phases | An animated keyed-each reorder interleaved abort / read / create per item, forcing up to N style-layout passes in one microtask (34.6ms of gBCR self-time in a sidebar-reorder burst, 2026-08-26). Three phased loops instead: identical geometry, one forced pass. PR candidate. | `svelte-patch-flip-phases.test.ts` |

`patches/marked@16.4.2.patch`, allocation-free extension dispatch: one
typed receiver per Lexer and indexed loops, replacing a closure and a
fresh `{ lexer }` receiver per extension candidate at every token
position. The public shape is unchanged and Marked's own suites pass
patched. Drop it when upstream stops allocating inside the token loops.

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
-ts` rather than editing it. Backend to frontend flow:
[`data-flow.md`](../docs/architecture/data-flow.md). Extension playbooks:
[`how-to.md`](../docs/architecture/how-to.md). When Wails or provider
behavior is unclear, spike outside the repo
([`spike-policy.md`](../docs/references/spike-policy.md)).

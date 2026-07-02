# Bespoke Timeline Virtualizer Plan (virtua replacement)

Status: PROPOSED — awaiting review before implementation.
Branch: `virtualizer/bespoke-engine`. Evidence:
[virtualizer-replacement-inventories.md](virtualizer-replacement-inventories.md)
(Part A: upstream anatomy; Part B: exhaustive app touchpoints).
Prerequisite work: the scroll re-architecture
([scroll-rearchitecture-plan.md](scroll-rearchitecture-plan.md), Stages
0–4, merged to main at `90ba7f77`) — its pure resolver, single-writer
chokepoint, and engine-agnostic browser outcome tests are what make
this swap low-risk.

## 1. Decision and why now

This executes the Stage-5 decision gate from the scroll plan,
deliberately: replace `virtua` with a bespoke, bottom-anchored timeline
virtualizer that lives *inside* the scroll controller's decision loop
instead of beside it as a second writer.

Why now rather than after a longer soak:

- **The patch surface is the standing risk.** Both hunks of
  `virtua@0.49.1.patch` land in minified name-mangled core; every
  version bump is a manual re-roll against mangled names
  (inventories §A4 note). The bespoke engine deletes the patch, the
  tripwire browser tests that guard it, and the version coupling.
- **Everything hard is already built and tested.** Intent
  classification, the spring, the pure resolver, the warm gate, and
  the browser outcome suites all live in `utils/scroll/` — the engine
  is the *last* moving piece, not the first. The outcome tests
  (`streamingOutcome` / `remountReturn` / `compensationOutcome` /
  `rowMarginContainment` `.browser.test.ts`) were written
  engine-agnostic and are the acceptance gate.
- **The wins are policy-level and unpatchable upstream**: one-pass
  streaming hot path (no double observation + reaction loop), a
  range-equality early-out on the scroll path, live per-item height
  priors (deleting the constructor-once cache-replay dance), and
  per-row settle knowledge (re-sources the warm gate in V4).

virtua was the right v1 dependency; the mismatch is architectural
(top-anchored estimate→measure with internal compensation writes vs a
bottom-anchored streaming log with a single-writer controller).

## 2. Target architecture

### Performance priorities and frame budget (project-wide, stated 2026-07-02)

Priority order for this project, and how the engine binds to it:

1. **Perceived performance first — high refresh.** Target displays run
   165–244Hz; the per-frame budget for any work the engine or adapter
   does during streaming follow, user scroll, or hot-thread switch is
   **~4–6ms including style/layout/paint**, not the 16ms folklore
   number. Design consequences, enforced in review at each phase:
   - **Range-equality early-out.** Scroll events that don't change the
     visible range must be near-free. (Today every controller pin write
     triggers virtua's scroll handler → store version bump → the
     adapter recomputing offset/hide for every mounted row, per event,
     at up to 244Hz. Deleting that recompute is the engine's single
     biggest per-frame win.)
   - **No forced synchronous layout on hot paths.** The engine answers
     geometry questions from its own model; DOM reads stay behind the
     controller's existing chokepoint discipline.
   - **Minimal per-frame DOM writes.** One container-height write when
     totalSize changes; row style writes only for rows whose offset
     actually changed; window recompute batched per input batch, never
     per scroll event.
   - **Eviction off the gesture.** Row unmounts happen at
     scrollend/idle in bounded batches, never mid-gesture
     (`remountReturn.browser.test.ts` already bounds batch sizes).
   - **Hot-thread switch is a measured scenario**: with a priors hit,
     a revisited thread must paint once at final geometry — zero
     correction passes. V1 adds a dev-trace time-to-stable-paint
     metric so this is verified, not vibed.
2. **Memory second — keep only what's needed.** The DOM window (the
   symmetric `bufferSize` overscan) is the memory budget; cost-aware
   row retention was considered and cut entirely (§8 D3) — measured
   sizes survive eviction in the size store, so scroll-back stability
   does not require keeping DOM alive. Engine bookkeeping is plain
   number arrays (KBs); the priors LRU stays at 50 snapshot entries.
   Payload/expansion budgets are untouched by this plan.
3. **General performance third** — algorithmic wins (prepend splice,
   watermark-preserving invalidation) land where cheap, but never at
   the cost of 1 or 2.

One deliberate tradeoff to keep visible: virtua's
`pointer-events: none`-while-scrolling suppresses hover style recalcs
mid-scroll (FPS-positive) at the cost of hover death during scroll.
The plan drops it at cutover; the V2 soak includes a frame-time check
on hover-heavy timelines, and if that shows a regression the explicit
controller-driven class ships in this branch — the decision resolves
inside V2, not as a follow-up (§8 D4).

### Ownership model

- **The engine never writes `scrollTop`. Ever.** It owns geometry
  (sizes, offsets, total height) and windowing (which rows are
  mounted). When geometry changes in a way that requires a scroll
  adjustment (above-viewport remeasure, head splice), it emits a
  **compensation observation** into the scroll controller — the same
  seam `applyVirtuaScrollCompensation` occupies today, now first-class
  instead of patched-in. The resolver decides; `writeScrollTop`
  writes. Single-writer becomes literal: no marking, no store-poking
  decline protocol, no direction-latch misclassification to defend
  against.
- **Coordinates stay top-anchored prefix sums** (inventories,
  validation note 1). "Bottom-anchored" is policy, not coordinates:
  - *Mount seeding*: first render mounts the **tail** window (last K
    rows / last `bufferSize` px), so the rows that determine the
    user-visible landing measure first and above-viewport estimate
    error cannot move the initial viewport.
  - *Pinned follow*: while sticky, the per-beat target is
    `scrollHeight − clientHeight` (the controller already owns this);
    above-viewport remeasures need no compensation at all — the pin
    write lands the same frame.
  - *Reading position*: when not pinned, an above-viewport size delta
    emits a compensation observation (Δ, cause) and the resolver holds
    the reading anchor — the outcomes currently pinned by
    `compensationOutcome.browser.test.ts` tiers ①–④.
- **The window is direction-independent.** virtua's directional buffer
  trim (drop the backward overscan when scrolling down) is the
  mechanism the marking patch existed to suppress — the bespoke window
  keeps symmetric `bufferSize` overscan and deletes the entire
  buffer-drop failure class by construction.
- **The intent machine stays the classifier.** The engine gets
  user-vs-programmatic knowledge from the controller (which already
  has it) instead of sniffing scroll deltas.
- **ChannelView is untouched.** It stays virtualizer-free on the same
  scroll controller; nothing in this plan crosses that boundary.

### File layout (target)

```
frontend/src/lib/utils/virtual/
  sizes.ts             ported virtua cache: prefix-sum size store (attributed)
  sizes.test.ts        ported cache.spec.ts, adapted
  window.ts            visible-range + symmetric buffer policy (pure)
  engine.ts            reducer: length changes / measurements / viewport in →
                       window + compensation observations out (pure)
  priors.ts            per-item height priors: persistence (LRU, keyed by
                       width + structure + expansion sig) + live application
  types.ts             handle, props, observation types
frontend/src/lib/components/chat/
  TimelineVirtualizer.svelte   adapter: single RO, keyed each, absolute
                               positioning, style contracts, handle export
  VirtualRow.svelte            one absolutely-positioned row wrapper
```

Each file <500 lines. Pure modules (`sizes`, `window`, `engine`,
`priors`) have no DOM imports and are exhaustively unit-tested like
`scroll/resolver.ts`.

### The engine ↔ controller seam

`engine.ts` produces, per input batch (one RO delivery, one data-length
change, or one viewport resize):

```
EngineUpdate {
  window: [startIndex, endIndex]          // rows to mount
  totalSize: number                       // container height
  compensation?: {                        // at most one per batch
    kind: 'remeasure-above' | 'head-splice',
    delta: number,                        // px the content above moved
    target: number,                       // absolute scrollTop that holds position
  }
}
```

The adapter applies `window`/`totalSize` to the DOM and forwards
`compensation` to the controller via a new controller entry point
`applyEngineCompensation` — the renamed successor of
`applyVirtuaScrollCompensation`, backed by `resolveEngineCompensation`
in the resolver (today's tiers ①–④ and ⑥ survive; tier ⑤'s
decline-store-poke protocol dies because there is no second model to
desync — a decline simply means "the pin write this frame already
covers it").

Wiring inside `MessageTimeline` stays a props/handle relationship; the
component owns the row template and the pane choreography exactly as
today.

### Handle and props (parity contract)

Adopted verbatim from inventories Part B §Cutover-surface summary:

- Handle: `scrollToIndex(index, {align, offset})`, `getScrollOffset()`,
  `getViewportSize()`, `getScrollSize()`, `findItemIndex(offset)`,
  `getItemOffset(index)`, `revalidate()` (new — the explicit pane-move
  geometry recheck that replaces the `scrollTo(getScrollOffset())`
  self-rewrite hack), `sizeAt(index)` (new — geometry-probe
  introspection replacing `getCache()[0]`).
- Props: `data`, `getKey`, `estimateSize` (56px fallback under
  priors), `bufferSize` (px, symmetric), `shift` (head-splice hint,
  same one-flush contract `pendingTimelineShiftAtHead` implements
  today), `scrollRef` (external scroller — engine never owns the
  container), `onscroll`, `onscrollend`, `renderAll` (first-class test
  seam replacing the `ssrCount: 100_000` shim), `getPrior(index)`.
- `scrollToIndex` is implemented as: engine computes the target offset
  (with virtua's lazy-recompute-on-remeasure convergence pattern,
  inventories §A5), and the **write goes through the controller
  chokepoint** — which retires the `runExternalScroll` wrapper
  requirement for chat (the controller performs the scroll itself and
  tags it natively). `runExternalScroll` stays in the controller API
  for genuinely external writers, but chat's six wrap sites die.

### DOM contracts (all preserved, per inventories §B7)

`[data-row-index]` wrappers with `contain: layout style` and
`visibility:hidden`-until-measured; container `position:relative;
height:{totalSize}px; contain: size style; overflow-anchor:none;
flex:none`; `contentEl.scrollHeight === totalSize` exactly; the
`{#key pane.threadId}` remount boundary; scrollEl styles
(scrollbar-gutter, composer padding, fade mask) unchanged. The
`pointer-events:none`-while-scrolling behavior virtua applied is
**dropped deliberately** — it caused hover loss during scroll, the app
never asked for it, and if scroll-jank measurements ever justify it,
it can return as an explicit controller-driven class.

### Priors (replaces the cache-replay dance)

`priors.ts` keeps `threadVirtuaSizeCache`'s proven persistence design —
LRU of measured-size snapshots keyed by
`{width, structureSignature, expansionSignature}`, same invalidation
call sites — but consumption changes from "constructor-once `cache`
prop resolved in `$effect.pre` before a keyed remount" to
`getPrior(index)`: the engine reads priors lazily whenever a row is
unmeasured (mount, or index shift), measured sizes always win, and a
key mismatch degrades per-row to the 56px estimate instead of
all-or-nothing. The `{#key}` remount stays (it's the per-thread state
reset), but the pre-resolve dance, the `virtuaReplayCacheThreadId`
dedupe, and the constructor-once footgun comments all die.

Scope (§8 D2): **kind-based fallback estimates ship in V0** — when
priors miss, the estimate comes from a small static per-kind table
(tool row, prose block, diff card, …; tuned during the V2 soak)
instead of one flat 56px, so cold threads predict closer to reality.
Partial-key reuse (consuming measured sizes under a mismatched
width/structure key) stays **rejected**: it is the only variant that
could place rows at knowably-wrong offsets. Estimate quality is the
safe place to be aggressive because priors only ever predict
*placement* — they never decide what a row renders. Measured sizes
always win, mount-time correction hides behind the warm gate, and
scroll-time correction rides the compensation path.

### Streaming hot path (the headline optimization)

Tail append while pinned becomes one pass: data-length change → engine
extends sizes with prior/estimate → `totalSize` grows → adapter sets
container height → controller's existing contentRO/notify path pins as
today. No second model reacting to the scroll event, no jump
accumulation, no frozen-range unfreeze protocol. Tail-row remeasure
(streamdown growth) invalidates only the tail of the prefix-sum memo
(watermark math, inventories validation note 1) — O(changed rows), not
O(window).

The second observation layer — the controller's contentEl RO — merges
into engine events **in this branch, as V4** (§4). It runs after the
cutover and deletion phases have their own green gates, so the engine
swap and the observation-source swap stay separately bisectable, but
nothing is deferred past the branch merge (§8 D4).

## 3. What we take from upstream (MIT, attributed)

Per inventories Part A §9. Ported files carry a header:
`Portions derived from virtua (https://github.com/inokawa/virtua),
Copyright (c) 2022 inokawa, MIT License` and the license text lands at
`frontend/src/lib/utils/virtual/VIRTUA_LICENSE`.

- `cache.ts` + `cache.spec.ts` → `sizes.ts` + `sizes.test.ts` (surgery:
  one-splice prepend, watermark-preserving shift invalidation, drop the
  median estimator).
- `createResizer` shape → the adapter's RO (lazy single RO, WeakMap
  index map, one-batch dispatch, `offsetParent` guard; content-box
  measurement kept — row margins stay contained by the existing
  `flow-root` contract and its oracle).
- The `isJustJumped` ±1px self-echo predicate → intent's programmatic
  tagging already covers this; keep the constant as a cross-check in
  the compensation observation.
- Style contracts (each line a shipped upstream bug fix) → adapter.
- Scrollend debounce + wheel-continuation inference (150ms/50–150ms) →
  the adapter's `onscrollend` synthesis, matching the timing the
  harness and snapshot logic already assume.
- Read-only edge-case references: `shouldKeep` issue taxonomy,
  scroll-to-unmeasured convergence loop, elastic-bounds guard,
  teardown-bug do-not-reproduce list (inventories §A8).

## 4. Phased build (single feature branch, commits per phase)

Roughly 1.5–2.5k lines of new code + tests; each phase leaves
`make check` + `pnpm test` green, browser suite green from V1 on.

### V0 — pure core

`sizes.ts` (+ ported spec), `window.ts`, `engine.ts`, `priors.ts`,
`types.ts`, with exhaustive unit tests: size-store matrix (ported),
window computation over synthetic geometries (seeding from tail,
symmetric buffer), engine reducer matrix
(append/prepend/remeasure/viewport × pinned/reading × measured/prior/
estimate → window + compensation), priors lifecycle (hit, mismatch
degrade, kind-estimate fallback, invalidation). Gate: unit suite
green. No DOM, no component.

### V1 — adapter component

`TimelineVirtualizer.svelte` + `VirtualRow.svelte`: single RO,
keyed-each rendering, absolute positioning, style contracts, handle
export, `renderAll` mode, scrollend synthesis, teardown correctness
(destroyed-flag on the mount tick, dispose cancels everything —
upstream's bug list as the checklist). New
`virtualizer.browser.test.ts` in the browser project via
`timelineBrowserHarness`: mount/measure/window/evict/return, tail
seeding, `totalSize === scrollHeight`, teardown under remount churn.
Gate: unit + browser green (existing suites untouched).

### V2 — cutover

- MessageTimeline: swap the mount; map the handle call sites
  (mechanical per inventories §B3); wire `getPrior`; delete the
  cache-replay dance, the marking wiring, the applier `$effect`, the
  TypeError teardown guards, and the host-layout self-rewrite (new
  `revalidate()`); keep the restore/warm-up choreography untouched.
- Controller: `applyVirtuaScrollCompensation` →
  `applyEngineCompensation`; resolver:
  `resolveVirtuaCompensation` → `resolveEngineCompensation` (tier ⑤
  simplifies, others carry over with their provenance comments);
  `onBeforeScrollTopWrite` option deleted from types + chokepoint.
- Tests: `StubVirtualizer` → `StubTimelineVirtualizer` (same shape,
  no marking recorder); scroll.test.ts cache-replay block → priors
  block; resolver compensation matrix renamed/adjusted.
- Gate — the actual acceptance bar: full unit suite, full browser
  suite **with the four outcome files unchanged**, `make check`,
  production build, and a manual dev soak (`make dev`) on: long-thread
  streaming follow, scroll-away-and-read during streaming, thread
  switch hot/cold, load-older during scroll, row expansion, pane
  reorder, search jump.

### V3 — deletion + docs

Remove: virtua dep + patch + lockfile entry + `pnpm-workspace.yaml`
patch key; both patch fixtures + both patch browser tests +
`virtuaShiftCache.test.ts` (replaced in V0 by head-splice tests) +
`messageTimelineVirtuaMarking.test.ts` + `virtuaMarkRecorder`;
`threadVirtuaSizeCache.ts` (superseded by `priors.ts`). Re-justify or
simplify: chat's `runExternalScroll` wraps (die with
controller-performed scrollToIndex), `widthReflowActive` export,
resolver virtua-branch comments, stale comment mentions (~35 files,
listed in inventories §B8). Warm gate: untouched through V3 (priors
make it near-idle on revisits); V4 re-sources it from engine
settlement. Docs: frontend-scroll.md owners + virtua sections,
frontend/AGENTS.md vendor-patches entry, chat AGENTS.md operational
rules, scroll-rearchitecture-plan.md Stage-5 verdict. Gate:
`make verify` + `scripts/release-check.sh`.

### V4 — observation-source unification (contentRO merge)

Chat stops running a second ResizeObserver over `contentEl`. The
adapter already knows every content-height change synchronously —
data-length change, remeasure batch, width-driven reflow — at the
moment it writes the container height, so it feeds the controller's
content-observation seam directly: same observation shape, one frame
earlier, no duplicate layout read on the streaming hot path. The
controller's observation *source* becomes pluggable at the
`observers.ts` attach seam; ChannelView (no virtualizer) keeps the
RO-backed source unchanged. Width-reflow classification re-sources
from the engine's viewport input. Warm gate: the content-quiet input
re-sources from engine measurement traffic, and per-row settle
knowledge (all mounted rows measured, priors resolved, no pending
remeasures) lets a priors-hit revisit reveal immediately instead of
waiting out the quiet timer — async late growth (mermaid, images)
still holds the gate exactly as today. Gate: full unit + browser
suites (four outcome files still unchanged), `make verify`, and a
repeat of the V2 manual soak — this phase rewires the streaming hot
path's observation plumbing, so the soak is not optional.

## 5. What dies (net effect)

The pnpm patch and its two tripwire browser suites; the marking
option/wiring; the applier seam-as-patch; the cache-replay
choreography (~6 files touched); the host-layout retry ladder's
self-rewrite; two TypeError teardown guards; the directional
buffer-drop failure class; chat's external-scroll wrapping; the
version-coupled `virtuaShiftCache` tripwire; the `ssrCount` shim;
chat's second content-observation layer (the contentEl RO, V4); ~35
stale comments. Expected net LOC across the repo: negative, before
counting the deleted patch infrastructure.

## 6. Risks and mitigations

- **Unknown-unknown windowing edge cases** (the reason virtua had
  years of issues): scope is narrow — one mount, vertical, desktop
  webview, external scroller — and the browser outcome tests +
  margin-divergence oracle + dev traces are already tuned to catch
  exactly this class. Mitigation: V1's dedicated browser suite runs
  the gesture harness against the engine before any production wiring.
- **happy-dom determinism**: `renderAll` is a first-class engine mode
  (not a store freeze side-effect), so the unit-project behavior is
  better-defined than today's `ssrCount` trick.
- **Fractional-DPR / WebKitGTK quirks**: already paid for — the
  epsilons and the arrival-readback handling live in the controller,
  which keeps owning every write.
- **Selection/hover during eviction**: same keyed-each + registry
  model as today; dropping `pointer-events:none` is strictly less
  intrusive. Watched by `remountReturn.browser.test.ts`.
- **Scrollbar drag during window changes**: intent machine's drag
  session logic is engine-agnostic and untouched.
- **One-shot scope risk**: phases are independently green and
  independently revertable; V2 is a single commit that can be reverted
  to leave V0/V1 (pure additions) in place, and V4 is a single commit
  on top of a fully-cut-over V3 state.

## 7. Test strategy

- **Acceptance oracle (unchanged)**: the four outcome browser suites +
  the margin-divergence trace + controller/resolver suites.
- **Ported**: virtua's cache spec (≈1.3k lines) → `sizes.test.ts`.
- **New**: engine reducer matrix (V0), adapter browser suite (V1),
  priors lifecycle + head-splice store tests (V0), stub swap (V2).
- **Deleted**: patch tripwires, marking seam test, shift-cache
  tripwire, cache-replay tests — each replaced by a bespoke-side
  equivalent listed above, not silently dropped.

## 8. Decisions (open questions resolved 2026-07-02)

1. **D1 — Naming**: as recommended — package `utils/virtual/`,
   component `TimelineVirtualizer.svelte`, handle
   `TimelineVirtualizerHandle`.
2. **D2 — Priors scope**: exact-key reuse (today's cache semantics,
   minus constructor-once) **plus kind-based fallback estimates**,
   both in V0. Aggressive on estimate quality, never on reuse under a
   mismatched key — priors predict placement, they never decide render
   content, and width or structure/leaf-content changes are key
   *misses* by construction, not wrong hits (see §2 Priors).
3. **D3 — Cost-aware retention**: cut entirely — no hook. Off-screen
   DOM is not worth optimizing for (user call). Measured sizes survive
   eviction in the size store, so scroll-back through previously-seen
   rows keeps stable geometry without retained DOM; residual shift
   (never-measured regions, async re-typeset transients) rides the
   compensation path. A policy hook with no consumer is speculative
   configurability — `window.ts` ships symmetric-buffer only.
4. **D4 — No follow-ups; full cutover**: the contentRO merge moves
   in-branch as V4, and the pointer-events question resolves inside
   the V2 soak. Nothing on this plan's surface is deferred past the
   branch merge. Internal phase commits with independent green gates
   stay — that is bisectability, not deferral.

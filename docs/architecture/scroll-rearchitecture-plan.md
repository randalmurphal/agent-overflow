# Scroll System Re-Architecture Plan

Status: APPROVED 2026-07-01 — in execution, stage by stage. Decisions taken
at approval: the browser suite gates into both `make test` and `make verify`;
adjacent cleanup and test refinement (deleting implementation-theater tests,
adding useful coverage) fold into each stage rather than waiting.
Baseline: commit `f42dc6e6` (virtua manual-scroll marking + fractional heights),
branch `scroll/settle-flicker-vibration`.

Inputs: six full-code inventories (stick controller: 45 mechanisms;
MessageTimeline: ~30; geometry caches: 5 layers; virtua dependency surface:
core verified from shipped source maps; consumer seams: all 57 raw-scroll hits
classified; test contracts: 399 tests classified outcome-vs-implementation).
Verbatim inventory reports:
[`scroll-rearchitecture-inventories.md`](scroll-rearchitecture-inventories.md).
Load-bearing claims were spot-verified against the code before inclusion.

## 1. Why this system is fragile (root causes, evidence-backed)

*(Root causes and line citations describe the pre-Stage-3 baseline; the
descriptor gate and the HOLD > RETAIN coupling below were deleted in
Stage 3c — see the Stage-3 verdict table.)*

1. **One shared mutable scalar, four writers, no ownership protocol.**
   `scrollTop` is written by the controller, virtua's `$fixScrollJump`, the
   browser (clamping), and the user. Every writer must guess what the others
   meant. The ~290-line property-descriptor gate
   (`useStickToBottom.svelte.ts:610-672, 2464-2688`) with nine decision tiers
   and two carve-outs exists *only* to arbitrate one of those writers, and
   each tier is a shipped regression (five documented bug-report cycles).
2. **Intent is unobservable, so it is inferred.** Tags, token FIFOs, 1ms
   deferred checks, resize-correlation budgets, version counters — all
   heuristic reconstructions of "who caused this scroll event." The root
   settle-flicker bug was exactly this in the other direction: virtua
   classifying our pin writes as user scroll-downs.
3. **Closed feedback loops amplified by sub-pixel noise.** Fractional DPR,
   `Math.round` residues, and LayoutUnit quantization turned no-op cycles
   into visible vibration twice (idle width oscillation `a5a5d032`, idle
   re-pin vibration `db77ba0d`, settle flicker amplifier `f42dc6e6`).
4. **Correctness is per-painted-frame.** A one-frame-wrong scrollTop is a
   user-visible flicker, so nothing can be deferred or debounced; every
   mechanism must run synchronously in the correct intra-frame phase
   (rAF-before-RO ordering is why the sentinel oscillation recovery exists
   twice, at `useStickToBottom.svelte.ts:1229` and `:1657`).
5. **Bugs live in the seams between ~10 interacting state machines** (spring,
   sentinel, warm-up, gate, intent, floors, replay, shift, prune, latch) —
   each individually defensible, each with cross-couplings like the
   `SPRING_MODE_HOLD_MS > RETAIN_ANIMATION_DURATION_MS` cross-file invariant
   that exist only to keep another machine's gate closed.
6. **The "second source of truth" anti-pattern recurs.** Two width sources
   (fixed in `a5a5d032`), two shift owners in MessageTimeline, two snapshot
   systems (timeline + diff sidebar), floors duplicating virtua's size store,
   integer heights beside fractional DOM.

The convergent conclusion of all six inventories: the fragility is
architectural (multi-writer + inference), not incidental. The fix is
ownership + observability, delivered in stages that are each independently
green and revertible.

## 2. Target architecture

### Ownership model

- **The controller is the only programmatic `scrollTop` writer.** virtua's
  jump compensation is *routed through* the controller (applier patch, §4
  Stage 3) instead of intercepted after the fact. External navigation
  (`scrollToIndex`) already funnels through `runExternalScroll`; under
  routing its time-window heuristic becomes an owned write.
- **One resolve per frame.** A pure reducer receives the frame's
  observations (content delta, composer delta, virtua jump request, scroll
  events with their classification inputs, lease state, intent state) and
  emits at most one write decision. Runs post-RO (RO callbacks feed
  observations; resolving pre-RO recreates the one-frame-stale paint that
  today forces the duplicated oscillation recovery).
- **Intent stays event-sourced, never geometry-inferred** (unchanged
  principle; the machinery shrinks because the resolver removes the 1ms
  deferral and its version counters, but keeps a
  "was this scroll layout-correlated" input — layout-induced scroll events
  also come from browser clamp and ChannelView, not just virtua).

### File layout (target)

```
frontend/src/lib/utils/scroll/
  resolver.ts        pure reducer: observations -> WriteDecision (exhaustively unit-tested)
  intent.ts          escape/re-stick/consent state machine (wheel/key/touch/scrollbar/selection)
  spring.ts          animation strategy: spring kinematics, sentinel-free retain, reduced-motion
  observers.ts       RO/scroll/pointer plumbing -> observation events
  index.svelte.ts    wiring + public API + lifecycle (attach/detach)
```

Each file <500 lines. `springAnimationLatch.ts` stays separate and outside
(data-layer semantics; correct as-is). MessageTimeline keeps restore
orchestration, paging, and window-anchor transactions — component-scoped
choreography, not controller business.

### Public API (target, from consumer-matrix evidence)

Every external consumer need maps 1:1 onto five concepts; the 21-member
surface is only fully exercised by MessageTimeline itself.

- **Pane/consumer surface (~7):** `isAtBottom` read, `lease()`
  (depth-counted; release-at-depth-0 re-pins synchronously — hard contract),
  `observe(kind)` (merges the 4 notifies; source hint:
  `content | live-content | composer-geometry | host-layout`),
  `preserveAnchor()` (13 row components via helper; keeps its transaction
  shape), `preserveTimelineWindowAnchor` (**must keep synchronous veto
  return** — thread store's defer logic reads `operationApplied` immediately),
  snap/escape intent.
- **Timeline-private surface:** warmup(arm|skip), routed external scroll,
  structural one-shot, restore.
- **Merge the restore triple:** `armRestoreSnap` + `forceStick('restore')` +
  `markAtBottom` consent-consume are one thread-restore transaction split
  across three calls purely by consumer `$effect` timing. Becomes one
  idempotent `restoreToBottom()` with consent handled internally.
- **Delete now (production-dead, verified):** `animateScrollTo`,
  `stopScroll`, `PaneScrollController.isAtBottom?` declaration.
- **Kill the handle-widening:** `Object.assign(stick,
  {notifyHostLayoutSettled, preserveTimelineWindowAnchor})` in
  MessageTimeline makes pane→timeline callbacks masquerade as controller
  methods. Replace with an explicit pane-facing interface the timeline
  implements.

## 3. Disposition inventory

Verdicts by lifecycle stage. "Gate-dependent" = deletable only at Stage 3.

### Delete immediately (Stage 1, no architecture change)

| Item | Where | Evidence |
|---|---|---|
| `animateScrollTo` + eased-scroll helpers | `useStickToBottom.svelte.ts:2170-2224, 1179-1207` | zero production callers (2× verified); stale docstring claims handleLoadOlder uses it |
| `stopScroll` | `useStickToBottom.svelte.ts:2100-2104` | zero production callers |
| `PaneScrollController.isAtBottom?` | `threadPaneShared.ts:151` | declared, never read through the pane |
| `lastEffectPreAt` diagnostic | MessageTimeline | write-only diagnostic |
| Floor rebind/content-swap re-arm — **DONE: deleted** (subsumed by the floor-system deletion, `b3bf9d55`) | `timelineRowGeometry.ts:154-181` (file deleted) | comment admitted trigger was unreachable today |
| Stale comments/docstrings | `MessageSearch.svelte:153`, controller `:418`, chat AGENTS.md method list | reference deleted/renamed mechanisms |

### Delete pending one capture experiment each (Stage 1)

| Item | Experiment | Expected outcome |
|---|---|---|
| **Per-row min-height floors** — Layer A state machine + Layer B height cache + `timelineRowGeometrySignature.ts` + retention/invalidation plumbing + HOLD path + 750ms timer + `hasSettledHeight` gate + `rowSettleFloor.browser.test.ts` + `timeline.row.geometry` taps — **DONE: deleted.** Capture experiment (mermaid-bearing scroll-away/return, floors on/off × content caches intact/defeated) showed floors-OFF outcome-identical to floors-ON on the stock build; all 9 stock-build "holds" were 0.23px fractional-compare artifacts. Width observer survives as `scrollSurfaceWidth.ts`; outcomes pinned by `remountReturn.browser.test.ts`. | Count `hold` trace events scrolling back over an image/mermaid-bearing thread on the patched build | Thread switch: floors provably contribute nothing (`rowUiState.clear()` at `thread.svelte.ts:1918` wipes the cache before the incoming thread renders — verified). Streaming: inert post-fix (skip-settled no-ops). Only residual duty is async-short remounts; fix those at the **content layer** (intrinsic dimensions on attachment `<img>`, min-height placeholder on mermaid hosts) — row-local, and covers first-ever mounts no cache can. Floors' 2 shipped bugs (`4a4f07ed`, `a5a5d032`) already exceed their documented saves; pre-floor churn was "invisible" per the analysis doc. |
| Oscillation snaps (spring-tick + contentRO twins) — **MOVED to Stage 2**; **Stage-2b verdict: RO-side collapsed into the resolver (`isSentinelOscillationStranded` + the `contentRO.oscillationSnap` decision); rAF-side RETAINED** — its inline check is deliberately weaker (exact inequality + arrival-readback carve-out) and covers strand-and-restore cycles with no contentRO delivery. Deleting it outright still needs the streaming capture | Count `spring.oscillationSnap` in a post-fix streaming capture | The "virtua row remount" dip source was plausibly the fixed buffer churn; streamdown-oscillation and browser-clamp legs may keep a *single* (resolver-phase) recovery |
| `IDLE_REPIN_DEADBAND_PX` — **MOVED to Stage 2**; **Stage-2b: DONE** — carried as the explicit `idlePinWithinDeadband` branch in `utils/scroll/resolver.ts`; outright deletion still needs the idle fractional-DPR capture | Idle-thread capture on fractional-DPR display | Fractional height caching may have removed the flip driver; deadband may be a second belt on a fixed suspender |
| Scroll-token duplicate budget (4) — **MOVED to Stage 2** (one write/frame makes the multi-token case structural, not tuned) | Streaming capture of token consumption | Plausibly tuned during churn era; 1-2 may suffice — fully moot under one-write-per-frame |

Deletion traps (must survive floor removal): `observeScrollSurfaceContentWidth`
+ `scrollSurfaceContentWidth` also feed the CacheSnapshot validity key — **keep**, and
keep the `a5a5d032` one-box/one-async-source invariant. The
`[data-row-geometry-content]` attribute + `flow-root` rule is margin
containment (`4b3759a1`), independent of the floors — keep or re-anchor
deliberately.

### Consolidate (Stage 1-2)

- ~~Two `shift` owners → one~~ — RECLASSIFIED KEEP during Stage 1: the two
  signals exist because their decision inputs live in different layers (the
  store owns load-flow shifts over items; pure-keyed-head-drop detection
  needs grouped node KEYS, a component-level projection). Merging would
  move a render-flush-timed concern into the store without deleting either
  lifecycle. The single `virtualizerShiftAtHead` derived is the one owner.
- ChannelView's escape→arm→restore switch dance, controller publication
  effect, attach effect, composer-RO notify, chip wiring — duplicated with
  MessageTimeline. DEFERRED to Stage 4: `restoreToBottom()` as a
  single-call API absorbs the dance naturally; extracting a shared helper
  now would be churned again by the API shrink. (Also corrected: the test
  inventory's "ChannelView has zero scroll tests" claim is false —
  ChannelView.test.ts covers overflow-anchor, initial-load sync-pin, chip
  reveal/escape/forceStick, escaped-while-posting, and composer-resize
  re-pin. The only hole is the switch-dance ordering itself; cover it in
  Stage 4 alongside the API change.)
- Redundant self-tag pair: `ignoreScrollToTop` exact tag + token FIFO
  (`exactTagged || tokenTagged` at `:1911`) → one expected-value check under
  a single write site. **Stage-2 verdict: DONE — `ignoreScrollToTop`
  deleted; the token FIFO (TTL 500ms, duplicate budget, cap 128) is the sole
  self-tag mechanism. The exact tag had no TTL, so a stale tag could swallow
  a real user scroll landing at the same value arbitrarily later — the
  tokens-only form is simpler and strictly more correct. The duplicate
  budget stays at 4 unchanged: event-side duplication (browser re-firing
  scroll for one write) is independent of how many writes we make.**
- `lastTargetChangedAt` bumped at 4 sites → resolver-internal.
- (Optional, low priority) DiffSidebarBody's private snapshot/restore is a
  second scroll-persistence system; panel-local, so acceptable — note only.

### Delete at Stage 3 (dies with single-writer routing)

| Item | Lines | Why it exists today | Stage-3c verdict |
|---|---|---|---|
| scrollTop descriptor gate: install/uninstall + 9-tier setter + anchor-redirect + magnitude carve-outs | ~290 | Sole purpose is filtering virtua's direct `$fixScrollJump` writes; every tier is a drop-vs-pass regression. Suppression desyncs virtua's model (it re-syncs *from scroll events*) — under routing, "decline to write" pokes ACTION_SCROLL instead, safe by construction | **DELETED.** Implementation-level gate tests deleted; decision-level coverage moved to `resolver.test.ts` + the applier describe; outcome coverage in `compensationOutcome.browser.test.ts` |
| Sentinel gate-holding rAF loop (zombie rAF keeping `springToken` nonzero) | ~40 | Keeps the gate closed across >350ms inter-chunk gaps | **KEPT (divergence).** The loop was never gate-only: it (a) keeps `springActive` true for the compensation resolver's decline tier — the routed replacement for "keep the gate closed across gaps"; (b) keeps the negative-delta carve-out engaged; (c) owns `sentinelEntryTarget`, the oscillation-recovery state both snap sites consume. Only its gate-referencing comments died |
| `SPRING_MODE_HOLD_MS > RETAIN_ANIMATION_DURATION_MS` cross-file invariant + its test | — | Only keeps the gate closed for the sentinel's lifetime | **DELETED** (the invariant + its test + the wire-round-gap describe). The compensation resolver is mode-free; a compensation after sentinel death resolves through pass/redirect, both safe. Constants remain as independent tuning values; a wire-round-gap redirect test at the applier seam (real latch) replaces the describe |
| `resizeCorrelatedUntaggedScrollBudget` | ~15 | "virtua emits one untagged scroll jump per measurement correction" — routed writes are tagged by construction | **KEPT (divergence).** Routing removed virtua's untagged jump, but browser clamp scrolls (scrollHeight shrinking under `scrollTop + clientHeight` from virtua's row-RO style mutations) are still untagged, resize-correlated, and race the same 1ms+rAF clear. Comment updated to name the remaining producer |
| Negative-delta re-pin's mid-chase spring carve-out | ~20 | Two racing responses (RO pin vs spring) to virtua's estimate/correction pair | **KEPT (divergence).** The race is controller-vs-controller (sync pin vs in-flight spring over contentRO deltas); routing virtua's *scrollTop writes* removes neither racer. Also the sentinel's reason-to-exist (b) |
| H1/H2 structural nudge machine + M1 rAF retry ladder (MessageTimeline) | — | Compensations for unobservable virtua timing; resolver absorbs | **DEFERRED (divergence).** "Resolver absorbs" assumed a per-frame resolver; the shipped Stage-2 resolver is delivery-driven. H1/H2 compensates for deliveries that never happen (thinking rows tail-pin internally → no outer RO delta), M1 for `listRef` being unbound during pane reorder — neither is a virtua write-path problem. Re-evaluate at Stage 4/5 |
| `runExternalScroll`'s 100ms time-window heuristic | — | Navigation becomes an owned write; the wrapper API stays | **KEPT (divergence).** Only the compensation write was routed; scheduled scrolls (`scrollToIndex`) still write the DOM inside virtua, and the window is what classifies their scroll *events* as non-user. Its gate tier died with the gate. Routing scheduled scrolls (the optional patch extension) would retire it — Stage 4/5 candidate |

### Keep (independent of virtua and the churn)

Geometry bands and near-bottom epsilons; width-reflow settle window; warm-up
gate (until Stage 5 makes it deletable *by construction*); quiet-context
signal; spring kinematics (frame-rate-independent integration, momentum
carry, arrival readback acceptance, overshoot policy small-leg,
reduced-motion); structural-append one-shot; write chokepoint (embryo of the
single writer); marking hook (evolves into the routing seam); self-tagging
(the DOM scroll stream is writer-anonymous forever); full intent machinery
(escape paths, recent-down-intent consent, scrollbar-drag session — gesture
vs layout-clamp disambiguation is native-browser, not virtua); restore
consent; leases; anchors; CacheSnapshot replay (Layer C — evidence-backed,
self-refusing key); scroll snapshots (Layer D — also drives slice-anchor
selection in the thread store, i.e. data loading); trace surface.

## 4. Staged migration

Each stage independently: green on all gates, committable, revertible.
No stage begins until the previous is merged and observed in real use.

### Stage 0 — Safety nets first

1. **Browser suite into the verify gate.** Today `pnpm test:browser`
   (8 tests, 2.4s wall, incl. the patch drop-rule tripwire and margin
   containment) runs in *no* gate, and CI (`release-build.yml`) runs zero
   tests — `make verify` is the only gate convention. `playwright` +
   `@vitest/browser-playwright` are already pinned devDeps; only the
   Chromium binary is missing. Two-line change: `make install` appends
   `pnpm exec playwright install chromium` (~170MB once per machine,
   idempotent), and `scripts/release-check.sh` runs `pnpm run test:browser`
   after the unit line.
2. **Streaming outcome harness** —
   `chat/streamingOutcome.browser.test.ts`. Mount the **real
   MessageTimeline with a real pane in Chromium**: `buildPane` and the
   bindings mocks already resolve in the browser project (shared resolve
   aliases, `vitest.config.ts:88`); drive beats via `pane.upsertItem(...)`
   + threadStatuses turn transitions (the same seam `scroll.test.ts` uses);
   import production `app.css`. Assert outcomes only: scrollTop
   monotonically non-decreasing while pinned and growing, distance-to-bottom
   ≤ epsilon at every quiet point, no `[data-row-index]` unmount bursts
   (MutationObserver counter, per the buffer-retention tripwire pattern),
   chip never appears. Two small enablers: a seam to disable
   `ssrCount=100_000` under the browser project (one production line —
   otherwise the initial flat render pollutes counters) and a browser
   `setupFiles` with the store resets (the browser project has none today).
   ~3-5s per scenario. This is the rewrite's safety net — it tests
   contracts, not mechanisms, so it survives every stage; it also closes
   the top coverage hole (§5) where the entire settle-flicker family lived.
3. **Commit the contract list.** The 27 must-survive behavioral contracts
   (§5) land as `docs/architecture/scroll-contracts.md` so every stage
   reviews against the same checklist.
4. **Runtime invariant oracles (dev-only)** — MOVED to Stage 2, then
   **CLOSED at Stage 2 with verdict: subsumed, no runtime oracle
   shipped.** What the oracle was meant to catch is now enforced
   structurally: the resolver's `ContentDecision` carries at most one
   write by type (≤1 write per RO delivery is unrepresentable to
   violate), `writeScrollTop(caller, value)` makes attribution a
   required parameter instead of mutable ambient state, and the
   resolver test's invariant sweep asserts the write budget and effect
   exclusivity over the full state × observation product at test time.
   The one thing a runtime oracle could still add — per-*frame* write
   budgets — stays imprecise for the reason above: a spring rAF-tick
   write and an RO delivery write can legitimately share a frame, so a
   naive per-frame check false-positives. Shipping that would be
   ceremony. Re-evaluate only if Stage 4's spring extraction makes
   phases first-class and a per-phase budget becomes the real
   invariant.

### Stage 1 — Prune and consolidate (no architecture change)

The "delete immediately" table, the capture experiments and their
consequent deletions (floors most prominently, with the content-layer
residual fixes landing in the same commit), and the consolidations. Every
deletion keeps or re-covers its outcome-level test; implementation-level
tests of deleted mechanisms are deleted with them (list in §5).

### Stage 2 — Per-frame resolver (pure reducer, behavior-preserving)

Extract the decision logic into `resolver.ts`: a pure function
`(state, observations) -> {write?: number, effects}`. The controller's
existing frame-ordering facts are the spec:

- Resolve runs post-RO, once; kills the rAF-before-RO duplicate recovery.
- Up to 2 writes per RO delivery today (overshoot snap then pin) collapse to
  one; the intermediate overshoot value never paints (observable change:
  none user-visible; tests asserting the intermediate write are
  implementation-level and rewritten).
- One write → one token; version counters around the 1ms deferral go, but
  the resolver keeps a layout-correlation input for untagged scroll events.
  **Stage-2 verdict on the version counters: RECLASSIFIED KEEP — git
  provenance shows both are genuine race guards, not churn-era scaffolding.
  `recentDownIntentVersion` (afce64c0) invalidates a down-intent captured
  before an escape so it can't re-stick after the user escapes inside the
  1ms deferral window. `scrollbarDragSessionVersion` (48d81531) implements
  keep-captured-events-across-pointer-release for scrollbar drags. Neither
  is tied to write multiplicity, so one-write-per-delivery doesn't obsolete
  them.**
- `writeCaller` attribution folds into the single write site.

Exhaustive unit tests over the reducer (every state × observation product
that today's 6400-line controller test file exercises via choreography).
Public API unchanged at this stage.

### Stage 3 — Single writer (virtua applier patch)

Extend `patches/virtua@0.49.1.patch` with the applier seam (verified against
un-minified core): an applier slot in `createScroller` + one-line branch at
the single compensation write (`scroller.ts:334-358` un-minified), exposed
through the Svelte adapter (~5 core lines + ~10 adapter/type lines).

Applier contract (from the core protocol): receives absolute target +
`(jump, shift)`; must act synchronously in the same call (runs post-flush
pre-paint); may write a different value (model re-syncs from the scroll
event); if it declines to write it must poke ACTION_SCROLL with the current
DOM offset (mirroring core's clamped-shift fallback) so suppression can't
desync; `shift` honored verbatim (head-anchor preservation).

Routing raises write volume through the chokepoint, and every routed write
records a self-tag token — check the token FIFO's cap-128 headroom in the
token-consumption capture (live tokens ≈ refresh-rate × 0.5s TTL ×
(writers-per-frame − 1); fine at 60-120Hz, brushes the cap at 240Hz with
three same-frame writers). Eviction blast radius is small (oldest/stalest
tokens go first), and the concern is moot once the resolver enforces one
write per frame.

Then delete the descriptor gate and everything in the "dies with routing"
table. **This deletion inherits five regression histories** (bug-reports
20260524T200233Z, 20260524T183128Z, 20260622T041049Z, revert-to-top,
seq-509 family) — each gets an outcome-level regression test against the
resolver before the gate is removed, plus a paired browser tripwire
("applier receives the delta / default write no longer fires") as the new
patch drop-rule.

**Stage-3c executed:** the gate (~290 lines incl. rationale + tests'
implementation tier) and the HOLD > RETAIN invariant are gone; per-item
verdicts — including the four table rows that did NOT die and why — are
recorded in the table's verdict column above. Gate-flavored tests were
rewritten at the `applyVirtuaScrollCompensation` seam (redirect quartet,
pause/escape/restore/mode-flip passes, sentinel decline/pass pair, plus a
new post-restore decline-re-arm test and a wire-round-gap redirect test
driven by the real production latch).

Cost acknowledged: the core hunk lands in minified, name-mangled code —
re-rolls on version bumps are more brittle than the current adapter-only
patch. Mitigation: the drop-rule tripwire fails loudly on a bad re-roll;
upstream proposal filed in parallel (novel — no existing issue requests
this; it would subsume `markProgrammaticScroll`).

### Stage 4 — API shrink + file split

The §2 target API and file layout. Consumers migrate mechanically
(matrix shows none need rework; ChannelView shrinks). The `Object.assign`
handle-widening is replaced by the explicit pane-facing interface.
Docs (`frontend-scroll.md`, AGENTS.md files) rewritten to the new ownership
map — the current doc's "do not add another owner" rule becomes "the
resolver is the owner."

**Stage-4 verdict: DONE**, shipped as three sub-stages — 4a API shrink
(`f207552e`), 4b file split (`416a9c4d`, `395d02b4`, `9ee748db`,
`7e5a05b1`, `b147d730`) plus its post-task-review pair (`d2a6df7f`
behavioral fix, `644aa6dd` seam tightening), 4c this docs pass.
Divergences from the §2 target, each deliberate:

| §2 target | Shipped | Why |
|---|---|---|
| `lease()` | `pauseAutoScroll()` keeps its name | Same depth-counted contract (release-at-depth-0 re-pins synchronously, test-pinned); the existing name says what it does and every consumer already used it — renaming was churn without information. |
| Restore triple → one idempotent `restoreToBottom()` | Partial fold only: `armRestoreSnap()` absorbed the defensive escape both consumers always paired with it; arm and consume remain separate calls | The arm must run in `$effect.pre` before DOM flush and the snap after, and a user gesture between the two must be able to invalidate the consent (the seq-509 gesture-invalidation window). A single call cannot hold that window open. |
| `observe(kind)` merging the four notifies | Shipped as specced (`content`, `live-content`, `composer-geometry`, `host-layout`; exhaustive switch) | MessageTimeline's pane adapter intercepts `host-layout` into its listRef retry ladder; ChannelView's composer RO reports `composer-geometry` (was the instant notify) — behaviorally identical on a no-animation surface. |
| `observers.ts` = "RO/scroll/pointer plumbing → observation events" | `observers.ts` owns the contentRO pipeline only (gather → resolve → apply, in one place); the scroll/pointer/wheel/key/touch handlers live in `intent.ts` | The plan drew the line at event kind; the shipped cut draws it at ownership. The DOM handlers ARE the intent machine's inputs, and a delivery's gather/decide/apply reads as one unit (mirroring the spring owning a chase). A thin observation-events-only layer would split single concerns across files. |
| Each file <500 lines | `index.svelte.ts` 886, `intent.ts` 699, `observers.ts` 538, `spring.ts` 521, `resolver.ts` 517 | Overages are comment mass (~40% of the controller is load-bearing prose) plus irreducible wiring. The post-task review judged the remaining clean seams (arrival-readback cluster + dev-hook extraction, ~−150 lines from the controller; the token ring, ~−55 from intent) not worth taking now; they are the path back under target if the files grow. |

Also under Stage 4: the shipped layout adds three files beyond the §2
five — `types.ts` (the consumer contract, re-exported through the
controller), `trace.ts` (shared dev-trace helper), `time.ts` (`nowMs`) —
and the controller file is `index.svelte.ts` imported as
`utils/scroll/index.svelte` (runes require the `.svelte.ts` suffix, which
directory-index resolution does not find). `writeProgrammaticScrollTop`
merged into the single `writeScrollTop` chokepoint; the module-state test
reset hook moved with its state to `scroll/intent.ts` as
`resetScrollIntentModuleStateForTest`.
The 4b post-task review (six lenses over the full split) found one
genuine behavioral drift — `quietContextSignal` snapshotted at
construction instead of read live per call, latent because no consumer
mutates its options object — fixed with a fails-without regression test
(`d2a6df7f`).

The two §3 rows marked "re-evaluate at Stage 4/5" were re-evaluated here
and both roll to the Stage-5 gate: H1/H2+M1 compensate for non-write-path
problems (deliveries that never happen; `listRef` unbound during pane
reorder) — the split changes nothing about either, and they die only *by
construction* with a bespoke virtualizer. `runExternalScroll`'s 100ms
window still classifies scheduled-scroll events because scheduled scrolls
(`scrollToIndex`) still write inside virtua; routing them is the optional
patch extension bundled into the Stage-5 decision.

Cleanup candidates carried out of Stage 4 (not commitments — evaluate
opportunistically or at the Stage-5 gate): folding the controller's
notify*/pause-release inline decisions into resolver-shaped decision
values; expressing the resolver's decision shapes as shared types;
deduplicating ChannelView's thread-switch dance with MessageTimeline's.

### Stage 5 — Decision gate: bespoke bottom-anchored virtualizer

Not scheduled — a decision point after 1-4 have soaked. Evidence from the
virtua inventory tilts further toward eventual replacement than expected:

- The dependency surface is small (1 mount, 6 props, 9 handle methods,
  head-only `shift`, no keepMounted/VList/horizontal; `itemSize=56` makes
  virtua's median re-estimation dead code for us).
- A bespoke engine is ~9 subsystems, only two genuinely hard (bottom-anchored
  one-resolve anchor policy — which *eliminates* virtua's entire
  jump/frozen-range feedback protocol and most of its shouldKeep table — and
  DPR-epsilon equality, already learned via `db77ba0d`). Intent
  classification already lives in our controller; no duplication.
- Nine actively-fought virtua behaviors vanish: buffer-drop marking (patch +
  wiring + two suites), `$fixScrollJump` second writer, 150ms scrollend
  debounce coupling, estimate→measure mount cascade (warm-up hide becomes
  deletable *by construction*: tail measures first, above-viewport error
  can't move a bottom-anchored viewport), margin-trap flow-root class +
  oracle + browser test, `pointer-events:none` hover loss during scroll,
  deferred `tick()` attach race, teardown TypeErrors (4 catch sites),
  version-coupled tripwires against a minified core.
- Against: it's new code with its own bug tail; virtua's edge cases (iOS
  momentum, wheel inference) took years to accumulate — though most are
  out-of-scope for a desktop webview app.

Criteria to pull the trigger: seam bugs persist after Stages 1-4, or a
virtua version bump forces a painful patch re-roll, or Stage 3's applier
proposal is rejected upstream and the patch surface grows.

## 5. Test strategy

Principle (from the test-contracts inventory, 399 tests across 10 files
classified): **outcome tests port, implementation tests are rewritten with
the mechanism they pin.**

**The acceptance list is 27 deduplicated must-survive contracts**, each with
provenance (six bug-report captures, two verbatim user reports): intent
model (8 — incl. the clamp-then-wheel lockout and Bug A moving-bottom
re-stick), programmatic-write/virtualizer arbitration (3 — incl.
mark-before-write and the sole-writer-during-follow matrix that distills
five shipped regressions), thread switch/restore (4 — incl. A→B→A replay
validity and switch-into-streaming), streaming motion quality (2 composite),
rows/geometry (5 — incl. one-width-source and never-re-floor-settled),
host/UX invariants (5 — composer same-frame re-pin, chip placement, CSS
opt-outs, paging/prune invisibility, item ordering). Committed as
`scroll-contracts.md` in Stage 0.

Known dies-with-implementation set (rewrite re-covers the contract, does not
port the test): trace-assertion tests, physics/velocity tests, gate-condition
coupling block, hysteresis-constant inequality, descriptor-restore, sentinel
`mockNow` choreography, `window.__stickState` assertions, floor tests (die
with floors), marking-seam tests (die if Stage 5 removes external sync
writes entirely — per the documented drop rule).

**Coverage holes worth knowing (11 found; the load-bearing four):**
(1) nothing tests stick-controller × real virtua × real RO timing
end-to-end — exactly where the settle-flicker family lived (Stage 0 harness
closes this); (2) every test registers a single `'main'` pane while
module-level controller state exists — cross-pane interference unexplored;
(3) ChannelView has zero scroll tests despite the doc claiming the same
contracts; (4) escape gestures never run against trusted input in a real
engine (`@vitest/browser` userEvent can close this later).

**Cost note:** the controller suite is 27.2s — dominant cost of the whole
frontend unit gate, mostly real-timer sleeps around spring warm-ups. Stage
2's pure reducer converts that choreography into fast exhaustive unit tests;
expect the suite to shrink in wall time while growing in coverage.

## 6. Open items — resolutions

1. **Browser suite gating scope** — RESOLVED: gated in both `make test` and
   `make verify`; `make install` provisions the Chromium binary.
2. **Stage-1 floor deletion** — proceed with the capture experiment first;
   content-layer residual fixes (img intrinsic dimensions, mermaid
   placeholder) land alongside.
3. **Stage-3 patch depth** — routing `$fixScrollJump` only; the
   scheduled-scroll write stays marked + wrapped.
4. **Stage-5 disposition** — stays a decision gate after Stages 1-4 soak.
5. **ChannelView test debt** — cover during Stage 1 once the shared
   switch-dance helper exists.

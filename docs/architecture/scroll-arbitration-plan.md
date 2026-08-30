# Scroll Arbitration Plan

Status: **shipped** (2026-08-01, all five sequencing items). The
durable rules live in [`frontend-scroll.md`](frontend-scroll.md); this
file is the rationale record, like
[`scroll-rearchitecture-plan.md`](scroll-rearchitecture-plan.md) before
it.

## Why

Five consecutive incidents (2026-07-31 → 2026-08-01) were each a pair of
*individually correct* mechanisms interacting:

| Incident | Writer A | Writer B | Local fix |
|---|---|---|---|
| bug-report-20260731T141600Z | auto-collapse release's bottom restore | structural append's armed spring | `yieldToStructuralAppend` |
| bug-report-20260731T211929Z | align-end navigation convergence | streaming growth of the destination row | destination-growth exclusion |
| bug-report-20260801T213259Z | head-splice compensation | sentinel oscillation guard | `invalidateSentinelBaseline` |
| bug-report-20260801T214455Z | prune's `restoreBottomEdge` | mid-glide spring | `autoScrollInFlight` stand-down |
| bug-report-20260801T214455Z (2nd head) | `pauseAutoScroll` release repin | same spring across a sub-frame lease | repin yield |

Earlier members of the same family: the seq-509 stale restore (fixed by
restore-snap consent), the scrollToIndex takeover guard, the
release-then-glide yank. Every one of these is the same defect:

> **A deferred one-shot absolute write, derived from a stale
> observation, lands while a continuous position program owns the
> viewport.**

The continuous programs are: the spring chase (+ its sentinel), the
engine's compensation stream, and a converging `scrollToIndex`
navigation. The one-shot writers are: anchor-transaction restores, the
pause-release repin, notify-path instant pins, and thread restores. Each
collision so far got a pairwise guard. Pairwise guards are O(N²): every
new mechanism must know about every existing one, and the next feature
re-derives these lessons as a user-visible bug.

A second, related problem: **structural mutations are scheduled at wire
events, not visual quiet.** The recent-window prune runs at
`settleTurn`. But the reveal smoother deliberately keeps draining for
seconds after the wire settles (the reveal is never rushed), so the most
expensive flush in the app (40–80ms measured; 78–186ms in production
traces) lands mid-glide by construction. The auto-collapse gate and the
row-UI prune each independently grew the correct "structural change +
scrollend, debounced, stand down while animating" cadence; the prune
never got it.

## Design

Four pieces. Each is a generalization of a fix that already shipped.
None is speculative.

### 1. Bottom-edge arbitration: `requestBottom`

The bottom is the only contested destination. Every collision above
except the head-splice one was two writers fighting over the trip to the
bottom. Replace the scattered one-shot bottom writes with one controller
entry point that states the priority rule once:

```ts
requestBottom(opts: {
  // 'claim'  — reader-asked (toggle restore, chip click): cancel any
  //            program and place instantly. The clicked delta never
  //            animates; that is the click's contract.
  // 'yield'  — system-asked (prune restore, pause-release repin,
  //            auto-collapse restore): if the bottom-follow program is
  //            engaged (spring active or armed), stand down — the
  //            program already owns the trip and re-reads the target
  //            every tick. Otherwise place instantly (or hand to the
  //            live-content path when a structural append is owed a
  //            glide).
  takeover: 'claim' | 'yield';
  // Virtualized panes place via scrollToIndex(last, align end) so the
  // write self-converges as measurements land; ChannelView's default is
  // the direct chokepoint write. Both end at markAtBottom-equivalent
  // flag state.
  write?: () => void;
}): void
```

What this deletes:

- `PreserveViewportBottomOptions.yieldToStructuralAppend`: every
  system restore yields by construction; reader restores pass `'claim'`.
- The `pauseAutoScroll` release's inline
  `structuralAppendPending() || isActive()` branch: the release calls
  `requestBottom({ takeover: 'yield' })`.
- `restoreTimelineWindowAnchorAfterPrune`'s inline `autoScrollInFlight`
  stand-down: its sticky branch calls the same thing.
- The duplicated `observe('live-content') + saveScrollSnapshot + return`
  choreography in both transactions.

What this deliberately does NOT change: the resolver stays the decision
authority for *deliveries* (contentRO, engine compensation); `forceStick`
keeps its consent gate (it is the `'claim'` path for restores and already
correct); `scrollToIndex` navigations keep the takeover guard (they are
programs, not one-shots, and the guard is their own revalidation, which
is the right shape for a program).

The invariant, stated once and testable: **while the bottom-follow
program is engaged, no system-initiated write may retarget the viewport;
user intent always may.**

### 2. Quiet scheduler: one cadence for structural mutations

Extract the cadence the auto-collapse gate and row-UI prune both
implement into one module (`components/chat/timelineQuietWork.ts`) and
make the recent-window prune its third consumer:

- **Triggers** (unchanged, now in one place): structural effects
  (`threadId` / `timelineRevision` / `revealBoundary` /
  `activityRuns.revision`), listRef/scrollEl attach, and
  `handleTimelineScrollEnd`. Debounced one tick.
- **Quiet condition**, shared: `!autoScrollInFlight()`. This is already
  the visual-quiet predicate: the sentinel holds `springActive` true
  across the whole reveal drain (liveness stamps on every reveal tick),
  so "spring idle" *means* "nothing is streaming visibly and no glide is
  running". The glide's own settle synthesizes the scrollend that
  re-runs the scheduler. Deferral loses nothing.
- **Sequencing**: at most one *geometry-mutating* pass per quiet
  callback, in deterministic order: (1) recent-window prune retry,
  (2) auto-collapse releases. The remainder is re-scheduled for the
  next tick, so two expensive flushes never stack on one frame. The
  row-UI prune mutates no geometry and always runs.

The prune migration itself:

- `settleTurn` stops calling `pruneToRecentWindowIfNeeded()` directly
  when a scroll controller is registered; it marks the existing
  `recentWindowPrunePending` flag instead (the deferred-retry plumbing
  `hasDeferredRecentWindowPrune` / `retryDeferredRecentWindowPrune`
  already exists for the anchor-veto case and is reused, replacing the
  `stick.isSticky`-keyed retry `$effect` in MessageTimeline).
  A pane with no registered controller (no mounted timeline) prunes
  immediately as today. No reader to disturb.
- The streaming append path and its active-turn defer are unchanged.
- `ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS` is unchanged and remains
  the only force: a run of back-to-back turns that never reaches quiet
  keeps deferring until the ceiling forces the prune mid-stream, exactly
  as a runaway single turn does today. (Measured headroom: 800 → 1600 is
  many turns; any quiet gap drains the pending prune.)

### 3. Provenance: evidence-based clamp detection

The sentinel oscillation guard exists to rescue `scrollTop` from a
**browser clamp** (content dipped, browser clamped, content restored,
leaving scrollTop stranded low). It currently *infers* the clamp from a baseline
equality (`target ≈ sentinelEntryTarget`), which is why an authored
head-splice displacement (same numeric shape) tripped it
(bug-report-20260801T213259Z) and needed `invalidateSentinelBaseline`.

Replace inference with evidence. The chokepoint already reads back the
browser-rounded `scrollTop` after every authored write (`taggedTop`),
and the intent machine already classifies user gestures. So the
controller can maintain a one-field ledger: **the last explained
scrollTop**, updated on every authored write and on every classified
user scroll. A sentinel tick that observes `el.scrollTop` differing from
the ledger has *witnessed* unexplained movement (a clamp, the only
unexplained mover left). The snap then requires baseline match **AND**
witnessed unexplained movement since sentinel entry.

What this deletes: `invalidateSentinelBaseline` and its head-splice call
site (an authored write updates the ledger, so it can never read as a
clamp), and the same evidence gates the resolver's
`isSentinelOscillationStranded` via a new snapshot field. Genuine
dip-restore recovery keeps working. The dip's clamp is exactly what the
ledger witnesses.

Risk note: this assumes a clamp changes `el.scrollTop` synchronously
with the layout that shrank `scrollHeight` (it does: clamping is part
of layout, not an async scroll event), and the sentinel tick reads
geometry fresh each frame. The interleaving suite pins both the
dip-restore recovery and the head-splice glide against the new
mechanism.

### 4. Interleaving invariants: test the class, not the instance

The five incidents were all *transitions*. States were individually
fine. Alongside the per-incident browser tests (kept), add a
combinatorial driver in the mock-geometry unit suite
(`utils/scroll/scrollInterleavings.test.ts`) built on the
`index.svelte.test.ts` scaffolding (geom object, RO fire, frame
advance):

- **Ops**: append-growth, prune transaction (pause + head-splice + restore
  + release), collapse transaction, bare pause/release, user escape,
  browser clamp, composer resize, thread-restore snap.
- **Starting states**: mid-glide, sentinel-idle, at-rest, escaped,
  paused.
- **Frame invariants** checked continuously, not per-scenario:
  1. While sticky and no `'claim'` op ran this frame: `scrollTop` never
     moves opposite the chase direction except by an authored
     compensation's exact delta.
  2. No single frame moves the viewport more than the spring's bounded
     step (velocity cap × catch-up steps) plus authored compensation
     deltas, unless the op is a declared snap (`'claim'`, oscillation
     recovery, reduced motion).
  3. Once inputs go quiet, the viewport reaches the current target and
     stays (no residual writers).

Every scenario runs the cartesian product of ops × states where the
combination is constructible, so the next pairwise interaction is a
failing test before it is a bug report.

## Sequencing

1. Quiet scheduler + prune migration (kills the settle-time stutter, the
   user-visible driver for this work). **Shipped** (62c101cb,
   2026-08-01): `timelineQuietWork.ts`, `settleRecentWindowPrune`, both
   gates migrated.
2. `requestBottom` + writer migration (deletes the pairwise guards).
   **Shipped** (2026-08-01): controller entry + trace record; migrated
   the pause-release repin, both anchor-transaction restores
   (`PreserveViewportBottomOptions.takeover` replaces
   `yieldToStructuralAppend`), and MessageTimeline's host-layout re-pin.
3. Provenance ledger (deletes the baseline heuristics). **Shipped**
   (2026-08-01): `lastExplainedScrollTop` at the chokepoint + intent's
   `noteUserScroll` (resize-correlated events excluded); both snap sites
   require `sentinelClampWitnessed` (latched per sentinel session);
   `invalidateSentinelBaseline` and its head-splice call site deleted.
4. Interleaving suite lands alongside 2 and 3 as their safety net;
   scenario browser tests keep covering real-layout behavior. **Shipped**
   (2026-08-01): `scroll/scrollInterleavings.test.ts` with 10 viewport
   ops × 5 starting states on the shared `testGeometry.ts` scaffolding.
   Op-time check: a system op that begins mid-glide may not author a
   write that lands at the bottom target (trace-attributed, so native
   clamps don't false-positive). Per drained frame: escaped viewports
   never move; motion is bounded by the spring's step envelope; sticky
   motion never runs counter to the chase; recovery snaps are forbidden
   outside the op that created their clamp evidence. Then quiet
   convergence: arrive at the bottom, liveness dies, the sentinel exits,
   and 20 frames of absolute stillness prove no residual writer. Both
   incident classes were mutation-tested: reverting the release repin to
   a direct write fails the three mid-glide lease cases, and removing
   the clamp witness from the sentinel guard fails the head-splice and
   composer cases from sentinel-idle.
5. Fold the shipped rules into `frontend-scroll.md`; update
   `chat/AGENTS.md` operational rules (the auto-collapse section's
   stand-down description moves to the scheduler). **Shipped**
   incrementally with 1–3: each phase folded its durable rules into
   `frontend-scroll.md` (§Intent And Programmatic Writes, §Live Window
   Bounds, §Run Height Changes) and `chat/AGENTS.md` as it landed.

Each step leaves `make check` + both test suites green and is
independently revertable.

## Measured baseline (2026-08-01, WSL Chromium harness)

For before/after comparison: single-append flush ~27ms total; 20-append
~51ms (~2–5ms per mounted markdown row); prune 800→500 with one mount
~33ms (sync data work 4.1ms: disposal, index rebuild, projection);
33 mounted short rows ~41ms. Pure projection over 770 items ~2ms.
Production traces: 78–186ms prune-correlated long tasks at settle,
mid-drain. Separate observation, out of scope: dropped-frame clusters
with zero long tasks (compositor/GC territory, likely WebView2/WSL
presentation path).

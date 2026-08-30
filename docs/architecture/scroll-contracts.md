# Scroll system behavioral contracts

The acceptance checklist for every stage of
[`scroll-rearchitecture-plan.md`](scroll-rearchitecture-plan.md). Each
contract is a user-observable behavior with shipped-regression provenance,
distilled from the full test-surface inventory (399 tests classified;
verbatim analysis in
[`scroll-rearchitecture-inventories.md`](scroll-rearchitecture-inventories.md)
§A6). A change that breaks one of these is a regression regardless of which
mechanism implements it. Contracts are numbered for review reference:
C1..C27.

## Intent

- **C1.** At-bottom threads auto-follow; growth pins same-frame, no lag.
- **C2.** Any upward user input (1px/sub-pixel/zero-movement wheel, keys,
  touch, scrollbar grab, middle-button, selection-while-scrolling) breaks
  follow synchronously. Same-frame growth must not pin.
- **C3.** While escaped, nothing layout-driven moves the viewport (no snap,
  pin, shrink re-pin, clamp, composer/live nudge).
- **C4.** Re-stick only via: input-backed scroll reaching bottom within ~4px
  (5px stays escaped; the 70px band is chip-visibility only), explicit
  forceStick, or wheel-down while already clamped at bottom (zero scroll
  events). In the bug-report-20260520T010930Z lockout, 180 wheel events
  could not re-stick.
- **C5.** Down-intent is fresh: expires (~300ms), cancelled by any up input
  including in a deferred-processing window; judged by distance seen at
  event time (Bug A: the bottom moves mid-window during streaming).
- **C6.** Intent mutates only on explicit signals, never inferred from
  geometry or untagged scrollTop direction (R4; applies to engine
  compensation observations and per-row resizes; virtua-era mechanism:
  `$fixScrollJump`).
- **C7.** Scrollend is inert; pinch-zoom (wheel+ctrl) is not intent; a
  wheel or touch gesture belongs to the scroller that consumes it.
  A registered nested scroller with room to move in that direction owns
  the gesture and the outer machine ignores it, and at the nested
  scroller's own edge the gesture chains outward and the outer machine
  reacts normally. Amended 2026-07-25: this previously read
  "nested-scroller wheel-up escapes the outer follow", which broke
  bottom-follow whenever a user scrolled inside a command-output,
  subagent, wait-group, or tool-result body. The outer pane never
  moved, so nothing about the user's relationship to it had changed.
  Attribution is a registry walk (`utils/scroll/wheelAttribution.ts`,
  opted into by the `nestedScroll` action), never a computed-style probe:
  wheel handling runs while layout is dirty mid-stream, so geometry reads
  stay confined to explicitly marked elements.
- **C8.** After re-stick/chip-click, every subsequent chunk follows, with
  no leaked one-shot state ("stops following until refresh").

## Programmatic writes / virtualizer arbitration

- **C9.** Controller writes are invisible to the intent model; each
  write records one self-tag token whose suppression is bounded (TTL +
  a small duplicate budget for browser-coalesced re-fires, so a genuine
  user scroll landing at the same value later is never swallowed); and
  a programmatic write must never trigger windowing buffer-drop remount
  churn: the streaming settle flicker, `./settle-flicker-analysis.md`
  2026-07-01 (under virtua this required announcing writes to the
  virtualizer before landing; the bespoke engine has no scroll-direction
  latch to mis-classify them, and `streamingOutcome.browser.test.ts` pins
  the outcome).
- **C10.** During active animated follow the controller is the sole
  scrollTop writer; escaped / paused / mount-cascade / post-restore /
  dormant / instant / viewport-scale corrections pass through untouched;
  brief wire-round gaps must not flip the arbitration (distills five
  shipped regressions: bug-reports 20260524T183128Z, 20260524T200233Z,
  20260622T041049Z, revert-puts-you-at-top, wire-round gap).
- **C11.** Settled-at-bottom: stale below-bottom anchor writes never paint
  even one frame short of the bottom.

## Thread switch / restore

- **C12.** Switch escapes first; restore-to-bottom needs one-shot consent
  invalidated by any gesture; stale restores lose to an escaped user;
  landing via the virtualizer then marking at bottom issues no redundant
  write; exactly one writer at restore plus one settle pass.
- **C13.** Per-thread position survives switch (anchor → same item, loading
  it into the window if needed; bottom → sticky, chip hidden); A→B→A reuses
  cached row geometry with a content-derived validity key (never a
  monotonic counter), keyed by width AND user expansion state (a
  force-collapsed override differs from force-expanded).
- **C14.** Cold loads: the measurement cascade stays hidden until it
  actually settles (evidence-based, never a bare timer while data loads);
  warm state doesn't leak across switches; a renderer settled-signal may
  shorten the reveal only when geometry holds still.
- **C15.** Switching INTO a streaming thread must not arm an append-follow
  animation over the restore backlog (bug-report-20260622T041049Z:
  multi-hundred-px scroll on switch).

## Streaming motion quality

- **C16.** Follow reads as continuous motion: frame-rate independent,
  bounded catch-up after stalls, no per-chunk restarts, never moving away
  from the bottom; total visual travel equals real content growth
  (estimate→measure pairs, phantom nudges, net-zero oscillations add zero);
  a net-zero oscillation whose low point browser-clamped scrollTop recovers
  synchronously (no one-frame strand, bug-report-20260615T182227Z).
- **C17.** Small mid-stream shrinks (~22px streamdown fence rebalance)
  cause no downward jitter; viewport-scale corrections land instantly;
  >350ms inter-chunk gaps don't degrade into snaps; width-reflow height
  changes pin instantly rather than animating; prefers-reduced-motion
  disables all animation.

## Rows / geometry

- **C18.** Row vertical margins are contained inside the measured row box;
  markdown first-children render flush-top (the rule is portable; the
  `[data-row-geometry-content]` + `flow-root` pairing is the current
  mechanism).
- **C19.** Height reservations (while they exist): exact fractional heights
  (rounding = ±0.5px settle-flicker amplifier); keyed to the measured
  width, not a laggy prop width; cold-mount bridge only, never re-floor a
  settled visible row (2-6px twitch); hold through transient short remount
  measurements; self-expire (no permanent wrong floor); invalidated
  synchronously on deliberate user height changes; pruned with the row
  window.
- **C20.** Row-geometry width has exactly one source (RO content-box) with
  no sync layout reads. Mixing sources recreates the idle
  width-oscillation loop (incident 2026-06-26, `a5a5d032`).
- **C21.** Collapsed reasoning tail keeps the newest glyph visible through
  width-only re-wraps with no text delta.
- **C22.** Idle at bottom under fractional DPR: sub-pixel geometry
  oscillation never sustains a write feedback loop (no shimmer,
  bug-report-20260701T012813Z), while genuine line-height growth still
  pins exactly.

## Host / UX invariants

- **C23.** Composer growth re-pins in the same frame (no 200-400px flash);
  the live-capable path lets active output spring through activity-rail
  height changes while idle composer geometry sync-pins.
- **C24.** Scroll-to-bottom chip lives outside the scroll container; shown
  iff escaped AND not at bottom; never stranded over a draft/empty pane.
- **C25.** `overflow-anchor: none` and symmetric scrollbar-gutter
  reservation on the timeline scroller; banners overlay without reserving
  height (no reflow on appearance).
- **C26.** Load-older: one batch per explicit gesture, never an
  auto-cascade, no request loops on null/in-flight/exhausted cursors (both
  edges). Window pruning is invisible: vetoed if it would drop the visible
  anchor; kept anchor restored; bottom re-pin must not consume user escape
  intent. Disclosure toggles keep sticky users at bottom and escaped users
  anchored to the toggled element, releasing after DOM flush. Pause leases
  are depth-counted, block every auto-scroll path, re-pin sticky users
  synchronously at release, and never strand.
- **C27.** Items stay `(turnIndex, itemIndex)`-ordered under late/out-of-
  order arrivals so anchor resolution and last-index auto-follow stay
  correct.

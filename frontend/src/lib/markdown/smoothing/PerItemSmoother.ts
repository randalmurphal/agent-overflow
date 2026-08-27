// PerItemSmoother — per-item word-aligned reveal controller.
//
// Owns two strings: `received` (full accumulator that grows as the
// model emits) and `revealed` (animated cursor into received). A
// subscriber callback fires whenever `revealed` advances. The reveal
// cadence is word-aligned at a base rate of ~160 chars/sec; a gap above
// 80 chars engages adaptive catch-up, rate-clamped at
// MAX_ADAPTIVE_CHARS_PER_SEC. The 500ms in that math is a lower bound
// on urgency, not a completion promise — under the clamp a fat backlog
// takes as long as the ceiling implies.
//
// Word-alignment rule: a "word" is a run of non-whitespace; whitespace
// is revealed with the preceding word's trailing space so whitespace
// never appears alone. Each tick advances by zero or more whole
// (word + trailing whitespace) units, never landing mid-word. If the
// next unit is bigger than the per-tick char budget, the budget
// accumulates until it's large enough; long unbroken tokens (URLs)
// reveal as a single chunk when the budget catches up.
//
// Nothing is ever skipped, rushed, or popped. Every character the wire
// delivers animates, in order, at no more than the ceiling. That is
// affordable because the stream is BURSTY: tool-call execution, API
// round-trips, and the model's own pauses are idle stretches with no
// appends, and the drain keeps running through them. A backlog is
// therefore transient by construction — it is paid off in the next gap,
// not carried forever — so a fat burst just means the reveal keeps
// working for a while. Do not add a skip, a rush regime, or a bound to
// "fix" a backlog; the queue is self-correcting and the smooth in-order
// animation is the product.
//
// Pure TypeScript — no Svelte coupling. Consumers wire `onReveal` to
// whatever store update they need.

export interface SmoothingClock {
  now(): number;
  schedule(cb: () => void): number;
  cancel(handle: number): void;
}

const defaultClock: SmoothingClock = {
  now() {
    return performance.now();
  },
  schedule(cb) {
    return requestAnimationFrame(cb);
  },
  cancel(handle) {
    cancelAnimationFrame(handle);
  },
};

export const BASE_CHARS_PER_SEC = 160;
export const ADAPTIVE_TRIGGER_CHARS = 80;
export const ADAPTIVE_CATCHUP_MS = 500;
// Reveal processing is decoupled from display refresh. rAF fires at
// panel rate, and during catch-up a 165Hz panel would re-parse
// markdown, mutate the DOM, force layout, and re-raster the content
// layer on every ~6ms frame — roughly 3× a 60Hz display's render work
// for zero perceptual gain (word appearance above ~50Hz reads
// identically). Sustained per-frame churn like that is also exactly
// the load that aggravates system-level compositor contention
// (2026-07-04: desktop-wide 0.2–0.5s hitches with video playback,
// only while streaming). Ticks arriving sooner than this interval
// re-schedule without processing; budget accrual uses real elapsed
// time, so reveal RATES are unchanged — only the mutation cadence is
// bounded. 15ms lets 60Hz process every frame; 165Hz every third
// (~55Hz effective), 144Hz every third (~48Hz effective).
//
// The gate is a SHARED wall-clock grid (`floor(now / interval)`), not a
// per-instance elapsed check. Independent phases meant three streaming
// panes processed on three different frames, so at 165Hz every frame
// carried some pane's markdown re-parse + DOM patch and the full native
// pipeline behind it (style, layout, paint, Layerize, and Blink's
// post-layout hover hit-test — HitTest alone averaged 0.84ms/frame in a
// 3-pane storm trace, 2026-08-26). On the grid every active smoother
// processes in the SAME frame — one pipeline run per interval — and the
// frames between are quiet. Per-pane cadence is unchanged: budgets
// accrue by real dt, so a short slot advances proportionally less.
export const MIN_REVEAL_TICK_INTERVAL_MS = 15;
// Ceiling on the adaptive catch-up RATE. When a fat wire burst (an
// Anthropic-API paragraph landing in one chunk) opens a large lag, the
// adaptive rate (`lag / 0.5s`) wants thousands of chars/sec; this
// clamps the reveal to a bounded pace instead. Refresh-rate
// independent by construction — the per-tick cap below used to be the
// only ceiling, and since rAF ticks at display refresh it allowed
// cap × Hz chars/sec (~840 cps at 60Hz, ~2310 at 165Hz; 2026-07-04
// report: "catches up by speeding up a ton...too fast at its peak").
//
// Superseded values: 840 (the intended 60Hz ceiling) → 1000 (2026-07,
// alongside removing the oversized-backlog reveal snap) → 400
// (2026-07-29, the sustained rush read as an unwanted zoom) → 320
// (2026-08-05, below).
//
// 320 is exactly 2× BASE_CHARS_PER_SEC — the user's ceiling on how fast
// catch-up may ever read ("MAYBE 2x at most"), set when the
// successor-waiting fast-drain was removed. With no rush path left this
// is the only speed the reveal can reach; the ceiling is a
// taste-and-load dial and a backlog simply takes proportionally longer.
//
// The contract this replaces the old rush with: a successor row behind a
// big backlog WAITS. It waits for the whole readable drain, at this rate,
// however long that is — nothing is skipped to release it sooner. That is
// not a stall in practice because the wire is bursty: the drain keeps
// ticking through tool-call execution, API round-trips, and model pauses,
// which is exactly when it catches back up to zero. Lag is transient by
// construction. If a queue seems to pile up, the fix is here (this
// ceiling, this cadence) — never a skip or a rush.
//
// The scroll spring's follow cap must stay comfortably above the px/s
// this implies (~345 px/s; see scroll/spring.ts
// SPRING_MAX_VELOCITY_PX_PER_FRAME) — raise the two in step.
export const MAX_ADAPTIVE_CHARS_PER_SEC = 320;
// Hard cap on how many characters the smoother may reveal in a single
// PROCESSED tick, regardless of accumulated budget. With the rate
// ceiling above owning speed, this is purely the per-frame WORK
// bound: one tick's markdown re-parse + DOM mutation stays bounded
// even right after a stalled frame (where dt, and so the tick's
// budget, is large). 21 keeps the 320cps ceiling reachable at the
// slowest throttled cadence with recovery headroom: the floor is
// 320/48 ≈ 6.7 chars per processed tick, and the extra room lets a
// jittered stretch (a ~50ms gap at 320cps = 16 chars of accrued
// budget) catch back up in one tick instead of throttling the average
// below the ceiling. Excess budget is clamped after each tick (not
// rolled over) so a stall can't burst a multi-tick chunk. No rush
// regime survives — the end-of-turn drain (END_OF_TURN_DRAIN_MS) went
// in 2026-07 and the successor-waiting fast-drain in 2026-08, both
// because rushed reveal motion read as jank; a long final message
// finishing a few seconds after the wire settles is the accepted trade
// for uniform reveal speed. One path still puts more than this cap in a
// single delta, and it doesn't speed the ANIMATION up: `revealImmediately`
// (low power / streaming-off) hands over the pending buffer in one
// mutation instead of animating at all.
export const MAX_ADVANCE_PER_TICK_CHARS = 21;

export interface PerItemSmootherOptions {
  /**
   * Seed for the received buffer (mid-flight resume). `revealed` seeds
   * from it too — a smoother always starts caught up, so a mid-flight
   * feature deploy or turn-resume sees no visible jump and the emitted
   * deltas reconstruct everything AFTER the seed exactly.
   */
  initialReceived?: string;
  /** Fires every time `revealed` advances (including on `snap`). */
  onReveal: (revealed: string, delta: string) => void;
  /**
   * Low-power seam: when this returns true, the next processed tick
   * snaps everything pending instead of animating, so revealed text
   * tracks the wire chunk-for-chunk with one DOM mutation per chunk.
   * Sampled per tick (not latched), so flipping the app's low-power
   * setting mid-stream takes effect on the next scheduled frame.
   */
  revealImmediately?: () => boolean;
  /** Optional clock injection for deterministic tests. */
  clock?: SmoothingClock;
}

export class PerItemSmoother {
  private received: string;
  private revealed: string;
  private readonly onReveal: (revealed: string, delta: string) => void;
  private readonly revealImmediately: (() => boolean) | undefined;
  private readonly clock: SmoothingClock;

  private lastTickAt: number;
  private rafHandle: number | null = null;
  private disposed = false;
  // Fractional character budget accumulates across ticks so a slow
  // tick (low rAF rate) still averages out to ~rate chars per second.
  private revealBudget = 0;
  // While paused, the smoother accumulates `received` (appendDelta still
  // grows it) but advances `revealed` zero — `scheduleTick` no-ops. The
  // reveal sequencer pauses a row whose turn to reveal hasn't come yet, so
  // when it is finally revealed it animates from where it left off rather
  // than snapping in a chunk that streamed invisibly. It replays the whole
  // withheld backlog, unconditionally — a row that streamed while it was
  // withheld gets the same word-by-word reveal as one that streamed live.
  private paused = false;

  constructor(opts: PerItemSmootherOptions) {
    this.received = opts.initialReceived ?? '';
    this.revealed = this.received;
    this.onReveal = opts.onReveal;
    this.revealImmediately = opts.revealImmediately;
    this.clock = opts.clock ?? defaultClock;
    this.lastTickAt = this.clock.now();
  }

  /** Append a new wire delta. Schedules the next tick if needed. */
  appendDelta(delta: string): void {
    if (this.disposed) return;
    if (delta.length === 0) return;
    this.received += delta;
    this.scheduleTick();
  }

  /** Synchronously reveal everything in `received`. */
  snap(): void {
    if (this.disposed) return;
    if (this.rafHandle != null) {
      this.clock.cancel(this.rafHandle);
      this.rafHandle = null;
    }
    if (this.revealed.length >= this.received.length) return;
    const delta = this.received.slice(this.revealed.length);
    this.revealed = this.received;
    this.revealBudget = 0;
    this.onReveal(this.revealed, delta);
  }

  /**
   * Hold reveal at its current cursor. `appendDelta` keeps accumulating
   * `received`; no rAF advances `revealed` until `resume`. Idempotent.
   */
  pause(): void {
    if (this.disposed || this.paused) return;
    this.paused = true;
    if (this.rafHandle != null) {
      this.clock.cancel(this.rafHandle);
      this.rafHandle = null;
    }
  }

  /** Resume reveal after `pause`. Re-schedules a tick if behind. Idempotent. */
  resume(): void {
    if (this.disposed || !this.paused) return;
    this.paused = false;
    this.lastTickAt = this.clock.now();
    this.scheduleTick();
  }

  isPaused(): boolean {
    return this.paused;
  }

  isCaughtUp(): boolean {
    return this.revealed.length >= this.received.length;
  }

  getRevealed(): string {
    return this.revealed;
  }

  getReceived(): string {
    return this.received;
  }

  /**
   * Pending unrevealed characters (received − revealed). Visibility-resume
   * uses `snap()` directly (which no-ops when caught up) rather than gating
   * on this; exposed as an accessor for tests and lag-based decisions.
   */
  getLag(): number {
    return this.received.length - this.revealed.length;
  }

  /** Cancel pending rAF. Idempotent. */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    if (this.rafHandle != null) {
      this.clock.cancel(this.rafHandle);
      this.rafHandle = null;
    }
  }

  private scheduleTick(): void {
    if (this.disposed) return;
    if (this.paused) return;
    if (this.rafHandle != null) return;
    if (this.isCaughtUp()) return;
    this.rafHandle = this.clock.schedule(() => {
      this.rafHandle = null;
      this.tick();
    });
  }

  private tick(): void {
    if (this.disposed) return;
    // Low-power: reveal everything pending in one mutation instead of
    // animating. Checked at the tick (not in appendDelta) so the
    // reveal stays asynchronous — a synchronous snap inside
    // getOrCreateSmoothing's construction path would fire onReveal
    // before the caller's smoother reference exists. lastTickAt is
    // advanced so a later flip back to animated cadence doesn't see
    // the whole low-power stint as one giant dt (a ballooned budget
    // dumps a full per-tick cap on the first animated frame instead
    // of easing in at word cadence).
    if (this.revealImmediately?.()) {
      this.lastTickAt = this.clock.now();
      this.snap();
      return;
    }
    const now = this.clock.now();
    // Refresh-rate decoupling: high-Hz panels re-schedule without
    // processing until the next wall-clock grid slot. The grid is shared
    // across ALL smoothers (see MIN_REVEAL_TICK_INTERVAL_MS) so
    // concurrent panes process in the same frame instead of spreading
    // one render pipeline over every frame. lastTickAt is NOT advanced
    // on skipped frames, so the processed tick's dt (and budget) covers
    // the full elapsed time.
    if (
      Math.floor(now / MIN_REVEAL_TICK_INTERVAL_MS) ===
      Math.floor(this.lastTickAt / MIN_REVEAL_TICK_INTERVAL_MS)
    ) {
      this.scheduleTick();
      return;
    }
    const dt = Math.max(0, now - this.lastTickAt);
    this.lastTickAt = now;
    if (this.isCaughtUp()) return;

    // Always exactly where the last reveal left off — there is no floor,
    // no skip-ahead, and no way for `revealed` to disagree with the sum
    // of the emitted deltas.
    const start = this.revealed.length;
    const lag = this.received.length - start;
    // Two regimes only: steady word cadence, and adaptive catch-up
    // clamped at MAX_ADAPTIVE_CHARS_PER_SEC. There is no rush mode.
    let charsPerSec = BASE_CHARS_PER_SEC;
    if (lag > ADAPTIVE_TRIGGER_CHARS) {
      // Rate-clamped: the target-500ms drain math is a lower bound on
      // urgency, MAX_ADAPTIVE_CHARS_PER_SEC an upper bound on speed —
      // a fat burst drains at the ceiling for as long as it takes
      // rather than proportionally faster the fatter it is.
      charsPerSec = Math.min(
        MAX_ADAPTIVE_CHARS_PER_SEC,
        Math.max(charsPerSec, (lag * 1000) / ADAPTIVE_CATCHUP_MS),
      );
    }
    this.revealBudget += (charsPerSec * dt) / 1000;

    // Per-tick advance cap: even when adaptive math wants to drain a
    // large lag in one frame, never ANIMATE more than the cap, so a fat
    // backlog never lands as one giant re-parse in a single frame.
    const nextUnitLen = nextWordUnitEnd(this.received, start) - start;
    // The cap expands for one oversized word unit (URL, long identifier)
    // so the reveal never stalls on a token no tick can fit; word
    // alignment wins over the work bound in that single case.
    const perTickCap =
      nextUnitLen > MAX_ADVANCE_PER_TICK_CHARS
        ? nextUnitLen
        : MAX_ADVANCE_PER_TICK_CHARS;
    const rawCap = Math.floor(this.revealBudget);
    const advanceCap = rawCap > perTickCap ? perTickCap : rawCap;
    let nextEnd = start;
    if (advanceCap > 0) {
      nextEnd = computeAdvanceEnd(this.received, start, advanceCap);
    }
    this.revealBudget -= nextEnd - start;
    if (nextEnd > start) {
      const delta = this.received.slice(start, nextEnd);
      this.revealed = this.received.slice(0, nextEnd);
      this.onReveal(this.revealed, delta);
    }
    // Clamp residual budget so accumulated catch-up budget can't burst
    // into a multi-tick chunk on the next frame. Budget first: on nearly
    // every steady tick it sits at or below the cap, and the word scan
    // exists only to spot the oversized-unit case, which genuinely needs
    // more budget than the cap to make any progress at all — clamping
    // there would stall that token forever.
    if (this.revealBudget > MAX_ADVANCE_PER_TICK_CHARS) {
      const unitAhead =
        this.revealed.length === start
          ? nextUnitLen
          : nextWordUnitEnd(this.received, this.revealed.length)
            - this.revealed.length;
      if (unitAhead <= MAX_ADVANCE_PER_TICK_CHARS) {
        this.revealBudget = MAX_ADVANCE_PER_TICK_CHARS;
      }
    }
    this.scheduleTick();
  }
}

// Advance up to `chars` characters but stop at word boundaries so we
// never split a word. Each step is one (word + trailing whitespace)
// unit; a unit larger than `chars` is held back for the next tick
// (budget accumulates across ticks).
export function computeAdvanceEnd(
  text: string,
  from: number,
  chars: number,
): number {
  if (from >= text.length) return from;
  let end = from;
  let budget = chars;
  while (end < text.length) {
    const unitEnd = nextWordUnitEnd(text, end);
    const unitLen = unitEnd - end;
    if (unitLen > budget) break;
    end = unitEnd;
    budget -= unitLen;
  }
  return end;
}

// Returns the offset one past the end of the next (word + trailing
// whitespace) unit starting at `from`. Whitespace at `from` is
// consumed as a leading whitespace-only run; in steady state the
// previous unit already pulled trailing whitespace with its word.
function nextWordUnitEnd(text: string, from: number): number {
  let i = from;
  while (i < text.length && isWhitespace(text[i])) i++;
  while (i < text.length && !isWhitespace(text[i])) i++;
  while (i < text.length && isWhitespace(text[i])) i++;
  return i;
}

function isWhitespace(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r';
}

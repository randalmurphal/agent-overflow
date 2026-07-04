// PerItemSmoother — per-item word-aligned reveal controller.
//
// Owns two strings: `received` (full accumulator that grows as the
// model emits) and `revealed` (animated cursor into received). A
// subscriber callback fires whenever `revealed` advances. The reveal
// cadence is word-aligned at a base rate of ~160 chars/sec with
// adaptive catch-up that drains the unrevealed buffer within 500ms
// whenever the gap exceeds 80 chars.
//
// Word-alignment rule: a "word" is a run of non-whitespace; whitespace
// is revealed with the preceding word's trailing space so whitespace
// never appears alone. Each tick advances by zero or more whole
// (word + trailing whitespace) units, never landing mid-word. If the
// next unit is bigger than the per-tick char budget, the budget
// accumulates until it's large enough; long unbroken tokens (URLs)
// reveal as a single chunk when the budget catches up.
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
// Default window for `requestFastDrain`: the reveal sequencer calls it on
// a top-level item the instant a successor row is waiting behind it, so the
// predecessor finishes animating quickly instead of stalling the queue for
// seconds. Bounded so a long collapsed thinking block can't delay a live
// tool call by more than a moment, while still reading as motion rather than
// an instant snap. Fast-drain raises the per-tick cap to
// FAST_DRAIN_MAX_ADVANCE_PER_TICK_CHARS instead of lifting it: an unbounded
// drain used to reveal the whole lag in one rAF tick, and that single giant
// markdown re-parse + DOM mutation was visible as a hitch exactly at row
// boundaries (the moment a tool row lands behind a streaming text frontier).
export const FAST_DRAIN_MS = 200;
// Per-tick reveal cap while fast-draining. 4× the steady-state cap:
// ~3360 cps at 60Hz — clearly "rushing to finish" motion, while keeping
// each frame's markdown re-parse bounded. A lag of ~1200 chars drains in
// ~21 ticks (≈350ms); callers that consider a lag too large to animate
// at all snap instead (see FAST_DRAIN_SNAP_LAG_CHARS).
export const FAST_DRAIN_MAX_ADVANCE_PER_TICK_CHARS = 56;
// Caller policy, not mechanism: above this lag a "finish the predecessor"
// fast-drain would still take >~350ms of sustained per-frame re-parse
// work, so callers snap outright — one deliberate burst, same cost the
// unbounded drain used to pay, reserved for the outlier case.
export const FAST_DRAIN_SNAP_LAG_CHARS = 1200;
// Ceiling on the adaptive catch-up RATE. When a fat wire burst (an
// Anthropic-API paragraph landing in one chunk) opens a large lag, the
// adaptive rate (`lag / 0.5s`) wants thousands of chars/sec; this
// clamps the reveal to a bounded "rushing but followable" pace
// instead. Refresh-rate independent by construction — the per-tick
// cap below used to be the only ceiling, and since rAF ticks at
// display refresh it allowed cap × Hz chars/sec: ~840 cps on the
// 60Hz panel the cap was sized against, but ~2310 cps on a 165Hz
// panel (2026-07-04 report: "catches up by speeding up a ton...too
// fast at its peak"). 840 preserves the originally intended ceiling
// on every display; lower it to slow peak catch-up everywhere.
export const MAX_ADAPTIVE_CHARS_PER_SEC = 840;
// Hard cap on how many characters the smoother may reveal in a single
// rAF tick, regardless of accumulated budget. With the rate ceiling
// above owning speed, this is purely the per-frame WORK bound: one
// tick's markdown re-parse + DOM mutation stays bounded even right
// after a stalled frame (where dt, and so the tick's budget, is
// large). 14 chars ≈ 2 short words. Excess budget is clamped after
// each tick (not rolled over) so a stall can't burst a multi-tick
// chunk; the backlog drains at the elevated fast-drain cap only when
// a successor row is waiting. A solo tail row's end-of-turn backlog
// drains at the steady cadence — there used to be an end-of-turn
// fast-drain (END_OF_TURN_DRAIN_MS, removed 2026-07) that rushed it,
// and the rushed motion read as jank; a long final message finishing
// a few seconds after the wire settles is the accepted trade for
// uniform reveal speed.
export const MAX_ADVANCE_PER_TICK_CHARS = 14;

export interface PerItemSmootherOptions {
  /** Seed for the received buffer (mid-flight resume). */
  initialReceived?: string;
  /** Seed for the revealed cursor. Defaults to `initialReceived` so a
   * mid-flight feature deploy sees no visible jump. */
  initialRevealed?: string;
  /** Fires every time `revealed` advances (including on `snap`). */
  onReveal: (revealed: string, delta: string) => void;
  /** Optional clock injection for deterministic tests. */
  clock?: SmoothingClock;
}

export class PerItemSmoother {
  private received: string;
  private revealed: string;
  private readonly onReveal: (revealed: string, delta: string) => void;
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
  // when it is finally revealed it animates from where it left off (the
  // start) rather than snapping in a chunk that streamed invisibly.
  private paused = false;
  // When non-null, the smoother is fast-draining toward this wall-clock
  // deadline: charsPerSec is sized to reveal the current lag by then and the
  // per-tick cap is lifted. Cleared once caught up (or on snap).
  private fastDrainEndsAt: number | null = null;

  constructor(opts: PerItemSmootherOptions) {
    this.received = opts.initialReceived ?? '';
    this.revealed = opts.initialRevealed ?? this.received;
    this.onReveal = opts.onReveal;
    this.clock = opts.clock ?? defaultClock;
    this.lastTickAt = this.clock.now();
    if (!this.isCaughtUp()) this.scheduleTick();
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
    this.fastDrainEndsAt = null;
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

  /**
   * Drain the current lag aggressively so the row finishes animating within
   * ~`targetMs`. First call wins the deadline; later calls are ignored so a
   * repeated trigger can't keep pushing the finish out. No-op once caught up.
   */
  requestFastDrain(targetMs: number = FAST_DRAIN_MS): void {
    if (this.disposed) return;
    if (this.fastDrainEndsAt !== null) return;
    if (this.isCaughtUp()) return;
    this.fastDrainEndsAt = this.clock.now() + Math.max(0, targetMs);
    this.scheduleTick();
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
    const now = this.clock.now();
    const dt = Math.max(0, now - this.lastTickAt);
    this.lastTickAt = now;
    if (this.isCaughtUp()) return;

    const lag = this.received.length - this.revealed.length;
    // Fast-drain mode: size the rate to clear the remaining lag by the
    // deadline. Floor the window at ~one frame so the rate stays finite,
    // and once the deadline has passed the lag/16ms rate reveals the
    // remainder in a single tick (a hard finish).
    const draining = this.fastDrainEndsAt !== null;
    let charsPerSec = BASE_CHARS_PER_SEC;
    if (draining) {
      const msLeft = Math.max(16, (this.fastDrainEndsAt as number) - now);
      charsPerSec = Math.max(charsPerSec, (lag * 1000) / msLeft);
    } else if (lag > ADAPTIVE_TRIGGER_CHARS) {
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
    // large lag in one frame, never reveal more than the cap in normal
    // word streaming. Fast-drain raises the cap (rush-to-finish motion)
    // but stays finite so a fat backlog never lands as one giant
    // re-parse in a single frame. The exception in both modes is a
    // single word unit bigger than the cap (URL, long identifier) —
    // the cap expands to that one word's size so the smoother doesn't
    // stall forever on it.
    const previousLength = this.revealed.length;
    const nextUnitLen =
      nextWordUnitEnd(this.received, previousLength) - previousLength;
    const basePerTickCap = draining
      ? FAST_DRAIN_MAX_ADVANCE_PER_TICK_CHARS
      : MAX_ADVANCE_PER_TICK_CHARS;
    const perTickCap = nextUnitLen > basePerTickCap ? nextUnitLen : basePerTickCap;
    const rawCap = Math.floor(this.revealBudget);
    const advanceCap = rawCap > perTickCap ? perTickCap : rawCap;
    const nextEnd =
      advanceCap > 0
        ? computeAdvanceEnd(this.received, previousLength, advanceCap)
        : previousLength;
    const advanced = nextEnd - previousLength;
    if (advanced > 0) {
      this.revealBudget -= advanced;
      const delta = this.received.slice(previousLength, nextEnd);
      this.revealed = this.received.slice(0, nextEnd);
      this.onReveal(this.revealed, delta);
    }
    // Drain finished: drop back to the normal cadence for any later
    // appended text (defensive — a frontier's wire is already complete).
    if (this.isCaughtUp()) this.fastDrainEndsAt = null;
    // Clamp residual budget so accumulated catch-up budget can't burst
    // into a multi-tick chunk on the next frame. Reads fastDrainEndsAt
    // (not the tick-start `draining`) so a drain that just finished
    // clamps back to the steady-state cap immediately. The clamp is
    // skipped when the now-next word is still oversized — that case
    // genuinely needs more budget than the cap to make any progress.
    const clampCap = this.fastDrainEndsAt !== null
      ? FAST_DRAIN_MAX_ADVANCE_PER_TICK_CHARS
      : MAX_ADVANCE_PER_TICK_CHARS;
    const newNextLen =
      nextWordUnitEnd(this.received, this.revealed.length)
      - this.revealed.length;
    if (newNextLen <= clampCap && this.revealBudget > clampCap) {
      this.revealBudget = clampCap;
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

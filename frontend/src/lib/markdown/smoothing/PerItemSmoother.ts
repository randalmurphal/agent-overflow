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
// seconds. Fast-drain raises the per-tick cap to
// FAST_DRAIN_MAX_ADVANCE_PER_TICK_CHARS instead of lifting it: an unbounded
// drain used to reveal the whole lag in one rAF tick, and that single giant
// markdown re-parse + DOM mutation was visible as a hitch exactly at row
// boundaries (the moment a tool row lands behind a streaming text frontier).
export const FAST_DRAIN_MS = 200;
// Per-tick reveal cap while fast-draining: the per-frame WORK bound (one
// tick's markdown re-parse + DOM mutation), sized so the drain rate
// ceiling stays reachable at the slowest throttled cadence (48Hz
// processed: 2400/48 = 50 ≤ 56).
export const FAST_DRAIN_MAX_ADVANCE_PER_TICK_CHARS = 56;
// Ceiling on the fast-drain RATE. The deadline math below wants
// `lag / msLeft` — unbounded in the lag — so without a ceiling a large
// backlog (a reconnect replay, a fat burst right before a tool row)
// dumps as an unreadable blur even though each tick stays bounded.
// Clamped, any backlog drains at a constant, deliberately-fast pace and
// simply takes as long as it takes (an essay-sized backlog reveals in
// seconds, at streaming speed, rather than as one wholesale write —
// the reveal never snaps the whole buffer in a single frame).
// This exceeds the scroll spring's follow cap (~1620 px/s,
// scroll/spring.ts SPRING_MAX_VELOCITY_PX_PER_FRAME), which is fine for
// a bounded drain: the viewport trails at its own cap and closes the
// gap when the drain ends — deliberate catch-up motion, not steady-state
// follow (the spring invariant governs the STEADY reveal ceiling below).
export const FAST_DRAIN_MAX_CHARS_PER_SEC = 2400;
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
export const MIN_REVEAL_TICK_INTERVAL_MS = 15;
// Ceiling on the adaptive catch-up RATE. When a fat wire burst (an
// Anthropic-API paragraph landing in one chunk) opens a large lag, the
// adaptive rate (`lag / 0.5s`) wants thousands of chars/sec; this
// clamps the reveal to a bounded pace instead. Refresh-rate
// independent by construction — the per-tick cap below used to be the
// only ceiling, and since rAF ticks at display refresh it allowed
// cap × Hz chars/sec: ~840 cps on the 60Hz panel the cap was sized
// against, but ~2310 cps on a 165Hz panel (2026-07-04 report:
// "catches up by speeding up a ton...too fast at its peak").
// Originally 840 (the intended 60Hz ceiling); raised to 1000 in
// 2026-07 alongside the removal of the oversized-backlog reveal snap;
// lowered to 400 (2.5× steady) on 2026-07-29: a mid-turn wire stall
// resuming into a fat backlog rode the 1000cps ceiling for seconds,
// and the sustained rush read as an unwanted zoom — there is no
// urgency to visually catch up, and the ~60Hz DOM-mutation churn it
// sustains is exactly the load that aggravates presentation-side
// frame drops on fragile compositors (per-line stutter under
// WSL2/WebView2). The drain motion itself measures smooth at either
// rate (2026-07-29 harness profile: 3-5px/frame, spring.tick writes
// only); the ceiling is a taste-and-load dial, and a backlog simply
// takes proportionally longer to finish revealing. The scroll
// spring's follow cap must stay comfortably above the px/s this
// implies (~430 px/s; see scroll/spring.ts
// SPRING_MAX_VELOCITY_PX_PER_FRAME) — raise the two in step.
export const MAX_ADAPTIVE_CHARS_PER_SEC = 400;
// Hard cap on how many characters the smoother may reveal in a single
// PROCESSED tick, regardless of accumulated budget. With the rate
// ceiling above owning speed, this is purely the per-frame WORK
// bound: one tick's markdown re-parse + DOM mutation stays bounded
// even right after a stalled frame (where dt, and so the tick's
// budget, is large). 21 keeps the 400cps ceiling reachable at the
// slowest throttled cadence with recovery headroom: the floor is
// 400/48 ≈ 8.4 chars per processed tick, and the extra room lets a
// jittered stretch (a ~50ms gap at 400cps ≈ 20 chars of accrued
// budget) catch back up in one tick instead of throttling the average
// below the ceiling. Excess budget is clamped after each tick (not rolled over)
// so a stall can't burst a multi-tick chunk; the backlog drains at
// the elevated fast-drain cap only when a successor row is waiting. A
// solo tail row's end-of-turn backlog drains at the steady cadence —
// there used to be an end-of-turn fast-drain (END_OF_TURN_DRAIN_MS,
// removed 2026-07) that rushed it, and the rushed motion read as
// jank; a long final message finishing a few seconds after the wire
// settles is the accepted trade for uniform reveal speed.
export const MAX_ADVANCE_PER_TICK_CHARS = 21;

export interface PerItemSmootherOptions {
  /** Seed for the received buffer (mid-flight resume). */
  initialReceived?: string;
  /** Seed for the revealed cursor. Defaults to `initialReceived` so a
   * mid-flight feature deploy sees no visible jump. */
  initialRevealed?: string;
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
    this.revealImmediately = opts.revealImmediately;
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
    // processing until the minimum interval has elapsed. lastTickAt is
    // NOT advanced on skipped frames, so the processed tick's dt (and
    // budget) covers the full elapsed time.
    if (now - this.lastTickAt < MIN_REVEAL_TICK_INTERVAL_MS) {
      this.scheduleTick();
      return;
    }
    const dt = Math.max(0, now - this.lastTickAt);
    this.lastTickAt = now;
    if (this.isCaughtUp()) return;

    const lag = this.received.length - this.revealed.length;
    // Fast-drain mode: size the rate to clear the remaining lag by the
    // deadline, clamped to the drain ceiling. Floor the window at ~one
    // frame so the deadline math stays finite; small lags finish by the
    // deadline, large ones ride the ceiling for as long as they take —
    // a bounded rush, never a wholesale reveal (see
    // FAST_DRAIN_MAX_CHARS_PER_SEC).
    const draining = this.fastDrainEndsAt !== null;
    let charsPerSec = BASE_CHARS_PER_SEC;
    if (draining) {
      const msLeft = Math.max(16, (this.fastDrainEndsAt as number) - now);
      charsPerSec = Math.min(
        FAST_DRAIN_MAX_CHARS_PER_SEC,
        Math.max(charsPerSec, (lag * 1000) / msLeft),
      );
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

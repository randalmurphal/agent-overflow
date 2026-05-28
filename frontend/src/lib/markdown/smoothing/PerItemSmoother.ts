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
// Hard cap on how many characters the smoother may reveal in a single
// rAF tick, regardless of how much budget the adaptive catch-up math
// has produced. Without this cap, sustained high-rate wire bursts
// (e.g. Claude reasoning at 1500–3000 chars/sec) push lag into the
// hundreds-of-chars range, and the adaptive rate (`lag / 0.5s`) then
// produces 30–80 chars per tick — the user perceives those as a chunk
// of 5–15 words appearing instantly rather than a smooth reveal.
// 14 chars ≈ 2 short words at 60Hz ≈ 840 cps effective ceiling, which
// is comfortably above readable streaming pace but well below a "burst
// looks like a chunk" threshold. Excess budget is silently discarded
// (not rolled over) so a sustained over-rate wire grows lag instead of
// building per-tick chunks; the catchup happens at completion when the
// patch/extension drains the smoother at the same capped per-tick rate.
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

  isCaughtUp(): boolean {
    return this.revealed.length >= this.received.length;
  }

  getRevealed(): string {
    return this.revealed;
  }

  getReceived(): string {
    return this.received;
  }

  /** Pending lag in characters. Use to drive visibility-resume snaps. */
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
    let charsPerSec = BASE_CHARS_PER_SEC;
    if (lag > ADAPTIVE_TRIGGER_CHARS) {
      charsPerSec = Math.max(charsPerSec, (lag * 1000) / ADAPTIVE_CATCHUP_MS);
    }
    this.revealBudget += (charsPerSec * dt) / 1000;

    // Per-tick advance cap: even when adaptive math wants to drain a
    // large lag in one frame, never reveal more than
    // `MAX_ADVANCE_PER_TICK_CHARS` chars in normal word streaming.
    // The exception is a single word unit bigger than the cap (URL,
    // long identifier) — we let the cap expand to that one word's
    // size so the smoother doesn't stall forever on it.
    const previousLength = this.revealed.length;
    const nextUnitLen =
      nextWordUnitEnd(this.received, previousLength) - previousLength;
    const isOversizedNext = nextUnitLen > MAX_ADVANCE_PER_TICK_CHARS;
    const perTickCap = isOversizedNext
      ? nextUnitLen
      : MAX_ADVANCE_PER_TICK_CHARS;
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
    // Clamp residual budget so accumulated catch-up budget can't burst
    // into a multi-tick chunk on the next frame. The clamp is skipped
    // when the now-next word is still oversized — that case genuinely
    // needs more budget than the cap to make any progress at all.
    const newNextLen =
      nextWordUnitEnd(this.received, this.revealed.length)
      - this.revealed.length;
    const stillOversizedNext = newNextLen > MAX_ADVANCE_PER_TICK_CHARS;
    if (
      !stillOversizedNext
      && this.revealBudget > MAX_ADVANCE_PER_TICK_CHARS
    ) {
      this.revealBudget = MAX_ADVANCE_PER_TICK_CHARS;
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

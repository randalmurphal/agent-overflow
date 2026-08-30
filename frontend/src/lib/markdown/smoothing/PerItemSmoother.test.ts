import { afterEach, describe, it, expect, vi } from 'vitest';
import {
  PerItemSmoother,
  computeAdvanceEnd,
  BASE_CHARS_PER_SEC,
  ADAPTIVE_TRIGGER_CHARS,
  ADAPTIVE_CATCHUP_MS,
  MAX_ADVANCE_PER_TICK_CHARS,
  MAX_ADAPTIVE_CHARS_PER_SEC,
  MIN_REVEAL_TICK_INTERVAL_MS,
  type SmoothingClock,
} from './PerItemSmoother';

class FakeClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  private callbacks = new Set<() => void>();

  now(): number {
    return this.current;
  }
  schedule(cb: () => void): number {
    const handle = this.nextHandle++;
    this.pending.set(handle, cb);
    this.callbacks.add(cb);
    return handle;
  }
  cancel(handle: number): void {
    this.pending.delete(handle);
  }
  // Advance the clock and fire callbacks that were pending BEFORE this
  // call. Callbacks scheduled during firing are deferred to the next
  // tickFrame, mirroring rAF semantics (one batch per frame).
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.entries()];
    this.pending.clear();
    for (const [, cb] of toFire) cb();
  }
  pendingCount(): number {
    return this.pending.size;
  }
  scheduledCallbackCount(): number {
    return this.callbacks.size;
  }
}

interface RevealEntry {
  revealed: string;
  delta: string;
}

function makeSmoother(initial = '') {
  const clock = new FakeClock();
  const reveals: RevealEntry[] = [];
  const smoother = new PerItemSmoother({
    initialReceived: initial,
    onReveal: (delta) =>
      reveals.push({
        revealed: (reveals.at(-1)?.revealed ?? initial) + delta,
        delta,
      }),
    clock,
  });
  return { clock, reveals, smoother };
}

function drain(clock: FakeClock, smoother: PerItemSmoother, maxFrames = 2000) {
  let frames = 0;
  while (!smoother.isCaughtUp() && frames < maxFrames) {
    clock.tickFrame(16);
    frames++;
  }
  return frames;
}

function concatDeltas(reveals: RevealEntry[]): string {
  return reveals.map((r) => r.delta).join('');
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('computeAdvanceEnd', () => {
  it('returns the from offset when from is at end', () => {
    expect(computeAdvanceEnd('abc', 3, 10)).toBe(3);
  });

  it('takes a whole word+trailing-space unit when it fits the budget', () => {
    // "hello " is 6 chars; budget 6 fits exactly.
    expect(computeAdvanceEnd('hello world', 0, 6)).toBe(6);
  });

  it('holds back a unit larger than the budget', () => {
    // First unit "supercalifragilistic " is 21 chars; budget 5 won't fit.
    expect(computeAdvanceEnd('supercalifragilistic word', 0, 5)).toBe(0);
  });

  it('takes the trailing tail (no trailing whitespace) when at end', () => {
    expect(computeAdvanceEnd('hi', 0, 5)).toBe(2);
  });

  it('takes multiple word units when budget covers several', () => {
    // "one two three" — "one " (4) + "two " (4) + "three" (5) = 13.
    expect(computeAdvanceEnd('one two three', 0, 13)).toBe(13);
    // Budget 8: "one " + "two " = 8. Holds back "three" (5 chars).
    expect(computeAdvanceEnd('one two three', 0, 8)).toBe(8);
  });

  it('consumes leading whitespace with the next word', () => {
    // From offset 0 on "  hello world": leading whitespace + "hello" +
    // trailing whitespace = 8 chars.
    expect(computeAdvanceEnd('  hello world', 0, 8)).toBe(8);
  });

  it('treats a multi-newline gap as part of a unit', () => {
    // "para\n\nmore" — at offset 0, unit is "para\n\n" (6 chars) then
    // "more" (4 chars).
    expect(computeAdvanceEnd('para\n\nmore', 0, 6)).toBe(6);
    expect(computeAdvanceEnd('para\n\nmore', 0, 10)).toBe(10);
  });
});

// Relationships between the tuned constants that the behavioral tests
// below silently assume. Kept separate so a constant change fails HERE,
// naming the assumption, rather than inside a timing assertion.
describe('PerItemSmoother — constant relationships', () => {
  it('adaptive catch-up is genuinely faster than the base rate', () => {
    expect(MAX_ADAPTIVE_CHARS_PER_SEC).toBeGreaterThan(BASE_CHARS_PER_SEC);
  });

  it('the ceiling makes the 500ms trigger window a lower bound on urgency, not a promise', () => {
    // A backlog exactly at the trigger threshold would need
    // ADAPTIVE_TRIGGER_CHARS/ceiling seconds; anything materially larger
    // overruns ADAPTIVE_CATCHUP_MS, which is why the drain-in-500ms
    // wording in the tick's math is not a completion guarantee.
    const drainMsAtCeiling = (chars: number) =>
      (chars / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000;
    expect(drainMsAtCeiling(ADAPTIVE_TRIGGER_CHARS)).toBeLessThan(
      ADAPTIVE_CATCHUP_MS,
    );
    expect(drainMsAtCeiling(200)).toBeGreaterThan(ADAPTIVE_CATCHUP_MS);
  });

  it('the per-tick work cap keeps the rate ceiling reachable at the slowest throttled cadence', () => {
    // Processed ticks bottom out at ~48Hz (MIN_REVEAL_TICK_INTERVAL_MS
    // against a 144Hz panel); the cap must clear the per-tick chars the
    // ceiling implies there, or the cap — not the rate — becomes the
    // ceiling.
    const slowestProcessedHz = 1000 / (MIN_REVEAL_TICK_INTERVAL_MS * 1.4);
    expect(MAX_ADVANCE_PER_TICK_CHARS).toBeGreaterThan(
      MAX_ADAPTIVE_CHARS_PER_SEC / slowestProcessedHz,
    );
  });
});

describe('PerItemSmoother', () => {
  it('starts with empty state and no scheduled callbacks', () => {
    const { clock, reveals, smoother } = makeSmoother();
    expect(smoother.getRevealed()).toBe('');
    expect(smoother.getReceived()).toBe('');
    expect(smoother.isCaughtUp()).toBe(true);
    expect(clock.pendingCount()).toBe(0);
    expect(reveals).toEqual([]);
  });

  it('reuses one scheduled callback across reveal frames', () => {
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta('word '.repeat(80));
    for (let frame = 0; frame < 20; frame += 1) clock.tickFrame(16);

    expect(clock.scheduledCallbackCount()).toBe(1);
  });

  it('batches default-clock smoothers into one native animation frame', () => {
    const nativeCallbacks: FrameRequestCallback[] = [];
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      nativeCallbacks.push(callback);
      return nativeCallbacks.length;
    });
    vi.stubGlobal('cancelAnimationFrame', vi.fn());
    const a = new PerItemSmoother({ onReveal: () => {} });
    const b = new PerItemSmoother({ onReveal: () => {} });

    a.appendDelta('first pane keeps revealing words');
    b.appendDelta('second pane keeps revealing words');
    expect(nativeCallbacks).toHaveLength(1);

    a.dispose();
    b.dispose();
  });

  it('seeds revealed = received on mid-flight start (no jump, no callback)', () => {
    const { clock, reveals, smoother } = makeSmoother(
      'already revealed text',
    );
    expect(smoother.getRevealed()).toBe('already revealed text');
    expect(smoother.isCaughtUp()).toBe(true);
    expect(clock.pendingCount()).toBe(0);
    expect(reveals).toEqual([]);
  });

  it('reveals one or more word units per tick at the base rate', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('hello world foo bar baz');
    // Budget per 16ms tick = 160 * 0.016 = 2.56 chars.
    // After two ticks (32ms), budget = 5.12. "hello " is 6 chars,
    // which won't fit, so no progress yet.
    clock.tickFrame(16);
    expect(smoother.getRevealed()).toBe('');
    clock.tickFrame(16);
    expect(smoother.getRevealed()).toBe('');
    // Third tick: budget = 7.68. Floor 7 covers "hello " (6).
    clock.tickFrame(16);
    expect(smoother.getRevealed()).toBe('hello ');
    expect(reveals.at(-1)).toEqual({ revealed: 'hello ', delta: 'hello ' });
  });

  it('produces a steady stream of word reveals at base rate', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('one two three four five six seven');
    // 1000ms / 160 cps ≈ 6.25 chars per frame at 60fps. Run for ~1s.
    const totalFrames = 62; // ~992ms
    for (let i = 0; i < totalFrames; i++) clock.tickFrame(16);
    // Should have revealed roughly 160 chars worth (whole string is 33 chars).
    expect(smoother.getRevealed()).toBe('one two three four five six seven');
    // Each reveal should end on a whitespace OR at the very end of received.
    for (const r of reveals) {
      const last = r.revealed[r.revealed.length - 1];
      const endsAtBoundary = last === ' ' || r.revealed === smoother.getReceived();
      expect(endsAtBoundary).toBe(true);
    }
  });

  it('engages adaptive catch-up when lag exceeds the threshold', () => {
    const { clock, smoother } = makeSmoother();
    // Lag of 200 chars (> 80 trigger). The adaptive math wants
    // 200*1000/500 = 400 cps; the ceiling clamps it to
    // MAX_ADAPTIVE_CHARS_PER_SEC, so the drain takes the ceiling's time,
    // not the 500ms the trigger math asks for.
    const text = 'a'.repeat(200);
    smoother.appendDelta(text);
    const ceilingMs = (text.length / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000;
    const frames = Math.ceil((ceilingMs + 32) / 16);
    for (let i = 0; i < frames; i++) clock.tickFrame(16);
    // Single long word reveals all-at-once when budget catches up.
    expect(smoother.getRevealed().length).toBe(200);
    expect(smoother.isCaughtUp()).toBe(true);
  });

  it('keeps adaptive engaged while lag stays above the trigger', () => {
    // Adaptive math: when lag > 80, rate = lag * 1000 / 500 = 2 * lag.
    // After base time t at base rate, lag drops below 80 eventually.
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta('x'.repeat(ADAPTIVE_TRIGGER_CHARS + 1));
    const baseFramesFor1s = Math.ceil(1000 / 16);
    // At base rate, 81 chars would take 81/160 ≈ 506ms. Adaptive cuts
    // that to ~500ms regardless.
    for (let i = 0; i < baseFramesFor1s; i++) clock.tickFrame(16);
    expect(smoother.isCaughtUp()).toBe(true);
  });

  it('snaps synchronously, emitting the remaining delta', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('hello world');
    clock.tickFrame(16);
    clock.tickFrame(16);
    clock.tickFrame(16);
    expect(smoother.getRevealed()).toBe('hello ');
    smoother.snap();
    expect(smoother.getRevealed()).toBe('hello world');
    const last = reveals.at(-1)!;
    expect(last.revealed).toBe('hello world');
    expect(last.delta).toBe('world');
    expect(smoother.isCaughtUp()).toBe(true);
  });

  it('snap is a no-op when already caught up', () => {
    const { clock, reveals, smoother } = makeSmoother('done text');
    const before = reveals.length;
    smoother.snap();
    expect(reveals.length).toBe(before);
    expect(clock.pendingCount()).toBe(0);
  });

  it('snap cancels any pending rAF', () => {
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta('hello world');
    expect(clock.pendingCount()).toBe(1);
    smoother.snap();
    expect(clock.pendingCount()).toBe(0);
  });

  it('finishes naturally after the upstream completes', () => {
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta('one two three four');
    // Stream ends — no further appendDelta.
    // Let it run until caught up. Worst case ~120ms at base rate
    // for 18 chars, but we run longer to be safe.
    let frames = 0;
    while (!smoother.isCaughtUp() && frames < 200) {
      clock.tickFrame(16);
      frames++;
    }
    expect(smoother.isCaughtUp()).toBe(true);
    expect(smoother.getRevealed()).toBe('one two three four');
  });

  it('dispose stops further ticks and is idempotent', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('hello world foo bar');
    smoother.dispose();
    expect(clock.pendingCount()).toBe(0);
    const before = reveals.length;
    clock.tickFrame(16);
    clock.tickFrame(16);
    clock.tickFrame(16);
    expect(reveals.length).toBe(before);
    // Second dispose: no-op.
    smoother.dispose();
    expect(clock.pendingCount()).toBe(0);
  });

  it('appendDelta after dispose is a no-op', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.dispose();
    smoother.appendDelta('something');
    expect(reveals).toEqual([]);
    expect(clock.pendingCount()).toBe(0);
    expect(smoother.getReceived()).toBe('');
  });

  it('coalesces multiple appendDeltas into one tick budget', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('hello ');
    smoother.appendDelta('world ');
    smoother.appendDelta('foo bar baz');
    expect(clock.pendingCount()).toBe(1);
    // Run long enough to drain.
    let frames = 0;
    while (!smoother.isCaughtUp() && frames < 500) {
      clock.tickFrame(16);
      frames++;
    }
    expect(smoother.getRevealed()).toBe('hello world foo bar baz');
    // No reveal should have split a word — every revealed should end
    // on a word boundary (space) or be exactly the final value.
    const finalReveal = reveals.at(-1)!;
    expect(finalReveal.revealed).toBe('hello world foo bar baz');
  });

  it('getLag reflects the unrevealed gap', () => {
    const { smoother } = makeSmoother();
    smoother.appendDelta('hello world');
    expect(smoother.getLag()).toBe(11);
    smoother.snap();
    expect(smoother.getLag()).toBe(0);
  });

  it('animates only what arrives after the seed (mid-flight resume)', () => {
    // A resumed smoother starts caught up on its seed and animates the
    // wire from there — the seed text is already on screen, so replaying
    // it would be a visible rewind.
    const { clock, reveals, smoother } = makeSmoother('already on screen ');
    expect(smoother.isCaughtUp()).toBe(true);
    expect(clock.pendingCount()).toBe(0);

    smoother.appendDelta('and the rest arrives');
    drain(clock, smoother);
    expect(smoother.getRevealed()).toBe('already on screen and the rest arrives');
    expect(concatDeltas(reveals)).toBe('and the rest arrives');
  });

  it('rate stays close to 160 cps over a full stream', () => {
    const { clock, smoother } = makeSmoother();
    // 30 short words averaging ~5 chars each + space = ~180 chars.
    const text =
      'one two three four five six seven eight nine ten ' +
      'alpha beta gamma delta epsilon zeta eta theta iota kappa ' +
      'red blue green yellow purple orange black white gray pink';
    smoother.appendDelta(text);
    const start = clock.now();
    let frames = 0;
    while (!smoother.isCaughtUp() && frames < 1000) {
      clock.tickFrame(16);
      frames++;
    }
    const elapsed = clock.now() - start;
    // At pure 160 cps, text.length chars would take text.length/160 * 1000 ms.
    const ideal = (text.length / BASE_CHARS_PER_SEC) * 1000;
    // Allow ±25% slack for word-boundary stutter and fractional budget.
    expect(elapsed).toBeGreaterThanOrEqual(ideal * 0.75);
    expect(elapsed).toBeLessThanOrEqual(ideal * 1.5);
  });
});

describe('PerItemSmoother — rate ceiling and per-tick work cap', () => {
  // A fat wire burst opens a large lag; the adaptive catch-up must
  // drain it at MAX_ADAPTIVE_CHARS_PER_SEC regardless of display
  // refresh. Before the rate clamp, only the per-tick cap bounded the
  // reveal, and since ticks run at display refresh a 165Hz panel
  // revealed cap × 165 ≈ 2310 cps vs the intended ≈ 840 (2026-07-04
  // "catches up by speeding up a ton" report).
  function revealAfterOneSecond(frameMs: number): number {
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta('word '.repeat(1000)); // 5000-char burst
    const frames = Math.round(1000 / frameMs);
    for (let i = 0; i < frames; i++) clock.tickFrame(frameMs);
    return smoother.getRevealed().length;
  }

  it('drains a large burst at (not above) the rate ceiling on a 60Hz display', () => {
    // Ceiling-relative bounds: word-unit quantization and fractional
    // budget land a touch below the exact ceiling — acceptable; the
    // ceiling is an upper bound, and the lower bound proves the drain
    // genuinely runs NEAR it rather than at the 160cps base rate.
    const revealed = revealAfterOneSecond(1000 / 60);
    expect(revealed).toBeGreaterThan(MAX_ADAPTIVE_CHARS_PER_SEC * 0.8);
    expect(revealed).toBeLessThanOrEqual(MAX_ADAPTIVE_CHARS_PER_SEC + 20);
  });

  it('holds the ceiling on a 165Hz display instead of scaling with refresh', () => {
    const at165 = revealAfterOneSecond(1000 / 165);
    expect(at165).toBeGreaterThan(MAX_ADAPTIVE_CHARS_PER_SEC * 0.8);
    // THE regression bound: the per-tick-only ceiling revealed
    // cap × 165 ≈ 2310 chars here; the rate clamp holds it at the
    // refresh-independent ceiling.
    expect(at165).toBeLessThanOrEqual(MAX_ADAPTIVE_CHARS_PER_SEC + 20);
  });

  it('bounds DOM-mutation cadence on a 165Hz display (refresh decoupling)', () => {
    // Each onReveal is a markdown re-parse + DOM mutation + re-raster.
    // Unthrottled, catch-up revealed on every ~6ms frame (165/sec of
    // render work); MIN_REVEAL_TICK_INTERVAL_MS bounds processing to
    // ~55Hz while the reveal RATE (chars/sec) stays unchanged.
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('word '.repeat(1000));
    const frameMs = 1000 / 165;
    for (let i = 0; i < 165; i++) clock.tickFrame(frameMs);
    expect(reveals.length).toBeGreaterThan(30);
    expect(reveals.length).toBeLessThanOrEqual(67); // 1000ms / 15ms
  });

  it('phase-offset smoothers coalesce onto the shared wall-clock grid', () => {
    // The throttle gate is floor(now / interval) against a SHARED grid,
    // not per-instance elapsed time. Two panes whose streams started at
    // different moments must process in the SAME frame, so concurrent
    // streaming costs one render pipeline per interval instead of one
    // per frame (the storm-trace finding behind the grid, 2026-08-26).
    const clock = new FakeClock();
    const timesA: number[] = [];
    const timesB: number[] = [];
    const a = new PerItemSmoother({
      onReveal: () => timesA.push(clock.now()),
      clock,
    });
    a.appendDelta('word '.repeat(400));
    // B starts mid-slot, 7ms into A's stream — a per-instance elapsed
    // gate would put its processed ticks on different frames forever
    // (A on t=19,37,55…, B on t=25,43,61… at 6ms frames).
    clock.tickFrame(7);
    const b = new PerItemSmoother({
      onReveal: () => timesB.push(clock.now()),
      clock,
    });
    b.appendDelta('word '.repeat(400));
    for (let i = 0; i < 300; i++) clock.tickFrame(6);
    expect(timesA.length).toBeGreaterThan(30);
    expect(timesB.length).toBeGreaterThan(30);
    // On the shared grid both smoothers process (and so reveal) in the
    // same frames; per-instance phases would make the two sets disjoint.
    const setA = new Set(timesA);
    const shared = timesB.filter((t) => setA.has(t)).length;
    expect(shared).toBeGreaterThan(0.8 * Math.min(timesA.length, timesB.length));
    a.dispose();
    b.dispose();
  });

  it('advances through a word unit larger than the per-tick cap', () => {
    // A single unbroken token bigger than the cap (long URL / identifier).
    // The cap expands to that one unit's size — budget accumulates until it
    // fits — so the reveal never stalls on it and the word never splits.
    const giant = 'x'.repeat(MAX_ADVANCE_PER_TICK_CHARS + 24);
    const text = `${giant} tail words follow now`;
    const { clock, smoother, reveals } = makeSmoother();
    smoother.appendDelta(text);
    drain(clock, smoother);
    expect(smoother.getRevealed()).toBe(text);
    // The giant token revealed as one whole chunk, never split mid-word.
    const giantReveal = reveals.find((r) => r.delta.includes(giant));
    expect(giantReveal).toBeDefined();
  });

  it('clamps a stalled frame’s ballooned budget instead of bursting later frames', () => {
    // A long frame gap (occluded window, a blocked main thread) accrues a
    // budget far past the per-tick cap. The tick spends at most the cap and
    // must clamp the residual, or the next frames sustain full-cap advances
    // off leftover budget instead of easing back to the reveal cadence.
    const backlog = 'ab '.repeat(67); // 201 chars
    const { clock, smoother, reveals } = makeSmoother();
    smoother.appendDelta(backlog);
    clock.tickFrame(1000); // ~320 chars of budget accrued in one tick
    expect(reveals.at(-1)!.revealed.length).toBeLessThanOrEqual(
      MAX_ADVANCE_PER_TICK_CHARS,
    );

    reveals.length = 0;
    clock.tickFrame(16);
    clock.tickFrame(16);
    clock.tickFrame(16);
    const revealedAfterStall = reveals.reduce((n, r) => n + r.delta.length, 0);
    // Clamped: one residual spend of ≤ the cap, then the ordinary
    // rate-limited accrual. Unclamped, all three frames would run at the
    // full cap off the stall's leftover budget.
    expect(revealedAfterStall).toBeLessThanOrEqual(
      MAX_ADVANCE_PER_TICK_CHARS * 2,
    );
    expect(revealedAfterStall).toBeGreaterThan(0);
  });
});

describe('PerItemSmoother — reveal sequencing primitives', () => {
  it('pause holds the reveal cursor while received keeps accumulating', () => {
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta('hello world here we go');
    // Three frames at base rate reveal the first word.
    clock.tickFrame(16);
    clock.tickFrame(16);
    clock.tickFrame(16);
    const revealedAtPause = smoother.getRevealed();
    expect(revealedAtPause).toBe('hello ');
    expect(clock.pendingCount()).toBe(1);

    smoother.pause();
    expect(smoother.isPaused()).toBe(true);
    // Pausing cancels the pending rAF so no further reveal fires.
    expect(clock.pendingCount()).toBe(0);

    // Wire keeps arriving and many frames pass — reveal must not move.
    smoother.appendDelta(' and more');
    for (let i = 0; i < 50; i++) clock.tickFrame(16);
    expect(smoother.getRevealed()).toBe(revealedAtPause);
    expect(smoother.getReceived()).toBe('hello world here we go and more');

    // Resume continues from where it left off and drains to completion.
    smoother.resume();
    expect(smoother.isPaused()).toBe(false);
    let frames = 0;
    while (!smoother.isCaughtUp() && frames < 500) {
      clock.tickFrame(16);
      frames++;
    }
    expect(smoother.getRevealed()).toBe('hello world here we go and more');
  });

  it('pause is idempotent and resume on a non-paused smoother is a no-op', () => {
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta('hello world');
    expect(clock.pendingCount()).toBe(1);
    smoother.resume(); // not paused → no-op
    expect(clock.pendingCount()).toBe(1);
    smoother.pause();
    smoother.pause(); // idempotent
    expect(smoother.isPaused()).toBe(true);
    expect(clock.pendingCount()).toBe(0);
  });

  it('snap reveals everything even while paused (interrupt while withheld)', () => {
    const { smoother } = makeSmoother();
    smoother.appendDelta('hello world');
    smoother.pause();
    smoother.snap();
    expect(smoother.getRevealed()).toBe('hello world');
    expect(smoother.isCaughtUp()).toBe(true);
  });

});

describe('PerItemSmoother — never skips (the queue is self-correcting)', () => {
  it('animates every character of a multi-KB backlog, never jumping ahead', () => {
    // A fat burst (a whole reasoning block landing in one wire chunk) is
    // drained in full, at the ceiling, in order. There is no bound, no
    // floor, and no skip: the reader sees every character.
    const text = 'word '.repeat(1200); // 6000 chars
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta(text);
    expect(smoother.getLag()).toBe(text.length);

    const frames = drain(clock, smoother, 20000);
    expect(smoother.getRevealed()).toBe(text);
    expect(concatDeltas(reveals)).toBe(text);
    // Nothing arrived as an over-cap jump — every frame is bounded work.
    for (const r of reveals) {
      expect(r.delta.length).toBeLessThanOrEqual(MAX_ADVANCE_PER_TICK_CHARS);
    }
    // And it genuinely took the rate-limited time. A skip of any kind
    // would land this well under the ceiling's implied duration.
    const ceilingFrames = (text.length / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000 / 16;
    expect(frames).toBeGreaterThan(ceilingFrames * 0.9);
  });

  it('an idle wire gap drains the backlog to zero — lag is transient, not carried', () => {
    // THE rationale for having no skip: the wire is bursty. Tool calls,
    // API round-trips and model pauses are stretches with no appends, and
    // the drain keeps running through them. Two bursts with a gap between
    // them must leave the smoother fully caught up in the gap, not
    // accumulating across bursts.
    const { clock, reveals, smoother } = makeSmoother();
    const burst = 'word '.repeat(60); // 300 chars

    smoother.appendDelta(burst);
    expect(smoother.getLag()).toBe(burst.length);
    // The gap: a tool call executing. No appends, frames keep ticking.
    const gapFrames = drain(clock, smoother, 20000);
    expect(smoother.getLag()).toBe(0);
    expect(smoother.isCaughtUp()).toBe(true);
    // The gap needed is bounded by the ceiling, i.e. ~1s here — well
    // inside a real tool call, which is why the queue self-corrects.
    expect(gapFrames * 16).toBeLessThan(
      (burst.length / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000 * 1.6,
    );

    // Second burst after the gap starts from zero lag, so backlog does
    // not compound across bursts.
    smoother.appendDelta(burst);
    expect(smoother.getLag()).toBe(burst.length);
    drain(clock, smoother, 20000);
    expect(smoother.getRevealed()).toBe(burst + burst);
    expect(concatDeltas(reveals)).toBe(burst + burst);
  });

  it('replays a withheld row\'s whole backlog when it resumes', () => {
    // A row the reveal sequencer held back streamed invisibly. On resume
    // it animates from where it left off — the start — rather than
    // catching up to the wire head. Nothing that streamed while it was
    // withheld is dropped.
    const { clock, reveals, smoother } = makeSmoother();
    smoother.pause();
    const withheld = 'word '.repeat(60); // 300 chars
    smoother.appendDelta(withheld);
    for (let f = 0; f < 40; f++) clock.tickFrame(16);
    expect(reveals).toHaveLength(0);
    expect(smoother.getLag()).toBe(withheld.length);

    smoother.resume();
    clock.tickFrame(16);
    // First resumed frame is ordinary bounded work starting at index 0,
    // not a jump to the tail.
    expect(reveals).toHaveLength(1);
    expect(reveals[0].delta.length).toBeLessThanOrEqual(
      MAX_ADVANCE_PER_TICK_CHARS,
    );
    expect(withheld.startsWith(reveals[0].revealed)).toBe(true);

    drain(clock, smoother, 20000);
    expect(smoother.getRevealed()).toBe(withheld);
    expect(concatDeltas(reveals)).toBe(withheld);
  });

  it('keeps the emitted deltas gap-free under interleaved bursts (expansion-payload integrity)', () => {
    // Consumers accumulating deltas (the live payload expansion) rebuild
    // the text from the delta stream alone, so `revealed` must always be
    // exactly the running concatenation.
    const { clock, reveals, smoother } = makeSmoother();
    let received = '';
    for (let burst = 0; burst < 4; burst++) {
      const chunk = `burst${burst} ` + 'word '.repeat(30);
      received += chunk;
      smoother.appendDelta(chunk);
      for (let f = 0; f < 3; f++) clock.tickFrame(16);
      const small = `tail${burst} `;
      received += small;
      smoother.appendDelta(small);
      for (let f = 0; f < 2; f++) clock.tickFrame(16);
    }
    drain(clock, smoother, 20000);

    expect(smoother.getReceived()).toBe(received);
    expect(smoother.getRevealed()).toBe(received);
    expect(concatDeltas(reveals)).toBe(received);
    let running = '';
    for (const r of reveals) {
      running += r.delta;
      expect(r.revealed).toBe(running);
    }
  });

  it('extends an unrevealed word unit across provider chunk boundaries', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('hel');
    smoother.appendDelta('lo ');

    clock.tickFrame(100);

    expect(reveals.map((entry) => entry.delta)).toEqual(['hello ']);
    expect(smoother.getRevealed()).toBe('hello ');
  });

  it('starts a new unit when a provider continues a word after its prefix was revealed', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('hel');
    clock.tickFrame(100);
    expect(smoother.getRevealed()).toBe('hel');

    smoother.appendDelta('lo ');
    clock.tickFrame(100);

    expect(reveals.map((entry) => entry.delta)).toEqual(['hel', 'lo ']);
    expect(smoother.getRevealed()).toBe('hello ');
  });

  it('keeps a leading-whitespace unit intact across provider chunks', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.appendDelta('  ');
    smoother.appendDelta('hello');
    smoother.appendDelta(' ');

    clock.tickFrame(100);

    expect(reveals.map((entry) => entry.delta)).toEqual(['  hello ']);
  });

  it('preserves source and reveal order across source-part compaction', () => {
    const { clock, reveals, smoother } = makeSmoother();
    smoother.pause();
    const chunks = Array.from({ length: 600 }, (_, index) => `${index} `);
    for (const chunk of chunks) smoother.appendDelta(chunk);
    const source = chunks.join('');

    expect(smoother.getReceived()).toBe(source);
    smoother.resume();
    drain(clock, smoother, 20_000);

    expect(smoother.getRevealed()).toBe(source);
    expect(concatDeltas(reveals)).toBe(source);
  });

  it('preserves every cursor invariant across randomized call sequences', () => {
    const atoms = [
      'word', ' ', '  ', '\n', '\r\n', '\t', 'punctuation, ',
      '**markdown** ', 'café ', '漢字', 'e\u0301 ', 'https://example.test/a_b',
    ];
    for (let seed = 1; seed <= 24; seed++) {
      let state = seed >>> 0;
      const random = () => {
        state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0;
        return state;
      };
      const initial = seed % 2 === 0 ? 'seed ' : '';
      const clock = new FakeClock();
      let immediate = false;
      let expectedReceived = initial;
      let expectedRevealed = initial;
      const smoother = new PerItemSmoother({
        initialReceived: initial,
        revealImmediately: () => immediate,
        clock,
        onReveal: (delta, revealedEnd, previousCodeUnit) => {
          expect(delta.length).toBeGreaterThan(0);
          expect(previousCodeUnit).toBe(
            expectedRevealed.length === 0
              ? -1
              : expectedRevealed.charCodeAt(expectedRevealed.length - 1),
          );
          expectedRevealed += delta;
          expect(revealedEnd).toBe(expectedRevealed.length);
          expect(expectedReceived.startsWith(expectedRevealed)).toBe(true);
        },
      });

      for (let step = 0; step < 800; step++) {
        switch (random() % 8) {
          case 0:
          case 1:
          case 2: {
            const delta = atoms[random() % atoms.length];
            expectedReceived += delta;
            smoother.appendDelta(delta);
            break;
          }
          case 3:
            clock.tickFrame(1 + (random() % 100));
            break;
          case 4:
            smoother.pause();
            break;
          case 5:
            smoother.resume();
            break;
          case 6:
            smoother.snap();
            break;
          case 7:
            immediate = !immediate;
            break;
        }
        expect(smoother.getLag()).toBe(
          expectedReceived.length - expectedRevealed.length,
        );
        expect(smoother.isCaughtUp()).toBe(
          expectedReceived.length === expectedRevealed.length,
        );
        if (step % 23 === 0) {
          expect(smoother.getReceived()).toBe(expectedReceived);
          expect(smoother.getRevealed()).toBe(expectedRevealed);
        }
      }

      smoother.resume();
      smoother.snap();
      expect(smoother.getReceived()).toBe(expectedReceived);
      expect(smoother.getRevealed()).toBe(expectedReceived);
      expect(expectedRevealed).toBe(expectedReceived);

      smoother.dispose();
      smoother.appendDelta('ignored');
      smoother.snap();
      clock.tickFrame(100);
      expect(smoother.getReceived()).toBe(expectedReceived);
      expect(smoother.getRevealed()).toBe(expectedReceived);
    }
  });
});

describe('PerItemSmoother — revealImmediately (low power)', () => {
  function makeImmediateSmoother(active: { value: boolean }) {
    const clock = new FakeClock();
    const reveals: RevealEntry[] = [];
    const smoother = new PerItemSmoother({
      onReveal: (delta) =>
        reveals.push({
          revealed: (reveals.at(-1)?.revealed ?? '') + delta,
          delta,
        }),
      revealImmediately: () => active.value,
      clock,
    });
    return { clock, reveals, smoother };
  }

  it('reveals the whole backlog in one mutation on the next tick', () => {
    const active = { value: true };
    const { clock, reveals, smoother } = makeImmediateSmoother(active);

    smoother.appendDelta('a long paragraph that would normally animate word by word over many frames');
    // Reveal stays asynchronous — nothing is revealed inside appendDelta.
    expect(reveals).toHaveLength(0);

    clock.tickFrame(1);
    expect(reveals).toHaveLength(1);
    expect(smoother.isCaughtUp()).toBe(true);
    expect(smoother.getRevealed()).toBe(
      'a long paragraph that would normally animate word by word over many frames',
    );

    // Each subsequent chunk is one reveal on its next frame — no
    // animated multi-frame cadence.
    smoother.appendDelta(' and a second wire chunk');
    clock.tickFrame(1);
    expect(reveals).toHaveLength(2);
    expect(smoother.isCaughtUp()).toBe(true);
  });

  it('is sampled per tick: flipping mid-animation snaps the remainder', () => {
    const active = { value: false };
    const { clock, reveals, smoother } = makeImmediateSmoother(active);

    smoother.appendDelta('one two three four five six seven eight nine ten eleven twelve');
    // Animate a couple of frames at the normal word cadence.
    clock.tickFrame(16);
    clock.tickFrame(16);
    expect(smoother.isCaughtUp()).toBe(false);
    const animatedReveals = reveals.length;

    active.value = true;
    clock.tickFrame(16);
    expect(smoother.isCaughtUp()).toBe(true);
    // Exactly one additional reveal carrying everything left.
    expect(reveals).toHaveLength(animatedReveals + 1);
    expect(reveals[reveals.length - 1].revealed).toBe(
      'one two three four five six seven eight nine ten eleven twelve',
    );
  });

  it('flipping off resumes the animated cadence for later chunks', () => {
    const active = { value: true };
    const { clock, smoother } = makeImmediateSmoother(active);

    smoother.appendDelta('first chunk of text');
    clock.tickFrame(1);
    expect(smoother.isCaughtUp()).toBe(true);

    active.value = false;
    smoother.appendDelta(
      'a much longer follow-up that should animate word by word again across frames',
    );
    clock.tickFrame(16);
    // Base cadence: far from caught up after one frame.
    expect(smoother.isCaughtUp()).toBe(false);
  });

  it('flipping off after a long low-power stint does not burst the first animated frame', () => {
    const active = { value: true };
    const { clock, reveals, smoother } = makeImmediateSmoother(active);

    // Stream in low power for ~20s: each chunk snaps on its next frame.
    // Every one of these ticks must keep lastTickAt current — a stale
    // value would make the first animated tick below see the whole
    // stint as one dt, balloon the budget, and dump a full per-tick
    // cap (~18 chars) instead of easing in at the base word cadence.
    for (let i = 0; i < 100; i++) {
      smoother.appendDelta(`chunk${i} `);
      clock.tickFrame(200);
    }
    expect(smoother.isCaughtUp()).toBe(true);

    active.value = false;
    reveals.length = 0;
    smoother.appendDelta('alpha beta gamma delta epsilon zeta eta');
    clock.tickFrame(16);
    const firstFrameChars = reveals.reduce((n, r) => n + r.delta.length, 0);
    // One 16ms frame of base-rate budget is ~2.5 chars — at most one
    // short word unit, nowhere near the 18-char per-tick cap.
    expect(firstFrameChars).toBeLessThanOrEqual(8);
  });

  it('a paused smoother that resumes under low power snaps in one reveal', () => {
    const active = { value: true };
    const { clock, reveals, smoother } = makeImmediateSmoother(active);

    smoother.pause();
    smoother.appendDelta('text streamed while withheld behind the reveal gate');
    clock.tickFrame(16);
    // Paused: nothing ticks, nothing reveals.
    expect(reveals).toHaveLength(0);

    smoother.resume();
    clock.tickFrame(16);
    expect(reveals).toHaveLength(1);
    expect(smoother.isCaughtUp()).toBe(true);
    expect(smoother.getRevealed()).toBe(
      'text streamed while withheld behind the reveal gate',
    );
  });
});

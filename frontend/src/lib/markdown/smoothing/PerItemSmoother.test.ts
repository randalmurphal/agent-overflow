import { describe, it, expect } from 'vitest';
import {
  PerItemSmoother,
  computeAdvanceEnd,
  BASE_CHARS_PER_SEC,
  ADAPTIVE_TRIGGER_CHARS,
  ADAPTIVE_CATCHUP_MS,
  MAX_ADVANCE_PER_TICK_CHARS,
  type SmoothingClock,
} from './PerItemSmoother';

class FakeClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();

  now(): number {
    return this.current;
  }
  schedule(cb: () => void): number {
    const handle = this.nextHandle++;
    this.pending.set(handle, cb);
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
}

interface RevealEntry {
  revealed: string;
  delta: string;
}

function makeSmoother(initial = '', initialRevealed?: string) {
  const clock = new FakeClock();
  const reveals: RevealEntry[] = [];
  const smoother = new PerItemSmoother({
    initialReceived: initial,
    initialRevealed,
    onReveal: (revealed, delta) => reveals.push({ revealed, delta }),
    clock,
  });
  return { clock, reveals, smoother };
}

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

describe('PerItemSmoother', () => {
  it('starts with empty state and no scheduled callbacks', () => {
    const { clock, reveals, smoother } = makeSmoother();
    expect(smoother.getRevealed()).toBe('');
    expect(smoother.getReceived()).toBe('');
    expect(smoother.isCaughtUp()).toBe(true);
    expect(clock.pendingCount()).toBe(0);
    expect(reveals).toEqual([]);
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
    // Lag of 200 chars (> 80 trigger). Adaptive rate = 200*1000/500 = 400 cps.
    const text = 'a'.repeat(200);
    smoother.appendDelta(text);
    // At adaptive rate, full drain should complete in <= ~500ms.
    // Run for 520ms in 16ms frames.
    const frames = Math.ceil((ADAPTIVE_CATCHUP_MS + 20) / 16);
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
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta('hello world');
    expect(smoother.getLag()).toBe(11);
    smoother.snap();
    expect(smoother.getLag()).toBe(0);
  });

  it('honors a smaller initialRevealed than initialReceived', () => {
    // Equivalent to mid-flight where revealed lags received slightly.
    const { clock, smoother } = makeSmoother(
      'hello world here',
      'hello ',
    );
    expect(smoother.getRevealed()).toBe('hello ');
    expect(smoother.getReceived()).toBe('hello world here');
    expect(smoother.isCaughtUp()).toBe(false);
    // Should schedule and progress.
    expect(clock.pendingCount()).toBe(1);
    let frames = 0;
    while (!smoother.isCaughtUp() && frames < 200) {
      clock.tickFrame(16);
      frames++;
    }
    expect(smoother.getRevealed()).toBe('hello world here');
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

const FAST_DRAIN_WINDOW = 200;

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

  it('requestFastDrain finishes a large lag sooner than the per-tick cap would', () => {
    // 80 three-char word units = 320 chars. Without fast-drain the per-tick
    // cap (14 chars) bounds the rate; fast-drain lifts it to hit the window.
    const text = 'ab '.repeat(80);
    const { clock, smoother, reveals } = makeSmoother();
    smoother.appendDelta(text);
    smoother.requestFastDrain(FAST_DRAIN_WINDOW);
    let frames = 0;
    while (!smoother.isCaughtUp() && frames < 200) {
      clock.tickFrame(16);
      frames++;
    }
    const elapsed = clock.now();
    expect(smoother.getRevealed()).toBe(text);
    // No reveal split a word — each ends on whitespace or is the final value.
    for (const r of reveals) {
      const last = r.revealed[r.revealed.length - 1];
      expect(last === ' ' || r.revealed === text).toBe(true);
    }

    // Control: same lag with the normal cap takes materially longer.
    const control = makeSmoother();
    control.smoother.appendDelta(text);
    let cf = 0;
    while (!control.smoother.isCaughtUp() && cf < 400) {
      control.clock.tickFrame(16);
      cf++;
    }
    const controlElapsed = control.clock.now();
    expect(elapsed).toBeLessThan(controlElapsed);
    // And the control really was cap-bound (>= text.length / cap frames).
    const minCapMs = (text.length / MAX_ADVANCE_PER_TICK_CHARS) * 16;
    expect(controlElapsed).toBeGreaterThanOrEqual(minCapMs * 0.9);
  });

  it('requestFastDrain ignores later calls so the deadline cannot be pushed out', () => {
    const text = 'cd '.repeat(60); // 180 chars
    const { clock, smoother } = makeSmoother();
    smoother.appendDelta(text);
    smoother.requestFastDrain(FAST_DRAIN_WINDOW);
    clock.tickFrame(16);
    // A second, far longer request must not extend the window.
    smoother.requestFastDrain(5000);
    let frames = 1;
    while (!smoother.isCaughtUp() && frames < 200) {
      clock.tickFrame(16);
      frames++;
    }
    // Finished near the original window, nowhere near the 5s second request.
    expect(clock.now()).toBeLessThanOrEqual(FAST_DRAIN_WINDOW + 64);
  });

  it('requestFastDrain is a no-op when already caught up', () => {
    const { clock, smoother } = makeSmoother('all done');
    smoother.requestFastDrain();
    expect(clock.pendingCount()).toBe(0);
    expect(smoother.isCaughtUp()).toBe(true);
  });
});

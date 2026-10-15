import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
// The real stylesheet: the ring is the theme's accent, read through its
// custom property inside the keyframes.
import '../../app.css';
import { jumpPulse, REVIEW_JUMP_PULSE_MS } from './reviewJumpPulse';

// A Web Animations ring on a real element: happy-dom has no animation
// timeline and no computed box-shadow, so this needs the `browser` vitest
// project (real Chromium). Run with `pnpm test:browser`.

const hosts: HTMLElement[] = [];
const cleanups: Array<() => void> = [];

function card(): HTMLElement {
  const el = document.createElement('div');
  el.style.width = '120px';
  el.style.height = '40px';
  document.body.appendChild(el);
  hosts.push(el);
  return el;
}

/** Runs the attachment the way Svelte does: apply, keep the cleanup. */
function apply(el: HTMLElement, generation: number): void {
  const cleanup = jumpPulse(generation)(el);
  if (typeof cleanup === 'function') cleanups.push(cleanup);
}

function ring(el: HTMLElement): string {
  return getComputedStyle(el).boxShadow;
}

let reduced = false;

beforeEach(() => {
  reduced = false;
  const real = window.matchMedia.bind(window);
  vi.stubGlobal('matchMedia', (query: string) =>
    query === '(prefers-reduced-motion: reduce)'
      ? ({ matches: reduced, addEventListener() {}, removeEventListener() {} } as unknown as MediaQueryList)
      : real(query));
});

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
  for (const el of hosts.splice(0)) el.remove();
  vi.unstubAllGlobals();
});

describe('jumpPulse', () => {
  it('does nothing for generation 0', () => {
    const el = card();
    apply(el, 0);
    expect(el.getAnimations()).toHaveLength(0);
    expect(ring(el)).toBe('none');
  });

  it('paints an accent ring that fades out, and the ring is gone once the pulse ends', async () => {
    const el = card();
    apply(el, 1);
    const [animation] = el.getAnimations();
    expect(animation).toBeDefined();
    expect(ring(el)).not.toBe('none');
    expect(ring(el)).toContain('2px');
    const frames = (animation.effect as KeyframeEffect).getKeyframes();
    expect(frames).toHaveLength(3);
    expect(frames[0].boxShadow).toBe(frames[1].boxShadow);
    expect(String(frames[2].boxShadow)).toContain('transparent');
    expect(animation.effect!.getTiming().duration).toBe(REVIEW_JUMP_PULSE_MS);
    expect(animation.effect!.getTiming().fill).toBe('none');
    // At the end of the pulse the element is exactly as it was.
    animation.finish();
    await animation.finished;
    expect(el.getAnimations()).toHaveLength(0);
    expect(ring(el)).toBe('none');
  });

  it('restarts for a new generation and cancels on cleanup', () => {
    const el = card();
    const first = jumpPulse(1)(el) as () => void;
    const [a] = el.getAnimations();
    a.currentTime = 900;
    // Svelte tears the old attachment down before running the new one.
    first();
    expect(el.getAnimations()).toHaveLength(0);
    apply(el, 2);
    const [b] = el.getAnimations();
    expect(b).not.toBe(a);
    expect(Number(b.currentTime)).toBeLessThan(100);
    expect(ring(el)).not.toBe('none');
  });

  it('holds the ring unchanged under reduced motion, then clears', async () => {
    reduced = true;
    const el = card();
    apply(el, 1);
    const [animation] = el.getAnimations();
    const frames = (animation.effect as KeyframeEffect).getKeyframes();
    expect(frames).toHaveLength(2);
    expect(frames[0].boxShadow).toBe(frames[1].boxShadow);
    const painted = ring(el);
    expect(painted).not.toBe('none');
    animation.currentTime = REVIEW_JUMP_PULSE_MS - 1;
    expect(ring(el)).toBe(painted);
    animation.finish();
    await animation.finished;
    expect(ring(el)).toBe('none');
  });
});

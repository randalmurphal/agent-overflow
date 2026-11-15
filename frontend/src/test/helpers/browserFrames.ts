// Frame-stepping helpers for the real-Chromium `browser` vitest project.
// happy-dom suites fake time; these tests instead wait on the real engine —
// a frame at a time — for ResizeObserver deliveries, scroll events, and
// layout to settle.

export function raf(): Promise<void> {
  return new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

/**
 * How long `waitFor` waits by default: far above any settle on a loaded
 * runner, and below the suite's hang timeout so the label names the wait.
 */
const SETTLE_TIMEOUT_MS = 20_000;

// Poll an observable predicate once a frame until it holds. The bound is
// wall-clock time, not a frame count: work that yields between frames (a
// chunked scan, a measurement pass) takes more frames on a slow runner, and
// a frame budget would fail it there.
export async function waitFor(
  predicate: () => boolean,
  label: string,
  timeoutMs = SETTLE_TIMEOUT_MS,
): Promise<void> {
  const deadline = performance.now() + timeoutMs;
  for (;;) {
    if (predicate()) return;
    if (performance.now() >= deadline) throw new Error(`timed out after ${timeoutMs} ms waiting for: ${label}`);
    await raf();
  }
}

// Assert that a predicate holds within `frames` frames: for behavior whose
// claim is a frame count (an install inside a presentation ceiling), not
// for waiting on a settle.
export async function withinFrames(
  predicate: () => boolean,
  label: string,
  frames: number,
): Promise<void> {
  for (let i = 0; i < frames; i++) {
    if (predicate()) return;
    await raf();
  }
  if (predicate()) return;
  throw new Error(`not within ${frames} frames: ${label}`);
}

export function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

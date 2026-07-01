// Frame-stepping helpers for the real-Chromium `browser` vitest project.
// happy-dom suites fake time; these tests instead wait on the real engine —
// a frame at a time — for ResizeObserver deliveries, scroll events, and
// layout to settle.

export function raf(): Promise<void> {
  return new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

// Poll an observable predicate across frames — deterministic settle detection
// without guessing a fixed number of rAFs for the engine to deliver.
export async function waitFor(
  predicate: () => boolean,
  label: string,
  frameBudget = 120,
): Promise<void> {
  for (let i = 0; i < frameBudget; i++) {
    if (predicate()) return;
    await raf();
  }
  throw new Error(`timed out waiting for: ${label}`);
}

export function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

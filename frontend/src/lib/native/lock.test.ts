// The app lock, and the one thing about it that is not a pure function:
// WHEN the screen gets covered. Covering on resume left the app's own
// pixels in the task switcher's thumbnail for the whole time the phone was
// away, and on screen for the frame between the window being shown again
// and the resume handler running. So the cover goes up on PAUSE, and what
// resume decides is whether to PROMPT.

import { beforeEach, describe, expect, it, vi } from 'vitest';

const { plugins } = vi.hoisted(() => ({
  plugins: {
    listeners: new Map<string, () => void>(),
    authenticate: vi.fn(async () => undefined),
    nativeShell: true,
  },
}));

vi.mock('./platform', () => ({
  isNativeShell: () => plugins.nativeShell,
}));

vi.mock('./plugins', () => ({
  biometricPlugin: async () => ({ authenticate: plugins.authenticate }),
  appPlugin: async () => ({
    addListener: async (event: string, handler: () => void) => {
      plugins.listeners.set(event, handler);
      return { remove: async () => plugins.listeners.delete(event) };
    },
  }),
}));

import { DEFAULT_LOCK_WINDOW_MS, installAppLock, shouldLock } from './lock';

function fire(event: 'pause' | 'resume'): void {
  const handler = plugins.listeners.get(event);
  if (!handler) throw new Error(`nothing listening for ${event}`);
  handler();
}

beforeEach(() => {
  plugins.listeners.clear();
  plugins.authenticate.mockReset();
  plugins.authenticate.mockResolvedValue(undefined);
  plugins.nativeShell = true;
  vi.useRealTimers();
});

describe('shouldLock', () => {
  it('locks a cold start, a zero window, and a clock that moved backwards', () => {
    expect(shouldLock(null, 1000, DEFAULT_LOCK_WINDOW_MS)).toBe(true);
    expect(shouldLock(900, 1000, 0)).toBe(true);
    expect(shouldLock(2000, 1000, DEFAULT_LOCK_WINDOW_MS)).toBe(true);
  });

  it('lets a short trip back into the app pass', () => {
    expect(shouldLock(1000, 6000, DEFAULT_LOCK_WINDOW_MS)).toBe(false);
    expect(shouldLock(1000, 1000 + DEFAULT_LOCK_WINDOW_MS, DEFAULT_LOCK_WINDOW_MS)).toBe(true);
  });
});

describe('installAppLock', () => {
  it('covers the screen on pause, before any resume can run', async () => {
    const changes: boolean[] = [];
    const lock = await installAppLock({ onChange: (locked) => changes.push(locked) });
    await Promise.resolve();
    await Promise.resolve();
    expect(lock.locked()).toBe(false); // the cold-start prompt passed

    fire('pause');

    expect(lock.locked()).toBe(true);
    expect(changes.at(-1)).toBe(true);
    lock.dispose();
  });

  it('lifts the cover without a prompt when the trip was short', async () => {
    const lock = await installAppLock({ backgroundWindowMs: 60_000 });
    await Promise.resolve();
    await Promise.resolve();
    plugins.authenticate.mockClear();

    fire('pause');
    expect(lock.locked()).toBe(true);
    fire('resume');

    expect(lock.locked()).toBe(false);
    expect(plugins.authenticate).not.toHaveBeenCalled();
    lock.dispose();
  });

  it('keeps the cover up and prompts when the trip was long', async () => {
    const lock = await installAppLock({ backgroundWindowMs: 0 });
    await Promise.resolve();
    await Promise.resolve();
    plugins.authenticate.mockClear();

    fire('pause');
    fire('resume');

    expect(plugins.authenticate).toHaveBeenCalledTimes(1);
    lock.dispose();
  });

  it('stays covered when the prompt is dismissed', async () => {
    const lock = await installAppLock({ backgroundWindowMs: 0 });
    await Promise.resolve();
    await Promise.resolve();
    plugins.authenticate.mockRejectedValue(new Error('dismissed'));

    fire('pause');
    fire('resume');
    await Promise.resolve();
    await Promise.resolve();

    expect(lock.locked()).toBe(true);
    lock.dispose();
  });

  it('does not re-announce a pause that arrives while already covered', async () => {
    const changes: boolean[] = [];
    const lock = await installAppLock({ onChange: (locked) => changes.push(locked) });
    await Promise.resolve();
    await Promise.resolve();

    fire('pause');
    const after = changes.length;
    fire('pause');

    expect(changes.length).toBe(after);
    lock.dispose();
  });

  // The two facts the lock keeps apart. A cover raised by a pause comes
  // down on a quick return; a prompt that was owed before the trip does
  // not. With one flag for both, a three-second trip to another app
  // revealed an app whose prompt nobody had passed.
  it('keeps a prompt owed across a short trip, and joins the one still up', async () => {
    let pass: () => void = () => {};
    plugins.authenticate.mockImplementation(
      () => new Promise<undefined>((resolve) => { pass = () => resolve(undefined); }),
    );
    const lock = await installAppLock({ backgroundWindowMs: 60_000 });
    await Promise.resolve();
    expect(lock.locked()).toBe(true); // the cold-start prompt is up

    fire('pause');
    fire('resume');

    // Still covered, and still the ONE prompt: the resume joined it.
    expect(lock.locked()).toBe(true);
    expect(plugins.authenticate).toHaveBeenCalledTimes(1);

    pass();
    await Promise.resolve();
    await Promise.resolve();
    expect(lock.locked()).toBe(false);
    lock.dispose();
  });

  it('re-raises a dismissed prompt on the next resume, however short the trip', async () => {
    plugins.authenticate.mockRejectedValue(new Error('dismissed'));
    const lock = await installAppLock({ backgroundWindowMs: 60_000 });
    await Promise.resolve();
    await Promise.resolve();
    expect(lock.locked()).toBe(true);
    expect(plugins.authenticate).toHaveBeenCalledTimes(1);

    fire('pause');
    fire('resume');
    await Promise.resolve();
    await Promise.resolve();

    expect(plugins.authenticate).toHaveBeenCalledTimes(2);
    expect(lock.locked()).toBe(true);
    lock.dispose();
  });

  it('does not stack a second prompt on one that is still up', async () => {
    let pass: () => void = () => {};
    plugins.authenticate.mockImplementation(
      () => new Promise<undefined>((resolve) => { pass = () => resolve(undefined); }),
    );
    const lock = await installAppLock();
    await Promise.resolve();

    const again = lock.unlock();
    expect(plugins.authenticate).toHaveBeenCalledTimes(1);

    pass();
    await expect(again).resolves.toBe(true);
    expect(lock.locked()).toBe(false);
    lock.dispose();
  });

  // The prompt is an activity of its own: it pauses this app on the way
  // up and resumes it on the way down, after the answer. That resume is
  // not a trip, however the window is set.
  it('does not prompt again on the resume its own prompt produces', async () => {
    let pass: () => void = () => {};
    plugins.authenticate.mockImplementation(
      () => new Promise<undefined>((resolve) => { pass = () => resolve(undefined); }),
    );
    const lock = await installAppLock({ backgroundWindowMs: 0 });
    await Promise.resolve();
    fire('pause'); // the prompt's activity came up
    pass();
    await Promise.resolve();
    await Promise.resolve();
    expect(lock.locked()).toBe(false);

    fire('resume'); // and closed

    expect(lock.locked()).toBe(false);
    expect(plugins.authenticate).toHaveBeenCalledTimes(1);
    lock.dispose();
  });

  it('leaves a dismissed prompt down until the button raises it', async () => {
    let dismiss: () => void = () => {};
    plugins.authenticate.mockImplementation(
      () => new Promise<undefined>((_, reject) => { dismiss = () => reject(new Error('dismissed')); }),
    );
    const lock = await installAppLock({ backgroundWindowMs: 60_000 });
    await Promise.resolve();
    fire('pause');
    dismiss();
    await Promise.resolve();
    await Promise.resolve();

    fire('resume');

    expect(lock.locked()).toBe(true);
    expect(plugins.authenticate).toHaveBeenCalledTimes(1);
    void lock.unlock();
    expect(plugins.authenticate).toHaveBeenCalledTimes(2);
    lock.dispose();
  });

  it('listens to nothing off the shell', async () => {
    plugins.nativeShell = false;
    const lock = await installAppLock();
    expect(lock.locked()).toBe(false);
    expect(plugins.listeners.size).toBe(0);
  });
});

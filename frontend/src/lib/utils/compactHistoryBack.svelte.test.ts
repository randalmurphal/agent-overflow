// The browser's Back under compact: one sentinel history entry while the
// app is away from root, popped by a Back and answered with the shell's
// own back ladder. happy-dom implements pushState/back/popstate, and its
// `back()` is asynchronous like a browser's, so pops are awaited.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';

const shell = vi.hoisted(() => ({ native: false }));
vi.mock('../native/platform', () => ({
  isNativeShell: () => shell.native,
  nativePlatform: () => 'web',
}));

import { installCompactHistoryBack, isCompactBackSentinel } from './compactHistoryBack.svelte';
import {
  getCompactScreen,
  setCompactLayoutForTest,
  showCompactList,
  showCompactThread,
} from '../stores/layoutMode.svelte';
import { registerAirspaceSurface, resetAirspaceForTest } from './paneAirspace.svelte';

let dispose: () => void = () => {};

function nextPop(): Promise<void> {
  return new Promise((resolve) => {
    window.addEventListener('popstate', () => resolve(), { once: true });
  });
}

/** Press the browser's Back and wait for the page to answer it. */
async function pressBack(): Promise<void> {
  const popped = nextPop();
  window.history.back();
  await popped;
  await tick();
}

/** A surface that closes on Escape and claims the press, like every overlay primitive. */
function openSurface(): { closed: () => boolean } {
  const el = document.createElement('div');
  document.body.appendChild(el);
  const release = registerAirspaceSurface(el);
  let closed = false;
  const onKey = (event: KeyboardEvent) => {
    if (event.key !== 'Escape' || closed) return;
    event.preventDefault();
    closed = true;
    release();
    el.remove();
    document.removeEventListener('keydown', onKey);
  };
  document.addEventListener('keydown', onKey);
  return { closed: () => closed };
}

beforeEach(async () => {
  shell.native = false;
  resetAirspaceForTest();
  setCompactLayoutForTest(true);
  // Start every case on the page's own entry with no sentinel current.
  if (isCompactBackSentinel(window.history.state)) window.history.replaceState(null, '');
  await tick();
});

afterEach(async () => {
  dispose();
  dispose = () => {};
  resetAirspaceForTest();
  setCompactLayoutForTest(false);
  showCompactList();
  document.body.innerHTML = '';
  await tick();
});

describe('installCompactHistoryBack', () => {
  it('pushes the sentinel when the thread screen opens, and drops it when the app returns to the list itself', async () => {
    dispose = installCompactHistoryBack();
    await tick();
    expect(isCompactBackSentinel(window.history.state)).toBe(false);

    showCompactThread();
    await tick();
    expect(isCompactBackSentinel(window.history.state)).toBe(true);

    // The header's back button: the sentinel goes with the screen, so the
    // next Back leaves the page rather than needing a second press.
    const popped = nextPop();
    showCompactList();
    await tick();
    await popped;
    await tick();
    expect(isCompactBackSentinel(window.history.state)).toBe(false);
    expect(getCompactScreen()).toBe('list');
  });

  it('answers a Back with the ladder: a surface closes and the sentinel comes back, then the thread goes to the list', async () => {
    dispose = installCompactHistoryBack();
    showCompactThread();
    await tick();
    const surface = openSurface();
    await tick();
    expect(isCompactBackSentinel(window.history.state)).toBe(true);

    await pressBack();
    expect(surface.closed()).toBe(true);
    expect(getCompactScreen()).toBe('thread');
    // Still away from root, so the sentinel was pushed again.
    expect(isCompactBackSentinel(window.history.state)).toBe(true);

    await pressBack();
    expect(getCompactScreen()).toBe('list');
    // At root now: nothing pushed, the next Back is the browser's.
    expect(isCompactBackSentinel(window.history.state)).toBe(false);
  });

  it('keeps a surface opened from the list screen behind one sentinel too', async () => {
    dispose = installCompactHistoryBack();
    await tick();
    const surface = openSurface();
    await tick();
    expect(isCompactBackSentinel(window.history.state)).toBe(true);

    await pressBack();
    expect(surface.closed()).toBe(true);
    expect(getCompactScreen()).toBe('list');
    expect(isCompactBackSentinel(window.history.state)).toBe(false);
  });

  it('pushes nothing on a pop at root', async () => {
    dispose = installCompactHistoryBack();
    await tick();
    const pushState = vi.spyOn(window.history, 'pushState');
    window.dispatchEvent(new PopStateEvent('popstate', { state: null }));
    await tick();
    expect(pushState).not.toHaveBeenCalled();
    expect(getCompactScreen()).toBe('list');
    pushState.mockRestore();
  });

  it('is inert off compact: nothing is pushed and a pop moves nothing', async () => {
    setCompactLayoutForTest(false);
    dispose = installCompactHistoryBack();
    showCompactThread();
    await tick();
    expect(isCompactBackSentinel(window.history.state)).toBe(false);

    window.dispatchEvent(new PopStateEvent('popstate', { state: null }));
    await tick();
    expect(getCompactScreen()).toBe('thread');
  });

  it('is inert in the native shell, which has the hardware key', async () => {
    shell.native = true;
    dispose = installCompactHistoryBack();
    showCompactThread();
    await tick();
    expect(isCompactBackSentinel(window.history.state)).toBe(false);
  });

  it('stops after dispose', async () => {
    dispose = installCompactHistoryBack();
    await tick();
    dispose();
    dispose = () => {};
    showCompactThread();
    await tick();
    expect(isCompactBackSentinel(window.history.state)).toBe(false);
  });
});

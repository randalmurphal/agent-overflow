import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import TerminalSurface from './TerminalSurface.svelte';
import {
  getThreadTerminalState,
  terminalStateKeyForPane,
  resetThreadTerminalStatesForTest,
} from './terminalStore.svelte';
import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';
import {
  terminalFocusCalls,
  resetTerminalFocusCalls,
} from '../../../test/mocks/terminalBodyFocusRecorder';

// Outcome tests for the terminal focus-follows-active-tab fix. Intent-only
// assertions ("requestTerminalFocus was called") cannot prove the bug is fixed:
// TerminalBody is keyed on activeTerminalID, so every create/switch/close
// REMOUNTS it and the focus must re-land on the *new* instance. These tests
// stub TerminalBody with a focus() recorder, drive the real surface, and assert
// which tab's body actually received focus after the remount.

function terminalSummary(terminalID: string, threadID: string) {
  return {
    terminalID,
    threadID,
    shell: '/bin/bash',
    cwd: '/tmp',
    rows: 24,
    cols: 80,
    pid: 1,
    startedAt: 0,
    running: true,
    exitCode: 0,
    exitReason: '',
  };
}

vi.mock('../../stores/bindings', () => ({
  OpenTerminal: vi.fn(async (threadID: string) => ({
    terminalID: 't1',
    threadID,
    summary: terminalSummary('t1', threadID),
  })),
  CloseTerminal: vi.fn(async () => {}),
  ListTerminals: vi.fn(async () => []),
  RefreshTerminal: vi.fn(async () => {}),
  TerminalOpenOptions: class TerminalOpenOptions {
    cwd?: string;
    constructor(src: Partial<{ cwd: string }> = {}) {
      Object.assign(this, src);
    }
  },
}));

vi.mock('../../stores/toast.svelte', () => ({ addToast: vi.fn() }));

// Swap the real xterm-backed body for a stub whose focus() records the tab it
// was called for. The surface binds it and calls bodyEl.focus() from its rAF
// effect, so the stub's `export function focus()` is the exact contract we need.
vi.mock('./TerminalBody.svelte', async () => ({
  default: (await import('../../../test/mocks/StubTerminalBody.svelte')).default,
}));

function makeSurface(
  overrides: Partial<ThreadTerminalSurfaceContext> = {},
): ThreadTerminalSurfaceContext {
  return {
    paneId: 'pane-1',
    threadId: 'thread-A',
    workspacePath: '/tmp',
    setVisible: () => {},
    acquireResizeLease: () => null,
    consumeFocusRequest: () => false,
    ...overrides,
  };
}

function seededHandle() {
  return getThreadTerminalState(terminalStateKeyForPane('thread-A', 'pane-1'));
}

// Let the OpenTerminal microtask, the surface's reactive flush, and the rAF
// focus effect all settle before asserting.
async function settle() {
  for (let i = 0; i < 4; i += 1) await Promise.resolve();
  await tick();
}

beforeEach(() => {
  resetThreadTerminalStatesForTest();
  resetTerminalFocusCalls();
  // Make the surface's requestAnimationFrame(() => el.focus()) synchronous so
  // the focus lands within the test flush; happy-dom does not auto-run rAF.
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
    cb(0);
    return 0;
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  cleanup();
});

describe('TerminalSurface focus follows the active tab', () => {
  it('focuses the newly-active body when a tab is clicked (the remount path)', async () => {
    const handle = seededHandle();
    handle.addTab(terminalSummary('t1', 'thread-A'));
    handle.addTab(terminalSummary('t2', 'thread-A'));
    handle.setActive('t1');

    const { getByTestId, queryByTestId } = render(TerminalSurface, {
      surface: makeSurface() as never,
      manual: true,
    });
    await tick();
    // Mounting with no focus intent must NOT steal focus into the body.
    expect(terminalFocusCalls).toEqual([]);
    expect(queryByTestId('stub-terminal-body-t1')).not.toBeNull();

    await fireEvent.click(getByTestId('terminal-tab-t2'));
    await tick();

    // The body remounted for t2, and focus landed on it — not the destroyed t1.
    expect(queryByTestId('stub-terminal-body-t2')).not.toBeNull();
    expect(queryByTestId('stub-terminal-body-t1')).toBeNull();
    expect(terminalFocusCalls).toEqual(['t2']);
  });

  it('focuses the freshly-opened body when the ＋ button is clicked', async () => {
    const { getByTestId } = render(TerminalSurface, {
      surface: makeSurface() as never,
      manual: true,
    });
    await tick();
    expect(terminalFocusCalls).toEqual([]);

    await fireEvent.click(getByTestId('terminal-open'));

    // Opening is async (OpenTerminal resolves on a microtask); wait for the
    // focus to land rather than hard-coding how many microtasks that takes.
    await vi.waitFor(() => expect(terminalFocusCalls).toEqual(['t1']));
  });

  it('focuses the promoted sibling when the active tab is closed', async () => {
    const handle = seededHandle();
    handle.addTab(terminalSummary('t1', 'thread-A'));
    handle.addTab(terminalSummary('t2', 'thread-A')); // addTab activates t2

    const { getByTestId } = render(TerminalSurface, {
      surface: makeSurface() as never,
      manual: true,
    });
    await tick();
    expect(terminalFocusCalls).toEqual([]);

    await fireEvent.click(getByTestId('terminal-tab-close-t2'));

    // Closing is async (CloseTerminal); removeTab then promotes t1 to active and
    // the cursor follows into it.
    await vi.waitFor(() => expect(terminalFocusCalls).toEqual(['t1']));
  });

  it('does not grab focus when the last tab is closed (nothing to focus)', async () => {
    const handle = seededHandle();
    handle.addTab(terminalSummary('only', 'thread-A'));

    const { getByTestId } = render(TerminalSurface, {
      surface: makeSurface() as never,
      manual: true,
    });
    await tick();
    expect(terminalFocusCalls).toEqual([]);

    await fireEvent.click(getByTestId('terminal-tab-close-only'));
    await settle();

    expect(terminalFocusCalls).toEqual([]);
  });

  it('does not steal focus on a bare active-tab reassignment (no routed focus intent)', async () => {
    // A bare handle.removeTab() — the store half of a shell exit — promotes a
    // sibling and remounts TerminalBody, but carries NO focus intent of its own.
    // The surface must stay inert: focus only ever lands when an intent is
    // routed in (pendingFocus from a mouse handler, or the pane's
    // consumeFocusRequest). This guards against the tempting "blanket
    // activeTerminalID effect" simplification, which would grab focus on ANY
    // active-tab change and passes every other test in this file but fails here.
    //
    // Whether a *focused* active-tab self-exit SHOULD follow focus is decided in
    // events.ts applyTerminalExit (it inspects pane focus context and calls
    // pane.requestTerminalFocus), covered in events.terminalExit.test.ts — not by
    // the surface reacting to the store mutation alone.
    const handle = seededHandle();
    handle.addTab(terminalSummary('t1', 'thread-A'));
    handle.addTab(terminalSummary('t2', 'thread-A')); // addTab activates t2

    render(TerminalSurface, { surface: makeSurface() as never, manual: true });
    await tick();
    expect(terminalFocusCalls).toEqual([]);

    // Remove the active terminal directly, NOT via the surface's closeTerminal
    // (the user-action path that DOES re-latch focus) and with no pane intent.
    handle.removeTab('t2'); // promotes t1 to active → remount, no focus intent
    await settle();

    expect(terminalFocusCalls).toEqual([]);
  });

  it('focuses the body on mount when the pane focus intent is set (the consume path)', async () => {
    // The cold-open / nav-into / keyboard-command channel: the pane carries a
    // read-and-clear focus intent the surface drains via consumeFocusRequest.
    // The command tests assert the intent is *set*; this asserts the surface
    // half end-to-end — a set intent actually lands focus on the body once it
    // binds, across the same rAF effect the user-action paths use.
    const handle = seededHandle();
    handle.addTab(terminalSummary('t1', 'thread-A'));
    let intent = true;
    const surface = makeSurface({
      consumeFocusRequest: () => {
        const value = intent;
        intent = false; // read-and-clear, like ThreadPane.consumeTerminalFocusRequest
        return value;
      },
    });

    render(TerminalSurface, { surface: surface as never, manual: true });
    await tick();

    expect(terminalFocusCalls).toEqual(['t1']);
  });
});

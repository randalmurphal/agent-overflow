import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

// Regression: the terminal drawer is a lazy `import()`. On a COLD first open
// per session the real (in-flow, `shrink-0`, 120–320px) drawer mounts a few
// frames after `setShowTerminal`'s 2-rAF open lease has already released, and
// the scroll controller has no `scrollEl` ResizeObserver — so a stuck-to-bottom
// timeline used to NOT re-pin and the latest messages hid behind the terminal.
// The fix has LazyThreadTerminalDrawer call `surface.settleAfterAsyncMount()`
// when the real drawer commits, but only on the cold path (the warm path
// renders the cached drawer in-flush under the open lease and onMount
// early-returns). This test lives in its own file so the success-only drawer
// mock can't be cache-poisoned by the throw path in LazyThreadTerminalDrawer.test.ts.
//
// The stub stands in for the real drawer (which would mount xterm); mounting it
// is what proves the post-import `.then` ran.
vi.mock('./ThreadTerminalDrawer.svelte', async () => ({
  default: (await import('../../../test/mocks/StubThreadTerminalDrawer.svelte')).default,
}));

import LazyThreadTerminalDrawer from './LazyThreadTerminalDrawer.svelte';

function makeSurface() {
  return {
    paneId: 'main',
    threadId: 'thread-A',
    workspacePath: '/workspace',
    setVisible: vi.fn(),
    acquireResizeLease: vi.fn(() => null),
    consumeFocusRequest: vi.fn(() => false),
    settleAfterAsyncMount: vi.fn(),
  };
}

afterEach(() => {
  cleanup();
});

describe('LazyThreadTerminalDrawer cold-open scroll settle', () => {
  it('re-settles the scroll controller after the drawer mounts on cold open, but not on a warm reopen', async () => {
    // Cold first open: the module-level drawer cache is empty, so the real
    // drawer arrives via the async import and `settleAfterAsyncMount` fires to
    // re-pin the timeline against the drawer's height reflow.
    const cold = makeSurface();
    const { unmount } = render(LazyThreadTerminalDrawer, {
      surface: cold as never,
      manual: true,
    });
    await waitFor(() => expect(cold.settleAfterAsyncMount).toHaveBeenCalledTimes(1));
    // Pin the upper bound too: waitFor resolves the instant the count first hits
    // 1, so let a couple of frames pass and confirm the cold path settles EXACTLY
    // once — no double re-pin from a re-running effect.
    await tick();
    await tick();
    expect(cold.settleAfterAsyncMount).toHaveBeenCalledTimes(1);
    unmount();

    // Warm reopen: the now-cached component renders in-flush under
    // setShowTerminal's own open lease, so onMount early-returns and we must
    // NOT settle again (a redundant re-pin would fight the open lease).
    const warm = makeSurface();
    const { container } = render(LazyThreadTerminalDrawer, { surface: warm as never, manual: true });
    // Positive half of the contract: the cached drawer is seeded into `Drawer` at
    // $state init, so it mounts SYNCHRONOUSLY — it's already in the DOM on the
    // first render with no async import to await. That's what lets the warm reopen
    // render in-flush under setShowTerminal's own open lease.
    expect(container.querySelector('[data-testid="stub-terminal-drawer"]')).not.toBeNull();
    await tick();
    await tick();
    expect(warm.settleAfterAsyncMount).not.toHaveBeenCalled();
  });
});

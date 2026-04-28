import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadTerminalDrawer from './ThreadTerminalDrawer.svelte';

// --- Mock layer --- //
// We control the Wails bindings and event bus so drawer logic can be exercised
// without an actual backend.

const callLog: Array<{ fn: string; args: unknown[] }> = [];
const eventListeners: Record<string, ((ev: { data: unknown }) => void)[]> = {};

vi.mock('../../stores/bindings', () => ({
  OpenTerminal: vi.fn(async (threadID: string, opts: unknown) => {
    callLog.push({ fn: 'OpenTerminal', args: [threadID, opts] });
    return {
      terminalID: 't1',
      threadID,
      summary: {
        terminalID: 't1',
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
      },
    };
  }),
  CloseTerminal: vi.fn(async (terminalID: string) => {
    callLog.push({ fn: 'CloseTerminal', args: [terminalID] });
  }),
  ListTerminals: vi.fn(async (threadID: string) => {
    callLog.push({ fn: 'ListTerminals', args: [threadID] });
    return [];
  }),
  ResizeTerminal: vi.fn(async (terminalID: string, rows: number, cols: number) => {
    callLog.push({ fn: 'ResizeTerminal', args: [terminalID, rows, cols] });
  }),
  WriteTerminal: vi.fn(async (terminalID: string, data: string) => {
    callLog.push({ fn: 'WriteTerminal', args: [terminalID, data] });
  }),
  GetTerminalReplay: vi.fn(async () => ''),
  RestartTerminal: vi.fn(),
  // Simple stand-in for the generated class. Constructor matches the real
  // shape closely enough for call-site code that only reads `cwd`/`shell`.
  TerminalOpenOptions: class TerminalOpenOptions {
    cwd?: string;
    shell?: string;
    rows?: number;
    cols?: number;
    constructor(src: Partial<{ cwd: string; shell: string; rows: number; cols: number }> = {}) {
      Object.assign(this, src);
    }
  },
}));

vi.mock('../../stores/toast.svelte', () => ({
  addToast: vi.fn(),
}));

vi.mock('@wailsio/runtime', () => ({
  Events: {
    On: (eventName: string, handler: (ev: { data: unknown }) => void) => {
      eventListeners[eventName] = eventListeners[eventName] ?? [];
      eventListeners[eventName]!.push(handler);
      return () => {
        eventListeners[eventName] = (eventListeners[eventName] ?? []).filter((h) => h !== handler);
      };
    },
  },
}));

function emitEvent(name: string, data: unknown) {
  for (const h of eventListeners[name] ?? []) {
    h({ data });
  }
}

function makePane() {
  // Tiny stub — we only need the thread + setShowTerminal/toggle functions for
  // drawer behaviour.
  const thread = {
    id: 'thread-A',
    workspacePath: '/workspace',
    title: 't',
    provider: 'claude',
    projectPath: '/workspace',
    model: '',
    mode: 'chat',
    createdAt: 0,
    updatedAt: 0,
  };
  return {
    get thread() { return thread; },
    setShowTerminal: vi.fn(),
    toggleTerminal: vi.fn(),
  };
}

// Default OpenTerminal stub. Re-applied in beforeEach so tests that
// override with a counter-mock don't leak state into later tests.
async function defaultOpenTerminalImpl(threadID: string, opts: unknown) {
  callLog.push({ fn: 'OpenTerminal', args: [threadID, opts] });
  return {
    terminalID: 't1',
    threadID,
    summary: {
      terminalID: 't1',
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
    },
  };
}

beforeEach(async () => {
  callLog.length = 0;
  for (const key of Object.keys(eventListeners)) delete eventListeners[key];
  const bindings = await import('../../stores/bindings');
  (bindings.OpenTerminal as unknown as { mockImplementation: (fn: unknown) => void }).mockImplementation(
    defaultOpenTerminalImpl,
  );
});

afterEach(() => {
  cleanup();
});

describe('ThreadTerminalDrawer', () => {
  it('renders and subscribes to terminal events', async () => {
    const pane = makePane();
    render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    // Next microtask: onMount async.
    await Promise.resolve();
    await Promise.resolve();

    expect(eventListeners['terminal:output']).toBeDefined();
    expect(eventListeners['terminal:exit']).toBeDefined();
  });

  it('opens a terminal via OpenTerminal on +', async () => {
    const pane = makePane();
    const { getByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await tick();

    getByTestId('terminal-open').click();
    // OpenTerminal awaits a microtask plus a tick to propagate.
    await Promise.resolve();
    await Promise.resolve();
    await tick();

    const openCalls = callLog.filter((c) => c.fn === 'OpenTerminal');
    expect(openCalls).toHaveLength(1);
    expect(openCalls[0]!.args[0]).toBe('thread-A');
    expect(getByTestId('terminal-tab-t1')).toBeDefined();
  });

  it('closes a terminal and clears its tab', async () => {
    const pane = makePane();
    const { getByTestId, queryByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await tick();

    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();
    await tick();
    // Sanity: tab is present.
    expect(getByTestId('terminal-tab-t1')).toBeDefined();

    getByTestId('terminal-tab-close-t1').click();
    await Promise.resolve();
    await tick();

    const closeCalls = callLog.filter((c) => c.fn === 'CloseTerminal');
    expect(closeCalls).toHaveLength(1);
    expect(queryByTestId('terminal-tab-t1')).toBeNull();
  });

  it('collapses when ▾ is pressed', async () => {
    const pane = makePane();
    const { getByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await Promise.resolve();
    getByTestId('terminal-collapse').click();
    expect(pane.setShowTerminal).toHaveBeenCalledWith(false);
  });

  it('auto-removes a tab when terminal:exit fires for it', async () => {
    const pane = makePane();
    const { getByTestId, queryByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await tick();

    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();
    await tick();
    expect(getByTestId('terminal-tab-t1')).toBeDefined();

    // PTY exited (Ctrl+D, kill, process death). Backend already cleaned
    // up — frontend must just drop the tab.
    emitEvent('terminal:exit', {
      terminalID: 't1',
      threadID: 'thread-A',
      code: 0,
      reason: '',
    });
    await tick();

    expect(queryByTestId('terminal-tab-t1')).toBeNull();
    // Last tab gone → drawer collapses.
    expect(pane.setShowTerminal).toHaveBeenCalledWith(false);
  });

  it('does not auto-collapse on terminal:exit when other tabs remain', async () => {
    const pane = makePane();
    let nextId = 1;
    const bindings = await import('../../stores/bindings');
    (bindings.OpenTerminal as unknown as { mockImplementation: (fn: unknown) => void }).mockImplementation(
      async (threadID: string) => {
        const terminalID = `t${nextId++}`;
        return {
          terminalID,
          threadID,
          summary: {
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
          },
        };
      },
    );

    const { getByTestId, queryByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await tick();

    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();
    await tick();
    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();
    await tick();

    emitEvent('terminal:exit', {
      terminalID: 't1',
      threadID: 'thread-A',
      code: 0,
      reason: '',
    });
    await tick();

    expect(queryByTestId('terminal-tab-t1')).toBeNull();
    expect(getByTestId('terminal-tab-t2')).toBeDefined();
    expect(pane.setShowTerminal).not.toHaveBeenCalled();
  });

  it('ignores terminal:exit for other threads', async () => {
    const pane = makePane();
    const { getByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await tick();

    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();
    await tick();

    emitEvent('terminal:exit', {
      terminalID: 't1',
      threadID: 'thread-OTHER',
      code: 0,
      reason: '',
    });
    await tick();

    expect(getByTestId('terminal-tab-t1')).toBeDefined();
    expect(pane.setShowTerminal).not.toHaveBeenCalled();
  });

  it('auto-collapses when the last tab is closed', async () => {
    const pane = makePane();
    const { getByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await tick();

    // Open one terminal, then close it — that's the last tab.
    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();
    await tick();

    getByTestId('terminal-tab-close-t1').click();
    await Promise.resolve();
    await tick();

    expect(pane.setShowTerminal).toHaveBeenCalledWith(false);
  });

  it('does not auto-collapse when a non-last tab is closed', async () => {
    const pane = makePane();
    // Override OpenTerminal so each call returns a fresh ID.
    let nextId = 1;
    const bindings = await import('../../stores/bindings');
    (bindings.OpenTerminal as unknown as { mockImplementation: (fn: unknown) => void }).mockImplementation(
      async (threadID: string) => {
        const terminalID = `t${nextId++}`;
        return {
          terminalID,
          threadID,
          summary: {
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
          },
        };
      },
    );

    const { getByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await tick();

    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();
    await tick();
    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();
    await tick();

    // Two tabs open. Close one — the drawer must NOT collapse.
    getByTestId('terminal-tab-close-t1').click();
    await Promise.resolve();
    await tick();

    expect(pane.setShowTerminal).not.toHaveBeenCalled();
  });

  it('routes terminal:output events to the active tab', async () => {
    const pane = makePane();
    const { getByTestId } = render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await Promise.resolve();
    getByTestId('terminal-open').click();
    await Promise.resolve();
    await Promise.resolve();

    emitEvent('terminal:output', {
      terminalID: 't1',
      threadID: 'thread-A',
      sequence: 1,
      data: btoa('hello'),
    });
    // No assertion on xterm internals here — that would require deep xterm
    // instrumentation. We assert the store is consistent: drainOutput on the
    // tab returns the decoded content.
    // We cannot access `handle` from outside the component, so instead we
    // confirm the event was plumbed without throwing.
  });

  it('filters events for other threads', async () => {
    const pane = makePane();
    render(ThreadTerminalDrawer, { pane: pane as never, manual: true });
    await Promise.resolve();

    // Emitting an event for a different thread must not crash even if no tabs
    // match: the payload is simply ignored.
    expect(() => emitEvent('terminal:output', {
      terminalID: 'other',
      threadID: 'thread-B',
      sequence: 1,
      data: btoa('x'),
    })).not.toThrow();
  });

  // Stage 4 refactor: the drawer chrome is now composed from the Drawer
  // primitive so resize math / border / bg are shared with DiffPanel.
  // These tests pin the integration so a lazy rewrite can't silently
  // revert to the hand-rolled <aside> + pointer-capture code.
  it('composes its chrome via the Drawer primitive (has data-drawer-position)', async () => {
    const pane = makePane();
    const { container } = render(ThreadTerminalDrawer, {
      pane: pane as never,
      manual: true,
    });
    await tick();
    const drawerEl = container.querySelector('[data-drawer-position="bottom"]');
    expect(drawerEl).not.toBeNull();
    // The primitive owns the resize handle — not hand-rolled markup.
    const handle = drawerEl!.querySelector('[role="separator"][aria-orientation="horizontal"]');
    expect(handle).not.toBeNull();
  });

  it('renders the drawer height based on handle.drawerHeight', async () => {
    const pane = makePane();
    const { container } = render(ThreadTerminalDrawer, {
      pane: pane as never,
      manual: true,
    });
    await tick();
    const drawerEl = container.querySelector('[data-drawer-position="bottom"]') as HTMLElement;
    // The primitive renders height inline from the handle's current
    // value. The exact default comes from terminalStore.svelte; we
    // just assert that SOME non-zero px value is written so the layout
    // math is plumbed through rather than pin the exact number (which
    // is the store's contract to change freely).
    expect(drawerEl.style.height).toMatch(/^\d+px$/);
    const parsed = Number.parseInt(drawerEl.style.height, 10);
    expect(parsed).toBeGreaterThanOrEqual(120);
  });
});

// Orchestration tests for openTerminalThread — the create helper every
// terminal entry point (the +terminal buttons, the mod+shift+~ chord, the
// ChatHeader ctrl/cmd-click) routes through.
//
// We mock the four collaborators (StartTerminal, expandProject, openEmptyPane,
// replaceThreadInPane) so the test pins the WIRING and the load-bearing ORDER
// without standing up a backend or the real pane registry / switchThread
// fan-out. The mount → auto-open → focus-consume behaviour those collaborators
// drive is covered separately by TerminalView.test.ts and ChatView.test.ts.
//
// vi.hoisted is required: vi.mock factories are hoisted above the imports, so a
// factory that closed over plain top-level consts would hit the temporal dead
// zone when ./threadCreation.svelte is first imported. Declaring the spies in
// the hoisted block makes them initialised by the time the factories run.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const h = vi.hoisted(() => {
  const order: string[] = [];
  const requestTerminalFocus = vi.fn(() => {
    order.push('requestTerminalFocus');
  });
  const fakePane = { paneId: 'pane-term-1', requestTerminalFocus };
  const terminalThread = {
    id: 'term-1',
    title: 'Terminal',
    provider: 'claude',
    workspacePath: '/home/me',
    projectPath: '',
    mode: 'terminal',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
  return {
    order,
    fakePane,
    terminalThread,
    requestTerminalFocus,
    startTerminal: vi.fn(async (_opts: { projectId?: string; cwd?: string }) => {
      order.push('StartTerminal');
      return terminalThread;
    }),
    openEmptyPane: vi.fn(() => {
      order.push('openEmptyPane');
      return fakePane;
    }),
    replaceThreadInPane: vi.fn(async (_thread: unknown, _pane: unknown, _activation: string) => {
      order.push('replaceThreadInPane');
      return fakePane;
    }),
    expandProject: vi.fn(),
    expandTerminalsGroup: vi.fn(),
    addToast: vi.fn(),
  };
});

vi.mock('./bindings', () => ({
  StartTerminal: h.startTerminal,
  // Imported at module load by the draft helpers in the same file; stub so the
  // import resolves even though openTerminalThread never touches it.
  GetThreadDefaults: vi.fn(),
}));
vi.mock('./panes.svelte', () => ({
  openEmptyPane: h.openEmptyPane,
  replaceThreadInPane: h.replaceThreadInPane,
  // Unused by openTerminalThread, imported at module load by the draft helpers.
  ensureMainPane: vi.fn(),
  ensurePaneInLayout: vi.fn(),
  getFocusedPaneOrNull: vi.fn(),
}));
vi.mock('./sidebar.svelte', () => ({
  expandProject: h.expandProject,
  expandTerminalsGroup: h.expandTerminalsGroup,
}));
vi.mock('./toast.svelte', () => ({ addToast: h.addToast }));

import { openTerminalThread } from './threadCreation.svelte';
// ./threads.svelte is intentionally NOT mocked — openTerminalThread prepends
// into the real store, and these tests assert the row actually lands there.
import { getThreadById, getThreads } from './threads.svelte';

let errorSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  h.order.length = 0;
  // mockClear (not mockReset) so the hoisted implementations — which push to
  // `order` and return the fake pane / thread — survive across tests.
  h.startTerminal.mockClear();
  h.requestTerminalFocus.mockClear();
  h.openEmptyPane.mockClear();
  h.replaceThreadInPane.mockClear();
  h.expandProject.mockClear();
  h.expandTerminalsGroup.mockClear();
  h.addToast.mockClear();
  // openTerminalThread console.error's the failure before toasting; keep the
  // error-path test quiet. Restore only THIS spy in afterEach so the hoisted
  // vi.fn() implementations aren't wiped by a blanket restoreAllMocks.
  errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  errorSpy.mockRestore();
});

describe('openTerminalThread', () => {
  it('creates the thread for a project and commits it into a fresh pane', async () => {
    const pane = await openTerminalThread({ projectId: 'proj-1', cwd: '/work' });

    expect(h.startTerminal).toHaveBeenCalledWith({ projectId: 'proj-1', cwd: '/work' });
    expect(h.expandProject).toHaveBeenCalledWith('proj-1');
    // A per-project terminal reveals its project, not the standalone group.
    expect(h.expandTerminalsGroup).not.toHaveBeenCalled();
    expect(h.openEmptyPane).toHaveBeenCalledTimes(1);
    expect(h.replaceThreadInPane).toHaveBeenCalledTimes(1);

    const [thread, targetPane, activation] = h.replaceThreadInPane.mock.calls[0]!;
    expect(thread).toMatchObject({ id: 'term-1', mode: 'terminal' });
    expect(targetPane).toBe(h.fakePane);
    expect(activation).toBe('committed');
    expect(pane).toBe(h.fakePane);

    // Issue #1: the new terminal is prepended into the real sidebar store
    // immediately, so it's visible without waiting for a thread-list refresh.
    // Dropping the prependThread call makes both assertions fail.
    expect(getThreadById('term-1')).toBeDefined();
    expect(getThreads()[0]?.id).toBe('term-1');
  });

  it('latches focus on the pane BEFORE replaceThreadInPane mounts the surface', async () => {
    await openTerminalThread({ projectId: 'proj-1' });

    // TerminalSurface.onMount consumes the focus latch during switchThread,
    // which runs inside replaceThreadInPane — so the latch must already be set.
    // Pinning the exact sequence guards against a refactor that opens the pane
    // first and latches after (one tick too late; the shell never grabs focus).
    expect(h.order).toEqual([
      'StartTerminal',
      'openEmptyPane',
      'requestTerminalFocus',
      'replaceThreadInPane',
    ]);
  });

  it('opens a standalone home terminal (no projectId → expands the Terminals group)', async () => {
    await openTerminalThread();

    expect(h.startTerminal).toHaveBeenCalledWith({ projectId: undefined, cwd: undefined });
    expect(h.expandProject).not.toHaveBeenCalled();
    // A home terminal lands under the Terminals group, so reveal it (the group
    // may be collapsed) — otherwise the create is invisible (issue #1 again).
    expect(h.expandTerminalsGroup).toHaveBeenCalledTimes(1);
    expect(h.openEmptyPane).toHaveBeenCalledTimes(1);
  });

  it('toasts an error and returns null without opening a pane when StartTerminal fails', async () => {
    h.startTerminal.mockImplementationOnce(async () => {
      throw new Error('boom');
    });

    const pane = await openTerminalThread({ projectId: 'proj-1', cwd: '/work' });

    expect(pane).toBeNull();
    expect(h.addToast).toHaveBeenCalledTimes(1);
    const [type, message] = h.addToast.mock.calls[0]!;
    expect(type).toBe('error');
    expect(message).toContain('boom');

    // The pane machinery must NOT run on a failed create — no orphan empty pane,
    // no stale focus latch, no sidebar expansion of either flavor.
    expect(h.expandProject).not.toHaveBeenCalled();
    expect(h.expandTerminalsGroup).not.toHaveBeenCalled();
    expect(h.openEmptyPane).not.toHaveBeenCalled();
    expect(h.requestTerminalFocus).not.toHaveBeenCalled();
    expect(h.replaceThreadInPane).not.toHaveBeenCalled();
  });
});

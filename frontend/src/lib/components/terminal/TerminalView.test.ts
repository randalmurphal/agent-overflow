import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import TerminalView from './TerminalView.svelte';
import { destroyPane, getFocusedPaneId } from '../../stores/panes.svelte';
import { getProject } from '../../stores/projects.svelte';
import { resetThreadTerminalStatesForTest } from './terminalStore.svelte';
import { setupEventListeners } from '../../stores/events';
import { resetWailsMocks } from '../../../test/mocks/wailsio-runtime';

// Same mock layer as ThreadTerminalDrawer.test.ts: control the Wails bindings
// here; the event bus is the global @wailsio/runtime mock (vitest alias →
// src/test/mocks/wailsio-runtime.ts). The one addition is panes.svelte —
// TerminalView is the only terminal component that imports a runtime symbol
// from it (destroyPane), so we stub just that.

const callLog: Array<{ fn: string; args: unknown[] }> = [];
let cleanupEvents: (() => void) | null = null;

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
  OpenTerminal: vi.fn(async (threadID: string, opts: unknown) => {
    callLog.push({ fn: 'OpenTerminal', args: [threadID, opts] });
    return { terminalID: 't1', threadID, summary: terminalSummary('t1', threadID) };
  }),
  CloseTerminal: vi.fn(async (terminalID: string) => {
    callLog.push({ fn: 'CloseTerminal', args: [terminalID] });
  }),
  ListTerminals: vi.fn(async (threadID: string) => {
    callLog.push({ fn: 'ListTerminals', args: [threadID] });
    return [];
  }),
  ResizeTerminal: vi.fn(async () => {}),
  RefreshTerminal: vi.fn(async () => {}),
  WriteTerminal: vi.fn(async () => {}),
  GetTerminalReplay: vi.fn(async () => ''),
  RestartTerminal: vi.fn(),
  TerminalOpenOptions: class TerminalOpenOptions {
    cwd?: string;
    constructor(src: Partial<{ cwd: string }> = {}) {
      Object.assign(this, src);
    }
  },
}));

vi.mock('../../stores/toast.svelte', () => ({ addToast: vi.fn() }));

vi.mock('../../stores/panes.svelte', () => ({
  addPaneThreadMountedObserver: vi.fn(() => () => {}),
  destroyPane: vi.fn(),
  // The header's shared PaneTitleHandle reads pane focus (for the outline) and
  // writes the row back after a rename.
  getFocusedPaneId: vi.fn(() => null),
  syncThread: vi.fn(),
}));

// The header shows the project name; a home terminal (no project) shows "~".
vi.mock('../../stores/projects.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/projects.svelte')>()),
  getProject: vi.fn(() => undefined),
}));

interface MakePaneOpts {
  paneId?: string;
  focus?: boolean;
  thread?: Partial<{
    id: string;
    title: string;
    workspacePath: string;
    mode: string;
    projectId: string;
  }> | null;
}

function makePane(opts: MakePaneOpts = {}) {
  const thread =
    opts.thread === null
      ? null
      : {
          id: 'thread-A',
          title: 'My Terminal',
          workspacePath: '/home/me',
          mode: 'terminal',
          ...(opts.thread ?? {}),
        };
  return {
    paneId: opts.paneId ?? 'pane-term',
    get threadId() {
      return thread?.id ?? null;
    },
    get thread() {
      return thread;
    },
    consumeTerminalFocusRequest: vi.fn(() => opts.focus ?? false),
  };
}

beforeEach(() => {
  callLog.length = 0;
  resetThreadTerminalStatesForTest();
  vi.mocked(destroyPane).mockClear();
  vi.mocked(getFocusedPaneId).mockReturnValue(null);
  vi.mocked(getProject).mockReturnValue(undefined as never);
  resetWailsMocks();
  cleanupEvents = setupEventListeners();
});

afterEach(() => {
  cleanupEvents?.();
  cleanupEvents = null;
  cleanup();
});

describe('TerminalView', () => {
  it('renders the title, the project label, and a pane-close button', async () => {
    const pane = makePane();
    const { getByTestId } = render(TerminalView, { pane: pane as never });
    await tick();

    // The title is the shared, renameable PaneTitleHandle.
    expect(getByTestId('terminal-pane-title')).toHaveTextContent('My Terminal');
    // A home terminal (no project) shows "~"; the full cwd is no longer body
    // text — it moves to the label's hover tooltip.
    const project = getByTestId('terminal-pane-project');
    expect(project).toHaveTextContent('~');
    expect(project).toHaveAttribute('title', '/home/me');
    expect(getByTestId('terminal-pane-close')).toBeInTheDocument();
  });

  it('shows the project name (not the path) for a per-project terminal', async () => {
    vi.mocked(getProject).mockReturnValue({
      project: { id: 'p1', name: 'agent-overflow', path: '/repo' },
    } as never);
    const pane = makePane({ thread: { projectId: 'p1', workspacePath: '/repo' } });
    const { getByTestId } = render(TerminalView, { pane: pane as never });
    await tick();
    expect(getByTestId('terminal-pane-project')).toHaveTextContent('agent-overflow');
  });

  it('makes the title a drag handle and reflects pane focus for the outline', async () => {
    vi.mocked(getFocusedPaneId).mockReturnValue('pane-term');
    const onPaneDragStart = vi.fn();
    const pane = makePane();
    const { getByTestId } = render(TerminalView, {
      pane: pane as never,
      onPaneDragStart,
    });
    await tick();
    const title = getByTestId('terminal-pane-title');
    // Focus-outline parity with a chat pane: data-focused + the accent ring.
    expect(title).toHaveAttribute('data-focused', 'true');
    expect(title.className).toContain('ring-accent/40');
    // Drag-handle parity: dragging the title reorders the pane.
    expect(title.getAttribute('draggable')).toBe('true');
    await fireEvent.dragStart(title);
    expect(onPaneDragStart).toHaveBeenCalledTimes(1);
  });

  it('omits the ▾ collapse affordance — a full pane has nothing to collapse into', async () => {
    const pane = makePane();
    const { queryByTestId } = render(TerminalView, { pane: pane as never });
    await tick();

    // The tab strip still renders (＋ to open a new terminal); only the
    // drawer-specific collapse button is suppressed in pane mode.
    expect(queryByTestId('terminal-open')).not.toBeNull();
    expect(queryByTestId('terminal-collapse')).toBeNull();
  });

  it('closes the pane via destroyPane when the close button is clicked', async () => {
    const pane = makePane({ paneId: 'pane-xyz' });
    const { getByTestId } = render(TerminalView, { pane: pane as never });
    await tick();

    getByTestId('terminal-pane-close').click();
    expect(vi.mocked(destroyPane)).toHaveBeenCalledWith('pane-xyz');
  });

  it('auto-opens a fresh terminal on mount (no manual flag — the plan correction)', async () => {
    // TerminalView must NOT pass `manual` to TerminalSurface: a freshly opened
    // terminal pane has to reconcile + spawn its first shell, unlike the
    // test-only `manual` drawer path. This is the guard against re-introducing
    // the plan's mistaken `<TerminalSurface manual/>`.
    const pane = makePane();
    const { findByTestId } = render(TerminalView, { pane: pane as never });
    // onMount: ListTerminals([]) then auto OpenTerminal → tab t1.
    await Promise.resolve();
    await Promise.resolve();
    await tick();

    const listCalls = callLog.filter((c) => c.fn === 'ListTerminals');
    const openCalls = callLog.filter((c) => c.fn === 'OpenTerminal');
    expect(listCalls).toHaveLength(1);
    expect(listCalls[0]!.args[0]).toBe('thread-A');
    expect(openCalls).toHaveLength(1);
    expect(openCalls[0]!.args[0]).toBe('thread-A');
    expect(await findByTestId('terminal-tab-t1')).toBeInTheDocument();
  });

  it('consults the pane focus-request intent on mount so the new shell can grab focus', async () => {
    const pane = makePane({ focus: true });
    render(TerminalView, { pane: pane as never });
    await Promise.resolve();

    expect(pane.consumeTerminalFocusRequest).toHaveBeenCalled();
  });
});

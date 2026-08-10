import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import {
  __setSmoothingClockForTest,
  createThreadPane,
  LIVE_TODO_AUTOHIDE_MS,
  type TimelineWindowAnchorOperation,
} from './thread.svelte';
import {
  MAX_ADAPTIVE_CHARS_PER_SEC,
  MAX_ADVANCE_PER_TICK_CHARS,
  type SmoothingClock,
} from '../markdown/smoothing/PerItemSmoother';
import { getSettings, resetSettingsForTest } from './settings.svelte';
import {
  resetForTest as resetWorktreeIntent,
  setAttachBranch,
  setThreadEnvMode,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import type { Project } from '../types/models';
import {
  getActiveTurn,
  getThreadStatus,
  isThreadLiveStateHydrating,
  resetForTest as resetThreadStatuses,
} from './threadStatuses.svelte';
import {
  getFlushedForThread,
  getQueueForThread,
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
} from './sendQueue.svelte';
import type { Item } from '../types/models';
import {
  resetBindingMocks,
  setBindingMock,
} from '../../test/mocks/bindings-app';
import {
  buildPane,
  installThreadSwitchMocks,
  makeItem,
  makeThread,
  stubScrollController,
} from '../../test/helpers/chat';
import {
  clearAllThreadSizePriorsForTest,
  peekThreadSizePriorsForTest,
  setThreadSizePriors,
} from '../utils/virtual/priors';
import {
  resetLayoutMetricsForTest,
  setPaneWidth,
} from './layoutMetrics.svelte';
import {
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from './paneLayout.svelte';
import {
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
import {
  getExistingThreadTerminalState,
  getThreadTerminalState,
  resetThreadTerminalStatesForTest,
} from '../components/terminal/terminalStore.svelte';
import {
  ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
  ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
  SLICE_AROUND_ITEM_BUDGET,
} from './threadPaneShared';
import { MAX_CACHED_SNAPSHOT_CHARS } from './threadItemCache';

function nextFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

// Drain the microtask queue. Used where a test has to let an in-flight
// switch reach a deliberately-hanging binding mock: the cold-open item
// leg consults the durable replica before it issues its RPC, so the
// call no longer lands on the switch's own synchronous tick.
function flushMicrotasks(): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, 0);
  });
}

// FakeClock for smoothing reveal tests. Mirrors the same shape as
// PerItemSmoother.test.ts so per-tick assertions are deterministic.
class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  now(): number {
    return this.current;
  }
  schedule(cb: () => void): number {
    const h = this.nextHandle++;
    this.pending.set(h, cb);
    return h;
  }
  cancel(h: number): void {
    this.pending.delete(h);
  }
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.values()];
    this.pending.clear();
    for (const cb of toFire) cb();
  }
  pendingCount(): number {
    return this.pending.size;
  }
}

// Helper: how much *new content* appeared at the end of `cur` that
// wasn't already at the end of `prev`. Computed by finding the longest
// suffix of `prev` that's also a prefix of `cur`, and returning the
// length of `cur` past that match. Used by smoothing tests to verify
// per-tick reveal granularity once the trim engages.
function smoothingNewTailChars(prev: string, cur: string): number {
  const max = Math.min(prev.length, cur.length);
  for (let overlap = max; overlap > 0; overlap--) {
    if (prev.endsWith(cur.slice(0, overlap))) {
      return cur.length - overlap;
    }
  }
  return cur.length;
}

function designFence(payload: unknown): string {
  return ['```aoflow-design', JSON.stringify(payload), '```'].join('\n');
}

function seedThreadPaneLayout(paneId: string): void {
  setPaneLayoutItemsForTest([{ id: paneId, paneId, kind: 'thread', widthPx: 1 }]);
}

describe('createThreadPane', () => {
  beforeEach(() => {
    Object.defineProperty(window, 'innerWidth', {
      configurable: true,
      writable: true,
      value: 1400,
    });
    resetBindingMocks();
    resetLayoutMetricsForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    resetThreadTerminalStatesForTest();
    resetThreadStatuses();
    resetSendQueueForTest();
    setBindingMock('SwitchThread', async (threadId: unknown) =>
      makeThread({ id: typeof threadId === 'string' ? threadId : 'thread-1' }),
    );
    // switchThread loads the initial slice via ListThreadSliceAround
    // (works for both bottom-snapshot and saved-anchor cases — empty
    // anchor id resolves to the tail at the backend). Tests override
    // the mock to supply specific items; the default is an empty thread
    // so unrelated tests don't have to plumb it.
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [] as Item[],
      oldestTurnIndex: -1,
      hasMore: false,
    }));
    // Legacy/broad tail loader. Active panes should use ListThreadSliceAround;
    // tests that intentionally touch this older RPC override the mock.
    setBindingMock('ListRecentThreadItems', async () => ({
      items: [] as Item[],
      oldestTurnIndex: -1,
      hasMore: false,
    }));
    setBindingMock('ListItemsBeforeCursor', async () => ({
      items: [] as Item[],
      oldestTurnIndex: -1,
      newestTurnIndex: -1,
      hasMore: false,
      hasMoreOlder: false,
      hasMoreNewer: false,
    }));
    setBindingMock('ListItemsAfterCursor', async () => ({
      items: [] as Item[],
      oldestTurnIndex: -1,
      newestTurnIndex: -1,
      hasMore: false,
      hasMoreOlder: false,
      hasMoreNewer: false,
    }));
    setBindingMock('ListPendingInteractiveRequests', async () => ({
      approvals: [],
      userInputs: [],
    }));
    setBindingMock('GetThreadLiveState', async (threadId: string) => ({
      threadId,
      activeTurn: null,
      queueItems: [...getQueueForThread(threadId)],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    }));
    setBindingMock('ListItems', async () => [] as Item[]);
    // switchThread calls ListRecentTurns as part of rehydration. Default
    // to an empty list so tests that don't care about turn rehydration
    // don't need to plumb the mock themselves.
    setBindingMock('ListRecentTurns', async () => []);
  });

  it('starts empty', () => {
    const pane = createThreadPane();

    expect(pane.thread).toBeNull();
    expect(pane.threadId).toBeNull();
    expect(pane.items).toEqual([]);
    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.contextWindow).toBeNull();
    expect(pane.generalError).toBeNull();
    expect(pane.isLocked).toBe(false);
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });

  it('drops stale placeholder worktree intent when "+ New" replaces an unsent draft', () => {
    // Repeated "+ New" without typing would otherwise leak worktree
    // entries keyed by the prior placeholder id — they're unreachable
    // (no Thread points at them) but stay in the store until reset.
    // Verify startDraftPlaceholder cleans them up before staging the
    // next placeholder.
    resetWorktreeIntent();
    try {
      const pane = createThreadPane();
      const projectA: Project = {
        id: 'p-1',
        path: '/tmp/p1',
        name: 'p1',
        sortPosition: 0,
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      };
      // Use a distinct project for the second placeholder so the
      // synthesised draft id differs even when both startDraftPlaceholder
      // calls land in the same millisecond — otherwise the cleanup and
      // a "no-op" cannot be distinguished by querying the same id back.
      const projectB: Project = {
        ...projectA,
        id: 'p-2',
        path: '/tmp/p2',
        name: 'p2',
      };

      pane.startDraftPlaceholder(projectA, 'chat');
      const firstPlaceholder = pane.thread;
      expect(firstPlaceholder).not.toBeNull();
      expect(firstPlaceholder!.id.startsWith('draft:')).toBe(true);

      setThreadEnvMode(firstPlaceholder!, 'new-worktree');
      setAttachBranch(firstPlaceholder!, 'feature/x');

      expect(worktreeIntentForThread(firstPlaceholder!).attachBranch).toBe(
        'feature/x',
      );

      pane.startDraftPlaceholder(projectB, 'chat');
      expect(pane.thread?.id).not.toBe(firstPlaceholder!.id);

      // The intent stores key by thread.id — query against the prior
      // placeholder thread to confirm the entries are gone.
      expect(worktreeIntentForThread(firstPlaceholder!).mode).toBe('local');
      expect(worktreeIntentForThread(firstPlaceholder!).attachBranch).toBe('');
    } finally {
      resetWorktreeIntent();
    }
  });

  it('closes placeholder terminals when "+ New" replaces an unsent draft', () => {
    const pane = createThreadPane();
    const projectA: Project = {
      id: 'p-1',
      path: '/tmp/p1',
      name: 'p1',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    const projectB: Project = {
      ...projectA,
      id: 'p-2',
      path: '/tmp/p2',
      name: 'p2',
    };

    pane.startDraftPlaceholder(projectA, 'chat');
    const firstPlaceholderId = pane.thread!.id;
    getThreadTerminalState(firstPlaceholderId).addTab({
      terminalID: 'term-1',
      threadID: firstPlaceholderId,
      shell: '/bin/sh',
      cwd: '/tmp/p1',
      rows: 24,
      cols: 80,
      pid: 123,
      startedAt: 1,
      running: true,
      exitCode: 0,
      exitReason: '',
    });
    pane.setShowTerminal(true);
    const close = setBindingMock('CloseThreadTerminals', async () => undefined);

    pane.startDraftPlaceholder(projectB, 'chat');

    expect(close.mock.calls[0]).toEqual([firstPlaceholderId]);
    expect(getExistingThreadTerminalState(firstPlaceholderId)).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.thread?.projectId).toBe('p-2');
  });

  it('closes placeholder terminals when the placeholder workspace cwd changes', () => {
    const pane = createThreadPane();
    const project: Project = {
      id: 'p-1',
      path: '/tmp/project',
      name: 'project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };

    pane.startDraftPlaceholder(project, 'chat');
    const placeholderId = pane.thread!.id;
    getThreadTerminalState(placeholderId).addTab({
      terminalID: 'term-1',
      threadID: placeholderId,
      shell: '/bin/sh',
      cwd: '/tmp/project',
      rows: 24,
      cols: 80,
      pid: 123,
      startedAt: 1,
      running: true,
      exitCode: 0,
      exitReason: '',
    });
    pane.setShowTerminal(true);
    const close = setBindingMock('CloseThreadTerminals', async () => undefined);

    pane.applyDraftPlaceholderWorkspace({
      workspacePath: '/tmp/project-worktree',
      worktreePath: '/tmp/project-worktree',
      branch: 'feature/x',
    });

    expect(close.mock.calls[0]).toEqual([placeholderId]);
    expect(getExistingThreadTerminalState(placeholderId)).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.thread?.workspacePath).toBe('/tmp/project-worktree');
  });

  it('rejects late terminal opens after a placeholder cwd change starts cleanup', () => {
    const pane = createThreadPane();
    const project: Project = {
      id: 'p-1',
      path: '/tmp/project',
      name: 'project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };

    pane.startDraftPlaceholder(project, 'chat');
    const placeholderId = pane.thread!.id;
    pane.setShowTerminal(true);
    const close = setBindingMock('CloseThreadTerminals', async () => undefined);

    expect(pane.canAdoptOpenedTerminal(placeholderId, '/tmp/project')).toBe(
      true,
    );
    pane.applyDraftPlaceholderWorkspace({
      workspacePath: '/tmp/project-worktree',
      worktreePath: '/tmp/project-worktree',
      branch: 'feature/x',
    });

    expect(close.mock.calls[0]).toEqual([placeholderId]);
    expect(pane.canAdoptOpenedTerminal(placeholderId, '/tmp/project')).toBe(
      false,
    );
  });

  it('migrates placeholder terminals when content materializes the thread', async () => {
    const pane = createThreadPane();
    const project: Project = {
      id: 'p-1',
      path: '/tmp/project',
      name: 'project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };

    pane.startDraftPlaceholder(project, 'chat', {
      provider: 'claude',
      model: 'm',
      workspacePath: '/tmp/project',
      branch: 'main',
    });
    const placeholderId = pane.thread!.id;
    getThreadTerminalState(placeholderId).addTab({
      terminalID: 'term-1',
      threadID: placeholderId,
      shell: '/bin/sh',
      cwd: '/tmp/project',
      rows: 24,
      cols: 80,
      pid: 123,
      startedAt: 1,
      running: true,
      exitCode: 0,
      exitReason: '',
    });
    pane.setShowTerminal(true);
    setBindingMock('CreateThread', async () =>
      makeThread({
        id: 'thread-real',
        projectId: 'p-1',
        projectPath: '/tmp/project',
        workspacePath: '/tmp/project',
        branch: 'main',
        isDraft: true,
      }),
    );
    const move = setBindingMock('MoveThreadTerminals', async () => [
      {
        terminalID: 'term-1',
        threadID: 'thread-real',
        shell: '/bin/sh',
        cwd: '/tmp/project',
        rows: 24,
        cols: 80,
        pid: 123,
        startedAt: 1,
        running: true,
        exitCode: 0,
        exitReason: '',
      },
    ]);

    const threadId = await pane.ensureMaterializedThread();

    expect(threadId).toBe('thread-real');
    expect(move.mock.calls[0]).toEqual([placeholderId, 'thread-real']);
    expect(getExistingThreadTerminalState(placeholderId)).toBeNull();
    const migrated = getExistingThreadTerminalState('thread-real');
    expect(migrated?.tabs).toHaveLength(1);
    expect(migrated?.tabs[0]?.summary.threadID).toBe('thread-real');
    expect(pane.threadId).toBe('thread-real');
    expect(pane.showTerminal).toBe(true);
  });

  it('keeps selected workspace fields when applying placeholder model defaults', () => {
    const pane = createThreadPane();
    const project: Project = {
      id: 'p-1',
      path: '/tmp/project',
      name: 'project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };

    pane.startDraftPlaceholder(project, 'chat');
    pane.applyDraftPlaceholderWorkspace({
      workspacePath: '/tmp/project-worktree',
      worktreePath: '/tmp/project-worktree',
      branch: 'feature/x',
    });
    pane.applyDraftPlaceholderDefaults({
      provider: 'codex',
      model: 'gpt-5.4',
      reasoningEffort: 'high',
      fastMode: true,
      contextWindow: 200000,
      runtimeMode: 'full-access',
      workspacePath: '/tmp/other',
      branch: 'main',
    });

    expect(pane.thread?.provider).toBe('codex');
    expect(pane.thread?.model).toBe('gpt-5.4');
    expect(pane.thread?.workspacePath).toBe('/tmp/project-worktree');
    expect(pane.thread?.worktreePath).toBe('/tmp/project-worktree');
    expect(pane.thread?.branch).toBe('feature/x');
  });

  it('migrates worktree intent when an empty materialized draft returns to a placeholder', async () => {
    resetWorktreeIntent();
    try {
      const pane = await buildPane(
        makeThread({
          id: 'materialized-draft',
          projectId: 'p-1',
          projectPath: '/tmp/project',
          workspacePath: '/tmp/project',
          mode: 'chat',
          isDraft: true,
        }),
      );

      setThreadEnvMode(pane.thread!, 'new-worktree');
      setAttachBranch(pane.thread!, 'feature/x');
      expect(worktreeIntentForThread(pane.thread!).attachBranch).toBe(
        'feature/x',
      );

      const oldThread = pane.thread!;
      expect(pane.dematerializeEmptyDraftThread()).toBe(true);
      expect(pane.thread?.id).not.toBe(oldThread.id);
      expect(pane.thread?.id.startsWith('draft:')).toBe(true);
      expect(worktreeIntentForThread(oldThread).mode).toBe('local');
      expect(worktreeIntentForThread(pane.thread!).attachBranch).toBe(
        'feature/x',
      );
    } finally {
      resetWorktreeIntent();
    }
  });

  // `isLocked` tracks whether the user has committed the
  // provider/model selection by sending at least one message. It's
  // the gate for both the model-picker disable and the rate-limit
  // ring visibility — both paths read the same getter, so a behavior
  // drift here would mean those two affordances disagree on whether
  // the thread is configurable.
  it('flips isLocked once the timeline carries any item', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-lock' }));
    expect(pane.isLocked).toBe(false);

    pane.upsertItem(
      makeItem({
        id: 'user:0',
        threadId: 'thread-lock',
        kind: 'user_text',
        role: 'user',
      }),
    );
    expect(pane.isLocked).toBe(true);
  });

  it('marks live state as hydrating before the backend switch round-trip returns', async () => {
    const pane = createThreadPane();
    let releaseSwitch!: (value: unknown) => void;
    setBindingMock(
      'SwitchThread',
      (threadId: unknown) =>
        new Promise((resolve) => {
          releaseSwitch = resolve;
          void threadId;
        }),
    );

    const switching = pane.switchThread(makeThread({ id: 'thread-hydrating' }));
    expect(isThreadLiveStateHydrating('thread-hydrating')).toBe(true);

    releaseSwitch(makeThread({ id: 'thread-hydrating' }));
    await switching;
    expect(isThreadLiveStateHydrating('thread-hydrating')).toBe(false);
  });

  it('loads items and seeds the context window from thread.lastTokenUsage', async () => {
    const pane = createThreadPane();
    const items = [
      makeItem({
        id: 'user:0',
        kind: 'user_text',
        role: 'user',
        summary: 'hi',
      }),
      makeItem({ id: 'text:0:0', itemIndex: 1, summary: 'hello back' }),
    ];
    setBindingMock('ListThreadSliceAround', async () => ({
      items,
      oldestTurnIndex: 0,
      hasMore: false,
    }));

    await pane.switchThread(
      makeThread({
        lastTokenUsage: JSON.stringify({
          usedTokens: 1200,
          maxTokens: 200000,
          contextPercent: 0.6,
        }),
      }),
    );

    expect(pane.items).toEqual(items);
    expect(pane.contextWindow).toEqual({
      usedTokens: 1200,
      maxTokens: 200000,
      usedPercentage: 0.6,
      autoCompactPercent: 90,
      autoCompactTokenLimit: 180000,
    });
  });

  it('hydrates pending approval and user-input prompts on thread switch', async () => {
    const pane = createThreadPane();
    setBindingMock('GetThreadLiveState', async (threadId: string) => ({
      threadId,
      activeTurn: null,
      queueItems: [],
      interactive: {
        approvals: [
          {
            requestId: 'approval-1',
            threadId: 'thread-a',
            toolName: 'Bash',
            description: 'Run command',
            input: { command: 'pwd' },
            title: 'Approve command',
          },
        ],
        userInputs: [
          {
            requestId: 'input-1',
            threadId: 'thread-a',
            toolName: 'user_input',
            title: 'User Input Required',
            questions: [
              {
                id: 'scope',
                header: 'Scope',
                question: 'Choose a scope',
                options: [
                  { label: 'turn', description: 'Apply only to this turn' },
                ],
              },
            ],
          },
        ],
      },
      todo: null,
    }));

    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.pendingApprovals.map((request) => request.requestId)).toEqual([
      'approval-1',
    ]);
    expect(pane.pendingUserInputs[0]?.questions[0]?.options?.[0]?.label).toBe(
      'turn',
    );
  });

  it('does not re-add a prompt resolved while pending snapshot hydration is in flight', async () => {
    const pane = createThreadPane();
    let releaseSnapshot!: (value: unknown) => void;
    setBindingMock(
      'GetThreadLiveState',
      () =>
        new Promise((resolve) => {
          releaseSnapshot = resolve;
        }),
    );

    const switching = pane.switchThread(makeThread({ id: 'thread-a' }));
    await Promise.resolve();
    pane.removeUserInput('input-1');
    releaseSnapshot({
      threadId: 'thread-a',
      activeTurn: null,
      queueItems: [],
      interactive: {
        approvals: [],
        userInputs: [
          {
            requestId: 'input-1',
            threadId: 'thread-a',
            toolName: 'user_input',
            title: 'User Input Required',
            questions: [
              {
                id: 'scope',
                header: 'Scope',
                question: 'Choose a scope',
              },
            ],
          },
        ],
      },
      todo: null,
    });
    await switching;

    expect(pane.pendingUserInputs).toEqual([]);
  });

  it('uses the backend-returned thread from switchThread', async () => {
    const pane = createThreadPane();
    const selected = makeThread({ id: 'thread-a', lastReadAt: 100 });
    setBindingMock('SwitchThread', async () => ({
      ...selected,
      lastReadAt: 300,
    }));

    await pane.switchThread(selected);

    expect(pane.thread?.id).toBe('thread-a');
    expect(pane.thread?.lastReadAt).toBe(300);
  });

  it('preserves provider-emitted max tokens when rehydrating the context meter', async () => {
    const pane = createThreadPane();
    const selected = makeThread({
      id: 'thread-a',
      provider: 'codex',
      model: 'gpt-5.5',
      contextWindow: 1050000,
      lastTokenUsage: JSON.stringify({
        usedTokens: 136000,
        maxTokens: 1050000,
        contextPercent: 12.95,
      }),
    });
    setBindingMock('SwitchThread', async () => ({
      ...selected,
      contextWindow: 272000,
      autoCompactStandardPercent: 80,
      autoCompactExtendedPercent: 88,
    }));

    await pane.switchThread(selected);

    expect(pane.contextWindow).toEqual({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });
  });

  it('preserves provider-emitted max tokens for live context snapshots', async () => {
    const pane = createThreadPane();
    const selected = makeThread({
      id: 'thread-a',
      provider: 'codex',
      model: 'gpt-5.5',
      contextWindow: 272000,
      autoCompactStandardPercent: 80,
      autoCompactExtendedPercent: 88,
    });
    setBindingMock('SwitchThread', async () => selected);

    await pane.switchThread(selected);
    pane.setContextWindow({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });

    expect(pane.contextWindow).toEqual({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });
  });

  it('drops wrong-thread rows from initial history hydration', async () => {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [
        makeItem({ id: 'current', threadId: 'thread-a' }),
        makeItem({ id: 'leaked', threadId: 'thread-b' }),
      ],
      oldestTurnIndex: 0,
      hasMore: false,
    }));

    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.items.map((item) => item.id)).toEqual(['current']);
  });

  it('clears loading=false even when an inner mock throws synchronously', async () => {
    // Regression guard: a synchronous throw inside one of switchThread's
    // catch handlers (e.g. addToast) used to strand `loading=true`
    // because the function never reached its trailing `loading = false`.
    // The try/finally added in the wsClient defense-in-depth pass clears
    // loading on exit when no newer switch has superseded ours.
    setBindingMock('SwitchThread', () => {
      throw new Error('boom — synchronous failure');
    });
    setBindingMock('ListThreadSliceAround', () => {
      throw new Error('and the next call also blows up');
    });

    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-failing' }));

    expect(pane.loading).toBe(false);
  });

  it('clears pane-local state on thread switch', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-a',
      toolName: 'bash',
      description: 'Allow bash?',
      input: null,
      title: 'Approve bash',
    });
    pane.setGeneralError('boom');
    pane.setShowTerminal(true);
    pane.setShowPlanSidebar(true);

    await pane.switchThread(makeThread({ id: 'thread-b' }));

    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.generalError).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.showPlanSidebar).toBe(false);
  });

  // The terminal-focus intent is a pane-owned, consume-once flag that replaced
  // the old fire-once FOCUS_TERMINAL_EVENT. runTerminalToggle sets it before
  // showing the drawer; the drawer reads-and-clears it in onMount — whenever
  // its lazily-loaded chunk resolves. These pin the request/consume/clear
  // contract that makes the cold-open focus race impossible (the flag waits
  // for the drawer however late it mounts, instead of an event firing into a
  // window with no listener yet).
  describe('terminal focus intent', () => {
    it('has no pending request on a fresh pane', () => {
      const pane = createThreadPane();
      expect(pane.consumeTerminalFocusRequest()).toBe(false);
    });

    it('consumes a latched request exactly once', () => {
      const pane = createThreadPane();
      pane.requestTerminalFocus();
      // First consume sees the intent...
      expect(pane.consumeTerminalFocusRequest()).toBe(true);
      // ...and clears it, so a drawer remount ({#key threadId}) can't
      // re-grab focus the user never asked for.
      expect(pane.consumeTerminalFocusRequest()).toBe(false);
    });

    it('keeps the request across setShowTerminal(true) so the drawer can consume it', () => {
      const pane = createThreadPane();
      // runTerminalToggle requests BEFORE showing the drawer; opening must not
      // clear the intent or the cold-open focus would be lost again.
      pane.requestTerminalFocus();
      pane.setShowTerminal(true);
      expect(pane.consumeTerminalFocusRequest()).toBe(true);
    });

    it('drops a pending request when the terminal is hidden before it mounts', () => {
      const pane = createThreadPane();
      // Rapid open→close: focus was requested but the drawer never mounted to
      // consume it. Hiding must clear the intent so a later visibility-only
      // reopen — or a thread-restore mounting the drawer with showTerminal
      // persisted — doesn't inherit a stale "steal focus".
      pane.requestTerminalFocus();
      pane.setShowTerminal(false);
      expect(pane.consumeTerminalFocusRequest()).toBe(false);
    });

    it('reactively re-fires the consume effect when focus is requested on a LIVE pane', () => {
      // TerminalSurface consumes the intent inside a reactive $effect, so an
      // alt-h/l nav INTO an already-mounted terminal (requestTerminalFocus on a
      // warm surface) must re-run that effect. This only works because
      // pendingTerminalFocus is $state — a plain `let` would not re-fire the
      // effect, and the focus would never land on a warm nav-into. Requesting
      // AFTER the effect is live (not before) is the load-bearing scenario; a
      // pre-mount request would be consumed on first run regardless of $state.
      const pane = createThreadPane();
      let consumes = 0;
      const stop = $effect.root(() => {
        $effect(() => {
          if (pane.consumeTerminalFocusRequest()) consumes += 1;
        });
      });

      try {
        flushSync();
        // Surface is "mounted": the effect ran once and consumed nothing.
        expect(consumes).toBe(0);

        // Warm nav-into: focus requested AFTER the effect is established.
        pane.requestTerminalFocus();
        flushSync();
        // The effect re-ran and consumed the intent. A non-$state flag leaves
        // this at 0 (no re-run) — the regression this guards.
        expect(consumes).toBe(1);
      } finally {
        stop();
      }
    });
  });

  describe('companion panes', () => {
    it('closes every companion when the pane switches to a different thread', async () => {
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);
      openCompanion(pane.paneId, 'review');
      expect(pane.showPlanSidebar).toBe(true);
      expect(isCompanionOpen(pane.paneId, 'review')).toBe(true);

      await pane.switchThread(makeThread({ id: 'thread-b' }));

      expect(pane.showPlanSidebar).toBe(false);
      expect(isCompanionOpen(pane.paneId, 'review')).toBe(false);
      // Switching back does not resurrect them either — companions are
      // per-thread surfaces the user reopens explicitly.
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.showPlanSidebar).toBe(false);
    });

    it('closes take-control when switching to another claude-tui thread (no re-attach)', async () => {
      // The terminal mirror is pinned to the thread it was opened for. It
      // must never silently re-attach to the incoming thread's session —
      // keystrokes would land in the wrong PTY.
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      await pane.switchThread(makeThread({ id: 'thread-a', provider: 'claude-tui' }));
      openCompanion(pane.paneId, 'take-control');

      await pane.switchThread(makeThread({ id: 'thread-b', provider: 'claude-tui' }));

      expect(isCompanionOpen(pane.paneId, 'take-control')).toBe(false);
    });

    it('keeps companions open on a same-thread re-switch', async () => {
      // A forced in-place reload (same-thread re-switch) reloads items via
      // switchThread(currentThread); an open plan/review pane must
      // survive that.
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      const threadA = makeThread({ id: 'thread-a' });
      await pane.switchThread(threadA);
      pane.setShowPlanSidebar(true);
      openCompanion(pane.paneId, 'review');

      await pane.switchThread(makeThread({ id: 'thread-a' }));

      expect(pane.showPlanSidebar).toBe(true);
      expect(isCompanionOpen(pane.paneId, 'review')).toBe(true);
    });

    it('closes companions when "+ New" starts a draft placeholder in the pane', async () => {
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);

      pane.startDraftPlaceholder({
        id: 'p-1',
        path: '/tmp/p1',
        name: 'p1',
        sortPosition: 0,
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      });

      expect(pane.showPlanSidebar).toBe(false);
    });

    it('closes companions when the pane is cleared', async () => {
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);

      pane.clear();

      expect(pane.showPlanSidebar).toBe(false);
    });

    it('does not auto-open design preview for a fresh design thread', async () => {
      const pane = await buildPane(
        makeThread({ id: 'thread-a', mode: 'design' }),
      );

      expect(pane.showDesignPreviewPanel).toBe(false);
    });

    it('does not auto-open design preview when options hydrate while closed', async () => {
      const pane = await buildPane(
        makeThread({ id: 'thread-a', mode: 'design' }),
      );
      seedThreadPaneLayout(pane.paneId);
      setBindingMock('LatestDesignOptionSet', async () => ({
        setId: 'set-1',
        optionIds: ['alpha'],
      }));

      await pane.applyDesignOptionsUpdate('thread-a', 'set-1');

      expect(pane.activeOptionSet).toEqual({
        setId: 'set-1',
        optionPaths: ['options/set-1/alpha'],
      });
      expect(pane.showDesignPreviewPanel).toBe(false);

      pane.toggleDesignPreviewPanel();
      expect(pane.showDesignPreviewPanel).toBe(true);
    });

    it('closes design preview when switching to a different design thread', async () => {
      const threadA = makeThread({ id: 'thread-a', mode: 'design' });
      const threadB = makeThread({ id: 'thread-b', mode: 'design' });
      const pane = await buildPane(threadA);
      seedThreadPaneLayout(pane.paneId);
      pane.setShowDesignPreviewPanel(true);

      setBindingMock('SwitchThread', async () => threadB);
      await pane.switchThread(threadB);

      expect(pane.showDesignPreviewPanel).toBe(false);
    });

    it('closes design preview when switching to a non-design thread', async () => {
      const threadA = makeThread({ id: 'thread-a', mode: 'design' });
      const threadB = makeThread({ id: 'thread-b', mode: 'chat' });
      const pane = await buildPane(threadA);
      seedThreadPaneLayout(pane.paneId);
      pane.setShowDesignPreviewPanel(true);

      setBindingMock('SwitchThread', async () => threadB);
      await pane.switchThread(threadB);

      expect(pane.showDesignPreviewPanel).toBe(false);
      expect(isCompanionOpen(pane.paneId, 'design-preview')).toBe(false);
    });
  });

  it('ignores stale initial-load resolutions after a second thread switch', async () => {
    const pane = createThreadPane();
    type Paged = { items: Item[]; oldestTurnIndex: number; hasMore: boolean };
    let resolveA!: (paged: Paged) => void;
    let resolveB!: (paged: Paged) => void;
    const listA = new Promise<Paged>((resolve) => {
      resolveA = resolve;
    });
    const listB = new Promise<Paged>((resolve) => {
      resolveB = resolve;
    });

    setBindingMock('ListThreadSliceAround', (threadId: string) =>
      threadId === 'thread-a' ? listA : listB,
    );

    const switchA = pane.switchThread(makeThread({ id: 'thread-a' }));
    const switchB = pane.switchThread(makeThread({ id: 'thread-b' }));

    resolveB({
      items: [makeItem({ id: 'b', threadId: 'thread-b', summary: 'from b' })],
      oldestTurnIndex: 0,
      hasMore: false,
    });
    await switchB;
    resolveA({
      items: [makeItem({ id: 'a', threadId: 'thread-a', summary: 'from a' })],
      oldestTurnIndex: 0,
      hasMore: false,
    });
    await switchA;

    expect(pane.threadId).toBe('thread-b');
    expect(pane.items.map((item) => item.id)).toEqual(['b']);
  });

  it('upsertItem inserts in turn/item order and replaces rows in place', () => {
    const pane = createThreadPane();

    pane.upsertItem(makeItem({ id: 'late', turnIndex: 1, itemIndex: 0 }));
    pane.upsertItem(makeItem({ id: 'early', turnIndex: 0, itemIndex: 1 }));
    pane.upsertItem(makeItem({ id: 'first', turnIndex: 0, itemIndex: 0 }));

    expect(pane.items.map((item) => item.id)).toEqual([
      'first',
      'early',
      'late',
    ]);

    pane.upsertItem(
      makeItem({ id: 'early', turnIndex: 0, itemIndex: 1, summary: 'updated' }),
    );

    expect(pane.items.map((item) => item.id)).toEqual([
      'first',
      'early',
      'late',
    ]);
    expect(pane.items.find((item) => item.id === 'early')?.summary).toBe(
      'updated',
    );
  });

  it('allows upsertItem to be used as an unbound callback', () => {
    const pane = createThreadPane();
    const { upsertItem } = pane;

    upsertItem(makeItem({ id: 'unbound', turnIndex: 0, itemIndex: 0 }));

    expect(pane.items.map((item) => item.id)).toEqual(['unbound']);
  });

  it('upsertItems merges bursts in order and bumps timeline revision once', () => {
    const pane = createThreadPane();

    pane.upsertItems([
      makeItem({ id: 'late', turnIndex: 1, itemIndex: 0 }),
      makeItem({ id: 'early', turnIndex: 0, itemIndex: 1 }),
      makeItem({ id: 'first', turnIndex: 0, itemIndex: 0 }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual([
      'first',
      'early',
      'late',
    ]);
    expect(pane.timelineRevision).toBe(1);

    pane.upsertItems([
      makeItem({ id: 'late', turnIndex: 0, itemIndex: 2, summary: 'moved' }),
      makeItem({ id: 'early', turnIndex: 0, itemIndex: 1, summary: 'updated' }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual([
      'first',
      'early',
      'late',
    ]);
    expect(pane.items.find((item) => item.id === 'late')?.summary).toBe(
      'moved',
    );
    expect(pane.timelineRevision).toBe(2);
  });

  it('bumps timeline revision when switchThread installs the initial item window', async () => {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [
        makeItem({ id: 'loaded', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      ],
      oldestTurnIndex: 0,
      hasMore: false,
    }));
    const initialRevision = pane.timelineRevision;

    await pane.switchThread(makeThread({ id: 't' }));

    expect(pane.items.map((item) => item.id)).toEqual(['loaded']);
    expect(pane.timelineRevision).toBeGreaterThan(initialRevision);
  });

  it('bumps timeline revision when switchThread restores a cached item window', async () => {
    const pane = createThreadPane();
    const loadCalls: string[] = [];
    setBindingMock('ListThreadSliceAround', async (threadId: unknown) => {
      const id = String(threadId);
      loadCalls.push(id);
      return {
        items: [
          makeItem({
            id: `${id}-row`,
            threadId: id,
            turnIndex: 0,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: 0,
        hasMore: false,
      };
    });

    await pane.switchThread(makeThread({ id: 't' }));
    await pane.switchThread(makeThread({ id: 'other' }));
    const revisionBeforeCacheRestore = pane.timelineRevision;

    await pane.switchThread(makeThread({ id: 't' }));

    // Three loads, not two: the cache hit paints synchronously and then
    // still asks SyncThreadWindow whether the window moved. The old
    // skip-on-cache-hit was a staleness hole (another attached client can
    // rewrite history while a thread sits in the LRU).
    expect(loadCalls).toEqual(['t', 'other', 't']);
    expect(pane.items.map((item) => item.id)).toEqual(['t-row']);
    expect(pane.timelineRevision).toBeGreaterThan(revisionBeforeCacheRestore);
  });

  it('collapses same-batch wait-row enrichment into one final row', () => {
    const pane = createThreadPane();

    pane.upsertItems([
      makeItem({
        id: 'wait:pid-1:0',
        kind: 'terminal_interaction',
        summary: 'Waited for background terminal',
      }),
      makeItem({
        id: 'wait:pid-1:0',
        kind: 'terminal_interaction',
        summary: 'Background terminal completed',
        payloadId: 'payload-1',
        payloadKind: 'command_output',
        payloadMeta: JSON.stringify({ exitCode: 0 }),
      }),
    ]);

    expect(pane.items).toHaveLength(1);
    expect(pane.items[0].payloadKind).toBe('command_output');
    expect(pane.items[0].payloadId).toBe('payload-1');
    expect(pane.timelineRevision).toBe(1);
  });

  it('does not bump timeline revision for same-row Bash completion chrome', () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'bash',
        kind: 'tool_call',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: sleep 1',
        meta: JSON.stringify({ input: { command: 'sleep 1' } }),
      }),
    );
    const revision = pane.timelineRevision;

    pane.upsertItem(
      makeItem({
        id: 'bash',
        kind: 'tool_call',
        status: 'completed',
        toolName: 'Bash',
        summary: 'Bash: sleep 1',
        payloadId: 'payload-bash',
        payloadKind: 'command_output',
        payloadMeta: JSON.stringify({ command: 'sleep 1', exitCode: 0 }),
        meta: JSON.stringify({ input: { command: 'sleep 1' } }),
        updatedAt: 1,
      }),
    );

    expect(pane.items[0].status).toBe('completed');
    expect(pane.items[0].payloadKind).toBe('command_output');
    expect(pane.timelineRevision).toBe(revision);
  });

  it('does not bump timeline revision for collab-agent status-only chrome', () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'agent',
        kind: 'tool_call',
        status: 'running',
        toolName: 'collab_agent',
        meta: JSON.stringify({
          input: { tool: 'spawn_agent', receiverThreadIds: ['child-1'] },
        }),
        payloadMeta: JSON.stringify({
          input: { newAgentNickname: 'Reviewer' },
        }),
      }),
    );
    const revision = pane.timelineRevision;

    pane.upsertItem(
      makeItem({
        id: 'agent',
        kind: 'tool_call',
        status: 'completed',
        toolName: 'collab_agent',
        meta: JSON.stringify({
          input: { tool: 'spawn_agent', receiverThreadIds: ['child-1'] },
        }),
        payloadMeta: JSON.stringify({
          input: { newAgentNickname: 'Reviewer' },
        }),
        updatedAt: 1,
      }),
    );

    expect(pane.items[0].status).toBe('completed');
    expect(pane.timelineRevision).toBe(revision);
  });

  it('bumps timeline revision when an upsert changes timeline structure', () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'read',
        kind: 'tool_call',
        toolName: 'Read',
      }),
    );
    const revision = pane.timelineRevision;

    pane.upsertItem(
      makeItem({
        id: 'read',
        kind: 'tool_call',
        toolName: 'Edit',
      }),
    );

    expect(pane.timelineRevision).toBe(revision + 1);
  });

  it('preserves arrival order for rows with the same turn and item position', () => {
    const pane = createThreadPane();

    pane.upsertItems([
      makeItem({ id: 'later-position', turnIndex: 1, itemIndex: 0 }),
      makeItem({
        id: 'first-arrived',
        turnIndex: 0,
        itemIndex: 0,
        createdAt: 200,
      }),
      makeItem({
        id: 'second-arrived',
        turnIndex: 0,
        itemIndex: 0,
        createdAt: 100,
      }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual([
      'first-arrived',
      'second-arrived',
      'later-position',
    ]);
  });

  it('applies streaming deltas in place via replace-pattern', async () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'hello',
      }),
    );
    const initialItems = pane.items;
    const initialRevision = pane.timelineRevision;
    const initialLength = initialItems.length;

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: '!',
      updatedAt: 124,
    });
    // Smoothing routes streaming text through a per-item rAF smoother;
    // flush it synchronously so the assertion sees the fully revealed
    // accumulated text rather than the partial mid-reveal.
    pane.__flushItemSmoothersForTest();
    await nextFrame();

    // Replace-pattern semantics: deltas write `items[index] = { ...current, summary }`,
    // so the array proxy reference is stable, length is stable, and
    // timelineRevision does NOT bump (no insertions or sort). The summary
    // at the streaming row's slot reflects the accumulated deltas.
    expect(pane.items).toBe(initialItems);
    expect(pane.items.length).toBe(initialLength);
    expect(pane.timelineRevision).toBe(initialRevision);
    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
      'hello world!',
    );
    expect(pane.items.find((item) => item.id === 'text:0:0')?.updatedAt).toBe(
      124,
    );
  });

  it('keeps assistant text full even when the row has a payload link', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    try {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          kind: 'assistant_text',
          status: 'streaming',
          summary: 'seed',
          payloadId: 'assistant-text:thread-1:text:0:0',
          payloadKind: 'assistant_text',
        }),
      );

      const delta = Array.from({ length: 200 }, (_, index) => `word${index}`).join(' ');
      const expected = `seed${delta}`;
      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta,
        updatedAt: 125,
      });
      for (let frame = 0; frame < 500; frame += 1) {
        if ((pane.items.find((item) => item.id === 'text:0:0')?.summary ?? '') === expected) {
          break;
        }
        clock.tickFrame(16);
      }

      const summary =
        pane.items.find((item) => item.id === 'text:0:0')?.summary ?? '';
      expect(summary).toBe(expected);
    } finally {
      __setSmoothingClockForTest(undefined);
    }
  });

  it('low power mode reveals a streamed delta whole on the next frame', async () => {
    // Guards the revealImmediately wiring in threadStreamingReveal: with
    // the setting on, a fat delta lands as ONE summary write on the next
    // scheduled tick instead of animating across hundreds of frames.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    getSettings().lowPowerMode = true;
    try {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          kind: 'assistant_text',
          status: 'streaming',
          summary: 'seed',
        }),
      );

      const delta = Array.from({ length: 200 }, (_, index) => `word${index}`).join(' ');
      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta,
        updatedAt: 125,
      });
      clock.tickFrame(1);
      expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
        `seed${delta}`,
      );
    } finally {
      __setSmoothingClockForTest(undefined);
      resetSettingsForTest();
    }
  });

  it('disabling streaming reveals a streamed delta whole on the next frame', async () => {
    // The "Streaming enabled" setting is separate from low power: with it
    // OFF the smoother must also pass received straight through (one
    // summary write on the next tick), so ChatMarkdown's committed-block
    // gate reflects wire arrival rather than a rate-limited crawl. Guards
    // the streamingEnabled arm of threadStreamingReveal's revealImmediately.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    getSettings().streamingEnabled = false;
    try {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          kind: 'assistant_text',
          status: 'streaming',
          summary: 'seed',
        }),
      );

      const delta = Array.from({ length: 200 }, (_, index) => `word${index}`).join(' ');
      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta,
        updatedAt: 125,
      });
      clock.tickFrame(1);
      expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
        `seed${delta}`,
      );
    } finally {
      __setSmoothingClockForTest(undefined);
      resetSettingsForTest();
    }
  });

  it('thinking-row deltas trim to the 400-rune tail in place', async () => {
    // The frontend mirrors the server-side `thinkingPreviewRunes = 400`
    // cap so the completion upsert (which carries the same tail) does
    // not visibly shrink the row at settle. Full thinking content stays
    // on-demand via the expansion handle.
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'think:0:0',
        kind: 'thinking',
        status: 'streaming',
        summary: 'seed',
        payloadId: 'thinking-payload',
      }),
    );

    // Send an 800-rune block; only the last 400 should survive.
    const bigChunk = 'a'.repeat(800);
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'think:0:0',
      kind: 'thinking',
      delta: bigChunk,
      updatedAt: 100,
    });
    // Drain the smoother so the trim-to-tail logic in its reveal
    // callback has actually written through to the row.
    pane.__flushItemSmoothersForTest();

    const after =
      pane.items.find((item) => item.id === 'think:0:0')?.summary ?? '';
    expect([...after].length).toBe(400);
    expect(after.endsWith('a'.repeat(400))).toBe(true);
    expect(pane.items.find((item) => item.id === 'think:0:0')?.updatedAt).toBe(
      100,
    );
  });

  it('replaces the streaming row on completion upsert', async () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'hello',
      }),
    );
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    // The streaming-delta path goes through the smoother and the row's
    // summary catches up on rAF. The completion upsert below carries
    // the authoritative final summary, so the visible result snaps
    // through it regardless of how much the smoother had revealed.
    pane.upsertItem(
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'completed',
        summary: 'hello world',
      }),
    );

    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
      'hello world',
    );
  });

  it('ignores stale deltas for an item that already settled', async () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'completed',
        summary: 'yield timeouts',
      }),
    );

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: 'outs',
      updatedAt: 124,
    });

    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
      'yield timeouts',
    );
  });

  it('expansionStateFor returns the same handle across calls (survives row remount)', () => {
    // Why: the window's overscan eviction unmounts a row component when it
    // scrolls past the buffer; remounting reconstructs the snippet's
    // closure-scoped $state from scratch. The pane registry returns
    // the SAME handle reference for the same itemId, so toggle state
    // and loaded chunks survive the round-trip.
    const pane = createThreadPane();
    const item = makeItem({
      id: 'tool:5:0',
      kind: 'tool_call',
      payloadId: 'p-foo',
    });
    pane.upsertItem(item);

    const h1 = pane.expansionStateFor(item);
    const h2 = pane.expansionStateFor(item);
    expect(h2).toBe(h1);

    // Even when the Item reference is replaced (e.g. enrichment), the
    // handle stays stable because the cache key is item.id.
    const itemRefBumped = { ...pane.items[0], updatedAt: 999 } as Item;
    const h3 = pane.expansionStateFor(itemRefBumped);
    expect(h3).toBe(h1);
  });

  it('expansionStateForPayload returns the same handle for the same payloadId', () => {
    const pane = createThreadPane();
    const h1 = pane.expansionStateForPayload('p-foo', 'thread-1');
    const h2 = pane.expansionStateForPayload('p-foo', 'thread-1');
    expect(h2).toBe(h1);
  });

  it('payload-keyed expansion handles reload when their version changes', async () => {
    let version = 1;
    const preview = setBindingMock('GetPayloadPreview', async () => ({
      data: version === 1 ? 'payload v1' : 'payload v2',
      nextOffset: 10,
      totalSize: 10,
      isComplete: true,
    }));

    const pane = createThreadPane();
    const first = pane.expansionStateForPayload(
      'p-versioned',
      'thread-1',
      version,
    );
    await first.expand();
    expect(first.displayData).toBe('payload v1');

    version = 2;
    const second = pane.expansionStateForPayload(
      'p-versioned',
      'thread-1',
      version,
    );
    expect(second).toBe(first);

    await second.ensureLoaded();
    expect(second.displayData).toBe('payload v2');
    expect(preview).toHaveBeenCalledTimes(2);
  });

  it('subagent group expansion state is keyed by groupKey and survives lookup', () => {
    const pane = createThreadPane();
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(false);
    pane.toggleSubagentGroupExpanded('group-1');
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(true);
    expect(pane.isSubagentGroupExpanded('group-2')).toBe(false);
    pane.toggleSubagentGroupExpanded('group-1');
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(false);
  });

  it('attachmentCacheFor returns a stable view per itemId; survives lookup', () => {
    // Why: pre-rebuild, UserMessage.svelte allocated blob URLs in its
    // own onDestroy-revoking factory. The window's overscan eviction would
    // unmount + remount the row on a back-scroll, refetching every
    // attachment from Go. The pane-owned cache survives remount; the
    // factory seeds from it and writes loaded previews back.
    const pane = createThreadPane();
    const cacheA = pane.attachmentCacheFor('item-1');
    cacheA.set('att-1', {
      id: 'att-1',
      filename: 'a.png',
      mimeType: 'image/png',
      size: 1,
      url: 'data:img',
    });
    const cacheA2 = pane.attachmentCacheFor('item-1');
    expect(cacheA2.get('att-1')).toBeTruthy();
    expect(cacheA2.get('att-1')?.url).toBe('data:img');
    // Different itemId = isolated cache.
    const cacheB = pane.attachmentCacheFor('item-2');
    expect(cacheB.get('att-1')).toBeUndefined();
  });

  it('clears row UI state on switchThread', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));
    pane.upsertItem(
      makeItem({
        id: 'tool:0:0',
        kind: 'tool_call',
        payloadId: 'p-1',
        threadId: 'thread-a',
      }),
    );
    expect(pane.items.length).toBe(1);
    const h1 = pane.expansionStateFor(pane.items[0]);
    pane.toggleSubagentGroupExpanded('parent-x');
    expect(pane.isSubagentGroupExpanded('parent-x')).toBe(true);

    await pane.switchThread(makeThread({ id: 'thread-b' }));
    pane.upsertItem(
      makeItem({
        id: 'tool:0:0',
        kind: 'tool_call',
        payloadId: 'p-2',
        threadId: 'thread-b',
      }),
    );
    const h2 = pane.expansionStateFor(pane.items[0]);
    // Different thread → different handle (the previous one was cleared).
    expect(h2).not.toBe(h1);
    // SubagentGroup state was cleared too.
    expect(pane.isSubagentGroupExpanded('parent-x')).toBe(false);
  });

  it('clears discussion channel state on switchThread', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));
    pane.applyChannelMessages([
      {
        id: 'channel-message-1',
        channelId: 'channel-1',
        sequence: 1,
        fromType: 'agent',
        fromId: 'agent-1',
        fromRole: 'advocate',
        content: 'channel text',
        createdAt: 0,
      },
    ]);
    pane.applyChannelState({
      channelId: 'channel-1',
      threadId: 'thread-a',
      status: 'concluded',
      turnCount: 8,
      maxTurns: 8,
      awaitingResponse: false,
      currentSpeakerThreadId: '',
      currentSpeakerRole: '',
      participants: [],
    });

    await pane.switchThread(makeThread({ id: 'thread-b' }));

    expect(pane.channelMessages).toEqual([]);
    expect(pane.channelStatus).toBeNull();
  });

  it('drops deltas that arrive before the row exists', async () => {
    // With single-source-of-truth, deltas append in place to
    // `pane.items[i].summary`. A delta whose itemId has no entry in
    // `itemIndexById` is a no-op; events.ts batch ordering at
    // `flushPendingUpserts()` before `queueDelta` ensures the upsert
    // creates the row before any production delta touches the pane.
    const pane = createThreadPane();

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });

    expect(pane.items.find((item) => item.id === 'text:0:0')).toBeUndefined();
  });

  it('drops wrong-thread upserts for an active pane', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    pane.upsertItem(makeItem({ id: 'leaked', threadId: 'thread-b' }));
    pane.upsertItem(makeItem({ id: 'current', threadId: 'thread-a' }));

    expect(pane.items.map((item) => item.id)).toEqual(['current']);
  });

  it('resets design payload dedupe keys on thread switch', async () => {
    const pane = createThreadPane();
    setBindingMock('SwitchThread', async (threadId: unknown) =>
      makeThread({
        id: typeof threadId === 'string' ? threadId : 'design-a',
        mode: 'design',
      }),
    );
    const clarification = (questionId: string) =>
      designFence({
        kind: 'clarification_request',
        requestId: 'clarify-same-id',
        questions: [
          {
            id: questionId,
            prompt: 'Choose',
            choices: [{ id: 'yes', label: 'Yes' }],
          },
        ],
      });

    await pane.switchThread(makeThread({ id: 'design-a', mode: 'design' }));
    pane.upsertItem(
      makeItem({
        id: 'assistant-a',
        threadId: 'design-a',
        kind: 'assistant_text',
        summary: clarification('first-thread'),
      }),
    );
    expect(pane.pendingClarification?.questions[0]?.id).toBe('first-thread');

    await pane.switchThread(makeThread({ id: 'design-b', mode: 'design' }));
    pane.upsertItem(
      makeItem({
        id: 'assistant-b',
        threadId: 'design-b',
        kind: 'assistant_text',
        summary: clarification('second-thread'),
      }),
    );

    expect(pane.pendingClarification?.threadId).toBe('design-b');
    expect(pane.pendingClarification?.questions[0]?.id).toBe('second-thread');
  });

  it('derives isTurnActive strictly from activeTurn (invariant 22)', async () => {
    // Post-refactor, isTurnActive comes solely from the wire-pushed
    // activeTurn slot. Item state (streaming text, running tool_calls,
    // pending approvals) no longer leaks into the flag. The active-
    // turn registry is keyed by threadId, so the pane needs a thread
    // loaded before set/clear can route through to the global store.
    const pane = createThreadPane();
    await pane.switchThread(makeThread());

    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // A streaming assistant item alone doesn't flip the flag.
    pane.upsertItem(
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'streaming',
      }),
    );
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // A running foreground tool_call alone doesn't flip the flag either.
    pane.upsertItem(
      makeItem({
        id: 'tool-1',
        kind: 'tool_call',
        status: 'running',
        isBackground: false,
      }),
    );
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // Pending approvals no longer count on their own — they live INSIDE
    // an active turn (see invariant 22 rationale).
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      toolName: 'edit',
      description: 'Allow edit?',
      input: null,
      title: 'Approve edit',
    });
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // Wire-push flips it on.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 1 });
    expect(getActiveTurn(pane.threadId) !== null).toBe(true);

    // settleTurn clears it even if streaming items / approvals remain.
    pane.settleTurn({
      turnId: 't1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
    });
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });

  it('hydrates live server state on thread switch', async () => {
    const pane = createThreadPane();
    setBindingMock('GetThreadLiveState', async (threadId: string) => ({
      threadId,
      effectiveModel: 'claude-opus-4-8',
      effectiveModelRevision: 1,
      activeTurn: {
        threadId,
        turnId: 'round-1',
        turnIndex: 4,
        startedAt: 1_700_000_000_000,
      },
      queueItems: [
        {
          id: 'queue-1',
          threadId,
          message: 'queued while working',
          attachmentIds: ['att-1'],
          enqueuedAt: 1_700_000_000_100,
        },
      ],
      flushedItems: [
        {
          queueItemId: 'queue-flushed',
          userItemId: 'user:4:flush:1',
          message: 'already sent to provider',
        },
      ],
      interactive: {
        approvals: [
          {
            requestId: 'approval-1',
            threadId,
            toolName: 'Edit',
            description: 'Allow edit?',
            input: null,
            title: 'Approve edit',
          },
        ],
        userInputs: [],
      },
      todo: {
        threadId,
        steps: [{ step: 'keep working', status: 'inProgress' }],
        updatedAt: Date.now(),
      },
    }));

    await pane.switchThread(makeThread({ id: 'thread-live' }));

    expect(pane.activeModel).toBe('claude-opus-4-8');

    expect(getActiveTurn('thread-live')).toEqual({
      turnId: 'round-1',
      turnIndex: 4,
      startedAt: 1_700_000_000_000,
    });
    expect(getQueueForThread('thread-live')).toEqual([
      {
        id: 'queue-1',
        threadId: 'thread-live',
        message: 'queued while working',
        attachmentIds: ['att-1'],
        sourceProposedPlan: null,
        revisionSourceProposedPlan: null,
        revisionSourceCommentIds: undefined,
        revisionSourceDiffReview: null,
        revisionSourceDiffCommentIds: undefined,
        enqueuedAt: 1_700_000_000_100,
      },
    ]);
    expect(
      getFlushedForThread('thread-live').map((item) => ({
        queueItemId: item.queueItemId,
        userItemId: item.userItemId,
        message: item.message,
      })),
    ).toEqual([
      {
        queueItemId: 'queue-flushed',
        userItemId: 'user:4:flush:1',
        message: 'already sent to provider',
      },
    ]);
    expect(pane.pendingApprovals.map((approval) => approval.requestId)).toEqual(
      ['approval-1'],
    );
    expect(getThreadStatus('thread-live')).toBe('pending-approval');
    expect(pane.liveTodo?.steps).toEqual([
      { step: 'keep working', status: 'inProgress' },
    ]);
  });

  it('does not revive stale all-completed live todos from backend snapshot', async () => {
    const pane = createThreadPane();
    setBindingMock('GetThreadLiveState', async (threadId: string) => ({
      threadId,
      activeTurn: null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: {
        threadId,
        steps: [{ step: 'already done', status: 'completed' }],
        updatedAt: Date.now() - LIVE_TODO_AUTOHIDE_MS - 1,
      },
    }));

    await pane.switchThread(makeThread({ id: 'thread-done' }));

    expect(pane.liveTodo).toBeNull();
  });

  it('clears stale active turn when backend live snapshot is idle', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-idle' }));
    pane.setActiveTurn({ turnId: 'stale-round', turnIndex: 1, startedAt: 1 });
    expect(getActiveTurn('thread-idle')).not.toBeNull();

    await pane.refreshFromBackend();

    expect(getActiveTurn('thread-idle')).toBeNull();
  });

  it('does not let a delayed idle live snapshot clear a newer active turn', async () => {
    const pane = createThreadPane();
    let releaseSnapshot!: (value: unknown) => void;
    setBindingMock(
      'GetThreadLiveState',
      () =>
        new Promise((resolve) => {
          releaseSnapshot = resolve;
        }),
    );

    const switching = pane.switchThread(makeThread({ id: 'thread-race' }));
    await Promise.resolve();
    pane.setActiveTurn({ turnId: 'new-round', turnIndex: 2, startedAt: 2 });
    releaseSnapshot({
      threadId: 'thread-race',
      activeTurn: null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    });
    await switching;

    expect(getActiveTurn('thread-race')).toEqual({
      turnId: 'new-round',
      turnIndex: 2,
      startedAt: 2,
    });
  });

  it('does not let delayed live-state hydration overwrite a newer model fallback', async () => {
    const pane = createThreadPane();
    let releaseSnapshot!: (value: unknown) => void;
    setBindingMock(
      'GetThreadLiveState',
      () =>
        new Promise((resolve) => {
          releaseSnapshot = resolve;
        }),
    );

    const switching = pane.switchThread(makeThread({
      id: 'thread-model-race',
      provider: 'claude',
      model: 'claude-fable-5',
    }));
    await Promise.resolve();
    pane.setEffectiveModel('claude-opus-4-8');
    releaseSnapshot({
      threadId: 'thread-model-race',
      effectiveModel: '',
      activeTurn: null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    });
    await switching;

    expect(pane.activeModel).toBe('claude-opus-4-8');
  });

  it('does not let delayed live-state hydration overwrite a newer session account event', async () => {
    const pane = createThreadPane();
    let releaseSnapshot!: (value: unknown) => void;
    setBindingMock(
      'GetThreadLiveState',
      () =>
        new Promise((resolve) => {
          releaseSnapshot = resolve;
        }),
    );

    const switching = pane.switchThread(makeThread({
      id: 'thread-account-race',
      provider: 'codex',
    }));
    await Promise.resolve();
    pane.setProviderSessionAccount({
      threadId: 'thread-account-race',
      provider: 'codex',
      accountId: 'new-account',
      account: { email: 'new@example.com' },
      connected: true,
    });
    releaseSnapshot({
      threadId: 'thread-account-race',
      providerAccount: {
        threadId: 'thread-account-race',
        provider: 'codex',
        accountId: 'old-account',
        account: { email: 'old@example.com' },
        connected: true,
      },
      activeTurn: null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    });
    await switching;

    expect(pane.providerSessionAccount?.accountId).toBe('new-account');
    expect(pane.providerSessionAccount?.account.email).toBe('new@example.com');
  });

  it('does not let an older live-state hydration apply after a newer one completed', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-hydration-order' }));

    const releases: Array<(value: unknown) => void> = [];
    setBindingMock(
      'GetThreadLiveState',
      () =>
        new Promise((resolve) => {
          releases.push(resolve);
        }),
    );

    const older = pane.refreshFromBackend();
    for (let i = 0; i < 4 && releases.length < 1; i += 1)
      await Promise.resolve();
    const newer = pane.refreshFromBackend();
    for (let i = 0; i < 4 && releases.length < 2; i += 1)
      await Promise.resolve();
    expect(releases).toHaveLength(2);

    releases[1]({
      threadId: 'thread-hydration-order',
      activeTurn: {
        threadId: 'thread-hydration-order',
        turnId: 'new-round',
        turnIndex: 3,
        startedAt: 30,
      },
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    });
    await newer;

    releases[0]({
      threadId: 'thread-hydration-order',
      activeTurn: {
        threadId: 'thread-hydration-order',
        turnId: 'old-round',
        turnIndex: 2,
        startedAt: 20,
      },
      queueItems: [
        {
          id: 'stale-queue',
          threadId: 'thread-hydration-order',
          message: 'stale',
          attachmentIds: [],
          enqueuedAt: 1,
        },
      ],
      interactive: {
        approvals: [
          {
            requestId: 'stale-approval',
            threadId: 'thread-hydration-order',
            toolName: 'Edit',
            description: 'stale',
            input: null,
            title: 'Stale',
          },
        ],
        userInputs: [],
      },
      todo: {
        threadId: 'thread-hydration-order',
        steps: [{ step: 'stale todo', status: 'inProgress' }],
        updatedAt: Date.now(),
      },
    });
    await older;

    expect(getActiveTurn('thread-hydration-order')).toEqual({
      turnId: 'new-round',
      turnIndex: 3,
      startedAt: 30,
    });
    expect(getQueueForThread('thread-hydration-order')).toEqual([]);
    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.liveTodo).toBeNull();
  });

  it('does not let a delayed queue snapshot wipe a newer queue projection', async () => {
    const pane = createThreadPane();
    let releaseSnapshot!: (value: unknown) => void;
    setBindingMock(
      'GetThreadLiveState',
      () =>
        new Promise((resolve) => {
          releaseSnapshot = resolve;
        }),
    );

    const switching = pane.switchThread(
      makeThread({ id: 'thread-queue-race' }),
    );
    await Promise.resolve();
    replaceQueueForThread('thread-queue-race', [
      {
        id: 'queue-new',
        threadId: 'thread-queue-race',
        message: 'newer queue',
        attachmentIds: [],
        sourceProposedPlan: null,
        revisionSourceProposedPlan: null,
        enqueuedAt: 5,
      },
    ]);
    releaseSnapshot({
      threadId: 'thread-queue-race',
      activeTurn: null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    });
    await switching;

    expect(
      getQueueForThread('thread-queue-race').map((item) => item.message),
    ).toEqual(['newer queue']);
  });

  it('clear resets the pane completely', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread());
    const revoke = vi
      .spyOn(URL, 'revokeObjectURL')
      .mockImplementation(() => {});
    const item = makeItem({
      id: 'x',
      kind: 'tool_call',
      payloadId: 'payload-x',
    });
    pane.upsertItem(item);
    const revisionBeforeClear = pane.timelineRevision;
    pane.setGeneralError('boom');
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      toolName: 'bash',
      description: 'Allow bash?',
      input: null,
      title: 'Approve bash',
    });
    pane.applyChannelMessages([
      {
        id: 'channel-message-1',
        channelId: 'channel-1',
        sequence: 1,
        fromType: 'agent',
        fromId: 'agent-1',
        fromRole: 'advocate',
        content: 'channel text',
        createdAt: 0,
      },
    ]);
    pane.applyChannelState({
      channelId: 'channel-1',
      threadId: 'thread-1',
      status: 'concluded',
      turnCount: 8,
      maxTurns: 8,
      awaitingResponse: false,
      currentSpeakerThreadId: '',
      currentSpeakerRole: '',
      participants: [],
    });
    const expansion = pane.expansionStateFor(item);
    pane.toggleSubagentGroupExpanded('group-x');
    pane.attachmentCacheFor(item.id).set('attachment-x', {
      id: 'attachment-x',
      filename: 'x.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:pane-clear',
    });

    pane.clear();

    expect(pane.thread).toBeNull();
    expect(pane.items).toEqual([]);
    expect(pane.timelineRevision).toBeGreaterThan(revisionBeforeClear);
    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.channelMessages).toEqual([]);
    expect(pane.channelStatus).toBeNull();
    expect(pane.contextWindow).toBeNull();
    expect(pane.generalError).toBeNull();
    expect(pane.expansionStateFor(item)).not.toBe(expansion);
    expect(pane.isSubagentGroupExpanded('group-x')).toBe(false);
    expect(
      pane.attachmentCacheFor(item.id).get('attachment-x'),
    ).toBeUndefined();
    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:pane-clear');
    revoke.mockRestore();
  });

  describe('windowed history', () => {
    it('upsertItem drops new items below the window floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({
          id: 'at-floor',
          threadId: 'thread-windowed',
          turnIndex: 5,
          itemIndex: 0,
        }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: seed,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 'thread-windowed' }));
      expect(pane.oldestLoadedTurnIndex).toBe(5);

      // Upsert for a turn below the floor (e.g. interrupt-queue replay
      // of an older tool_completion). Must NOT land in the window — the
      // canonical row stays in SQLite and surfaces via loadOlder later.
      pane.upsertItem(
        makeItem({
          id: 'below',
          threadId: 'thread-windowed',
          turnIndex: 2,
          itemIndex: 0,
        }),
      );
      expect(pane.items.map((it) => it.id)).toEqual(['at-floor']);
    });

    it('upsertItem still accepts replacements for known ids below the floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({
          id: 'known',
          threadId: 't',
          turnIndex: 5,
          itemIndex: 0,
          summary: 'old',
        }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: seed,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      // Known id, turn below floor — cross-turn correction path. Must
      // still replace because the id is clearly in-window already.
      pane.upsertItem(
        makeItem({
          id: 'known',
          threadId: 't',
          turnIndex: 2,
          itemIndex: 0,
          summary: 'new',
        }),
      );
      expect(pane.items.find((it) => it.id === 'known')?.summary).toBe('new');
    });

    it('upsertItem rejects new streaming rows below the floor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'at-floor',
            threadId: 't',
            turnIndex: 5,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      pane.upsertItem(
        makeItem({
          id: 'below-streaming',
          threadId: 't',
          turnIndex: 2,
          itemIndex: 0,
          status: 'streaming',
          summary: 'old output',
        }),
      );

      // Window-floor guard rejects the below-floor item; nothing
      // lingers anywhere because the pane no longer carries a parallel
      // streaming overlay.
      expect(pane.items.map((it) => it.id)).toEqual(['at-floor']);
    });

    it('loadOlder prepends older items and updates the floor + hasMore', async () => {
      const pane = createThreadPane();
      const tail: Item[] = [
        makeItem({ id: 't5', threadId: 't', turnIndex: 5, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: tail,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [
          makeItem({ id: 't3', threadId: 't', turnIndex: 3, itemIndex: 0 }),
          makeItem({ id: 't4', threadId: 't', turnIndex: 4, itemIndex: 0 }),
        ],
        oldestTurnIndex: 3,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      const revisionBeforeLoadOlder = pane.timelineRevision;
      const result = await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual(['t3', 't4', 't5']);
      expect(pane.timelineRevision).toBeGreaterThan(revisionBeforeLoadOlder);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.loadingOlder).toBe(false);
      // The grow-flush set the shift flag; the finally always clears it, even
      // on this small window that never reaches the tail-prune.
      expect(pane.pendingTimelineShiftAtHead).toBe(false);
      expect(result).toEqual({
        status: 'loaded',
        insertedBeforeWindow: true,
        insertedRows: true,
      });
    });

    it('loadOlder is a no-op when hasMoreHistory is false', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'a', turnIndex: 0, itemIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      let calls = 0;
      setBindingMock('ListItemsBeforeCursor', async () => {
        calls += 1;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();
      expect(calls).toBe(0);
      expect(result).toEqual({
        status: 'noop',
        insertedBeforeWindow: false,
        insertedRows: false,
      });
    });

    it('loadOlder guards against a thread swap mid-fetch', async () => {
      const pane = createThreadPane();
      let resolveOlder!: (v: {
        items: Item[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }) => void;
      const olderPromise = new Promise<{
        items: Item[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }>((r) => {
        resolveOlder = r;
      });
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', () => olderPromise);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      const olderPending = pane.loadOlder();
      // Swap before the older fetch resolves.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      resolveOlder({
        items: [makeItem({ id: 'stale', turnIndex: 3, threadId: 'thread-a' })],
        oldestTurnIndex: 3,
        hasMore: true,
      });
      await olderPending;

      // thread-b has its own fresh window; the stale thread-a older
      // fetch must not leak into it.
      expect(pane.threadId).toBe('thread-b');
      expect(pane.items.some((it) => it.id === 'stale')).toBe(false);
      // loadOlder set the shift flag before awaiting the (stale) fetch; the
      // finally clears it even though the result was discarded as stale.
      expect(pane.pendingTimelineShiftAtHead).toBe(false);
    });

    it('loadUntilItem returns true when the item is already in-window', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'here', threadId: 't', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      let fetched = 0;
      setBindingMock('GetThreadItem', async () => {
        fetched += 1;
        return makeItem({ id: 'here', turnIndex: 5 });
      });
      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('here');
      expect(ok).toBe(true);
      expect(fetched).toBe(0);
    });

    it('loadUntilItem replaces the window to cover a below-floor item', async () => {
      const pane = createThreadPane();
      let sliceCalls = 0;
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId: string, anchorItemId: string) => {
          sliceCalls += 1;
          if (anchorItemId === 'target') {
            return {
              items: [
                makeItem({ id: 'target', threadId: 't', turnIndex: 1 }),
                makeItem({ id: 't2', threadId: 't', turnIndex: 2 }),
                makeItem({ id: 't3', threadId: 't', turnIndex: 3 }),
              ],
              oldestTurnIndex: 1,
              newestTurnIndex: 3,
              hasMore: false,
              hasMoreOlder: false,
              hasMoreNewer: true,
            };
          }
          return {
            items: [makeItem({ id: 't5', threadId: 't', turnIndex: 5 })],
            oldestTurnIndex: 5,
            newestTurnIndex: 5,
            hasMore: true,
            hasMoreOlder: true,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock(
        'GetThreadItem',
        async (_threadId: string, itemId: string) =>
          itemId === 'target'
            ? makeItem({ id: 'target', threadId: 't', turnIndex: 1 })
            : null,
      );
      await pane.switchThread(makeThread({ id: 't' }));
      const revisionBeforeLoadUntil = pane.timelineRevision;
      const ok = await pane.loadUntilItem('target');

      expect(ok).toBe(true);
      expect(pane.timelineRevision).toBeGreaterThan(revisionBeforeLoadUntil);
      expect(pane.oldestLoadedTurnIndex).toBe(1);
      expect(pane.newestLoadedTurnIndex).toBe(3);
      expect(pane.hasMoreNewer).toBe(true);
      expect(pane.items.map((it) => it.id)).toEqual(['target', 't2', 't3']);
      expect(sliceCalls).toBe(2);
    });

    it('loadUntilItem returns false when the item is unknown to the backend', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 't5', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async () => makeItem({ id: '' }));
      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('ghost');
      expect(ok).toBe(false);
    });

    it('requestScrollToItem bumps the nonce observed by the timeline', () => {
      const pane = createThreadPane();
      const first = pane.scrollToItemRequest.nonce;
      pane.requestScrollToItem('a');
      const second = pane.scrollToItemRequest.nonce;
      expect(second).toBeGreaterThan(first);
      expect(pane.scrollToItemRequest.itemId).toBe('a');
      expect(pane.scrollToItemRequest.flash).toBe(false);
      pane.requestScrollToItem('b');
      expect(pane.scrollToItemRequest.nonce).toBeGreaterThan(second);
      expect(pane.scrollToItemRequest.itemId).toBe('b');
    });

    it('requestScrollToItem carries flash option', () => {
      const pane = createThreadPane();
      pane.requestScrollToItem('revert-target-user-message', { flash: true });

      expect(pane.scrollToItemRequest.itemId).toBe('revert-target-user-message');
      expect(pane.scrollToItemRequest.flash).toBe(true);
    });

    it('scrollToItemRequest nonce stays monotonic across switchThread', async () => {
      // The timeline tracks `lastHandledScrollNonce` locally. If a pane
      // reset the nonce to 0 on switch, a follow-up intent with nonce=1
      // would compare against the lingering higher handled value and
      // silently not dispatch. Keep the nonce monotonic.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      pane.requestScrollToItem('before-switch');
      const beforeSwitch = pane.scrollToItemRequest.nonce;
      expect(beforeSwitch).toBeGreaterThan(0);

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.scrollToItemRequest.nonce).toBe(beforeSwitch);

      pane.requestScrollToItem('after-switch');
      expect(pane.scrollToItemRequest.nonce).toBeGreaterThan(beforeSwitch);
    });

    it('loadUntilItem loads the target turn when the pane has no floor yet', async () => {
      // An empty-thread pane (or one whose switchThread returned 0 items)
      // has `oldestLoadedTurnIndex = null`. The loader must still be able
      // to pull in history when the user triggers scroll-to-item from
      // search — not skip the fetch and short-circuit to `true`.
      const pane = createThreadPane();
      let sliceAnchor: string | null = null;
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId: string, anchorItemId: string) => {
          sliceAnchor = anchorItemId;
          if (anchorItemId === 'deep') {
            return {
              items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
              oldestTurnIndex: 3,
              newestTurnIndex: 3,
              hasMore: true,
              hasMoreOlder: true,
              hasMoreNewer: true,
            };
          }
          return {
            items: [],
            oldestTurnIndex: -1,
            newestTurnIndex: -1,
            hasMore: false,
            hasMoreOlder: false,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();

      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(sliceAnchor).toBe('deep');
      expect(pane.items.some((it) => it.id === 'deep')).toBe(true);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
      expect(pane.newestLoadedTurnIndex).toBe(3);
      expect(pane.hasMoreNewer).toBe(true);
    });

    it('loadUntilItem rejects an item whose threadId does not match the current pane', async () => {
      // Defense-in-depth: a mislayered binding or stale cache that
      // returns a row from a different thread should never cross-pollute
      // a pane. loadUntilItem must treat the mismatch as "not found"
      // rather than trying to page an item that doesn't belong here.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'wrong', threadId: 'other-thread', turnIndex: 1 }),
      );
      let paged = 0;
      setBindingMock('ListItemsBeforeCursor', async () => {
        paged += 1;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));

      const ok = await pane.loadUntilItem('wrong');
      expect(ok).toBe(false);
      expect(paged).toBe(0);
    });

    it('loadUntilItem resolves a subagent child by anchoring on its launch root and hydrating the subtree', async () => {
      // History windows exclude child rows, so a scroll-to-item target
      // inside a subagent transcript must (1) walk the parent chain to
      // the top-level launch root, (2) slice the window around the
      // root, and (3) hydrate the root's descendants so the containing
      // group card can resolve the scroll.
      const pane = createThreadPane();
      const sliceAnchors: string[] = [];
      setBindingMock(
        'GetThreadItem',
        async (_threadId: string, itemId: string) => {
          if (itemId === 'deep-child') {
            return makeItem({
              id: 'deep-child',
              threadId: 't',
              turnIndex: 4,
              itemIndex: 3,
              parentId: 'mid-launch',
            });
          }
          if (itemId === 'mid-launch') {
            return makeItem({
              id: 'mid-launch',
              threadId: 't',
              turnIndex: 4,
              itemIndex: 1,
              parentId: 'root-launch',
              kind: 'tool_call',
              toolName: 'Task',
            });
          }
          if (itemId === 'root-launch') {
            return makeItem({
              id: 'root-launch',
              threadId: 't',
              turnIndex: 4,
              itemIndex: 0,
              kind: 'tool_call',
              toolName: 'Task',
            });
          }
          return makeItem({ id: '' });
        },
      );
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId: string, anchorItemId: string) => {
          sliceAnchors.push(anchorItemId);
          if (anchorItemId === 'root-launch') {
            return {
              items: [
                makeItem({
                  id: 'root-launch',
                  threadId: 't',
                  turnIndex: 4,
                  itemIndex: 0,
                  kind: 'tool_call',
                  toolName: 'Task',
                }),
                makeItem({ id: 'after', threadId: 't', turnIndex: 5 }),
              ],
              oldestTurnIndex: 4,
              newestTurnIndex: 5,
              hasMore: true,
              hasMoreOlder: true,
              hasMoreNewer: false,
            };
          }
          return {
            items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 9 })],
            oldestTurnIndex: 9,
            newestTurnIndex: 9,
            hasMore: true,
            hasMoreOlder: true,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock(
        'ListSubagentDescendants',
        async (_threadId: string, rootItemId: string) =>
          rootItemId === 'root-launch'
            ? [
                makeItem({
                  id: 'mid-launch',
                  threadId: 't',
                  turnIndex: 4,
                  itemIndex: 1,
                  parentId: 'root-launch',
                  kind: 'tool_call',
                  toolName: 'Task',
                }),
                makeItem({
                  id: 'deep-child',
                  threadId: 't',
                  turnIndex: 4,
                  itemIndex: 3,
                  parentId: 'mid-launch',
                }),
              ]
            : [],
      );
      await pane.switchThread(makeThread({ id: 't' }));

      const ok = await pane.loadUntilItem('deep-child');

      expect(ok).toBe(true);
      expect(sliceAnchors.at(-1)).toBe('root-launch');
      expect(pane.items.map((it) => it.id)).toEqual([
        'root-launch',
        'mid-launch',
        'deep-child',
        'after',
      ]);
    });

    it('ensureSubagentChildren merges descendants additively and dedupes repeat calls', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'anchor',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 0,
            kind: 'tool_call',
            toolName: 'Task',
          }),
          makeItem({ id: 'tail', threadId: 't', turnIndex: 2 }),
        ],
        oldestTurnIndex: 1,
        newestTurnIndex: 2,
        hasMore: false,
      }));
      let listCalls = 0;
      setBindingMock('ListSubagentDescendants', async () => {
        listCalls += 1;
        return [
          makeItem({
            id: 'child-1',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 1,
            parentId: 'anchor',
          }),
          makeItem({
            id: 'child-2',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 2,
            parentId: 'anchor',
          }),
        ];
      });
      await pane.switchThread(makeThread({ id: 't' }));

      const first = await pane.ensureSubagentChildren('anchor');
      expect(first).toBe(true);
      expect(pane.items.map((it) => it.id)).toEqual([
        'anchor',
        'child-1',
        'child-2',
        'tail',
      ]);

      // A repeat call re-fetches once (children might have grown), adds
      // nothing, and marks the anchor exhausted.
      const second = await pane.ensureSubagentChildren('anchor');
      expect(second).toBe(false);
      expect(listCalls).toBe(2);

      // Exhausted anchors skip the backend entirely so a stale
      // decorated count can't loop the expansion effect.
      const third = await pane.ensureSubagentChildren('anchor');
      expect(third).toBe(false);
      expect(listCalls).toBe(2);
    });

    it('ensureSubagentChildren dedupes concurrent calls for the same anchor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'anchor',
            threadId: 't',
            turnIndex: 1,
            kind: 'tool_call',
            toolName: 'Task',
          }),
        ],
        oldestTurnIndex: 1,
        hasMore: false,
      }));
      let resolveList: (items: Item[]) => void = () => {};
      const listMock = setBindingMock(
        'ListSubagentDescendants',
        () =>
          new Promise((resolve) => {
            resolveList = resolve as (items: Item[]) => void;
          }),
      );
      await pane.switchThread(makeThread({ id: 't' }));

      const firstPromise = pane.ensureSubagentChildren('anchor');
      const duplicate = await pane.ensureSubagentChildren('anchor');
      expect(duplicate).toBe(false);

      resolveList([
        makeItem({
          id: 'child-1',
          threadId: 't',
          turnIndex: 1,
          itemIndex: 1,
          parentId: 'anchor',
        }),
      ]);
      expect(await firstPromise).toBe(true);
      expect(listMock.mock.calls).toHaveLength(1);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    it('ensureSubagentChildren discards a fetch that resolves after a thread switch', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'anchor',
            threadId: 'thread-a',
            turnIndex: 1,
            kind: 'tool_call',
            toolName: 'Task',
          }),
        ],
        oldestTurnIndex: 1,
        hasMore: false,
      }));
      let resolveList: (items: Item[]) => void = () => {};
      setBindingMock(
        'ListSubagentDescendants',
        () =>
          new Promise((resolve) => {
            resolveList = resolve as (items: Item[]) => void;
          }),
      );
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      const pending = pane.ensureSubagentChildren('anchor');

      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'b-item', threadId: 'thread-b', turnIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 'thread-b' }));

      resolveList([
        makeItem({
          id: 'stale-child',
          threadId: 'thread-a',
          turnIndex: 1,
          itemIndex: 1,
          parentId: 'anchor',
        }),
      ]);
      expect(await pending).toBe(false);
      expect(pane.items.some((it) => it.id === 'stale-child')).toBe(false);
    });

    it('ensureSubagentChildren recovers after a failed fetch', async () => {
      // A transient backend failure must not wedge the anchor: the
      // in-flight marker clears in finally and the anchor is NOT marked
      // exhausted, so the next call (the user re-expanding the card)
      // re-fetches instead of being suppressed.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'anchor',
            threadId: 't',
            turnIndex: 1,
            kind: 'tool_call',
            toolName: 'Task',
          }),
        ],
        oldestTurnIndex: 1,
        hasMore: false,
      }));
      let listCalls = 0;
      setBindingMock('ListSubagentDescendants', async () => {
        listCalls += 1;
        if (listCalls === 1) throw new Error('mock backend down');
        return [
          makeItem({
            id: 'child-1',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 1,
            parentId: 'anchor',
          }),
        ];
      });
      await pane.switchThread(makeThread({ id: 't' }));

      const failed = await pane.ensureSubagentChildren('anchor');
      expect(failed).toBe(false);
      expect(pane.items.map((it) => it.id)).toEqual(['anchor']);

      const retried = await pane.ensureSubagentChildren('anchor');
      expect(retried).toBe(true);
      expect(listCalls).toBe(2);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    it('loadOlder disables hasMoreHistory when the backend cannot advance the floor', async () => {
      // Pathological scenario: turns table claims more history exists
      // but the item range before the current cursor is empty (a sparse
      // turn row with no items). Without a progress guard the Load
      // Older button would keep firing the same query. The store must
      // break the loop by forcing hasMoreHistory=false when no rows
      // were returned AND the floor did not decrease.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 10 })],
        oldestTurnIndex: 10,
        hasMore: true,
      }));
      let calls = 0;
      setBindingMock('ListItemsBeforeCursor', async () => {
        calls += 1;
        // Backend cooperates: no items, floor unchanged, but still
        // claims more exists. Common when a turn row has zero items.
        return { items: [], oldestTurnIndex: 10, hasMore: true };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.hasMoreHistory).toBe(true);
      await pane.loadOlder();
      expect(calls).toBe(1);
      expect(pane.hasMoreHistory).toBe(false);
      // Second invocation should short-circuit; no network call.
      await pane.loadOlder();
      expect(calls).toBe(1);
    });

    it('loadOlder clears loadingOlder even when a concurrent loadUntilItem bumps the paging generation', async () => {
      // Regression pin: `loadingOlder` is a UI-only flag. If a
      // concurrent `loadUntilItem` increments `pagingGeneration`
      // while `loadOlder` is mid-fetch, the generation-guarded
      // finally block used to skip clearing the flag, greying out
      // the Load Older button forever. The fix resets the flag
      // unconditionally.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 10 })],
        oldestTurnIndex: 10,
        hasMore: true,
      }));
      let releaseOlder!: (v: {
        items: ReturnType<typeof makeItem>[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }) => void;
      const olderPending = new Promise<{
        items: ReturnType<typeof makeItem>[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }>((r) => {
        releaseOlder = r;
      });
      setBindingMock('ListItemsBeforeCursor', () => olderPending);

      await pane.switchThread(makeThread({ id: 't' }));
      const olderPromise = pane.loadOlder();
      expect(pane.loadingOlder).toBe(true);

      // Kick off loadUntilItem, which increments pagingGeneration and
      // takes its own path. It must not deadlock loadOlder's cleanup.
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
        oldestTurnIndex: 3,
        newestTurnIndex: 3,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));
      await pane.loadUntilItem('deep');

      releaseOlder({ items: [], oldestTurnIndex: 10, hasMore: false });
      await olderPromise;

      expect(pane.loadingOlder).toBe(false);
    });

    it('loadUntilItem uses a bounded centered slice when the pane floor is null', async () => {
      // Regression pin for the MAX_SAFE_INTEGER itemBudget bug: search
      // jumps must request a bounded centered slice, not an unbounded
      // page from the target to the tail.
      const pane = createThreadPane();
      let capturedAnchor = '';
      let capturedTargetCount = 0;
      setBindingMock(
        'ListThreadSliceAround',
        async (_id, anchor, targetCount) => {
          capturedAnchor = anchor as string;
          capturedTargetCount = targetCount as number;
          if (anchor === 'deep') {
            return {
              items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
              oldestTurnIndex: 3,
              newestTurnIndex: 3,
              hasMore: true,
              hasMoreOlder: true,
              hasMoreNewer: true,
            };
          }
          return {
            items: [],
            oldestTurnIndex: -1,
            newestTurnIndex: -1,
            hasMore: false,
            hasMoreOlder: false,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();
      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(capturedAnchor).toBe('deep');
      expect(capturedTargetCount).toBeLessThanOrEqual(500);
      expect(capturedTargetCount).toBeGreaterThan(0);
    });

    it('pagingGeneration stays monotonic across switchThread', async () => {
      // Regression: earlier the reset to 0 on swap meant a stale
      // in-flight paging fetch could see its captured generation
      // match the freshly-reset counter and proceed to clobber
      // state. The switchGeneration guard catches the common case
      // but pinning the monotonicity invariant here prevents a
      // future refactor from reintroducing the reset.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'a', threadId: 't', turnIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'x', threadId: 't', turnIndex: 0 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      // Trigger a paging call so pagingGeneration advances to 1.
      await pane.loadOlder();
      await pane.switchThread(makeThread({ id: 't2' }));
      // After switch, another paging call should advance the counter
      // further — never regress to a prior value. We observe by
      // chaining two calls and ensuring the second still makes a
      // network call (i.e. the guards remain accurate).
      let postSwitchCalls = 0;
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'b', threadId: 't2', turnIndex: 3 })],
        oldestTurnIndex: 3,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => {
        postSwitchCalls += 1;
        return { items: [], oldestTurnIndex: 2, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't3' }));
      await pane.loadOlder();
      expect(postSwitchCalls).toBe(1);
    });

    it('loadOlder dedupes by id when the backend re-returns a loaded row', async () => {
      // Defensive contract: a paging response can re-return a row the
      // window already holds (overlapping ranges after a prune, or a
      // row that arrived via a streamed upsert mid-fetch). The store
      // must not duplicate it in `items` — the dedup happens via
      // `mergeItemsById`.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        // Backend legitimately returns the ancestor again (it sits
        // below the new paging floor and the recursive CTE pulls it
        // in for any child-in-range query).
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'between', threadId: 't', turnIndex: 3 }),
        ],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['ancestor', 'child']);

      await pane.loadOlder();
      // 'ancestor' appears once — duplicates are filtered out.
      const ancestors = pane.items.filter((it) => it.id === 'ancestor');
      expect(ancestors.length).toBe(1);
      // Ordering: the newly prepended 'between' sits before the
      // existing tail. The duplicate ancestor row was dropped so the
      // original position is preserved.
      expect(pane.items.map((it) => it.id)).toEqual([
        'ancestor',
        'between',
        'child',
      ]);
    });

    it('loadOlder replaces duplicate rows with enriched backend copies', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'ancestor',
            threadId: 't',
            turnIndex: 0,
            summary: 'summary-only',
          }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [
          makeItem({
            id: 'ancestor',
            threadId: 't',
            turnIndex: 0,
            summary: 'enriched',
            payloadId: 'payload-ancestor',
          }),
          makeItem({ id: 'between', threadId: 't', turnIndex: 3 }),
        ],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadOlder();

      const ancestor = pane.items.find((it) => it.id === 'ancestor');
      expect(ancestor?.summary).toBe('enriched');
      expect(ancestor?.payloadId).toBe('payload-ancestor');
      expect(pane.items.filter((it) => it.id === 'ancestor')).toHaveLength(1);
    });

    it('loadOlder reports rows inserted after an ancestor above the floor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [makeItem({ id: 'between', threadId: 't', turnIndex: 3 })],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual([
        'ancestor',
        'between',
        'child',
      ]);
      expect(result).toEqual({
        status: 'loaded',
        insertedBeforeWindow: false,
        insertedRows: true,
      });
    });

    it('loadUntilItem dedupes by id when pulling in a below-floor target', async () => {
      // Same contract as loadOlder's dedup, but via the scroll-to-item
      // entry point. The centered replacement can include rows that were
      // already present by id; no duplicate should land in the window.
      const pane = createThreadPane();
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId: string, anchorItemId: string) => {
          if (anchorItemId === 'deep') {
            return {
              items: [
                makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
                makeItem({ id: 'deep', threadId: 't', turnIndex: 2 }),
              ],
              oldestTurnIndex: 2,
              newestTurnIndex: 2,
              hasMore: false,
              hasMoreOlder: false,
              hasMoreNewer: true,
            };
          }
          return {
            items: [
              makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
              makeItem({ id: 'tail', threadId: 't', turnIndex: 5 }),
            ],
            oldestTurnIndex: 5,
            newestTurnIndex: 5,
            hasMore: true,
            hasMoreOlder: true,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 2 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(pane.items.filter((it) => it.id === 'ancestor').length).toBe(1);
      expect(pane.items.some((it) => it.id === 'deep')).toBe(true);
      expect(pane.hasMoreNewer).toBe(true);
    });

    it('upsertItem accepts new items when the pane floor is null (empty thread)', async () => {
      // Regression: the floor guard short-circuits when
      // `oldestLoadedTurnIndex` is null so streamed upserts on a
      // fresh pane still land. Without the null check, every first
      // item on a brand-new thread would be dropped.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();
      pane.upsertItem(
        makeItem({ id: 'first', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      );
      expect(pane.items.map((it) => it.id)).toEqual(['first']);
    });

    it('holds newer upserts behind the newer-history gap when reading an old window', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'old-window',
            threadId: 't',
            turnIndex: 3,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: 3,
        newestTurnIndex: 3,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const changed = pane.upsertItem(
        makeItem({ id: 'newer', threadId: 't', turnIndex: 9, itemIndex: 0 }),
      );

      expect(changed).toBe(true);
      expect(pane.items.map((it) => it.id)).toEqual(['old-window']);
      expect(pane.hasMoreNewer).toBe(true);
      expect(pane.newestLoadedTurnIndex).toBe(3);
    });

    it('loadNewer pages forward from the current ceiling', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 't3', threadId: 't', turnIndex: 3, itemIndex: 0 }),
        ],
        oldestTurnIndex: 3,
        newestTurnIndex: 3,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));
      let afterCursor: unknown = null;
      setBindingMock('ListItemsAfterCursor', async (_threadId, after) => {
        afterCursor = after;
        return {
          items: [
            makeItem({ id: 't4', threadId: 't', turnIndex: 4, itemIndex: 0 }),
            makeItem({ id: 't5', threadId: 't', turnIndex: 5, itemIndex: 0 }),
          ],
          oldestTurnIndex: 4,
          newestTurnIndex: 5,
          hasMore: true,
          hasMoreOlder: true,
          hasMoreNewer: false,
        };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadNewer();

      expect(afterCursor).toEqual({ turnIndex: 3, itemIndex: 0, itemId: 't3' });
      expect(result).toEqual({
        status: 'loaded',
        insertedBeforeWindow: true,
        insertedRows: true,
      });
      expect(pane.items.map((it) => it.id)).toEqual(['t3', 't4', 't5']);
      expect(pane.newestLoadedTurnIndex).toBe(5);
      expect(pane.hasMoreNewer).toBe(false);
    });

    it('loadNewer preserves the older-history flag when the merged window still starts at the thread head', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 't0', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        ],
        oldestTurnIndex: 0,
        newestTurnIndex: 0,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: true,
      }));
      setBindingMock('ListItemsAfterCursor', async () => ({
        items: [
          makeItem({ id: 't1', threadId: 't', turnIndex: 1, itemIndex: 0 }),
        ],
        oldestTurnIndex: 1,
        newestTurnIndex: 1,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadNewer();

      expect(pane.items.map((it) => it.id)).toEqual(['t0', 't1']);
      expect(pane.oldestLoadedTurnIndex).toBe(0);
      expect(pane.hasMoreHistory).toBe(false);
      expect(pane.hasMoreNewer).toBe(false);
    });

    it('loadNewer preserves the newer-history gap when pruning older head rows', async () => {
      const pane = createThreadPane();
      const initial = Array.from(
        { length: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS },
        (_, index) =>
          makeItem({
            id: `t${index}`,
            threadId: 't',
            turnIndex: index,
            itemIndex: 0,
          }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: true,
      }));
      setBindingMock('ListItemsAfterCursor', async () => ({
        items: [
          makeItem({
            id: `t${ACTIVE_TIMELINE_WINDOW_MAX_ITEMS}`,
            threadId: 't',
            turnIndex: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
        newestTurnIndex: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadNewer();

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasMoreNewer).toBe(true);
    });

    // === virtualizer `shift` signal (pendingTimelineShiftAtHead) ===
    // The prepend/append and the window prune must land in SEPARATE flushes
    // so the `shift` hint can be correct for each end. Coalesced, a
    // head-grow + tail-shrink collapse into one net length change that no
    // single `shift` boolean can represent, and the engine's size store
    // scrambles (the load jank). See the spike notes in
    // docs/architecture/frontend-scroll.md.
    it('loadOlder prepends (head-shift) and prunes the tail in a later flush', async () => {
      const pane = createThreadPane();
      const initial = Array.from(
        { length: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS },
        (_, index) =>
          makeItem({
            id: `t${index}`,
            threadId: 't',
            turnIndex: 1000 + index,
            itemIndex: 0,
          }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 1000,
        newestTurnIndex: 1000 + ACTIVE_TIMELINE_WINDOW_MAX_ITEMS - 1,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: false,
      }));
      let releaseOlder!: (value: unknown) => void;
      setBindingMock(
        'ListItemsBeforeCursor',
        () =>
          new Promise((resolve) => {
            releaseOlder = resolve;
          }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_MAX_ITEMS);
      expect(pane.pendingTimelineShiftAtHead).toBe(false);

      const pending = pane.loadOlder();
      releaseOlder({
        items: [
          makeItem({ id: 'older', threadId: 't', turnIndex: 999, itemIndex: 0 }),
        ],
        oldestTurnIndex: 999,
        hasMore: true,
        hasMoreOlder: true,
      });
      // Resume loadOlder past the fetch await, through the synchronous
      // prepend, up to its first internal `await tick()` — but NOT through
      // the deferred tail-prune (which lives after that tick).
      await Promise.resolve();

      // Flush 1: head-grow applied with shift=true, prune NOT yet — the
      // window has grown past the cap, proving the two are separate flushes.
      expect(pane.pendingTimelineShiftAtHead).toBe(true);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_MAX_ITEMS + 1);
      expect(pane.items[0]?.id).toBe('older');

      await pending;
      await tick();

      // Flush 2: tail-prune ran; head (the freshly prepended rows) kept,
      // newer history now exists, and the one-shot shift hint is cleared.
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0]?.id).toBe('older');
      expect(pane.hasMoreNewer).toBe(true);
      expect(pane.pendingTimelineShiftAtHead).toBe(false);
    });

    it('loadNewer appends (no shift) then head-prunes (head-shift) in a later flush', async () => {
      const pane = createThreadPane();
      const initial = Array.from(
        { length: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS },
        (_, index) =>
          makeItem({
            id: `t${index}`,
            threadId: 't',
            turnIndex: index,
            itemIndex: 0,
          }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: true,
      }));
      setBindingMock('ListItemsAfterCursor', async () => ({
        items: [
          makeItem({
            id: `t${ACTIVE_TIMELINE_WINDOW_MAX_ITEMS}`,
            threadId: 't',
            turnIndex: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
        newestTurnIndex: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));

      // Record (length, shift) after every flush so the two-flush sequence
      // is observable without racing internal awaits.
      const snapshots: Array<{ len: number; shift: boolean }> = [];
      const stop = $effect.root(() => {
        $effect(() => {
          snapshots.push({
            len: pane.items.length,
            shift: pane.pendingTimelineShiftAtHead,
          });
        });
      });
      try {
        flushSync();
        snapshots.length = 0;

        await pane.loadNewer();
        await tick();
        flushSync();
      } finally {
        stop();
      }

      // Tail-grow flush: appended past the cap with shift=false.
      expect(snapshots).toContainEqual({
        len: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS + 1,
        shift: false,
      });
      // Head-prune flush: a SEPARATE flush at the target length carrying
      // shift=true (the engine splices its size store from the front).
      expect(snapshots).toContainEqual({
        len: ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
        shift: true,
      });
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.pendingTimelineShiftAtHead).toBe(false);
    });

    it('loadOlder does not invent a newer-history gap from the older page response', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'tail', threadId: 't', turnIndex: 5, itemIndex: 0 }),
        ],
        oldestTurnIndex: 5,
        newestTurnIndex: 5,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: false,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [
          makeItem({ id: 'older', threadId: 't', turnIndex: 4, itemIndex: 0 }),
        ],
        oldestTurnIndex: 4,
        newestTurnIndex: 4,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual(['older', 'tail']);
      expect(pane.hasMoreNewer).toBe(false);
    });

    it('refreshFromBackend reloads through the bounded slice API instead of the broad recent loader', async () => {
      const pane = createThreadPane();
      const sliceCalls: Array<{ anchor: unknown; budget: unknown }> = [];
      setBindingMock('AutoResumeThread', async () => {});
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId, anchor, budget) => {
          sliceCalls.push({ anchor, budget });
          if (sliceCalls.length === 1) {
            return {
              items: [
                makeItem({
                  id: 'window-ceiling',
                  threadId: 't',
                  turnIndex: 3,
                  itemIndex: 0,
                }),
              ],
              oldestTurnIndex: 3,
              newestTurnIndex: 3,
              hasMore: true,
              hasMoreOlder: true,
              hasMoreNewer: true,
            };
          }
          return {
            items: [
              makeItem({
                id: 'refreshed',
                threadId: 't',
                turnIndex: 4,
                itemIndex: 0,
              }),
            ],
            oldestTurnIndex: 4,
            newestTurnIndex: 4,
            hasMore: true,
            hasMoreOlder: true,
            hasMoreNewer: true,
          };
        },
      );
      setBindingMock('ListRecentThreadItems', async () => {
        throw new Error(
          'refreshFromBackend should not use the broad recent loader',
        );
      });

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.refreshFromBackend();

      expect(sliceCalls).toEqual([
        { anchor: '', budget: SLICE_AROUND_ITEM_BUDGET },
        {
          anchor: 'window-ceiling',
          budget: ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
        },
      ]);
      expect(pane.items.map((it) => it.id)).toEqual(['refreshed']);
    });

    it('prunes older rows when live tail growth exceeds the active window cap', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 't',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      pane.upsertItem(
        makeItem({ id: 't800', threadId: 't', turnIndex: 800, itemIndex: 0 }),
      );

      expect(pane.items).toHaveLength(500);
      expect(pane.items[0].id).toBe('t301');
      expect(pane.items.at(-1)?.id).toBe('t800');
      expect(pane.oldestLoadedTurnIndex).toBe(301);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasMoreNewer).toBe(false);
    });
  });

  describe('subagent live eviction (fold)', () => {
    // Live turns stream subagent child rows into pane memory; once a
    // child settles and nothing can render it (collapsed inline card,
    // suppressed background launch), the pane drops the row and folds
    // its count/preview into the per-anchor registry. SQLite keeps the
    // canonical rows (triage persists before emitting), so expansion
    // re-hydrates through ListSubagentDescendants.
    function launchItem(threadId: string, overrides: Partial<Item> = {}): Item {
      return makeItem({
        id: 'anchor',
        threadId,
        turnIndex: 1,
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Task',
        status: 'running',
        summary: 'Task: investigate',
        ...overrides,
      });
    }

    function childItem(threadId: string, overrides: Partial<Item> = {}): Item {
      return makeItem({
        id: 'child-1',
        threadId,
        turnIndex: 1,
        itemIndex: 1,
        parentId: 'anchor',
        status: 'completed',
        summary: 'ran the build',
        ...overrides,
      });
    }

    async function paneWithAnchor(threadId: string, anchor?: Item) {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'pre', threadId, turnIndex: 0, itemIndex: 0 }),
          anchor ?? launchItem(threadId),
        ],
        oldestTurnIndex: 0,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: threadId }));
      return pane;
    }

    it('evicts a terminal child of a collapsed inline card into the fold', async () => {
      const pane = await paneWithAnchor('fold-evict');

      pane.upsertItem(childItem('fold-evict'));

      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        evictedCount: 1,
        terminalPreview: 'ran the build',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });

      // A replayed upsert for the folded id (transport reconnect echo)
      // must not re-insert the row or double-count it.
      pane.upsertItem(childItem('fold-evict'));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);
    });

    it('keeps a streaming child in memory and evicts it when it settles', async () => {
      const pane = await paneWithAnchor('fold-streaming');

      pane.upsertItem(
        childItem('fold-streaming', { status: 'streaming', summary: 'working...' }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();

      pane.upsertItem(childItem('fold-streaming', { summary: 'finished the build' }));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')?.terminalPreview).toBe(
        'finished the build',
      );
    });

    it('evicts a child settled by a wire status patch (streaming-text settle shape)', async () => {
      const pane = await paneWithAnchor('fold-patch');

      pane.upsertItem(
        childItem('fold-patch', { status: 'streaming', summary: 'partial' }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);

      // Streaming text/thinking rows settle via triage field patches,
      // not upserts — the eviction policy must cover this path too.
      pane.applyItemPatch({
        threadId: 'fold-patch',
        itemId: 'child-1',
        kind: 'assistant_text',
        patch: { status: 'completed', summary: 'full text', updatedAt: 2 },
      });

      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        evictedCount: 1,
        terminalPreview: 'full text',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });
    });

    it('retains settled children while the card is expanded and evicts them on collapse', async () => {
      const pane = await paneWithAnchor('fold-collapse');

      expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(true);
      pane.upsertItem(childItem('fold-collapse', { summary: 'ran tests' }));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();

      expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(false);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        evictedCount: 1,
        terminalPreview: 'ran tests',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });

      // Re-expansion hydrates from SQLite and reclaims the fold — the
      // id is folded XOR loaded, never both.
      setBindingMock('ListSubagentDescendants', async () => [
        childItem('fold-collapse', { summary: 'ran tests' }),
      ]);
      expect(pane.toggleSubagentGroupExpanded('anchor')).toBe(true);
      await expect(pane.ensureSubagentChildren('anchor')).resolves.toBe(true);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
    });

    it('sweeps the settled subtree out when the anchor flips to a suppressed launch', async () => {
      const pane = await paneWithAnchor('fold-backgrounded');
      pane.toggleSubagentGroupExpanded('anchor');
      pane.upsertItem(childItem('fold-backgrounded'));
      // Nested expanded launch with a settled grandchild — both retained
      // while the foreground cards are open.
      pane.upsertItem(
        childItem('fold-backgrounded', {
          id: 'nested',
          itemIndex: 2,
          kind: 'tool_call',
          toolName: 'Task',
          status: 'running',
          summary: 'Task: nested',
        }),
      );
      pane.toggleSubagentGroupExpanded('nested');
      pane.upsertItem(
        childItem('fold-backgrounded', {
          id: 'grandchild',
          itemIndex: 3,
          parentId: 'nested',
          summary: 'deep work',
        }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.items.some((it) => it.id === 'grandchild')).toBe(true);

      // Backgrounding the launch mid-run makes the whole transcript
      // unrenderable (the grouping walk suppresses it) regardless of
      // expansion state. Nested launches stay as fold keys; their
      // settled children fold under their own anchor.
      pane.upsertItem(launchItem('fold-backgrounded', { isBackground: true }));

      expect(pane.items.some((it) => it.id === 'anchor')).toBe(true);
      expect(pane.items.some((it) => it.id === 'nested')).toBe(true);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
      expect(pane.items.some((it) => it.id === 'grandchild')).toBe(false);
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);
      expect(pane.subagentLiveAggregate('nested')?.evictedCount).toBe(1);
    });

    it('folds terminal children of a suppressed anchor while keeping streaming ones', async () => {
      const pane = await paneWithAnchor(
        'fold-suppressed',
        launchItem('fold-suppressed', { isBackground: true }),
      );

      pane.upsertItem(
        childItem('fold-suppressed', { status: 'streaming', summary: 'live' }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);

      pane.upsertItem(
        childItem('fold-suppressed', {
          id: 'child-2',
          itemIndex: 2,
          summary: 'done already',
        }),
      );
      expect(pane.items.some((it) => it.id === 'child-2')).toBe(false);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);
    });

    it('never folds rows whose parent is missing or not a launch', async () => {
      const pane = await paneWithAnchor('fold-flat');

      // Parent not loaded → orphan leaf, stays.
      pane.upsertItem(
        childItem('fold-flat', { id: 'stray', itemIndex: 5, parentId: 'missing' }),
      );
      // Parent loaded but not a launch → flat leaf, stays.
      pane.upsertItem(
        childItem('fold-flat', { id: 'flat-child', itemIndex: 6, parentId: 'pre' }),
      );

      expect(pane.items.some((it) => it.id === 'stray')).toBe(true);
      expect(pane.items.some((it) => it.id === 'flat-child')).toBe(true);
      expect(pane.subagentLiveAggregate('missing')).toBeUndefined();
      expect(pane.subagentLiveAggregate('pre')).toBeUndefined();
    });

    it('drops the fold with its anchor on revert so re-upserts are not swallowed', async () => {
      const pane = await paneWithAnchor('fold-revert');
      pane.upsertItem(childItem('fold-revert'));
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);

      const removed = pane.removeItemsFromTurn(1);

      expect(removed.map((it) => it.id)).toEqual(['anchor']);
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
      // The backend truncate deleted the child's row too; if the same id
      // arrives again (rolled-back revert re-inserts the turn) it must
      // land in pane memory instead of being treated as a folded echo.
      pane.upsertItem(childItem('fold-revert'));
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    it('carries folds through the thread-switch snapshot cache', async () => {
      const pane = createThreadPane();
      const sliceByThread: Record<string, Item[]> = {
        'fold-cache-a': [
          makeItem({ id: 'pre', threadId: 'fold-cache-a', turnIndex: 0, itemIndex: 0 }),
          launchItem('fold-cache-a'),
        ],
        'fold-cache-b': [
          makeItem({ id: 'b-only', threadId: 'fold-cache-b', turnIndex: 0, itemIndex: 0 }),
        ],
      };
      setBindingMock('ListThreadSliceAround', async (threadId: string) => ({
        items: sliceByThread[threadId] ?? [],
        oldestTurnIndex: 0,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));

      await pane.switchThread(makeThread({ id: 'fold-cache-a' }));
      pane.upsertItem(childItem('fold-cache-a'));
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);

      // Folds belong to the thread — they must not leak into the next one.
      await pane.switchThread(makeThread({ id: 'fold-cache-b' }));
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();

      // Warm re-entry restores the fold with the cached window, so the
      // collapsed card's count survives without any live event.
      await pane.switchThread(makeThread({ id: 'fold-cache-a' }));
      expect(pane.subagentLiveAggregate('anchor')).toEqual({
        evictedCount: 1,
        terminalPreview: 'ran the build',
        terminalTurnIndex: 1,
        terminalItemIndex: 1,
      });
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
    });

    it('drops a fold when the window prune drops its anchor', async () => {
      const pane = createThreadPane();
      const initial = [
        launchItem('fold-prune', { turnIndex: 0 }),
        ...Array.from({ length: 799 }, (_, index) =>
          makeItem({
            id: `t${index + 1}`,
            threadId: 'fold-prune',
            turnIndex: index + 1,
            itemIndex: 0,
          }),
        ),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-prune' }));

      pane.upsertItem(childItem('fold-prune', { turnIndex: 0 }));
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);
      // Folded children no longer count toward the window cap.
      expect(pane.items).toHaveLength(800);

      pane.upsertItem(
        makeItem({ id: 't800', threadId: 'fold-prune', turnIndex: 800, itemIndex: 0 }),
      );

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items.some((it) => it.id === 'anchor')).toBe(false);
      // Folds are only meaningful while their anchor row is loaded —
      // the next load of that region decorates anchors from SQLite.
      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
    });

    it('clears folds on re-entry when the outgoing snapshot was too large to cache', async () => {
      const pane = createThreadPane();
      const big = [
        launchItem('fold-reject', { turnIndex: 0 }),
        // Blows MAX_CACHED_SNAPSHOT_CHARS so the switch-away snapshot is
        // rejected and re-entry takes the fresh-state path. (The char
        // budget, not the item cap, keeps the window prune out of play.)
        makeItem({
          id: 'huge',
          threadId: 'fold-reject',
          turnIndex: 1,
          itemIndex: 0,
          summary: 'x'.repeat(MAX_CACHED_SNAPSHOT_CHARS + 1),
        }),
      ];
      setBindingMock('ListThreadSliceAround', async (threadId: string) => ({
        items: threadId === 'fold-reject' ? big : [],
        oldestTurnIndex: 0,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-reject' }));
      pane.upsertItem(childItem('fold-reject', { turnIndex: 0 }));
      expect(pane.subagentLiveAggregate('anchor')?.evictedCount).toBe(1);

      await pane.switchThread(makeThread({ id: 'fold-reject-other' }));
      await pane.switchThread(makeThread({ id: 'fold-reject' }));

      expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
      // A stale fold would swallow this re-streamed row outright; the
      // fresh-state clear lets it land (streaming rows always stay).
      pane.upsertItem(
        childItem('fold-reject', {
          turnIndex: 0,
          status: 'streaming',
          summary: 'live again',
        }),
      );
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    it('keeps unrelated exhausted-hydration markers across evictions', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          launchItem('fold-exhaust', { id: 'anchor-a', turnIndex: 0 }),
          launchItem('fold-exhaust', { id: 'anchor-b', turnIndex: 1 }),
        ],
        oldestTurnIndex: 0,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      let listCalls = 0;
      setBindingMock('ListSubagentDescendants', async () => {
        listCalls += 1;
        return [];
      });
      await pane.switchThread(makeThread({ id: 'fold-exhaust' }));

      // Anchor A fetches nothing → marked exhausted; repeats skip the wire.
      await pane.ensureSubagentChildren('anchor-a');
      await pane.ensureSubagentChildren('anchor-a');
      expect(listCalls).toBe(1);

      // Evicting a child of anchor B clears only B's marker. A wholesale
      // clear here would re-arm A into a refetch per eviction.
      pane.upsertItem(
        childItem('fold-exhaust', { parentId: 'anchor-b', turnIndex: 1 }),
      );
      expect(pane.subagentLiveAggregate('anchor-b')?.evictedCount).toBe(1);
      await pane.ensureSubagentChildren('anchor-a');
      expect(listCalls).toBe(1);

      // B's own transcript changed, so its fetch goes through.
      await pane.ensureSubagentChildren('anchor-b');
      expect(listCalls).toBe(2);
    });

    it('defers the recent-window prune while a turn is active and runs it on settle', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'fold-defer',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-defer' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });

      // Mid-turn growth past the cap: a head-drop here repaints the
      // visible timeline (incident 2026-06-10), so the prune waits.
      pane.upsertItem(
        makeItem({ id: 't800', threadId: 'fold-defer', turnIndex: 800, itemIndex: 0 }),
      );
      expect(pane.items).toHaveLength(801);

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t301');
      expect(pane.hasMoreHistory).toBe(true);
    });

    it('records the settle prune as pending and runs it inside the transaction on retry', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'prune-rebase',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-rebase' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'prune-rebase',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      let itemCountDuringTransaction = 0;
      const preserveTimelineWindowAnchor = vi.fn((
        operation: TimelineWindowAnchorOperation,
      ) => {
        itemCountDuringTransaction = pane.items.length;
        expect(operation.keepsItem('t300')).toBe(false);
        expect(operation.keepsItem('t301')).toBe(true);
        operation.run();
        return true;
      });
      pane.attachScrollController(
        stubScrollController({ preserveTimelineWindowAnchor }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });

      // Wire settle is not visual quiet: a pane with a mounted timeline
      // (the controller offers the anchor transaction) records the prune
      // as pending for the quiet scheduler instead of repainting the
      // head-drop into the reveal drain's glide.
      expect(preserveTimelineWindowAnchor).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(801);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.retryDeferredRecentWindowPrune();

      expect(preserveTimelineWindowAnchor).toHaveBeenCalledTimes(1);
      expect(itemCountDuringTransaction).toBe(801);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t301');
      expect(pane.pendingTimelineShiftAtHead).toBe(false);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('a retry landing while the next turn already streams keeps the prune pending', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'prune-next-turn',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-next-turn' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'prune-next-turn',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      const preserveTimelineWindowAnchor = vi.fn((
        operation: TimelineWindowAnchorOperation,
      ) => {
        operation.run();
        return true;
      });
      pane.attachScrollController(
        stubScrollController({ preserveTimelineWindowAnchor }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      // The user fires the next turn before quiet ever arrives: the
      // retry must stand down (mid-stream head-drops are banned —
      // incident 2026-06-10) but keep the debt recorded for the next
      // quiet window.
      pane.setActiveTurn({ turnId: 'turn-801', turnIndex: 801, startedAt: 3 });
      pane.retryDeferredRecentWindowPrune();

      expect(preserveTimelineWindowAnchor).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(801);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.settleTurn({
        turnId: 'turn-801',
        turnIndex: 801,
        startedAt: 3,
        completedAt: 4,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      pane.retryDeferredRecentWindowPrune();

      expect(preserveTimelineWindowAnchor).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('defers a recent-window prune when the scroll-controller cannot preserve the visible anchor', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'prune-veto',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-veto' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'prune-veto',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      const preserveTimelineWindowAnchor = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ preserveTimelineWindowAnchor }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      // The quiet retry runs into the anchor veto: the reader is parked
      // on a row the prune would drop, so the window stays and the debt
      // stays recorded.
      pane.retryDeferredRecentWindowPrune();

      expect(preserveTimelineWindowAnchor).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(801);
      expect(pane.items[0].id).toBe('t0');
      expect(pane.hasMoreHistory).toBe(false);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      const retryPreserve = vi.fn((
        operation: TimelineWindowAnchorOperation,
      ) => {
        operation.run();
        return true;
      });
      pane.attachScrollController(
        stubScrollController({ preserveTimelineWindowAnchor: retryPreserve }),
      );

      pane.retryDeferredRecentWindowPrune();

      expect(retryPreserve).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t301');
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('does not treat a prune as applied unless the scroll-controller runs it', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'prune-missing-run',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'prune-missing-run' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'prune-missing-run',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      const preserveTimelineWindowAnchor = vi.fn(() => true);
      pane.attachScrollController(
        stubScrollController({ preserveTimelineWindowAnchor }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.retryDeferredRecentWindowPrune();

      expect(preserveTimelineWindowAnchor).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(801);
      expect(pane.items[0].id).toBe('t0');
      expect(pane.hasMoreHistory).toBe(false);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);
    });

    it('prunes mid-turn anyway once the hard ceiling is exceeded', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'fold-ceiling',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-ceiling' }));
      pane.setActiveTurn({ turnId: 'turn-x', turnIndex: 800, startedAt: 1 });

      // Grow to exactly the ceiling — still deferred.
      pane.upsertItems(
        Array.from({ length: 800 }, (_, index) =>
          makeItem({
            id: `t${800 + index}`,
            threadId: 'fold-ceiling',
            turnIndex: 800 + index,
            itemIndex: 0,
          }),
        ),
      );
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS);

      // One more row breaches the ceiling: memory wins over the repaint.
      pane.upsertItem(
        makeItem({ id: 't1600', threadId: 'fold-ceiling', turnIndex: 1600, itemIndex: 0 }),
      );
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items.at(-1)?.id).toBe('t1600');
      expect(pane.hasMoreHistory).toBe(true);
    });

    it('forces the hard-ceiling prune when visible-anchor preservation vetoes it', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'fold-ceiling-veto',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'fold-ceiling-veto' }));
      pane.setActiveTurn({ turnId: 'turn-x', turnIndex: 800, startedAt: 1 });

      const preserveTimelineWindowAnchor = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ preserveTimelineWindowAnchor }),
      );

      pane.upsertItems(
        Array.from({ length: 800 }, (_, index) =>
          makeItem({
            id: `t${800 + index}`,
            threadId: 'fold-ceiling-veto',
            turnIndex: 800 + index,
            itemIndex: 0,
          }),
        ),
      );
      expect(preserveTimelineWindowAnchor).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS);

      pane.upsertItem(
        makeItem({
          id: 't1600',
          threadId: 'fold-ceiling-veto',
          turnIndex: 1600,
          itemIndex: 0,
        }),
      );

      expect(preserveTimelineWindowAnchor).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t1101');
      expect(pane.items.at(-1)?.id).toBe('t1600');
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });

    it('forces the hard-ceiling prune after a settled deferred prune keeps growing', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 'settled-ceiling-veto',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 'settled-ceiling-veto' }));
      pane.setActiveTurn({ turnId: 'turn-800', turnIndex: 800, startedAt: 1 });
      pane.upsertItem(
        makeItem({
          id: 't800',
          threadId: 'settled-ceiling-veto',
          turnIndex: 800,
          itemIndex: 0,
        }),
      );

      const preserveTimelineWindowAnchor = vi.fn(() => false);
      pane.attachScrollController(
        stubScrollController({ preserveTimelineWindowAnchor }),
      );

      pane.settleTurn({
        turnId: 'turn-800',
        turnIndex: 800,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      expect(preserveTimelineWindowAnchor).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(801);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.upsertItem(
        makeItem({
          id: 't801',
          threadId: 'settled-ceiling-veto',
          turnIndex: 801,
          itemIndex: 0,
        }),
      );
      expect(preserveTimelineWindowAnchor).not.toHaveBeenCalled();
      expect(pane.items).toHaveLength(802);
      expect(pane.hasDeferredRecentWindowPrune).toBe(true);

      pane.upsertItems(
        Array.from({ length: 799 }, (_, index) =>
          makeItem({
            id: `t${802 + index}`,
            threadId: 'settled-ceiling-veto',
            turnIndex: 802 + index,
            itemIndex: 0,
          }),
        ),
      );

      expect(preserveTimelineWindowAnchor).toHaveBeenCalledTimes(1);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0].id).toBe('t1101');
      expect(pane.items.at(-1)?.id).toBe('t1600');
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasDeferredRecentWindowPrune).toBe(false);
    });
  });

  describe('switchThread cache + initial load', () => {
    it('paints cached items synchronously on re-entry without waiting for the network', async () => {
      const pane = createThreadPane();
      const items = [
        makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        makeItem({ id: 'b', threadId: 't', turnIndex: 1, itemIndex: 0 }),
      ];
      // Initial switch: cache is empty, the load returns the items.
      setBindingMock('ListThreadSliceAround', async () => ({
        items,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);

      // Switch away — outgoing thread snapshot lands in the cache.
      await pane.switchThread(makeThread({ id: 'other' }));

      // Make the load hang so the cache is the only painter on re-entry.
      // (Cache hit short-circuits the load; this hang would only apply
      // if the cache lookup failed.)
      setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));

      // Kick off the re-entry but DON'T await — assert items are
      // already painted from cache.
      const switching = pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);
      expect(pane.oldestLoadedTurnIndex).toBe(0);
      // Don't actually await — the load mock hangs forever; cache hit
      // means we never wait on it anyway.
      void switching;
    });

    it('skips the cache write when the outgoing pane is empty', async () => {
      const pane = createThreadPane();
      // Empty thread — first switch yields no items.
      await pane.switchThread(makeThread({ id: 'empty' }));
      expect(pane.items).toEqual([]);

      // Switch away to a thread with items.
      const other = [
        makeItem({ id: 'x', threadId: 'other', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: other,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 'other' }));

      // Make the load hang. With no cached items the empty re-entry
      // would have to wait on the network — we assert items stays []
      // before it resolves.
      setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));

      // Re-enter the empty thread. No cached items → items stays [].
      const switching = pane.switchThread(makeThread({ id: 'empty' }));
      // Yield once for the load's microtask.
      await Promise.resolve();
      expect(pane.items).toEqual([]);
      // Don't actually await — the load hangs forever.
      void switching;
    });

    it('initial-load result preserves items appended via streamed events during the load', async () => {
      const pane = createThreadPane();
      // Stage: load hangs so a streamed upsert can land before its
      // result.
      let releaseLoad!: (value: unknown) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () =>
          new Promise((resolve) => {
            releaseLoad = resolve;
          }),
      );

      const switching = pane.switchThread(makeThread({ id: 't' }));
      // Drain microtasks so the switch sets up.
      await Promise.resolve();
      await Promise.resolve();

      // Streamed event arrives mid-load — upsert into the same items
      // array. mergeMissingItemsById in the load callback must keep it.
      pane.upsertItem(
        makeItem({
          id: 'streamed',
          threadId: 't',
          turnIndex: 1,
          itemIndex: 0,
        }),
      );
      expect(pane.items.map((it) => it.id)).toEqual(['streamed']);

      // Load returns the canonical view. Triage's persist-then-emit
      // contract means the load SHOULD include 'streamed'; simulate
      // that.
      releaseLoad({
        items: [
          makeItem({ id: 'load', threadId: 't', turnIndex: 0, itemIndex: 0 }),
          makeItem({
            id: 'streamed',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: 0,
        hasMore: false,
      });
      await switching;

      // Both items survive; no duplicates from mergeMissingItemsById.
      const ids = pane.items.map((it) => it.id);
      expect(ids).toEqual(['load', 'streamed']);
    });

    it('a same-thread re-switch invalidates the in-flight load result', async () => {
      const pane = createThreadPane();
      // First switch: load hangs.
      let releaseFirstLoad!: (value: unknown) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () =>
          new Promise((resolve) => {
            releaseFirstLoad = resolve;
          }),
      );

      const firstSwitch = pane.switchThread(makeThread({ id: 't' }));
      // The item leg consults the durable replica before it issues the
      // RPC, so the hanging mock is reached a few microtasks in rather
      // than on the switch's own tick.
      await flushMicrotasks();

      // Second switch comes in before the first resolves. Backend
      // returns a fresh canonical view.
      const secondItems = [
        makeItem({ id: 'second', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: secondItems,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      const secondSwitch = pane.switchThread(makeThread({ id: 't' }));
      await secondSwitch;

      expect(pane.items.map((it) => it.id)).toEqual(['second']);

      // Now release the first switch's load with stale data using
      // an id DISJOINT from `secondItems`. Without the gen-guard,
      // mergeMissingItemsById would happily slot 'stale-only' in next
      // to 'second' (no id collision). The assertion below confirms
      // the guard short-circuits the callback before the merge runs.
      releaseFirstLoad({
        items: [makeItem({ id: 'stale-only', threadId: 't', turnIndex: 99 })],
        oldestTurnIndex: 99,
        hasMore: true,
      });
      await firstSwitch;

      expect(pane.items.map((it) => it.id)).toEqual(['second']);
    });

    it('forces a fresh fetch on same-thread re-switch (revert-then-switch UX)', async () => {
      const pane = createThreadPane();
      // First load returns [a, b].
      const initialItems = [
        makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        makeItem({ id: 'b', threadId: 't', turnIndex: 1, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initialItems,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);

      // Revert removes 'b'. Same-thread re-switch should NOT cache the
      // pre-revert view and read it back — that would flash 'b' before
      // the load corrects. Stage the load to return only 'a'.
      const revertedItems = [
        makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: revertedItems,
        oldestTurnIndex: 0,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));

      // 'b' must never appear after the re-switch resolves. The
      // pre-revert items would be the cached snapshot if the
      // sameThreadReswitch guard were missing.
      expect(pane.items.map((it) => it.id)).toEqual(['a']);
    });

    it('bumps switchGeneration on every switchThread (including same-thread re-switch)', async () => {
      // A forced in-place reload calls pane.switchThread(currentThread).
      // pane.threadId does not change on that path, so MessageTimeline's
      // restore $effect.pre would miss the event if it keyed only on
      // pane.threadId. Exposing switchGeneration gives the timeline a
      // second discriminator so the reset path (restoredThreadId = null,
      // armWarmup, armRestoreSnap) still fires and the viewport restores
      // to bottom instead of sticking at scrollTop=0 with the "Load older
      // messages" banner visible. This test locks in the contract
      // MessageTimeline depends on; the timeline-side behavior is
      // covered by the integration test for revert flow.
      const pane = createThreadPane();
      const initial = pane.switchGeneration;

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      const afterFirst = pane.switchGeneration;
      expect(afterFirst).toBeGreaterThan(initial);

      // Different thread — generation bumps as expected.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      const afterSecond = pane.switchGeneration;
      expect(afterSecond).toBeGreaterThan(afterFirst);

      // Same-thread re-switch (the revert path). Without the bump,
      // MessageTimeline's restore reset path would never fire.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      const afterReswitch = pane.switchGeneration;
      expect(afterReswitch).toBeGreaterThan(afterSecond);
    });

    it('switchGeneration getter is reactive: $effect re-fires on same-thread re-switch', async () => {
      // Imperative reads of `pane.switchGeneration` between awaits would
      // pass even if the underlying `let` weren't `$state` — they just
      // observe whatever value the getter happens to return. But
      // MessageTimeline's `$effect.pre` consumes the getter inside a
      // reactive scope; if the backing storage isn't `$state`, the
      // dependency never registers and the effect never re-fires on
      // same-thread re-switch. Symptom: revert still lands at the very
      // top with "Load older messages" visible, exactly the bug this
      // fix targets. This test mounts a real $effect on the getter and
      // asserts the effect re-fires after each bump.
      const pane = createThreadPane();
      const observed: number[] = [];

      const stop = $effect.root(() => {
        $effect(() => {
          observed.push(pane.switchGeneration);
        });
      });

      try {
        flushSync();
        const baseline = observed.length;

        await pane.switchThread(makeThread({ id: 'thread-a' }));
        flushSync();
        expect(observed.length).toBeGreaterThan(baseline);

        // Same-thread re-switch — the load-bearing case.
        await pane.switchThread(makeThread({ id: 'thread-a' }));
        flushSync();
        // Must increase again: a non-$state getter would NOT re-fire the
        // effect (Svelte 5 reactivity requires $state for tracking).
        expect(observed.at(-1)).toBeGreaterThan(observed[baseline] ?? -1);
      } finally {
        stop();
      }
    });

    it('mergeMissingItemsById preserves the existing item reference for unchanged rows', async () => {
      const pane = createThreadPane();
      // Initial load returns [a]; streaming upserts a fresh copy of a
      // mid-load so we can assert the load's merge keeps the
      // upserted reference rather than overwriting it.
      let releaseLoad!: (value: unknown) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () =>
          new Promise((resolve) => {
            releaseLoad = resolve;
          }),
      );

      const switching = pane.switchThread(makeThread({ id: 't' }));
      // Drain microtasks so the switch sets up.
      await Promise.resolve();
      await Promise.resolve();

      // Streamed upsert lands BEFORE the load resolves, seeding `a`.
      pane.upsertItem(
        makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      );
      const aRefBeforeLoad = pane.items[0];
      expect(aRefBeforeLoad.id).toBe('a');

      // Load returns [a (different shell), b]. Reference-preservation
      // contract says we keep the upserted `a` ref and only allocate
      // `b`.
      releaseLoad({
        items: [
          makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
          makeItem({ id: 'b', threadId: 't', turnIndex: 1, itemIndex: 0 }),
        ],
        oldestTurnIndex: 0,
        hasMore: false,
      });
      await switching;

      // a's reference survives unchanged; b is fresh.
      expect(pane.items[0]).toBe(aRefBeforeLoad);
      expect(pane.items.map((it) => it.id)).toEqual(['a', 'b']);
    });

    it('does not cache the outgoing pane while it is still loading', async () => {
      const pane = createThreadPane();
      // First switch hangs forever — outgoing items never resolve.
      setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));
      void pane.switchThread(makeThread({ id: 'first' }));
      // Yield so the load gets to the top of switchThread.
      await Promise.resolve();
      expect(pane.loading).toBe(true);

      // Switch to a fresh thread. The outgoing pane is loading so the
      // cache write must be skipped — otherwise we'd snapshot an
      // empty in-flight pane and a future switch back would paint
      // empty even though the real thread has content.
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 'second' }));

      const cacheModule = await import('./threadItemCache');
      expect(cacheModule.threadItemCache.get('first')).toBeNull();
    });

    it('runs all backend fetches in parallel rather than serialising them', async () => {
      const pane = createThreadPane();
      // Each mock records its own start timestamp on entry. With
      // parallelisation, all five start within a microtask of each
      // other; with the legacy serialised flow, ListRecentTurns would
      // wait for ListThreadSliceAround to resolve.
      const startedAt: Record<string, number> = {};
      let nextSlot = 0;
      const stamp = (name: string) => () => {
        startedAt[name] = nextSlot++;
        return new Promise(() => {}); // hang forever
      };
      setBindingMock('SwitchThread', stamp('SwitchThread'));
      setBindingMock('GetThreadLiveState', stamp('GetThreadLiveState'));
      setBindingMock('ListThreadSliceAround', stamp('ListThreadSliceAround'));
      setBindingMock('ListRecentTurns', stamp('ListRecentTurns'));

      // Don't await — every mock hangs intentionally.
      void pane.switchThread(makeThread({ id: 't' }));

      // Yield enough microtasks for all four Promise constructors to
      // run (each one assigns its slot synchronously inside the
      // `() => new Promise(() => {})` body).
      for (let i = 0; i < 8; i++) await Promise.resolve();

      // All four must have started. The exact ordering between them
      // is non-deterministic by design; we only assert that no fetch
      // is missing — which it would be under serialisation.
      expect(Object.keys(startedAt).sort()).toEqual([
        'GetThreadLiveState',
        'ListRecentTurns',
        'ListThreadSliceAround',
        'SwitchThread',
      ]);
    });

    it('does not call ListRecentThreadItems on switchThread (single-load contract)', async () => {
      // Pin the no-Phase-2 invariant: if the wider-window probe ever
      // creeps back into the switch path, the residual flicker
      // (wide prepend → applyJump fight with the controller's
      // sync-pin) returns.
      const calls: string[] = [];
      setBindingMock('ListThreadSliceAround', async () => {
        calls.push('ListThreadSliceAround');
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      setBindingMock('ListRecentThreadItems', async () => {
        calls.push('ListRecentThreadItems');
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));
      expect(calls).toEqual(['ListThreadSliceAround']);
    });

    it('uses the scroll snapshot anchor when calling ListThreadSliceAround', async () => {
      const { setThreadScrollSnapshot, clearThreadScrollSnapshotsForTest } =
        await import('../utils/threadScrollSnapshots');
      clearThreadScrollSnapshotsForTest();
      setThreadScrollSnapshot('t', {
        kind: 'anchor',
        itemId: 'wanted',
        offsetTop: -42,
      });

      const pane = createThreadPane();
      let observedAnchor = '';
      setBindingMock(
        'ListThreadSliceAround',
        async (threadID: unknown, anchorID: unknown, _count: unknown) => {
          observedAnchor = String(anchorID ?? '');
          void threadID;
          return { items: [], oldestTurnIndex: -1, hasMore: false };
        },
      );
      await pane.switchThread(makeThread({ id: 't' }));
      expect(observedAnchor).toBe('wanted');
      clearThreadScrollSnapshotsForTest();
    });

    it('passes empty anchor when the scroll snapshot is the bottom kind', async () => {
      const { setThreadScrollSnapshot, clearThreadScrollSnapshotsForTest } =
        await import('../utils/threadScrollSnapshots');
      clearThreadScrollSnapshotsForTest();
      setThreadScrollSnapshot('t', { kind: 'bottom' });

      const pane = createThreadPane();
      let observedAnchor = 'unset';
      setBindingMock(
        'ListThreadSliceAround',
        async (threadID: unknown, anchorID: unknown, _count: unknown) => {
          observedAnchor = String(anchorID ?? '');
          void threadID;
          return { items: [], oldestTurnIndex: -1, hasMore: false };
        },
      );
      await pane.switchThread(makeThread({ id: 't' }));
      expect(observedAnchor).toBe('');
      clearThreadScrollSnapshotsForTest();
    });

    it('cache hit completes loading=false even when SwitchThread fails', async () => {
      const pane = createThreadPane();
      const items = [makeItem({ id: 'cached', threadId: 't', turnIndex: 0 })];
      setBindingMock('ListThreadSliceAround', async () => ({
        items,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      await pane.switchThread(makeThread({ id: 'other' }));

      // SwitchThread fails — toast fires but the rest of the load
      // continues. loading must still flip false at the end.
      setBindingMock('SwitchThread', async () => {
        throw new Error('mock backend down');
      });
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.loading).toBe(false);
      // Items still surface from the cache.
      expect(pane.items.map((it) => it.id)).toEqual(['cached']);
    });

    it('a stale-gen rejection of the initial load does not blank items or stamp generalError', async () => {
      // Pins withGenGuard's contract: when capturedGen !== switchGeneration,
      // onError must NOT run. A regression that flipped the gen-check
      // order would let a slow load from switch #1 write generalError
      // and items=[] against the pane that switch #2 already populated.
      const pane = createThreadPane();
      // First switch: load hangs forever (a Promise that will be
      // rejected later).
      let rejectFirstLoad!: (err: Error) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () =>
          new Promise((_, reject) => {
            rejectFirstLoad = reject;
          }),
      );
      const firstSwitch = pane.switchThread(makeThread({ id: 'first' }));
      // See above: the replica read precedes the RPC, so let the leg
      // reach the hanging mock before replacing it.
      await flushMicrotasks();

      // Second switch supersedes; populates with real data.
      const secondItems = [
        makeItem({
          id: 'live',
          threadId: 'second',
          turnIndex: 0,
          itemIndex: 0,
        }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: secondItems,
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 'second' }));
      expect(pane.items.map((it) => it.id)).toEqual(['live']);
      expect(pane.generalError).toBeNull();

      // Now reject the first switch's load. Stale-gen guard MUST
      // suppress the onError side effects.
      rejectFirstLoad(new Error('initial load backend down'));
      await firstSwitch;

      // Items unchanged — second switch's data still painted.
      expect(pane.items.map((it) => it.id)).toEqual(['live']);
      // generalError still null — stale onError did not stamp.
      expect(pane.generalError).toBeNull();
    });
  });

  describe('switchThread spinner-flash gate', () => {
    it('cache hit never flips showLoadingSpinner true even past the threshold', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const pane = createThreadPane();
        const items = [
          makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        ];
        setBindingMock('ListThreadSliceAround', async () => ({
          items,
          oldestTurnIndex: 0,
          hasMore: false,
        }));
        await pane.switchThread(makeThread({ id: 't' }));
        await pane.switchThread(makeThread({ id: 'other' }));

        // Re-enter — initial load hangs so loading=true persists.
        setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));
        void pane.switchThread(makeThread({ id: 't' }));
        await Promise.resolve();
        // Items painted from cache.
        expect(pane.items.length).toBe(1);

        // Advance well past the 100ms threshold.
        vi.advanceTimersByTime(500);
        await Promise.resolve();
        // Spinner stayed false because items.length > 0.
        expect(pane.showLoadingSpinner).toBe(false);
      } finally {
        vi.useRealTimers();
      }
    });

    it('above-threshold empty load shows the spinner', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const pane = createThreadPane();
        // Initial load hangs so items stays empty and loading stays true.
        setBindingMock('ListThreadSliceAround', () => new Promise(() => {}));
        void pane.switchThread(makeThread({ id: 't' }));
        await Promise.resolve();
        expect(pane.showLoadingSpinner).toBe(false);

        vi.advanceTimersByTime(150);
        await Promise.resolve();
        expect(pane.showLoadingSpinner).toBe(true);
      } finally {
        vi.useRealTimers();
      }
    });

    it('sub-threshold load with items present never shows the spinner', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const pane = createThreadPane();
        const items = [
          makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        ];
        setBindingMock('ListThreadSliceAround', async () => ({
          items,
          oldestTurnIndex: 0,
          hasMore: false,
        }));
        const switching = pane.switchThread(makeThread({ id: 't' }));
        // Resolve fully before threshold elapses.
        await switching;

        // Threshold timer was already cleared when loading flipped
        // false; advancing time should not re-trigger anything.
        vi.advanceTimersByTime(500);
        await Promise.resolve();
        expect(pane.showLoadingSpinner).toBe(false);
        expect(pane.loading).toBe(false);
        expect(pane.items.length).toBe(1);
      } finally {
        vi.useRealTimers();
      }
    });
  });

  // --- Turn-lifecycle pane state (Wave 2) -----------------------------------

  it('setActiveTurn populates activeTurn and flips isTurnActive on', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread());
    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });

    expect(getActiveTurn(pane.threadId)).toEqual({
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
    });
    expect(getActiveTurn(pane.threadId) !== null).toBe(true);
  });

  it('setActiveTurn is idempotent by turnId — preserves startedAt on re-emit', async () => {
    // A Claude re-init / interrupt can re-send EventTurnStart for the same
    // (thread, turn). The pane must not rewind startedAt — otherwise the
    // working indicator's elapsed-seconds counter would jump backward each
    // time the provider re-initialises.
    const pane = createThreadPane();
    await pane.switchThread(makeThread());
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 9999 });
    expect(getActiveTurn(pane.threadId)?.startedAt).toBe(1000);
  });

  it('settleTurn clears activeTurn and writes latestSettledTurn', () => {
    const pane = createThreadPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });

    pane.settleTurn({
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
      assistantMessageId: 'text:0:3',
      tokenUsage: { inputTokens: 100, outputTokens: 50 },
      aborted: false,
      errorMessage: '',
    });

    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
    expect(pane.latestSettledTurn).toEqual({
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
      assistantMessageId: 'text:0:3',
      tokenUsage: { inputTokens: 100, outputTokens: 50 },
      aborted: false,
      errorMessage: '',
    });
  });

  it('clearTurnState resets both slots without rehydrating', () => {
    const pane = createThreadPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    pane.settleTurn({
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
    });
    expect(pane.latestSettledTurn).not.toBeNull();

    pane.clearTurnState();
    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(pane.latestSettledTurn).toBeNull();
  });

  it('switchThread rehydrates latestSettledTurn from the most recent completed row', async () => {
    setBindingMock('ListRecentTurns', async () => [
      {
        turnId: 'turn-1',
        threadId: 'thread-a',
        turnIndex: 1,
        startedAt: 1000,
        completedAt: 2000,
        stopReason: 'end_turn',
        assistantMessageId: 'text:1:4',
        tokenUsageJson: JSON.stringify({
          inputTokens: 150,
          outputTokens: 75,
          totalCostUsd: 0.012,
        }),
      },
    ]);

    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.latestSettledTurn).toEqual({
      turnId: 'turn-1',
      turnIndex: 1,
      startedAt: 1000,
      completedAt: 2000,
      stopReason: 'end_turn',
      assistantMessageId: 'text:1:4',
      tokenUsage: {
        inputTokens: 150,
        outputTokens: 75,
        totalCostUsd: 0.012,
      },
      aborted: false,
      errorMessage: '',
    });
    // activeTurn stays null even though rehydration ran — invariant 22.
    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });

  it('switchThread does NOT promote an in-flight historical turn to activeTurn', async () => {
    // Most-recent row has completedAt=null → a crashed / interrupted
    // turn that was never settled. The frontend MUST leave activeTurn
    // alone; only a fresh `provider:turn_started` push can light up the
    // working indicator (invariant 22).
    setBindingMock('ListRecentTurns', async () => [
      {
        turnId: 'turn-crashed',
        threadId: 'thread-a',
        turnIndex: 1,
        startedAt: 1000,
        completedAt: null,
      },
      {
        turnId: 'turn-settled',
        threadId: 'thread-a',
        turnIndex: 0,
        startedAt: 500,
        completedAt: 900,
        stopReason: 'end_turn',
        assistantMessageId: 'text:0:2',
        tokenUsageJson: '',
      },
    ]);

    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    // Not lit up.
    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
    // But the prior settled turn IS rehydrated for read-state and trace/debug
    // consumers.
    expect(pane.latestSettledTurn?.turnId).toBe('turn-settled');
  });

  it('switchThread tolerates malformed tokenUsageJson without crashing', async () => {
    setBindingMock('ListRecentTurns', async () => [
      {
        turnId: 'turn-1',
        threadId: 'thread-a',
        turnIndex: 0,
        startedAt: 1,
        completedAt: 2,
        stopReason: 'end_turn',
        assistantMessageId: '',
        tokenUsageJson: '{not valid json',
      },
    ]);

    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.latestSettledTurn?.tokenUsage).toBeNull();
  });

  it('switchThread tolerates a ListRecentTurns rejection', async () => {
    setBindingMock('ListRecentTurns', async () => {
      throw new Error('rpc down');
    });

    const pane = createThreadPane();
    // switchThread swallows the rehydration error so the thread still
    // renders its items.
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.latestSettledTurn).toBeNull();
    expect(getActiveTurn(pane.threadId)).toBeNull();
    // Items path was not touched.
    expect(pane.thread?.id).toBe('thread-a');
  });

  it('switchThread clears turn state between threads', async () => {
    const pane = createThreadPane();
    pane.setActiveTurn({ turnId: 'turn-a', turnIndex: 0, startedAt: 1 });
    pane.settleTurn({
      turnId: 'turn-a-prev',
      turnIndex: -1,
      startedAt: 0,
      completedAt: 0,
      stopReason: 'end_turn',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
    });

    // Switching to a new thread with no recent turns must clear both
    // slots so the prior thread's state doesn't bleed over.
    await pane.switchThread(makeThread({ id: 'thread-b' }));

    expect(getActiveTurn(pane.threadId)).toBeNull();
    expect(pane.latestSettledTurn).toBeNull();
  });

  describe('applyItemPatch', () => {
    it('applies status-only patch while preserving all other fields', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      const original = makeItem({
        id: 'text:0:0',
        threadId: 'thread-patch',
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'hello world, this is a long response',
        meta: '{"pathRefs":[]}',
        updatedAt: 1000,
      });
      pane.upsertItem(original);
      expect(pane.items).toHaveLength(1);

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: { status: 'completed', updatedAt: 2000 },
      });

      const patched = pane.items[0];
      expect(patched.status).toBe('completed');
      expect(patched.updatedAt).toBe(2000);
      expect(patched.summary).toBe('hello world, this is a long response');
      expect(patched.meta).toBe('{"pathRefs":[]}');
      expect(patched.kind).toBe('assistant_text');
      expect(patched.role).toBe('assistant');
    });

    it('is a no-op for an unknown item id', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(makeItem({ id: 'text:0:0', threadId: 'thread-patch' }));
      const before = pane.items[0];

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'nonexistent',
        kind: 'assistant_text',
        patch: { status: 'completed' },
      });

      expect(pane.items[0]).toBe(before);
    });

    it('is a no-op when patch changes nothing', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      const original = makeItem({
        id: 'text:0:0',
        threadId: 'thread-patch',
        status: 'completed',
        summary: 'hello',
      });
      pane.upsertItem(original);
      const before = pane.items[0];

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: { status: 'completed', summary: 'hello' },
      });

      expect(pane.items[0]).toBe(before);
    });

    it('ignores patches for a different thread', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-a' }));
      pane.upsertItem(
        makeItem({ id: 'text:0:0', threadId: 'thread-a', status: 'streaming' }),
      );

      pane.applyItemPatch({
        threadId: 'thread-b',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: { status: 'completed' },
      });

      expect(pane.items[0].status).toBe('streaming');
    });

    it('applies meta and decision patch fields', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'tool:0:0',
          threadId: 'thread-patch',
          kind: 'tool_call',
          meta: '{"toolName":"Bash"}',
        }),
      );

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'tool:0:0',
        kind: 'tool_call',
        patch: {
          meta: '{"toolName":"Bash","task_id":"t1"}',
          decision: 'approved',
        },
      });

      expect(pane.items[0].meta).toBe('{"toolName":"Bash","task_id":"t1"}');
      expect(pane.items[0].decision).toBe('approved');
    });

    it('reveals the full extending summary when status flips to completed mid-smooth', async () => {
      // A completed-status patch is intentionally NOT in the snap set:
      // the smoother is left running so the trailing characters reveal
      // naturally instead of snapping. The patch's summary, if it
      // extends what the smoother has already received, is appended as
      // a delta — and the patch's `summary` field is not written
      // directly to items[index] because the smoother now owns the
      // visible summary. Once the stream is fully revealed, the
      // smoother disposes itself. Without that handoff the row would
      // be stuck at the mid-stream cursor when the smoother eventually
      // ticked the auto-cleanup branch.
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          threadId: 'thread-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'initial',
          updatedAt: 1,
        }),
      );

      pane.applyItemDelta({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' middle',
        updatedAt: 2,
      });

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: {
          status: 'completed',
          summary: 'initial middle and the final tail',
          updatedAt: 3,
        },
      });

      pane.__flushItemSmoothersForTest();

      expect(pane.items[0].summary).toBe('initial middle and the final tail');
      expect(pane.items[0].status).toBe('completed');
      expect(pane.items[0].updatedAt).toBe(3);
    });

    it('snaps and lets the patch summary win on a non-extending completion overwrite', async () => {
      // When `completed` arrives with a summary that does NOT extend
      // what the smoother already received (a backwards correction or
      // a wholesale rewrite), the smoother snaps so its in-flight
      // reveal doesn't trample the patch, and the patch summary is
      // written through to items[index] as the final wire shape.
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          threadId: 'thread-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'initial received',
          updatedAt: 1,
        }),
      );

      pane.applyItemDelta({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' more streamed',
        updatedAt: 2,
      });

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: {
          status: 'completed',
          summary: 'completely different final wording',
          updatedAt: 3,
        },
      });

      expect(pane.items[0].summary).toBe('completely different final wording');
      expect(pane.items[0].status).toBe('completed');
    });

    it('snaps on errored status and lets the interrupted-prefix patch summary win', async () => {
      // Snap-status terminal patches (errored / killed / declined)
      // synchronously reveal the smoother's full received text before
      // writing the patch summary. The patch summary often carries an
      // "[interrupted] …" prefix or similar; it must land as the final
      // visible text, not be overwritten by a trailing reveal.
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          threadId: 'thread-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'partial reveal so far',
          updatedAt: 1,
        }),
      );

      pane.applyItemDelta({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' more',
        updatedAt: 2,
      });

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: {
          status: 'errored',
          summary: '[interrupted] partial reveal so far',
          updatedAt: 3,
        },
      });

      expect(pane.items[0].summary).toBe('[interrupted] partial reveal so far');
      expect(pane.items[0].status).toBe('errored');
    });

    it('handles a bare status-only completion patch (no summary) on a smoothing row', async () => {
      // A completion patch may arrive with only `status` and `updatedAt`
      // — no `summary`. The smoother is left running with the items
      // status already flipped; on the next natural rAF tick (or
      // synchronous flush) the smoother reveals the remaining received
      // characters and the onReveal auto-cleanup branch disposes the
      // entry once `current.status !== 'streaming' && isCaughtUp()`.
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          threadId: 'thread-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'seed',
          updatedAt: 1,
        }),
      );

      pane.applyItemDelta({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' more',
        updatedAt: 2,
      });

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: { status: 'completed', updatedAt: 3 },
      });

      pane.__flushItemSmoothersForTest();

      expect(pane.items[0].summary).toBe('seed more');
      expect(pane.items[0].status).toBe('completed');
      expect(pane.items[0].updatedAt).toBe(3);
    });
  });

  describe('thinking smoothing past the 400-rune tail cap', () => {
    function buildWords(n: number): string[] {
      // Short ~5-char words separated by spaces. ~6 chars per word means
      // 70 words ≈ 420 chars — enough to push past THINKING_TAIL_RUNES=400.
      const out: string[] = [];
      for (let i = 0; i < n; i++) out.push(`word${String(i).padStart(2, '0')}`);
      return out;
    }

    it('keeps writing items[].summary in word-sized advances after revealed > 400 runes', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-think' }));
        // Simulate the firstBlock upsert (Go-side initial thinking row),
        // then a long sequence of wire deltas — same shape Claude produces
        // for reasoning that flows past the 400-rune tail.
        const initial = 'seed ';
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-think',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: initial,
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );

        const words = buildWords(80); // ~7 chars × 80 ≈ 560 chars total.
        // Stream each word as its own wire delta with a few rAF frames
        // between them. This mimics Claude's bursty reasoning text where
        // 5–50 char chunks arrive every 30–100 ms.
        const summaryAtTick: { tick: number; len: number }[] = [];
        let frameCount = 0;
        for (let i = 0; i < words.length; i++) {
          pane.applyItemDelta({
            threadId: 'thread-think',
            itemId: 'think:0:0',
            kind: 'thinking',
            delta: words[i] + ' ',
            updatedAt: 100 + i,
          });
          // Run a handful of rAF frames per wire delta so the smoother
          // gets a chance to reveal between deltas. 6 frames × 16ms = 96ms.
          for (let f = 0; f < 6; f++) {
            clock.tickFrame(16);
            frameCount++;
            summaryAtTick.push({
              tick: frameCount,
              len: pane.items[0].summary.length,
            });
          }
        }
        // Drain remaining lag.
        while (clock.pendingCount() > 0) {
          clock.tickFrame(16);
          frameCount++;
          summaryAtTick.push({
            tick: frameCount,
            len: pane.items[0].summary.length,
          });
        }

        // Sanity: by the end, summary should equal the trimmed tail of the
        // full received text.
        const fullText = initial + words.map((w) => w + ' ').join('');
        const expectedTail = fullText.slice(-400);
        expect(pane.items[0].summary).toBe(expectedTail);

        // The smoother is the *only* writer to items[idx].summary for
        // thinking. Per-tick advances after each reveal land in word-sized
        // increments at the base rate (160 cps × 16ms ≈ 2.5 chars; word
        // units round up to ~7 chars). If anything in the pipeline starts
        // bypassing the smoother past the trim cap, we'd see a jump
        // equal to one wire delta (~7 chars) appear "all at once" without
        // the matching per-tick growth that precedes it.
        //
        // Find every transition where summary GREW (length increased).
        // Before the trim engages (summary < 400), growth jumps are
        // exactly the word advance. After the trim engages, summary
        // stays pinned at 400 chars but its CONTENT shifts — the
        // length-delta-only check no longer suffices, so we instead
        // verify that *no single tick* added more than ~14 chars (2
        // word-units worth) to either the length OR the trailing slice.
        let maxLengthJump = 0;
        let maxContentJump = 0;
        let prevSummary = initial;
        // Walk all rAF ticks again to also inspect content (not just len).
        // We approximate by replaying from the recorded snapshot: read the
        // *current* summary after each tick. But pane state has progressed
        // past the loop, so use the final state for content-jump checks
        // via getOrCreateSmoothing's revealed history — we don't have
        // that here. Instead, do a SECOND clean run with a fresh pane and
        // capture summary at each frame.
        {
          // Reset and re-run with snapshot capture.
          const clock2 = new FakeSmoothingClock();
          __setSmoothingClockForTest(clock2);
          const pane2 = await buildPane(makeThread({ id: 'thread-think-2' }));
          pane2.upsertItem(
            makeItem({
              id: 'think:0:0',
              threadId: 'thread-think-2',
              kind: 'thinking',
              role: 'assistant',
              status: 'streaming',
              summary: initial,
              payloadId: 'thinking:think:0:0',
              updatedAt: 1,
            }),
          );
          let prev = initial;
          for (let i = 0; i < words.length; i++) {
            pane2.applyItemDelta({
              threadId: 'thread-think-2',
              itemId: 'think:0:0',
              kind: 'thinking',
              delta: words[i] + ' ',
              updatedAt: 100 + i,
            });
            for (let f = 0; f < 6; f++) {
              clock2.tickFrame(16);
              const cur = pane2.items[0].summary;
              // Length jump (positive only — trim might shrink it back
              // to 400, which we don't penalize).
              const lenJump = Math.max(0, cur.length - prev.length);
              maxLengthJump = Math.max(maxLengthJump, lenJump);
              // Content jump: how much new text appeared at the END
              // relative to the previous summary. After trim, prev and
              // cur are both 400-char tails; new content is the part of
              // cur that doesn't overlap prev as a suffix-of-prev
              // prefix-of-cur match.
              const contentJump = smoothingNewTailChars(prev, cur);
              maxContentJump = Math.max(maxContentJump, contentJump);
              prev = cur;
            }
          }
          while (clock2.pendingCount() > 0) {
            clock2.tickFrame(16);
            const cur = pane2.items[0].summary;
            const lenJump = Math.max(0, cur.length - prev.length);
            maxLengthJump = Math.max(maxLengthJump, lenJump);
            const contentJump = smoothingNewTailChars(prev, cur);
            maxContentJump = Math.max(maxContentJump, contentJump);
            prev = cur;
          }
          // Reference the unused vars so lint stays clean.
          void prevSummary;
          void summaryAtTick;
        }
        // Word units in our test are 7 chars (e.g. "word00 "). Adaptive
        // catch-up can fire several word units in one tick when lag is
        // high, but should not approach the ~50+ chars/tick that wire
        // deltas would produce if the smoother were bypassed. Cap at
        // 28 chars (~4 word units in one frame) — well below "5 words
        // appearing as a chunk".
        expect(maxLengthJump).toBeLessThanOrEqual(28);
        expect(maxContentJump).toBeLessThanOrEqual(28);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not produce wire-chunk-sized reveals when the wire bursts faster than the base rate', async () => {
      // Reproduces the user-reported regression: past ~400 chars,
      // streamed text appears in chunks "exactly like the old behavior
      // before any smoothing changes" — 5 words, pause, 15 words. The
      // hypothesis is that the adaptive catch-up math (`drain lag in
      // 500ms`) scales the per-tick reveal proportional to lag, so a wire
      // that bursts faster than the 160 cps base rate eventually settles
      // at a steady-state lag where per-tick = wire_rate * (16/500) — for
      // a 2000 cps wire, that's 64 chars (~10 words) per tick.
      //
      // Run on assistant_text; the reasoning counterpart is the next test.
      // The guarantee is kind-independent — no smoother skips or chunks —
      // so both kinds hold the same per-tick cap.
      //
      // Wire pattern is realistic: 50-char wire bursts arriving every
      // 25ms (= 2000 cps sustained, close to Claude's burst rate for
      // reasoning text). Streamed for ~1.5s so we reach steady-state lag.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-burst' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-burst',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );

        // 50-char wire bursts with realistic word distribution (5–8 char
        // words separated by spaces). Each burst is ~7 words.
        function makeBurst(seed: number): string {
          const sizes = [4, 7, 5, 6, 8, 5, 7];
          const out: string[] = [];
          let used = 0;
          let i = 0;
          while (used < 50) {
            const sz = sizes[(seed + i) % sizes.length];
            const word = 'a'.repeat(sz);
            out.push(word);
            used += sz + 1; // +1 for space
            i++;
          }
          return out.join(' ') + ' ';
        }

        let maxContentJump = 0;
        let maxLengthJump = 0;
        let prev = '';
        let burstIdx = 0;
        // Wire arrives every 25ms; tick rAF every 16ms. We loop over a
        // 1500ms simulated window, emitting a wire burst on the 25ms
        // cadence and a rAF on the 16ms cadence (interleaved by time).
        const totalMs = 1500;
        const wireIntervalMs = 25;
        const rafIntervalMs = 16;
        let nextWireAt = 0;
        let nextRafAt = 0;
        let elapsed = 0;
        const measure = () => {
          const cur = pane.items[0].summary;
          const lenJump = Math.max(0, cur.length - prev.length);
          const contentJump = smoothingNewTailChars(prev, cur);
          maxLengthJump = Math.max(maxLengthJump, lenJump);
          maxContentJump = Math.max(maxContentJump, contentJump);
          prev = cur;
        };
        while (elapsed < totalMs) {
          if (nextWireAt <= nextRafAt) {
            const dt = nextWireAt - elapsed;
            if (dt > 0) {
              clock.tickFrame(dt);
              elapsed += dt;
              // Measure after every clock advance so per-tick reveals
              // are observed individually — without this, several
              // smoother ticks could fire between two rAF-branch
              // measurements and the recorded "jump" would be the sum
              // of all of them.
              measure();
            }
            const burst = makeBurst(burstIdx++);
            pane.applyItemDelta({
              threadId: 'thread-burst',
              itemId: 'text:0:0',
              kind: 'assistant_text',
              delta: burst,
              updatedAt: 100 + burstIdx,
            });
            nextWireAt = elapsed + wireIntervalMs;
          } else {
            const dt = nextRafAt - elapsed;
            if (dt > 0) {
              clock.tickFrame(dt);
              elapsed += dt;
              measure();
            }
            nextRafAt = elapsed + rafIntervalMs;
          }
        }
        // Drain any remaining lag.
        while (clock.pendingCount() > 0) {
          clock.tickFrame(16);
          measure();
        }

        // "5 words show up" ≈ 30 chars; "15 more words" ≈ 90 chars.
        // A healthy smoother stays under the per-tick work cap (~3
        // short words) even under steady-state burst. The cap inside
        // `PerItemSmoother.tick()` is what enforces this; without it,
        // adaptive math at lag ~= wire_rate * (catchup_ms / 1000)
        // produces 60–100+ chars/tick under sustained 2000 cps bursts
        // and the user perceives those as chunks of 5–15 words.
        expect(maxLengthJump).toBeLessThanOrEqual(MAX_ADVANCE_PER_TICK_CHARS);
        expect(maxContentJump).toBeLessThanOrEqual(MAX_ADVANCE_PER_TICK_CHARS);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('never chunks a reasoning row either — and the wire gap drains it to zero', async () => {
      // A collapsed reasoning row gets the SAME guarantee as prose: every
      // character animates, in order, at no more than the per-tick cap.
      // A bounded-backlog skip for reasoning rows was implemented and
      // rejected — dropping characters the reader might expand into is
      // worse than making a queued row wait.
      //
      // The second half is why the wait is acceptable: the wire is bursty.
      // Once the overspeed burst stops (tool call, API round-trip, model
      // pause), the drain keeps running and returns the row to zero lag.
      // If this test is ever "fixed" by capping the backlog, read the
      // rationale in PerItemSmoother.ts first.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-think-bound' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-think-bound',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );

        // ~50 chars every 16ms ≈ 3000 cps, an order above the reveal
        // ceiling, sustained for 100 frames.
        const burst = 'word '.repeat(10);
        let received = '';
        let maxJump = 0;
        let previousRevealed = 0;
        for (let i = 0; i < 100; i++) {
          received += burst;
          pane.applyItemDelta({
            threadId: 'thread-think-bound',
            itemId: 'think:0:0',
            kind: 'thinking',
            delta: burst,
            updatedAt: 100 + i,
          });
          clock.tickFrame(16);
          const revealed = (pane.liveThinkingTailForItem('think:0:0') ?? '')
            .length;
          maxJump = Math.max(maxJump, revealed - previousRevealed);
          previousRevealed = revealed;
        }
        // No frame delivered more than a tick's animated work — the wire
        // ran far ahead, and the row simply fell behind rather than
        // jumping to catch it.
        expect(maxJump).toBeLessThanOrEqual(MAX_ADVANCE_PER_TICK_CHARS);
        // It genuinely fell behind, so the drain below is not vacuous.
        const lagAtBurstEnd = received.length - previousRevealed;
        expect(lagAtBurstEnd).toBeGreaterThan(MAX_ADVANCE_PER_TICK_CHARS * 10);

        // The gap: the wire stops, frames keep coming. The backlog drains
        // to zero at the ceiling — no skip needed, and the reader gets
        // every character.
        let gapFrames = 0;
        while (clock.pendingCount() > 0 && gapFrames < 20000) {
          clock.tickFrame(16);
          gapFrames++;
        }
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(received);
        expect(pane.items[0].summary).toBe(received.slice(-400));
        // Drained at the reveal ceiling, not faster: a rush regime would
        // finish materially sooner than the rate implies.
        expect(gapFrames * 16).toBeGreaterThan(
          (lagAtBurstEnd / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000 * 0.9,
        );
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('reveals a single small wire delta over multiple ticks past 400 runes', async () => {
      // Reproduces the user's "5 words in one chunk even when only 5
      // words streamed" report. The smoother is past the trim threshold
      // and caught up (revealed == received, lag = 0). A SINGLE small
      // wire delta (≈5 words) arrives. With base rate 160 cps and the
      // per-tick cap of 14 chars, those 5 words must reveal over at
      // least ~5 rAF ticks (~80ms), never as one DOM update.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-think-burst' }));
        // Seed the item with > 400 chars already in the summary so the
        // trim is already engaged. The smoother starts caught up
        // (initialRevealed = initialReceived = seed), so this isolates
        // the per-tick reveal of the NEXT delta.
        const seedWords: string[] = [];
        for (let i = 0; i < 80; i++)
          seedWords.push(`word${String(i).padStart(2, '0')}`);
        const seed = seedWords.join(' ') + ' ';
        expect(seed.length).toBeGreaterThan(400);
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-think-burst',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: seed,
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        // Seed the smoother by sending a zero-impact delta. The
        // production path creates the smoother in applyItemDelta with
        // initialReceived = current.summary = seed; revealed = seed.
        // Lag = delta.length. We feed a single small 5-word burst.
        const fiveWords = 'hello bright cosmic future today ';
        pane.applyItemDelta({
          threadId: 'thread-think-burst',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: fiveWords,
          updatedAt: 2,
        });

        // Walk rAF ticks and record per-tick summary changes. Cap the
        // walk well past the expected drain so we can verify the
        // smoother caught up at the end.
        const tickAdvances: number[] = [];
        let prev = pane.items[0].summary;
        for (let i = 0; i < 30 && clock.pendingCount() > 0; i++) {
          clock.tickFrame(16);
          const cur = pane.items[0].summary;
          const advance = smoothingNewTailChars(prev, cur);
          if (advance > 0) tickAdvances.push(advance);
          prev = cur;
        }

        // Verify: the 5 words (33 chars) revealed over MULTIPLE ticks,
        // with each tick's advance bounded by the cap. None should be
        // the full 33-char delta.
        expect(tickAdvances.length).toBeGreaterThanOrEqual(2);
        for (const advance of tickAdvances) {
          expect(advance).toBeLessThanOrEqual(14);
        }
        // Sanity: the trailing 5 words are now in the summary.
        expect(pane.items[0].summary.endsWith(fiveWords)).toBe(true);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drains the remaining smoother backlog after status flips to completed with an extending summary', async () => {
      // The per-tick cap means catch-up can no longer outrun the wire
      // — accumulated lag at completion time must still drain to the
      // patch's extending summary. Verify the applyItemPatch
      // extending-summary branch appends the suffix as a delta, the
      // smoother continues at the capped rate, and the on-reveal
      // auto-cleanup (`!streaming && isCaughtUp`) eventually fires so
      // the row settles with the full final text and the smoother map
      // doesn't strand a stale entry.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-drain' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-drain',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        // Stream the first half (~150 chars) as deltas, then complete
        // with an extending summary that adds another ~150 chars on
        // top. This is the actual extending-summary path: smoother
        // received < patchSummary AND patchSummary.startsWith(received).
        const allWords: string[] = [];
        for (let i = 0; i < 50; i++)
          allWords.push(`item${String(i).padStart(2, '0')}`);
        const fullText = allWords.join(' ') + ' ';
        const streamed = allWords.slice(0, 25).join(' ') + ' ';
        pane.applyItemDelta({
          threadId: 'thread-drain',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: streamed,
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-drain',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', summary: fullText, updatedAt: 3 },
        });

        // Drain. With per-tick cap = 14 chars, ~300 chars takes ~22
        // ticks (~350ms). Allow more to be safe.
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) {
          clock.tickFrame(16);
        }
        // Final state: full text revealed (trimmed), status flipped,
        // smoother auto-disposed (no leftover pending callbacks).
        expect(pane.items[0].summary).toBe(fullText.slice(-400));
        expect(pane.items[0].status).toBe('completed');
        expect(clock.pendingCount()).toBe(0);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        // The onReveal auto-cleanup settles RETAINING the tail: the
        // extending summary drained fully, so the full final text keeps
        // serving past the settle (content-consistent with the trimmed
        // summary recorded above).
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(fullText);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps draining when the completion patch carries the trimmed tail preview', async () => {
      // Thinking rows persist the tail-trimmed preview as their summary
      // (Go thinkingSummaryPreview mirrors THINKING_TAIL_RUNES), so a
      // content-present settle patch re-asserts the TRIMMED text — not
      // the full received stream. Mid-drain, past 400 runes, that patch
      // summary neither equals nor extends the smoother's received text;
      // treating it as an overwrite snap+disposes the smoother and dumps
      // the unrevealed backlog wholesale (the Codex thinking completion
      // shape, and the recovered-block settle patch on Claude). The
      // patch must instead read as a re-assert: smoother survives, keeps
      // draining at the capped rate, and auto-disposes at catch-up.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-think-settle' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-think-settle',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        // > 400 runes so the trimmed preview provably differs from the
        // received text, delivered as one delta so the smoother holds a
        // large backlog when the settle patch lands.
        const full = buildWords(80).join(' ') + ' '; // 560 chars
        expect(full.length).toBeGreaterThan(400);
        pane.applyItemDelta({
          threadId: 'thread-think-settle',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: full,
          updatedAt: 2,
        });
        // A couple of frames in: genuinely mid-drain.
        clock.tickFrame(16);
        clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        const midDrain = pane.items[0].summary;
        expect(midDrain.length).toBeLessThan(full.length);

        // The settle patch as Go emits it: completed + trimmed preview.
        pane.applyItemPatch({
          threadId: 'thread-think-settle',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', summary: full.slice(-400), updatedAt: 3 },
        });

        // Smoother survives; the patch neither snapped the reveal nor
        // wrote the trimmed preview over the mid-drain summary.
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        expect(pane.items[0].summary).toBe(midDrain);
        expect(pane.items[0].status).toBe('completed');

        // Drain to completion: converges on the trimmed tail and the
        // onReveal auto-cleanup disposes the smoother.
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) {
          clock.tickFrame(16);
        }
        expect(pane.items[0].summary).toBe(full.slice(-400));
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(clock.pendingCount()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('exposes a monotonically-growing live tail past 400 runes for the collapsed view', async () => {
      // Regression guard for the user-reported "5 words appear at once
      // past 400 runes" symptom. The collapsed ThinkingBlock renders a
      // `<span>{bodyText}</span>` inside `whitespace-pre-wrap` +
      // `max-h-[3lh] overflow-hidden` + `scrollTop = scrollHeight`.
      // When bodyText is `item.summary` past the trim threshold, the
      // string is a sliding window — characters drop from the start as
      // new ones arrive at the end. Even a single 1-char-per-tick
      // reveal recomputes wrap for the full bounded string and can
      // shift the visible 3 lines wholesale when a word at the start
      // crosses a wrap boundary. `pane.liveThinkingTailForItem` exposes
      // the smoother's full revealed text instead, which grows append-
      // only — wrap layout never reshuffles older text and the visible
      // window scrolls by exactly the per-tick reveal.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-live-tail' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-live-tail',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );

        // Stream enough text to push well past 400 runes.
        const words: string[] = [];
        for (let i = 0; i < 100; i++)
          words.push(`tok${String(i).padStart(2, '0')}`);
        // Feed in word-by-word so the smoother has lag throughout.
        for (let i = 0; i < words.length; i++) {
          pane.applyItemDelta({
            threadId: 'thread-live-tail',
            itemId: 'think:0:0',
            kind: 'thinking',
            delta: words[i] + ' ',
            updatedAt: 100 + i,
          });
          clock.tickFrame(16);
        }
        // Drain remaining lag.
        while (clock.pendingCount() > 0) clock.tickFrame(16);

        const finalTail = pane.liveThinkingTailForItem('think:0:0');
        // Smoother is still live (status === 'streaming') so the live
        // tail must be populated and equal the full received text.
        expect(finalTail).not.toBeNull();
        expect(finalTail!.length).toBeGreaterThan(400);
        // items[].summary is the trimmed sliding window; live tail is the
        // full text. They must diverge in length once past the cap —
        // proving the collapsed render no longer reads the bounded
        // sliding-window source.
        expect(pane.items[0].summary.length).toBeLessThanOrEqual(400);
        expect(finalTail!.length).toBeGreaterThan(pane.items[0].summary.length);

        // Now sample monotonic growth across a fresh run: at each tick
        // the live tail must be a prefix-extension of the previous tail
        // (append-only, never sliding window).
        const clock2 = new FakeSmoothingClock();
        __setSmoothingClockForTest(clock2);
        const pane2 = await buildPane(makeThread({ id: 'thread-live-tail-2' }));
        pane2.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-live-tail-2',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        let prev = '';
        let pastTrimSamples = 0;
        let growthPastTrimSamples = 0;
        for (let i = 0; i < words.length; i++) {
          pane2.applyItemDelta({
            threadId: 'thread-live-tail-2',
            itemId: 'think:0:0',
            kind: 'thinking',
            delta: words[i] + ' ',
            updatedAt: 100 + i,
          });
          for (let f = 0; f < 3; f++) {
            clock2.tickFrame(16);
            const cur = pane2.liveThinkingTailForItem('think:0:0') ?? '';
            if (cur.length > 0) {
              // Append-only invariant: previous tail is always a prefix
              // of the new tail (no characters drop from the start).
              expect(cur.startsWith(prev)).toBe(true);
              if (cur.length > 400) pastTrimSamples++;
              // Real growth past the trim threshold (not the smoother
              // sitting idle re-reading the same value) — guards
              // against a regression that quietly clamps the live tail
              // to the trimmed-summary length.
              if (cur.length > 400 && cur.length > prev.length)
                growthPastTrimSamples++;
              prev = cur;
            }
          }
        }
        // We must have actually crossed the 400-rune threshold while
        // sampling — otherwise the test doesn't exercise the regression
        // path it claims to.
        expect(pastTrimSamples).toBeGreaterThan(10);
        // And the tail must have grown past the threshold more than
        // once: a single growth tick crossing 400 followed by an idle
        // smoother would still satisfy `pastTrimSamples > 10` because
        // the same value is re-read many times. Real append-only
        // behaviour produces growth on most reveals.
        expect(growthPastTrimSamples).toBeGreaterThan(5);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('retains the live thinking tail when the smoother disposes on completion', async () => {
      // Once the stream settles the smoother auto-disposes, but the live
      // tail entry is RETAINED: the collapsed clamp is rendering exactly
      // that string, and swapping to the trimmed summary at settle
      // re-wraps the visible 3 lines in front of the reader (wrap
      // depends on where the string starts; the trim starts
      // mid-sentence). The offscreen row-UI prune bounds the retention
      // (see the prune test below) — settle-time is the one moment the
      // swap must NOT happen.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-tail-cleanup' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-tail-cleanup',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        const words: string[] = [];
        for (let i = 0; i < 80; i++)
          words.push(`tok${String(i).padStart(2, '0')}`);
        const fullText = words.join(' ') + ' ';
        pane.applyItemDelta({
          threadId: 'thread-tail-cleanup',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: fullText,
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-tail-cleanup',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', summary: fullText, updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.items[0].status).toBe('completed');
        // Smoother disposed (the resource cleanup the settle owes) …
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        // … but the tail is retained byte-identical to the last reveal,
        // and diverges from the trimmed summary (fullText > 400 runes),
        // proving the collapsed render did not swap sources at settle.
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(fullText);
        expect(pane.items[0].summary.length).toBeLessThan(fullText.length);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('disposes the smoother on a bare status-completed patch with no summary', async () => {
      // Regression for a leak in applyItemPatch: a status-only patch
      // (e.g. Codex sometimes sends `{status: 'completed', updatedAt}`
      // without re-asserting `summary` when the wire summary already
      // matched what the smoother had received) took neither the snap
      // branch (status isn't errored/killed/declined) nor the
      // extend-or-snap branch (no summary). The `onReveal` auto-cleanup
      // at the smoother factory site only runs on a subsequent rAF
      // tick, so a smoother that's already caught up by the time the
      // patch lands would never re-fire — the `itemSmoothers` entry
      // (and its zombie rAF scheduling) leaked until the next thread
      // switch. The live TAIL, by contrast, is deliberately retained on
      // this content-consistent settle: the leak's harm was the
      // undisposed smoother, and the retained string is what keeps the
      // collapsed clamp from re-wrapping at the settle boundary.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-bare-status' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-bare-status',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-bare-status',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'reasoning text ',
          updatedAt: 2,
        });
        // Drain to caught-up so the next onReveal auto-cleanup branch
        // is unreachable — only the patch handler can dispose now.
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

        pane.applyItemPatch({
          threadId: 'thread-bare-status',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });

        expect(pane.items[0].status).toBe('completed');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('reasoning text ');
        // Drain again to confirm no zombie rAF ticks (a fresh tick
        // after a leak would re-fire onReveal against the disposed
        // slot); the retained tail must stay byte-stable through it.
        safety = 20;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('reasoning text ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops the retained tail when a snap-status patch overwrites the summary', async () => {
      // Tail retention is for content-consistent settles ONLY. A
      // kill/error patch rewrites the summary (e.g. an "[interrupted] "
      // prefix); a retained tail would keep the collapsed clamp showing
      // the pre-patch text and mask the authoritative summary.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-snap-drop' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-snap-drop',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-snap-drop',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'partial reasoning ',
          updatedAt: 2,
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

        pane.applyItemPatch({
          threadId: 'thread-snap-drop',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: {
            status: 'killed',
            summary: '[interrupted] partial reasoning ',
            updatedAt: 3,
          },
        });

        expect(pane.items[0].summary).toBe('[interrupted] partial reasoning ');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops a retained tail when the settled row is removed', async () => {
      // Guards the dispose-order fix: a settled row has a retained tail
      // but NO smoother, and disposeSmootherFor used to early-return on
      // the missing smoother before touching the tail map — a removal
      // after settle would have leaked the string until thread switch.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-remove-tail' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-remove-tail',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-remove-tail',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'settled reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-remove-tail',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

        pane.removeItemById('think:0:0', 'thread-remove-tail');
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('prunes retained tails offscreen, never live ones', async () => {
      // The offscreen row-UI prune is what bounds tail retention. A
      // settled tail outside the retention set drops; a STREAMING row's
      // tail survives regardless of retention — the live reveal owns it.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-tail-prune' }));
        for (const [itemIndex, id] of (['think:0:0', 'think:0:1'] as const).entries()) {
          pane.upsertItem(
            makeItem({
              id,
              threadId: 'thread-tail-prune',
              turnIndex: 0,
              itemIndex,
              kind: 'thinking',
              role: 'assistant',
              status: 'streaming',
              summary: '',
              payloadId: `thinking:${id}`,
              updatedAt: 1,
            }),
          );
        }
        // Settle the first row (tail retained, smoother gone).
        pane.applyItemDelta({
          threadId: 'thread-tail-prune',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'first reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-tail-prune',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        // Stream the second row and drain: the smoother catches up but
        // stays LIVE (no terminal status arrived), so its tail is owned
        // by the reveal, not the settled map.
        pane.applyItemDelta({
          threadId: 'thread-tail-prune',
          itemId: 'think:0:1',
          kind: 'thinking',
          delta: 'second reasoning ',
          updatedAt: 4,
        });
        safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('first reasoning ');
        expect(pane.liveThinkingTailForItem('think:0:1')).toBe('second reasoning ');

        // Retention keeps the settled row → both tails survive.
        pane.pruneRowUiState({ itemIds: new Set(['think:0:0']), payloads: [], groupKeys: new Set() });
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('first reasoning ');
        expect(pane.liveThinkingTailForItem('think:0:1')).toBe('second reasoning ');

        // Empty retention: the settled tail drops; the live one is owned
        // by its smoother and must survive.
        pane.pruneRowUiState({ itemIds: new Set(), payloads: [], groupKeys: new Set() });
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        expect(pane.liveThinkingTailForItem('think:0:1')).toBe('second reasoning ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('invalidates a retained tail when a terminal re-upsert rewrites the summary', async () => {
      // THE consistency case retention must survive: triage re-persists
      // a completed thinking row when a late content-present stop's
      // text differs (persistOrUpdateCompletedThinkingItem), and that
      // upsert lands on a row that already settled — no smoother, no
      // reconcile entry, nothing writer-side to notice. The read-time
      // validation in liveThinkingTailFor is what catches it: the
      // summary recorded at settle no longer matches the row, so the
      // stale tail must stop being served.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-divergent-upsert' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-divergent-upsert',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-divergent-upsert',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'original reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-divergent-upsert',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('original reasoning ');

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-divergent-upsert',
            kind: 'thinking',
            role: 'assistant',
            status: 'completed',
            summary: 'authoritative rewritten reasoning',
            payloadId: 'thinking:think:0:0',
            updatedAt: 4,
          }),
        );
        expect(pane.items[0].summary).toBe('authoritative rewritten reasoning');
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('invalidates a retained tail when a correction patch rewrites the settled summary', async () => {
      // Same consistency story as the re-upsert test, through the patch
      // path: a post-settle correction rewrites items[].summary with no
      // smoother alive to observe it.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-divergent-patch' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-divergent-patch',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-divergent-patch',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'settled reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-divergent-patch',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('settled reasoning ');

        pane.applyItemPatch({
          threadId: 'thread-divergent-patch',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { summary: 'corrected reasoning', updatedAt: 4 },
        });
        expect(pane.items[0].summary).toBe('corrected reasoning');
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps serving a retained tail through a consistent summary re-assert', async () => {
      // The validation must be a consistency check, not a
      // one-shot fuse: a patch that re-asserts the SAME summary the
      // settle recorded (Claude terminal replays do this) leaves the
      // rendered string untouched, so the tail keeps serving.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-reassert' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-reassert',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-reassert',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'stable reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-reassert',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('stable reasoning ');

        pane.applyItemPatch({
          threadId: 'thread-reassert',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', summary: pane.items[0].summary, updatedAt: 4 },
        });
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('stable reasoning ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('reseeds a resumed smoother from the retained tail when the summary is unchanged', async () => {
      // A replay upsert can flip a settled row back to streaming and
      // follow with deltas (turn resume). The fresh smoother must seed
      // from the retained FULL tail, not the trimmed summary — seeding
      // from the summary would shrink the rendered string and re-wrap
      // the clamp, the exact jump retention exists to prevent.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-reseed' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-reseed',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        const words: string[] = [];
        for (let i = 0; i < 80; i++) words.push(`tok${String(i).padStart(2, '0')}`);
        const fullText = words.join(' ') + ' ';
        pane.applyItemDelta({
          threadId: 'thread-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: fullText,
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        const trimmedSummary = pane.items[0].summary;
        // >400 runes, so the trim is a real shrink — the seed choice is
        // observable.
        expect(trimmedSummary.length).toBeLessThan(fullText.length);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(fullText);

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-reseed',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: trimmedSummary,
            payloadId: 'thinking:think:0:0',
            updatedAt: 4,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'resumed ',
          updatedAt: 5,
        });
        safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(`${fullText}resumed `);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops a stale retained tail when the row resumes with a rewritten summary', async () => {
      // The reseed's negative: if the resuming row's summary is NOT the
      // one the settle recorded, the retained tail belongs to a dead
      // version of the row. The seed must start from the new summary
      // and clear the stale entry rather than shadow the resumed reveal.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-stale-reseed' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-stale-reseed',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-stale-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'first life reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-stale-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('first life reasoning ');

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-stale-reseed',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: 'rewritten start ',
            payloadId: 'thinking:think:0:0',
            updatedAt: 4,
          }),
        );
        // Already invalid at read time before any delta arrives.
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        pane.applyItemDelta({
          threadId: 'thread-stale-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'next ',
          updatedAt: 5,
        });
        safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('rewritten start next ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('retains the tail when a caught-up smoother settles via terminal upsert', async () => {
      // Terminal upserts (upsertItemsBatch reconcile) are the third
      // settle path next to the summary-carrying and bare-status
      // patches; a caught-up reveal must retain its tail there too.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-upsert-settle' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-upsert-settle',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-upsert-settle',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'upsert settle reasoning ',
          updatedAt: 2,
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-upsert-settle',
            kind: 'thinking',
            role: 'assistant',
            status: 'completed',
            summary: pane.items[0].summary,
            payloadId: 'thinking:think:0:0',
            updatedAt: 3,
          }),
        );
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('upsert settle reasoning ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops the tail when a mid-drain smoother is replaced by a terminal upsert', async () => {
      // A mid-drain smoother's tail is partial — it can never match a
      // terminal summary, so the reconcile disposes rather than retains.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-middrain-upsert' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-middrain-upsert',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        const words: string[] = [];
        for (let i = 0; i < 80; i++) words.push(`tok${String(i).padStart(2, '0')}`);
        pane.applyItemDelta({
          threadId: 'thread-middrain-upsert',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: words.join(' ') + ' ',
          updatedAt: 2,
        });
        // A few frames: enough for a partial reveal (the tail entry
        // exists), far from caught up.
        for (let i = 0; i < 3; i++) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-middrain-upsert',
            kind: 'thinking',
            role: 'assistant',
            status: 'completed',
            summary: 'final from upsert',
            payloadId: 'thinking:think:0:0',
            updatedAt: 3,
          }),
        );
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('evicts the oldest settled tails past the retained-char budget', async () => {
      // The offscreen prune only runs while a MessageTimeline is
      // mounted; a backgrounded pane (Settings replaces the surface)
      // keeps settling rows with no prune cadence. The store-side char
      // budget is the backstop: oldest settled tails evict once the
      // total passes SETTLED_TAIL_BUDGET_CHARS (131072).
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-tail-budget' }));
        const tailFor = (i: number): string => `row${i} ` + 'x'.repeat(59_995);
        for (const i of [0, 1, 2]) {
          pane.upsertItem(
            makeItem({
              id: `think:0:${i}`,
              threadId: 'thread-tail-budget',
              turnIndex: 0,
              itemIndex: i,
              kind: 'thinking',
              role: 'assistant',
              status: 'streaming',
              summary: '',
              payloadId: `thinking:think:0:${i}`,
              updatedAt: 1,
            }),
          );
          pane.applyItemDelta({
            threadId: 'thread-tail-budget',
            itemId: `think:0:${i}`,
            kind: 'thinking',
            delta: tailFor(i),
            updatedAt: 2,
          });
        }
        // Snap + settle all three in insertion order; the third settle
        // pushes the total to ~180k and evicts the oldest back under
        // budget.
        pane.__flushItemSmoothersForTest();
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        expect(pane.liveThinkingTailForItem('think:0:1')).toBe(tailFor(1));
        expect(pane.liveThinkingTailForItem('think:0:2')).toBe(tailFor(2));
        expect(pane.debugMemoryStats().liveThinkingTailChars).toBe(tailFor(1).length * 2);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('disposes the smoother when a completed patch re-asserts the equal summary (Codex assistant_text)', async () => {
      // The sibling of the bare-status leak test. Codex content-block-stop
      // carries ContentPresent=true, so doSettleStreamingText re-asserts
      // the full summary on the completion patch. When that summary equals
      // what the smoother already received AND the smoother is caught up,
      // the extend/snap branches are both skipped (summary === received)
      // and the bare-status dispose branch is unreachable (it is an
      // else-if after the summary branch). Before the fix the smoother
      // leaked until the next thread switch. assistant_text has no
      // live-tail observable, so assert on the smoother count directly.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-equal-text' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-equal-text',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-equal-text',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: 'hello world ',
          updatedAt: 2,
        });
        // Drain to caught-up so the onReveal auto-cleanup can't fire later.
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        pane.applyItemPatch({
          threadId: 'thread-equal-text',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: { status: 'completed', summary: 'hello world ', updatedAt: 3 },
        });

        expect(pane.items[0].status).toBe('completed');
        expect(pane.items[0].summary).toBe('hello world ');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        // No zombie rAF ticks left behind.
        safety = 20;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('preserves the full revealed text when a snap-status patch omits a summary', async () => {
      // Regression for the dead-snap discard: the isSnapStatus branch
      // snaps the smoother (writing the full received text into
      // items[index] via onReveal), but the final item was rebuilt from
      // the PRE-snap `current` capture — discarding that write. A
      // kill/error patch that carries no summary would then revert to the
      // partial pre-snap text, losing the already-streamed tail. The fix
      // rebuilds from items[index] so the snap survives.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-snap-nosum' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-snap-nosum',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'partial so far',
            updatedAt: 1,
          }),
        );
        // Append a tail the smoother has received but NOT yet revealed
        // (no clock ticks fired), so snap has real work to do.
        pane.applyItemDelta({
          threadId: 'thread-snap-nosum',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: ' and then more',
          updatedAt: 2,
        });
        // Kill with status only — no summary in the patch.
        pane.applyItemPatch({
          threadId: 'thread-snap-nosum',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: { status: 'killed', updatedAt: 3 },
        });
        expect(pane.items[0].status).toBe('killed');
        // The snap revealed everything; the no-summary patch must keep it.
        expect(pane.items[0].summary).toBe('partial so far and then more');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });

  describe('visibility-resume snap (snapSmoothersToReceived)', () => {
    // requestAnimationFrame is suspended while a tab is hidden, but the
    // WebSocket keeps delivering deltas into each smoother's `received`
    // buffer. The FakeSmoothingClock models this exactly: appending a delta
    // without calling `tickFrame` leaves `received` ahead of `revealed` with
    // a pending callback that never fires — the hidden-tab state. The
    // visibilitychange→visible entry point (App.svelte) calls
    // `snapSmoothersToReceived` so the backlog catches up to the wire in one
    // frame instead of crawling in at MAX_ADAPTIVE_CHARS_PER_SEC on return.
    function manyWords(prefix: string, n: number): string {
      return Array.from(
        { length: n },
        (_, i) => `${prefix}${String(i).padStart(2, '0')}`,
      ).join(' ');
    }

    it('snaps a backlogged STILL-STREAMING row to the wire and keeps the smoother live', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-vis-a' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-vis-a',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );
        // ~700 chars in one delta — far more than one tick's 14-char cap.
        const big = manyWords('word', 100);
        pane.applyItemDelta({
          threadId: 'thread-vis-a',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: big,
          updatedAt: 2,
        });
        // Hidden: nothing revealed yet, a pending rAF would crawl it in.
        expect(pane.items[0].summary).toBe('');
        expect(clock.pendingCount()).toBeGreaterThan(0);

        pane.snapSmoothersToReceived();

        // Caught up to the wire in one call; the pending rAF is canceled.
        expect(pane.items[0].summary).toBe(big);
        expect(clock.pendingCount()).toBe(0);
        // Row is still streaming, so the smoother is retained for the rest
        // of the live turn rather than disposed.
        expect(pane.items[0].status).toBe('streaming');
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        // A later delta still animates — snap leaves the smoother usable.
        pane.applyItemDelta({
          threadId: 'thread-vis-a',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: ' more',
          updatedAt: 3,
        });
        expect(pane.items[0].summary).toBe(big); // not revealed until a tick
        while (clock.pendingCount() > 0) clock.tickFrame(16);
        expect(pane.items[0].summary).toBe(`${big} more`);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('snaps and disposes a row that COMPLETED while hidden instead of crawling on return', async () => {
      // The headline regression. The row streams AND completes in the
      // background; the completion patch re-asserts the equal summary (Codex
      // content-block-stop shape) but the smoother is still backlogged, so
      // none of applyItemPatch's dispose branches fire (summary === received,
      // not caught up). Without the visibility snap the finished response
      // would type itself in at the per-tick cap when the tab regains focus.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-vis-b' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-vis-b',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );
        const full = manyWords('tok', 100);
        pane.applyItemDelta({
          threadId: 'thread-vis-b',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: full,
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-vis-b',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: { status: 'completed', summary: full, updatedAt: 3 },
        });
        // Bug shape on return WITHOUT the snap: status is completed but the
        // text has not been revealed, and a pending rAF would drain it slowly.
        expect(pane.items[0].status).toBe('completed');
        expect(pane.items[0].summary).toBe('');
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        expect(clock.pendingCount()).toBeGreaterThan(0);

        pane.snapSmoothersToReceived();

        // Fully shown in one frame; the terminal-status onReveal cleanup
        // disposes the smoother; no lingering rAF to crawl the text in.
        expect(pane.items[0].summary).toBe(full);
        expect(pane.items[0].status).toBe('completed');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(clock.pendingCount()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('is a no-op when smoothers are caught up or absent', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-vis-c' }));
        // No smoothers yet: safe to call.
        expect(() => pane.snapSmoothersToReceived()).not.toThrow();

        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-vis-c',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-vis-c',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: 'short text ',
          updatedAt: 2,
        });
        // Fully drain so the smoother is caught up but still streaming.
        while (clock.pendingCount() > 0) clock.tickFrame(16);
        expect(pane.items[0].summary).toBe('short text ');
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        // A caught-up snap changes nothing and keeps the streaming smoother.
        pane.snapSmoothersToReceived();
        expect(pane.items[0].summary).toBe('short text ');
        expect(pane.items[0].status).toBe('streaming');
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        expect(clock.pendingCount()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });

  describe('reveal sequencer (revealBoundary)', () => {
    function streamingThinking(
      id: string,
      itemIndex: number,
      threadId: string,
    ) {
      return makeItem({
        id,
        threadId,
        kind: 'thinking',
        role: 'assistant',
        status: 'streaming',
        turnIndex: 0,
        itemIndex,
        summary: '',
        payloadId: `thinking:${id}`,
        updatedAt: 1,
      });
    }

    it('starts with no gate', async () => {
      const pane = await buildPane(makeThread({ id: 't' }));
      expect(pane.revealBoundary).toBeNull();
    });

    it('a solo streaming row gates at itself but withholds nothing at the tail', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        // Frontier is the only/last node → boundary points at it but the
        // slice helper (covered in subagentGrouping.test.ts) withholds nothing.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // It drains at the ordinary reveal cadence and the gate drops.
        for (let i = 0; i < 200 && pane.revealBoundary !== null; i++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('withholds the next top-level row until the streaming item drains', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        // Wire moves on: a tool call appears while the thinking still lags.
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );
        // Gate stays at the thinking row — the tool call is withheld.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // The thinking row finishes at the ordinary cadence — no rush —
        // and only then does the gate drop.
        for (let i = 0; i < 200 && pane.revealBoundary !== null; i++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('a waiting successor does not speed the frontier up', async () => {
      // The successor-waiting fast-drain is gone: a queued row changes
      // WHAT renders (it is withheld), never how fast the frontier
      // animates. Both panes get the same backlog; the one with a
      // successor must not finish sooner.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const solo = await buildPane(makeThread({ id: 'solo' }));
        solo.upsertItem(streamingThinking('think:0:0', 0, 'solo'));
        solo.applyItemDelta({
          threadId: 'solo',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });

        const gated = await buildPane(makeThread({ id: 'gated' }));
        gated.upsertItem(streamingThinking('think:0:0', 0, 'gated'));
        gated.applyItemDelta({
          threadId: 'gated',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        gated.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 'gated',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );

        // Both panes share the clock, so one loop drives both.
        let soloFrames = 0;
        let gatedFrames = 0;
        for (let i = 1; i <= 300; i++) {
          clock.tickFrame(16);
          if (soloFrames === 0 && solo.revealBoundary === null) soloFrames = i;
          if (gatedFrames === 0 && gated.revealBoundary === null) gatedFrames = i;
        }
        expect(soloFrames).toBeGreaterThan(0);
        expect(gatedFrames).toBe(soloFrames);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('pauses a withheld smoothed successor so it animates from the start', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        // A streaming assistant_text successor arrives and gets its deltas
        // while still withheld behind the thinking row.
        pane.upsertItem(
          makeItem({
            id: 'text:0:1',
            threadId: 't',
            kind: 'assistant_text',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 1,
            summary: '',
            updatedAt: 3,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:1',
          kind: 'assistant_text',
          delta: 'Hello world this is the answer',
          updatedAt: 4,
        });
        const textIdx = pane.items.findIndex((i) => i.id === 'text:0:1');

        // While withheld, the successor's reveal is paused at its seed.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        for (let i = 0; i < 5; i++) clock.tickFrame(16);
        expect(pane.items[textIdx].summary).toBe('');

        // Thinking drains → gate advances to the text row, which now
        // reveals from the start.
        for (
          let i = 0;
          i < 200 && pane.revealBoundary?.itemIndex === 0;
          i++
        ) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        for (let i = 0; i < 200 && pane.revealBoundary !== null; i++) {
          clock.tickFrame(16);
        }
        expect(pane.items[textIdx].summary).toBe(
          'Hello world this is the answer',
        );
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('never lets a subagent child become the frontier (no cross-branch gating)', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        // Agent launch (top-level, non-smoothed) + a streaming child thinking.
        pane.upsertItem(
          makeItem({
            id: 'agent:0:0',
            threadId: 't',
            kind: 'tool_call',
            toolName: 'Agent',
            status: 'running',
            turnIndex: 0,
            itemIndex: 0,
            summary: 'Agent',
            updatedAt: 1,
          }),
        );
        pane.upsertItem(
          makeItem({
            id: 'child:0:1',
            threadId: 't',
            kind: 'thinking',
            parentId: 'agent:0:0',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 1,
            summary: '',
            payloadId: 'thinking:child',
            updatedAt: 2,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'child:0:1',
          kind: 'thinking',
          delta: 'subagent reasoning '.repeat(5),
          updatedAt: 3,
        });
        // A subagent descendant must not gate the timeline.
        expect(pane.revealBoundary).toBeNull();

        // A later top-level text becomes the frontier; the child is ignored.
        pane.upsertItem(
          makeItem({
            id: 'text:0:2',
            threadId: 't',
            kind: 'assistant_text',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 2,
            summary: '',
            updatedAt: 4,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:2',
          kind: 'assistant_text',
          delta: 'top level answer',
          updatedAt: 5,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 2 });
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops the gate when the streaming item is interrupted', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Interrupt kills the thinking row → snap + dispose → gate drops.
        pane.applyItemPatch({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'killed', updatedAt: 4 },
        });
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('holds the gate while a completion patch extends the frontier, then drops it once the suffix drains', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: 'hello ',
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Turn-completion patch carries the final text, extending what streamed.
        pane.applyItemPatch({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: {
            status: 'completed',
            summary: 'hello world done',
            updatedAt: 4,
          },
        });
        // Gate still held — the appended suffix hasn't revealed yet.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        for (let i = 0; i < 80; i++) clock.tickFrame(16);
        expect(pane.revealBoundary).toBeNull();
        const text = pane.items.find((i) => i.id === 'text:0:0');
        expect(text?.summary).toBe('hello world done');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('reveals a thinking → text → tool_call chain in order', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'text:0:1',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 1,
            summary: '',
            updatedAt: 3,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:1',
          kind: 'assistant_text',
          delta: 'the answer here',
          updatedAt: 4,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:2',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 2,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 5,
          }),
        );
        // Gate at thinking; text AND tool both withheld.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // thinking drains → gate steps to the text row (not straight to null).
        for (let i = 0; i < 200 && pane.revealBoundary?.itemIndex === 0; i++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        // text drains → gate drops (tool has no smoother, reveals immediately).
        for (let i = 0; i < 200 && pane.revealBoundary !== null; i++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('resumes a paused successor when the frontier row is removed', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'text:0:1',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 1,
            summary: '',
            updatedAt: 3,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:1',
          kind: 'assistant_text',
          delta: 'the answer',
          updatedAt: 4,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Optimistic revert removes the streaming frontier row.
        pane.removeItemById('think:0:0', 't');
        // The withheld successor becomes the frontier and resumes from its start.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        for (let i = 0; i < 60; i++) clock.tickFrame(16);
        expect(pane.items.find((i) => i.id === 'text:0:1')?.summary).toBe(
          'the answer',
        );
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('resets the gate to null on thread switch', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        await pane.switchThread(makeThread({ id: 'other-thread' }));
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('leaves the gate null for a settled thread (no streaming)', async () => {
      const pane = await buildPane(makeThread({ id: 't' }), [
        makeItem({
          id: 'u:0',
          threadId: 't',
          kind: 'user_text',
          role: 'user',
          summary: 'hi',
          turnIndex: 0,
          itemIndex: 0,
        }),
        makeItem({
          id: 'a:1',
          threadId: 't',
          kind: 'assistant_text',
          summary: 'done',
          turnIndex: 0,
          itemIndex: 1,
        }),
      ]);
      expect(pane.revealBoundary).toBeNull();
    });

    it('holds a successor behind a multi-KB reasoning frontier for the whole readable drain', async () => {
      // The contract that replaced BOTH removed shortcuts (the
      // successor-waiting fast-drain, then the bounded-backlog skip): a
      // queued row waits. It waits for every character of the frontier to
      // animate, at no more than the reveal ceiling, and is released only
      // when the frontier is genuinely caught up.
      //
      // This is affordable because the wire is bursty — the drain below
      // runs with NO further appends, which is exactly what a tool call or
      // an API round-trip looks like. Do not "fix" a long wait here by
      // skipping, rushing, or popping the frontier.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        const text = 'word '.repeat(1200); // 6000 chars
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: text,
          updatedAt: 3,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 4,
          }),
        );
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        let revealedLength = 0;
        let frames = 0;
        // The wire gap: frames tick, nothing new arrives.
        while (pane.revealBoundary !== null && frames < 3000) {
          clock.tickFrame(16);
          frames++;
          const length = pane.liveThinkingTailForItem('think:0:0')?.length ?? 0;
          // Every frame is ordinary bounded work — no frame hands over a
          // skipped middle.
          expect(length - revealedLength).toBeLessThanOrEqual(
            MAX_ADVANCE_PER_TICK_CHARS,
          );
          revealedLength = length;
          // The gate may only release on the frame the frontier finishes.
          // Releasing on any earlier frame would mean it popped a still
          // -revealing row.
          if (pane.revealBoundary === null) {
            expect(length).toBe(text.length);
          }
        }
        // Released exactly at catch-up, with every character animated.
        expect(pane.revealBoundary).toBeNull();
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(text);
        // And it took the ceiling-implied time — the wait was paid, not
        // shortened. ~19s of frames for 6000 chars at 320cps.
        expect(frames * 16).toBeGreaterThanOrEqual(
          (text.length / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000,
        );
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('never skips any frontier — it animates every char at the adaptive cap', async () => {
      // The prose counterpart of the test above; the guarantee is
      // kind-independent, so both are asserted. A queued successor waits
      // out the whole reveal, and the reveal stays inside the rate ceiling
      // throughout.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 2,
          }),
        );
        const text = 'word '.repeat(400); // 2000 chars
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: text,
          updatedAt: 3,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 4,
          }),
        );
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        const summaryOf = () =>
          pane.items.find((i) => i.id === 'text:0:0')?.summary ?? '';
        let previousLength = summaryOf().length;
        expect(previousLength).toBe(0);
        let frames = 0;
        while (pane.revealBoundary !== null && frames < 900) {
          clock.tickFrame(16);
          frames++;
          const length = summaryOf().length;
          // Per-frame WORK bound: no frame ever dumps a chunk, and no
          // frame ever jumps a skipped middle.
          expect(length - previousLength).toBeLessThanOrEqual(
            MAX_ADVANCE_PER_TICK_CHARS,
          );
          previousLength = length;
        }
        expect(pane.revealBoundary).toBeNull();
        expect(summaryOf()).toBe(text);
        // Average rate over the whole drain stayed under the ceiling —
        // which also proves nothing was skipped.
        expect(frames * 16).toBeGreaterThanOrEqual(
          (text.length / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000,
        );
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps isItemSmoothing true through the post-completion drain, false once caught up', async () => {
      // The wire settles status to 'completed' while the smoother is
      // still revealing. Render code derives its streaming mode from
      // `status === 'streaming' || isItemSmoothing`, so this signal must
      // hold through the drain tail and clear exactly at catch-up —
      // otherwise ChatMarkdown drops its volatile-tail markdown guards
      // while the text is still visibly growing.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 2,
          }),
        );
        const text = 'word '.repeat(60); // 300 chars
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: text,
          updatedAt: 3,
        });
        pane.applyItemPatch({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: { status: 'completed', summary: text, updatedAt: 4 },
        });

        const item = () => pane.items.find((i) => i.id === 'text:0:0');
        // Status settles immediately; the reveal (and the smoothing
        // signal) keeps draining.
        expect(item()?.status).toBe('completed');
        expect(pane.isItemSmoothing('text:0:0')).toBe(true);
        expect((item()?.summary ?? text).length).toBeLessThan(text.length);

        let frames = 0;
        while (pane.isItemSmoothing('text:0:0') && frames < 500) {
          clock.tickFrame(16);
          frames++;
        }
        expect(frames).toBeGreaterThan(1);
        expect(item()?.summary).toBe(text);
        expect(pane.isItemSmoothing('text:0:0')).toBe(false);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('settleTurn keeps the normal reveal cadence — no end-of-turn rush', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 2,
          }),
        );
        // 2000-char backlog on a solo tail row. The historical
        // end-of-turn fast-drain rushed this at an elevated per-tick cap
        // (~3360 cps) — rushed motion the user read as jank.
        // Deliberately removed: the backlog drains at the same steady
        // cadence as live streaming (adaptive catch-up, ≤
        // MAX_ADAPTIVE_CHARS_PER_SEC).
        const text = 'word '.repeat(400);
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: text,
          updatedAt: 3,
        });
        pane.settleTurn({
          turnId: 'turn-1',
          turnIndex: 0,
          startedAt: 1,
          completedAt: 2,
          stopReason: 'end_turn',
          assistantMessageId: 'text:0:0',
          tokenUsage: null,
          aborted: false,
          errorMessage: '',
        });
        // Inside the historical 800ms drain window the backlog must
        // still be mid-reveal (the rush would have finished it) —
        // advancing steadily, not snapped and not stalled.
        for (let i = 0; i < 60; i++) clock.tickFrame(16);
        const midSummary =
          pane.items.find((i) => i.id === 'text:0:0')?.summary ?? '';
        expect(midSummary.length).toBeGreaterThan(0);
        expect(midSummary.length).toBeLessThan(text.length);
        // At the steady cadence (~5 word-aligned chars/frame while the
        // lag is large, tapering below) the full 2000-char backlog
        // completes within a few hundred frames.
        for (let i = 0; i < 600; i++) clock.tickFrame(16);
        expect(pane.items.find((i) => i.id === 'text:0:0')?.summary).toBe(
          text,
        );
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });

  // The pane data layer is the sole owner of structural-append spring
  // arming (`armStructuralSpring`): a wire append and a reveal-gate
  // release both arm SYNCHRONOUSLY with the data change
  // (bug-report-20260702T193212Z): an effect-based arm runs after the
  // virtualizer's same-flush geometry delivery, so the append's own
  // growth resolved as an instant sync-pin; and the effect's turn-keyed
  // signature is blind to appends landing after turn end (interrupt
  // echo, force-closed tool rows). Each arm also schedules a post-flush
  // 'live-content' observe so growth that never fires a content-geometry
  // delta still gets a bottom re-check.
  describe('scroll-controller registration', () => {
    // The slot is single-occupancy and its only guard is object identity, so
    // it has to survive going through the store unchanged. A plain `$state`
    // proxies it and every `===` against it fails silently: the detach guard
    // stops matching, the slot never empties, and a torn-down controller —
    // holding the detached timeline subtree — stays reachable from the pane.
    it('hands back the same object that registered', () => {
      const pane = createThreadPane();
      const stick = stubScrollController();

      pane.attachScrollController(stick);

      expect(pane.scrollController).toBe(stick);
    });

    it('clears the slot when the surface that registered tears down', () => {
      const pane = createThreadPane();
      const stick = stubScrollController();
      pane.attachScrollController(stick);

      pane.detachScrollController(stick);

      expect(pane.scrollController).toBeNull();
    });

    it('ignores a stale teardown from the surface it already replaced', () => {
      // MessageTimeline → ChannelView (or a fast thread switch): the outgoing
      // surface's teardown can land after the incoming one has registered, and
      // must not disown a live controller. Then the incoming one's own teardown
      // still empties the slot — a guard that rejected everything would look
      // identical here and leak on the last unmount.
      const pane = createThreadPane();
      const outgoing = stubScrollController();
      const incoming = stubScrollController();
      pane.attachScrollController(outgoing);
      pane.attachScrollController(incoming);

      pane.detachScrollController(outgoing);
      expect(pane.scrollController).toBe(incoming);

      pane.detachScrollController(incoming);
      expect(pane.scrollController).toBeNull();
    });
  });

  describe('structural-append arm (pane data layer)', () => {
    function attachMockScrollController(pane: ReturnType<typeof createThreadPane>) {
      const markStructuralContentPending = vi.fn();
      const observe = vi.fn();
      pane.attachScrollController(
        stubScrollController({ observe, markStructuralContentPending }),
      );
      return { markStructuralContentPending, observe };
    }

    it('arms synchronously when a provider upsert appends in-window', async () => {
      const thread = makeThread({ id: 'thread-arm' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({
          id: 'bash-1',
          threadId: thread.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_call',
          role: 'assistant',
          status: 'running',
          toolName: 'Bash',
          summary: 'Bash: ls',
        }),
      ]);

      // No tick/flush before the assertion: the arm must be ordered
      // before the flush in which the virtualizer measures the new row
      // and delivers its geometry sample.
      expect(markStructuralContentPending).toHaveBeenCalledTimes(1);
    });

    it('stamps live content alongside the wire-append arm', async () => {
      // A wire append entering the loaded tail is live content: besides
      // the 250ms one-shot, it must open the full
      // LIVE_CONTENT_ACTIVE_HOLD_MS rolling window so the controller
      // keeps expecting the appended rows' follow-up growth (payload
      // preview, markdown, highlight spans) and holds the spring
      // sentinel alive across the gaps between those deliveries rather
      // than cancelling on each arrival.
      const thread = makeThread({ id: 'thread-arm-stamp' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      attachMockScrollController(pane);
      expect(pane.lastLiveContentAt).toBe(0);

      const before = performance.now();
      pane.applyProviderItemUpserts([
        makeItem({
          id: 'bash-1:completion',
          threadId: thread.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_completion',
          role: 'assistant',
          status: 'completed',
          toolName: 'Bash',
          summary: 'Background command finished',
          completionOf: 'bash-1',
        }),
      ]);
      const after = performance.now();

      // Stamped synchronously with the apply, on the same
      // performance.now() timebase the MessageTimeline latch reads.
      expect(pane.lastLiveContentAt).toBeGreaterThanOrEqual(before);
      expect(pane.lastLiveContentAt).toBeLessThanOrEqual(after);
    });

    it('pane.armStructuralSpring (composer optimistic send) arms without stamping', async () => {
      // The composer's send is deliberately a one-shot: one append wants
      // one spring window, not 500ms of spring eligibility for
      // unrelated reflows.
      const thread = makeThread({ id: 'thread-arm-composer' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      pane.armStructuralSpring();

      expect(markStructuralContentPending).toHaveBeenCalledTimes(1);
      expect(pane.lastLiveContentAt).toBe(0);
    });

    it('does not arm for update-only batches', async () => {
      const thread = makeThread({ id: 'thread-arm-upd' });
      const seed = makeItem({
        id: 'bash-1',
        threadId: thread.id,
        kind: 'tool_call',
        role: 'assistant',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: ls',
      });
      const pane = await buildPane(thread, [seed]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        { ...seed, status: 'completed', updatedAt: seed.updatedAt + 1 },
      ]);

      // Mounted-row updates ride the live-content latch
      // (providerUpsertAdvancesLiveContent), not the one-shot.
      expect(markStructuralContentPending).not.toHaveBeenCalled();
    });

    it('does not arm for below-floor history rows', async () => {
      const thread = makeThread({ id: 'thread-arm-floor' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'tail', threadId: thread.id, turnIndex: 5, itemIndex: 0 }),
      ]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({ id: 'old', threadId: thread.id, turnIndex: 2, itemIndex: 0 }),
      ]);

      // Dropped by the window floor guard — never applied, never armed.
      expect(markStructuralContentPending).not.toHaveBeenCalled();
    });

    it('does not arm while the switch slice is still loading', async () => {
      const threadA = makeThread({ id: 'thread-arm-load-a' });
      const pane = await buildPane(threadA, [
        makeItem({ id: 'a-tail', threadId: threadA.id }),
      ]);
      const { markStructuralContentPending } = attachMockScrollController(pane);

      const threadB = makeThread({ id: 'thread-arm-load-b' });
      const bItem = makeItem({
        id: 'b-0',
        threadId: threadB.id,
        turnIndex: 0,
        itemIndex: 0,
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'thread B first',
      });
      setBindingMock('SwitchThread', async () => threadB);
      let releaseSlice!: (v: {
        items: Item[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }) => void;
      setBindingMock(
        'ListThreadSliceAround',
        () => new Promise((resolve) => { releaseSlice = resolve; }),
      );

      const switching = pane.switchThread(threadB);
      // Let switchThread reach its awaits; the deferred slice keeps
      // `loading` true (cache miss).
      await flushMicrotasks();
      expect(pane.loading).toBe(true);

      // A streaming upsert arriving mid-load must not arm — and must not
      // stamp the latch either (the stamp shares the arm's gates): the
      // whole switch+load settle is a restore, not an in-turn append
      // (bug-report-20260622T041049Z class).
      pane.applyProviderItemUpserts([bItem]);
      expect(markStructuralContentPending).not.toHaveBeenCalled();
      expect(pane.lastLiveContentAt).toBe(0);

      releaseSlice({ items: [bItem], oldestTurnIndex: 0, hasMore: false });
      await switching;

      // A genuine append to the settled window arms (and stamps) again.
      pane.applyProviderItemUpserts([
        makeItem({
          id: 'b-1',
          threadId: threadB.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_call',
          role: 'assistant',
          status: 'running',
          toolName: 'Bash',
          summary: 'Bash: pwd',
        }),
      ]);
      expect(markStructuralContentPending).toHaveBeenCalledTimes(1);
      expect(pane.lastLiveContentAt).toBeGreaterThan(0);
    });

    it("schedules the post-flush 'live-content' nudge alongside the arm", async () => {
      const thread = makeThread({ id: 'thread-arm-nudge' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { observe } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({
          id: 'row-1',
          threadId: thread.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_call',
          role: 'assistant',
          status: 'running',
          toolName: 'Bash',
          summary: 'Bash: ls',
        }),
      ]);

      // Never synchronous: the nudge waits for the Svelte flush plus one
      // frame so the virtualizer has published the new row before the
      // controller re-checks the bottom.
      expect(observe).not.toHaveBeenCalled();
      await vi.waitFor(() => {
        expect(observe).toHaveBeenCalledWith('live-content');
      });
    });

    it('arms when the reveal gate releases a withheld successor', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const thread = makeThread({ id: 'thread-arm-reveal' });
        const pane = await buildPane(thread, [
          makeItem({
            id: 'front',
            threadId: thread.id,
            turnIndex: 0,
            itemIndex: 0,
            kind: 'assistant_text',
            status: 'streaming',
            summary: '',
          }),
        ]);
        const { markStructuralContentPending } = attachMockScrollController(pane);

        // Short delta: enough lag to engage the gate, small enough that
        // the frontier finishes within the drain loop below.
        pane.applyItemDelta({
          threadId: thread.id,
          itemId: 'front',
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        // The successor arms once through the wire-append path and is
        // withheld behind the frontier.
        pane.applyProviderItemUpserts([
          makeItem({
            id: 'tool-1',
            threadId: thread.id,
            turnIndex: 0,
            itemIndex: 1,
            kind: 'tool_call',
            role: 'assistant',
            status: 'running',
            toolName: 'Bash',
            summary: 'Bash: ls',
          }),
        ]);
        markStructuralContentPending.mockClear();

        // Drain the frontier. The boundary drop that RELEASES the tool
        // row mounts it with no wire upsert in that flush, so only the
        // reveal-site arm can make its growth spring-eligible.
        for (let frame = 0; frame < 500 && pane.revealBoundary !== null; frame++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
        expect(markStructuralContentPending).toHaveBeenCalled();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not arm when the gate drops because the lone streaming row drained', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const thread = makeThread({ id: 'thread-arm-lone-drain' });
        const pane = await buildPane(thread, [
          makeItem({
            id: 'front',
            threadId: thread.id,
            turnIndex: 0,
            itemIndex: 0,
            kind: 'assistant_text',
            status: 'streaming',
            summary: '',
          }),
        ]);
        const { markStructuralContentPending } = attachMockScrollController(pane);

        pane.applyItemDelta({
          threadId: thread.id,
          itemId: 'front',
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        // Nothing is waiting behind the frontier: when it drains and the
        // gate drops, no rows mount — arming would open a pointless
        // spring window on whatever grows next.
        for (let frame = 0; frame < 500 && pane.revealBoundary !== null; frame++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
        expect(markStructuralContentPending).not.toHaveBeenCalled();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not arm on discussion-mode panes, whose controller belongs to ChannelView', async () => {
      const thread = makeThread({
        id: 'thread-arm-disc',
        mode: 'discussion',
        discussionId: 'disc-1',
      });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { markStructuralContentPending, observe } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({ id: 'row-1', threadId: thread.id, turnIndex: 0, itemIndex: 1 }),
      ]);

      // The chat timeline is swapped out for ChannelView in discussion
      // mode, so the registered controller watches channel messages —
      // arming it would spring unrelated channel growth for 250ms. The
      // append stamp shares the gate (the pane's timeline latch has no
      // reader on this surface).
      expect(markStructuralContentPending).not.toHaveBeenCalled();
      expect(pane.lastLiveContentAt).toBe(0);
      // Outwait the nudge's flush + frame (and its hidden-window timeout
      // fallback) so a skipped mark that still scheduled the observe
      // would be caught.
      await new Promise((resolve) => setTimeout(resolve, 60));
      expect(observe).not.toHaveBeenCalled();
    });

    it('cancels a scheduled nudge when the thread switches before it fires', async () => {
      const threadA = makeThread({ id: 'thread-nudge-cancel-a' });
      const pane = await buildPane(threadA, [
        makeItem({ id: 'seed', threadId: threadA.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const { observe } = attachMockScrollController(pane);

      pane.applyProviderItemUpserts([
        makeItem({
          id: 'row-1',
          threadId: threadA.id,
          turnIndex: 0,
          itemIndex: 1,
          kind: 'tool_call',
          role: 'assistant',
          status: 'running',
          toolName: 'Bash',
          summary: 'Bash: ls',
        }),
      ]);

      // Switch before the nudge's flush + frame elapses. The nudge's
      // switchGeneration capture must cancel it — a post-switch
      // observe('live-content') would re-check the bottom of a freshly
      // restored, unrelated timeline.
      const threadB = makeThread({ id: 'thread-nudge-cancel-b' });
      setBindingMock('SwitchThread', async () => threadB);
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(threadB);

      await new Promise((resolve) => setTimeout(resolve, 60));
      expect(observe).not.toHaveBeenCalled();
    });

    it('does not arm when a revert removes the frontier and its withheld successor', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const thread = makeThread({ id: 'thread-arm-revert' });
        const pane = await buildPane(thread, [
          makeItem({
            id: 'front',
            threadId: thread.id,
            turnIndex: 1,
            itemIndex: 0,
            kind: 'assistant_text',
            status: 'streaming',
            summary: '',
          }),
        ]);
        const { markStructuralContentPending } = attachMockScrollController(pane);

        pane.applyItemDelta({
          threadId: thread.id,
          itemId: 'front',
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        pane.applyProviderItemUpserts([
          makeItem({
            id: 'tool-1',
            threadId: thread.id,
            turnIndex: 1,
            itemIndex: 1,
            kind: 'tool_call',
            role: 'assistant',
            status: 'running',
            toolName: 'Bash',
            summary: 'Bash: ls',
          }),
        ]);
        expect(pane.revealBoundary).toEqual({ turnIndex: 1, itemIndex: 0 });
        markStructuralContentPending.mockClear();

        // Revert-on-interrupt truncates the tail: frontier AND withheld
        // successor go in one call. The boundary drops, but nothing
        // mounts — the timeline SHRANK — so arming would open a phantom
        // spring window over the revert settle.
        pane.removeItemsFromTurn(1);
        expect(pane.revealBoundary).toBeNull();
        expect(markStructuralContentPending).not.toHaveBeenCalled();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not arm across a switch away from an engaged reveal gate', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const threadA = makeThread({ id: 'thread-arm-switch-a' });
        const pane = await buildPane(threadA, [
          makeItem({
            id: 'front',
            threadId: threadA.id,
            turnIndex: 0,
            itemIndex: 0,
            kind: 'assistant_text',
            status: 'streaming',
            summary: '',
          }),
        ]);
        const { markStructuralContentPending } = attachMockScrollController(pane);

        pane.applyItemDelta({
          threadId: threadA.id,
          itemId: 'front',
          kind: 'assistant_text',
          delta: 'streamed words arriving',
          updatedAt: 2,
        });
        pane.applyProviderItemUpserts([
          makeItem({
            id: 'tool-1',
            threadId: threadA.id,
            turnIndex: 0,
            itemIndex: 1,
            kind: 'tool_call',
            role: 'assistant',
            status: 'running',
            toolName: 'Bash',
            summary: 'Bash: ls',
          }),
        ]);
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        markStructuralContentPending.mockClear();

        // disposeAllSmoothers nulls the boundary directly at switch
        // start (no publish through the reveal pass), and the loading
        // gate covers the slice load — the whole switch must not arm.
        const threadB = makeThread({ id: 'thread-arm-switch-b' });
        setBindingMock('SwitchThread', async () => threadB);
        setBindingMock('ListThreadSliceAround', async () => ({
          items: [],
          oldestTurnIndex: 0,
          hasMore: false,
        }));
        await pane.switchThread(threadB);
        expect(pane.revealBoundary).toBeNull();
        expect(markStructuralContentPending).not.toHaveBeenCalled();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });

  // The warm-up gate is armed at the switch edge, but on the FETCH path
  // the pane then sits empty for the whole round trip — and an empty
  // mount window still delivers a zero-height content-geometry sample,
  // which the gate reads as cascade evidence and opens on ~QUIET_MS
  // later. So by the time the slice lands, the gate is already open and
  // the estimate cascade runs in front of the reader. The pane data
  // layer re-closes it as part of applying that slice, synchronously
  // with the item mutation (see PaneScrollController.armWarmup).
  describe('warm-gate re-arm on initial slice', () => {
    function attachWarmupSpy(pane: ReturnType<typeof createThreadPane>) {
      const armWarmup = vi.fn();
      pane.attachScrollController(stubScrollController({ armWarmup }));
      return armWarmup;
    }

    it('re-arms when the initial slice mounts rows into an empty pane', async () => {
      const pane = createThreadPane();
      const armWarmup = attachWarmupSpy(pane);
      const thread = makeThread({ id: 'thread-cold' });
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'a', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
          makeItem({ id: 'b', threadId: thread.id, turnIndex: 0, itemIndex: 1 }),
        ],
        oldestTurnIndex: 0,
        hasMore: false,
      }));

      await pane.switchThread(thread);

      expect(armWarmup).toHaveBeenCalledTimes(1);
    });

    it('does not re-arm for a thread whose slice is empty', async () => {
      // A brand-new thread mounts nothing to cascade. Holding the gate
      // closed would leave the pane behind an empty 2.5s failsafe and
      // sync-pin the first streamed tokens instead of gliding them.
      const pane = createThreadPane();
      const armWarmup = attachWarmupSpy(pane);
      const thread = makeThread({ id: 'thread-empty' });
      setBindingMock('SwitchThread', async () => thread);
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: 0,
        hasMore: false,
      }));

      await pane.switchThread(thread);

      expect(armWarmup).not.toHaveBeenCalled();
    });

    it('does not re-arm on a cache-restore switch', async () => {
      // Cached items are present synchronously at the switch edge, so
      // the arm made there already covers their mount — and there is no
      // initial slice to apply.
      const thread = makeThread({ id: 'thread-cached' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'a', threadId: thread.id, turnIndex: 0, itemIndex: 0 }),
      ]);
      const other = makeThread({ id: 'thread-other' });
      setBindingMock('SwitchThread', async (id: unknown) =>
        id === thread.id ? thread : other,
      );
      setBindingMock('ListThreadSliceAround', async (threadId: unknown) => ({
        // The restore still syncs, and the page REPLACES the painted
        // rows — so the mock has to answer for the thread being asked
        // about, not with a blanket empty window.
        items:
          threadId === thread.id
            ? [makeItem({ id: 'a', threadId: thread.id, turnIndex: 0, itemIndex: 0 })]
            : [],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(other);

      const armWarmup = attachWarmupSpy(pane);
      await pane.switchThread(thread);

      expect(pane.items.map((it) => it.id)).toEqual(['a']);
      expect(armWarmup).not.toHaveBeenCalled();
    });

    it('does not re-arm for streaming appends or older paging', async () => {
      // Both mount against content the reader is already looking at;
      // hiding that is a blank flash, not a cascade defense.
      const thread = makeThread({ id: 'thread-live' });
      const pane = await buildPane(thread, [
        makeItem({ id: 'seed', threadId: thread.id, turnIndex: 5, itemIndex: 0 }),
      ]);
      const armWarmup = attachWarmupSpy(pane);

      pane.applyProviderItemUpserts([
        makeItem({ id: 'next', threadId: thread.id, turnIndex: 5, itemIndex: 1 }),
      ]);
      expect(armWarmup).not.toHaveBeenCalled();

      setBindingMock('ListThreadItemsBefore', async () => ({
        items: [makeItem({ id: 'older', threadId: thread.id, turnIndex: 4, itemIndex: 0 })],
        oldestTurnIndex: 4,
        hasMore: false,
      }));
      await pane.loadOlder();
      expect(armWarmup).not.toHaveBeenCalled();
    });

    it('re-arms once per switch across a rapid switch away and back', async () => {
      // Each switch's own slice application is its own re-arm; a
      // superseded switch's late response is gen-guarded out and must
      // not add one.
      const pane = createThreadPane();
      const armWarmup = attachWarmupSpy(pane);
      const threadA = makeThread({ id: 'thread-ab-a' });
      const threadB = makeThread({ id: 'thread-ab-b' });
      setBindingMock('SwitchThread', async (id: unknown) =>
        id === threadA.id ? threadA : threadB,
      );
      setBindingMock('ListThreadSliceAround', async (id: unknown) => ({
        items: [makeItem({ id: `${id}-row`, threadId: id as string, turnIndex: 0, itemIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      }));

      const first = pane.switchThread(threadA);
      const second = pane.switchThread(threadB);
      await Promise.all([first, second]);

      expect(armWarmup).toHaveBeenCalledTimes(1);
      expect(pane.threadId).toBe(threadB.id);
    });
  });

  // `pane.lastLiveContentAt` is the source the chat scroll controller
  // reads to decide whether more content is expected imminently
  // (MessageTimeline's liveContentActiveNow → isLiveContentActive).
  // It does not choose spring vs sync-pin — growth always glides.
  // Through the
  // PANE-INTERNAL paths exercised here (`upsertItems`, `applyItemDelta`,
  // `applyItemPatch`), it must advance ONLY on genuine smooth live
  // timeline content arriving — text reveals, final-summary patches —
  // and must NOT advance on thread switch, non-smooth delta growth,
  // bulk history loads, meta-only updates, or the optimistic-send /
  // rollback paths that drive `upsertItems` directly. (The provider
  // upsert fan-out in events.ts additionally stamps visible-field
  // updates to mounted rows of ANY kind — tool output previews,
  // completion chrome; those rules are covered in events.test.ts.)
  // Each test ticks the fake clock to a nonzero base first so a
  // `=== 0` assertion genuinely means "never stamped" rather than
  // "stamped at time 0".
  describe('live-content stamp (scroll animation latch source)', () => {
    // Long backlog so the smoother reveals across many frames (never
    // caught up in 2-3 ticks). 60 words ≈ 230 chars, which at the
    // adaptive ceiling is >40 frames. Short words on purpose: one 16ms
    // frame's budget at the ceiling is ~5 chars, so a 3-char word unit
    // means frames 1-3 each land a reveal (and therefore a stamp).
    const longText = (n: number) =>
      Array.from({ length: n }, (_, i) => `w${i}`).join(' ') + ' ';

    it('stamps on each smoother reveal frame, never on switch/upsert/delta-append', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(100); // base now()=100 so the `=== 0` checks are real
        const pane = await buildPane(makeThread({ id: 'stamp-reveal' }));
        // Switching into a thread (bulk slice load) is not live content.
        expect(pane.lastLiveContentAt).toBe(0);

        pane.upsertItem(
          makeItem({
            id: 'a:0:0',
            threadId: 'stamp-reveal',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'seed ',
            updatedAt: 1,
          }),
        );
        // Creating the streaming row is not yet a reveal.
        expect(pane.lastLiveContentAt).toBe(0);

        pane.applyItemDelta({
          threadId: 'stamp-reveal',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          delta: longText(60),
          updatedAt: 2,
        });
        // A smoothed delta only FEEDS the smoother; the reveal (and its
        // stamp) lands on the next rAF tick, not synchronously here.
        expect(pane.lastLiveContentAt).toBe(0);

        clock.tickFrame(16); // now()=116, first reveal fires onReveal
        expect(pane.lastLiveContentAt).toBe(116);
        clock.tickFrame(16); // now()=132, more words reveal
        expect(pane.lastLiveContentAt).toBe(132);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps stamping through the drain tail after a turn-completed patch', async () => {
      // The bug-2 case: the wire turn completes (getActiveTurn → null)
      // while the smoother still has seconds of word-by-word text to
      // reveal. Those trailing reveals must keep stamping so the tail
      // springs instead of jumping.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'stamp-drain' }));
        pane.upsertItem(
          makeItem({
            id: 'a:0:0',
            threadId: 'stamp-drain',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'seed ',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'stamp-drain',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          delta: longText(120),
          updatedAt: 2,
        });

        clock.tickFrame(16); // now()=16, partial reveal — far from caught up
        expect(pane.lastLiveContentAt).toBe(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        // Turn completes on the wire: a bare status patch with no summary.
        pane.applyItemPatch({
          threadId: 'stamp-drain',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          patch: { status: 'completed', updatedAt: 3 },
        });
        // The bare status patch itself adds no stamp (rigorous no-stamp
        // proof for status/meta patches is the next test); the smoother
        // survives because it is not caught up.
        expect(pane.lastLiveContentAt).toBe(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        // Reveals continue AFTER completion → stamps continue advancing.
        clock.tickFrame(16);
        expect(pane.lastLiveContentAt).toBe(32);
        clock.tickFrame(16);
        expect(pane.lastLiveContentAt).toBe(48);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not stamp on a non-smoothed streaming delta (bypasses the smoother)', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(16); // base now()=16
        const pane = await buildPane(makeThread({ id: 'stamp-nonsmooth' }));
        // tool_call is not a smoothable kind — applyItemDelta writes
        // summary directly. It deliberately does not stamp the spring latch:
        // command output geometry is measured by its own renderer, and
        // sync-pinning is less janky than animating transient estimates.
        pane.upsertItem(
          makeItem({
            id: 'tool:0:0',
            threadId: 'stamp-nonsmooth',
            kind: 'tool_call',
            role: 'assistant',
            status: 'streaming',
            summary: 'out',
            updatedAt: 1,
          }),
        );
        expect(pane.lastLiveContentAt).toBe(0);

        pane.applyItemDelta({
          threadId: 'stamp-nonsmooth',
          itemId: 'tool:0:0',
          kind: 'tool_call',
          delta: 'put',
          updatedAt: 2,
        });
        expect(pane.lastLiveContentAt).toBe(0);
        expect(pane.items[0].summary).toBe('output');
        expect(pane.__itemSmootherCountForTest()).toBe(0); // never smoothed
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('stamps on a direct-summary patch, not on status-only or meta-only patches', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(10); // base now()=10
        const pane = await buildPane(makeThread({ id: 'stamp-patch' }));
        // Settled row: no smoother, so a later summary patch writes
        // directly through applyItemPatch's direct-summary branch.
        pane.upsertItem(
          makeItem({
            id: 'a:0:0',
            threadId: 'stamp-patch',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'completed',
            summary: 'hello',
            updatedAt: 1,
          }),
        );
        expect(pane.lastLiveContentAt).toBe(0);

        // Status-only patch: no summary growth → no stamp.
        pane.applyItemPatch({
          threadId: 'stamp-patch',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          patch: { status: 'errored', updatedAt: 2 },
        });
        expect(pane.lastLiveContentAt).toBe(0);

        // Meta-only patch: no summary growth → no stamp.
        pane.applyItemPatch({
          threadId: 'stamp-patch',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          patch: { meta: '{"pathRefs":[]}' },
        });
        expect(pane.lastLiveContentAt).toBe(0);

        // Direct summary overwrite (no smoother present) → stamps.
        pane.applyItemPatch({
          threadId: 'stamp-patch',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          patch: { summary: 'hello world' },
        });
        expect(pane.lastLiveContentAt).toBe(10);
        expect(pane.items[0].summary).toBe('hello world');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not stamp on applyItemMeta, bulk merge, or direct upsertItems; markLiveContentAdvanced does', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(20); // base now()=20
        // Bulk slice load on switch (mergeMissingItemsById) is history,
        // not live content — must not stamp.
        const pane = await buildPane(makeThread({ id: 'stamp-neg' }), [
          makeItem({ id: 'seed:0:0', threadId: 'stamp-neg', summary: 'pre' }),
        ]);
        expect(pane.lastLiveContentAt).toBe(0);

        // applyItemMeta is the streaming path-link allowlist — meta only,
        // never content height → never stamps.
        pane.applyItemMeta({
          threadId: 'stamp-neg',
          itemId: 'seed:0:0',
          kind: 'assistant_text',
          meta: '{"pathRefs":[]}',
          updatedAt: 2,
        });
        expect(pane.lastLiveContentAt).toBe(0);

        // Driving pane.upsertItems directly (the Composer optimistic-send
        // echo and revertOnInterrupt rollback paths) must NOT stamp — only
        // the events.ts provider fan-out marks live content. This is what
        // keeps a user's own sent message and rollback restores sync-pinned.
        pane.upsertItems([
          makeItem({
            id: 'new:1:0',
            threadId: 'stamp-neg',
            turnIndex: 1,
            kind: 'assistant_text',
            summary: 'fresh',
          }),
        ]);
        expect(pane.lastLiveContentAt).toBe(0);

        // The public seam events.ts calls on a changed provider upsert.
        pane.markLiveContentAdvanced();
        expect(pane.lastLiveContentAt).toBe(20);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('resets lastLiveContentAt on thread switch (no stale stamp bleeds into the next thread)', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(100);
        const pane = await buildPane(makeThread({ id: 'A' }));
        pane.upsertItem(
          makeItem({
            id: 'a:0:0',
            threadId: 'A',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'seed ',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'A',
          itemId: 'a:0:0',
          kind: 'assistant_text',
          delta: longText(60),
          updatedAt: 2,
        });
        clock.tickFrame(16); // reveal stamps A as recently streaming
        expect(pane.lastLiveContentAt).toBe(116);

        // Switch to a settled thread B. A's recent stamp must NOT carry
        // over — otherwise B's late typesetting reflow (which never stamps)
        // would read 'spring' off A's timestamp within the 500ms hold and
        // chase B's settled content. The reset makes the latch read
        // 'instant' for B until B itself streams.
        await pane.switchThread(makeThread({ id: 'B' }));
        expect(pane.lastLiveContentAt).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });
});

describe('size-priors eviction on item mutation', () => {
  // With the self-validating per-row nodeSignature key these evictions are
  // memory housekeeping (a stale row is refused on its own signature mismatch
  // anyway), but they free the entry immediately instead of waiting for the
  // LRU. Guard each call site so a future edit that drops one is caught.
  const seedEntry = {
    width: 0,
    expansionSig: '',
    rows: new Map([['seed', 42]]),
  };

  beforeEach(() => {
    clearAllThreadSizePriorsForTest();
  });

  it('evicts the priors when an item is removed by id', async () => {
    const pane = await buildPane(makeThread({ id: 't' }), [makeItem({ id: 'x', threadId: 't' })]);
    setThreadSizePriors('t', { ...seedEntry });
    expect(peekThreadSizePriorsForTest('t')).toBeTruthy();
    pane.removeItemById('x', 't');
    expect(peekThreadSizePriorsForTest('t')).toBeUndefined();
  });

  it('refuses a removal aimed at a thread the pane no longer holds', async () => {
    // Every caller of removeItemById reaches it across an await or an
    // event hop (the composer's failed-send rollback, the queue-restored
    // event), and `user:<n>` ids collide across threads by construction
    // — the same id names a different row in whatever thread is mounted
    // now. Without the expected-thread guard the rollback lands on the
    // wrong conversation and takes that thread's cached window with it.
    const pane = await buildPane(makeThread({ id: 't' }), [
      makeItem({ id: 'user:1', threadId: 't' }),
    ]);
    installThreadSwitchMocks(makeThread({ id: 'other' }), [
      makeItem({ id: 'user:1', threadId: 'other' }),
    ]);
    await pane.switchThread(makeThread({ id: 'other' }));
    setThreadSizePriors('other', { ...seedEntry });
    expect(pane.items.map((it) => it.id)).toEqual(['user:1']);

    expect(pane.removeItemById('user:1', 't')).toBeNull();

    expect(pane.items.map((it) => it.id)).toEqual(['user:1']);
    expect(peekThreadSizePriorsForTest('other')).toBeTruthy();
  });

  it('evicts the priors when a turn is truncated', async () => {
    const pane = await buildPane(makeThread({ id: 't' }), [
      makeItem({ id: 'x', threadId: 't', turnIndex: 1 }),
    ]);
    setThreadSizePriors('t', { ...seedEntry });
    pane.removeItemsFromTurn(1);
    expect(peekThreadSizePriorsForTest('t')).toBeUndefined();
  });

  it('evicts the priors on a same-thread reswitch', async () => {
    const pane = await buildPane(makeThread({ id: 't' }));
    setThreadSizePriors('t', { ...seedEntry });
    await pane.switchThread(makeThread({ id: 't' }));
    expect(peekThreadSizePriorsForTest('t')).toBeUndefined();
  });
});

describe('removeRevertedItems', () => {
  // Mirrors the `user_message:reverted` contract: turns after the anchor
  // turn always go; within the anchor turn only the event's kept-set
  // survives. See eventsMessageRevert.ts and DeleteConversationFromItem.
  const revertItems = (threadId: string) => [
    makeItem({ id: 'u0', threadId, turnIndex: 0, itemIndex: 0, kind: 'user_text', role: 'user' }),
    makeItem({ id: 'a0', threadId, turnIndex: 0, itemIndex: 1 }),
    makeItem({ id: 'prompt', threadId, turnIndex: 1, itemIndex: 0, kind: 'user_text', role: 'user' }),
    makeItem({ id: 'pre', threadId, turnIndex: 1, itemIndex: 1 }),
    makeItem({ id: 'anchor', threadId, turnIndex: 1, itemIndex: 2, kind: 'user_text', role: 'user' }),
    makeItem({ id: 'tail', threadId, turnIndex: 1, itemIndex: 3 }),
    makeItem({ id: 'u2', threadId, turnIndex: 2, itemIndex: 0, kind: 'user_text', role: 'user' }),
  ];

  it('degenerates to whole-turn removal when the kept-set is empty', async () => {
    const pane = await buildPane(makeThread({ id: 't-rr-empty' }), revertItems('t-rr-empty'));
    const removed = pane.removeRevertedItems(1, []);
    expect(removed.map((it) => it.id)).toEqual(['prompt', 'pre', 'anchor', 'tail', 'u2']);
    expect(pane.items.map((it) => it.id)).toEqual(['u0', 'a0']);
  });

  it('keeps exactly the listed anchor-turn survivors and drops later turns', async () => {
    const pane = await buildPane(makeThread({ id: 't-rr-kept' }), revertItems('t-rr-kept'));
    // Non-contiguous kept-set — the promoted-anchor shape: prompt + the
    // interrupted tail survive, the anchor between them goes.
    const removed = pane.removeRevertedItems(1, ['prompt', 'pre', 'tail']);
    expect(removed.map((it) => it.id)).toEqual(['anchor', 'u2']);
    expect(pane.items.map((it) => it.id)).toEqual(['u0', 'a0', 'prompt', 'pre', 'tail']);
  });

  it('removes pane-only anchor-turn rows absent from the kept-set', async () => {
    // A streamed row the backend never persisted (e.g. an in-flight
    // thinking block) cannot appear in any backend enumeration; the
    // kept-set formulation still removes it.
    const pane = await buildPane(makeThread({ id: 't-rr-ephemeral' }), [
      ...revertItems('t-rr-ephemeral'),
      makeItem({ id: 'ephemeral', threadId: 't-rr-ephemeral', turnIndex: 1, itemIndex: 4, kind: 'thinking' }),
    ]);
    const removed = pane.removeRevertedItems(1, ['prompt', 'pre']);
    expect(removed.map((it) => it.id)).toEqual(['anchor', 'tail', 'ephemeral', 'u2']);
    expect(pane.items.map((it) => it.id)).toEqual(['u0', 'a0', 'prompt', 'pre']);
  });

  it('is idempotent: a second application removes nothing', async () => {
    const pane = await buildPane(makeThread({ id: 't-rr-idem' }), revertItems('t-rr-idem'));
    pane.removeRevertedItems(1, ['prompt']);
    expect(pane.removeRevertedItems(1, ['prompt'])).toEqual([]);
  });
});

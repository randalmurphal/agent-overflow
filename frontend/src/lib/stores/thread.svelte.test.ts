import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import {
  __setSmoothingClockForTest,
  createThreadPane,
  LIVE_TODO_AUTOHIDE_MS,
} from './thread.svelte';
import type { SmoothingClock } from '../markdown/smoothing/PerItemSmoother';
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
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { resetLayoutMetricsForTest, setPaneWidth } from './layoutMetrics.svelte';
import { RHS_PANEL_MIN_WIDTH } from './rhsPanelSlot.svelte';

function nextFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

// FakeClock for smoothing reveal tests. Mirrors the same shape as
// PerItemSmoother.test.ts so per-tick assertions are deterministic.
class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  now(): number { return this.current; }
  schedule(cb: () => void): number {
    const h = this.nextHandle++;
    this.pending.set(h, cb);
    return h;
  }
  cancel(h: number): void { this.pending.delete(h); }
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.values()];
    this.pending.clear();
    for (const cb of toFire) cb();
  }
  pendingCount(): number { return this.pending.size; }
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
  return [
    '```aoflow-design',
    JSON.stringify(payload),
    '```',
  ].join('\n');
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
    resetThreadStatuses();
    resetSendQueueForTest();
    setBindingMock('SwitchThread', async (threadId: unknown) =>
      makeThread({ id: typeof threadId === 'string' ? threadId : 'thread-1' }));
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
    // ListRecentThreadItems is still used by `refreshFromBackend` for the
    // transport-gap recovery path. Default to empty so tests that don't
    // exercise that path don't have to plumb the mock.
    setBindingMock('ListRecentThreadItems', async () => ({
      items: [] as Item[],
      oldestTurnIndex: -1,
      hasMore: false,
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
      const projectB: Project = { ...projectA, id: 'p-2', path: '/tmp/p2', name: 'p2' };

      pane.startDraftPlaceholder(projectA, 'chat');
      const firstPlaceholder = pane.thread;
      expect(firstPlaceholder).not.toBeNull();
      expect(firstPlaceholder!.id.startsWith('draft:')).toBe(true);

      setThreadEnvMode(firstPlaceholder!, 'new-worktree');
      setAttachBranch(firstPlaceholder!, 'feature/x');

      expect(worktreeIntentForThread(firstPlaceholder!).attachBranch).toBe('feature/x');

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
      const pane = await buildPane(makeThread({
        id: 'materialized-draft',
        projectId: 'p-1',
        projectPath: '/tmp/project',
        workspacePath: '/tmp/project',
        mode: 'chat',
        isDraft: true,
      }));

      setThreadEnvMode(pane.thread!, 'new-worktree');
      setAttachBranch(pane.thread!, 'feature/x');
      expect(worktreeIntentForThread(pane.thread!).attachBranch).toBe('feature/x');

      const oldThread = pane.thread!;
      expect(pane.dematerializeEmptyDraftThread()).toBe(true);
      expect(pane.thread?.id).not.toBe(oldThread.id);
      expect(pane.thread?.id.startsWith('draft:')).toBe(true);
      expect(worktreeIntentForThread(oldThread).mode).toBe('local');
      expect(worktreeIntentForThread(pane.thread!).attachBranch).toBe('feature/x');
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

    pane.upsertItem(makeItem({ id: 'user:0', threadId: 'thread-lock', kind: 'user_text', role: 'user' }));
    expect(pane.isLocked).toBe(true);
  });

  it('marks live state as hydrating before the backend switch round-trip returns', async () => {
    const pane = createThreadPane();
    let releaseSwitch!: (value: unknown) => void;
    setBindingMock('SwitchThread', (threadId: unknown) => new Promise((resolve) => {
      releaseSwitch = resolve;
      void threadId;
    }));

    const switching = pane.switchThread(makeThread({ id: 'thread-hydrating' }));
    expect(isThreadLiveStateHydrating('thread-hydrating')).toBe(true);

    releaseSwitch(makeThread({ id: 'thread-hydrating' }));
    await switching;
    expect(isThreadLiveStateHydrating('thread-hydrating')).toBe(false);
  });

  it('loads items and seeds the context window from thread.lastTokenUsage', async () => {
    const pane = createThreadPane();
    const items = [
      makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
      makeItem({ id: 'text:0:0', itemIndex: 1, summary: 'hello back' }),
    ];
    setBindingMock('ListThreadSliceAround', async () => ({
      items,
      oldestTurnIndex: 0,
      hasMore: false,
    }));

    await pane.switchThread(makeThread({
      lastTokenUsage: JSON.stringify({
        usedTokens: 1200,
        maxTokens: 200000,
        contextPercent: 0.6,
      }),
    }));

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
        approvals: [{
          requestId: 'approval-1',
          threadId: 'thread-a',
          toolName: 'Bash',
          description: 'Run command',
          input: { command: 'pwd' },
          title: 'Approve command',
        }],
        userInputs: [{
          requestId: 'input-1',
          threadId: 'thread-a',
          toolName: 'user_input',
          title: 'User Input Required',
          questions: [{
            id: 'scope',
            header: 'Scope',
            question: 'Choose a scope',
            options: [{ label: 'turn', description: 'Apply only to this turn' }],
          }],
        }],
      },
      todo: null,
    }));

    await pane.switchThread(makeThread({ id: 'thread-a' }));

    expect(pane.pendingApprovals.map((request) => request.requestId)).toEqual(['approval-1']);
    expect(pane.pendingUserInputs[0]?.questions[0]?.options?.[0]?.label).toBe('turn');
  });

  it('does not re-add a prompt resolved while pending snapshot hydration is in flight', async () => {
    const pane = createThreadPane();
    let releaseSnapshot!: (value: unknown) => void;
    setBindingMock('GetThreadLiveState', () => new Promise((resolve) => {
      releaseSnapshot = resolve;
    }));

    const switching = pane.switchThread(makeThread({ id: 'thread-a' }));
    await Promise.resolve();
    pane.removeUserInput('input-1');
    releaseSnapshot({
      threadId: 'thread-a',
      activeTurn: null,
      queueItems: [],
      interactive: {
        approvals: [],
        userInputs: [{
          requestId: 'input-1',
          threadId: 'thread-a',
          toolName: 'user_input',
          title: 'User Input Required',
          questions: [{
            id: 'scope',
            header: 'Scope',
            question: 'Choose a scope',
          }],
        }],
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
    pane.openDiffSidebar({ payloadId: 'p1' });

    await pane.switchThread(makeThread({ id: 'thread-b' }));

    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.generalError).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.showPlanSidebar).toBe(false);
    expect(pane.activeDiffPayload).toBeNull();
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

  describe('right-side panel mutex', () => {
    it('opening plan sidebar closes diff panel and diff sidebar', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.setDiffPanelOpen(true);
      expect(pane.diffPanel.open).toBe(true);

      pane.setShowPlanSidebar(true);
      expect(pane.showPlanSidebar).toBe(true);
      expect(pane.diffPanel.open).toBe(false);
      expect(pane.activeDiffPayload).toBeNull();

      pane.openDiffSidebar({ payloadId: 'p1' });
      expect(pane.activeDiffPayload).toEqual({ payloadId: 'p1' });
      expect(pane.showPlanSidebar).toBe(false);
    });

    it('opening diff panel closes plan sidebar and diff sidebar', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.openDiffSidebar({ payloadId: 'p1', filePath: 'src/foo.ts' });
      pane.setShowPlanSidebar(true);
      pane.setDiffPanelOpen(true);

      expect(pane.diffPanel.open).toBe(true);
      expect(pane.showPlanSidebar).toBe(false);
      expect(pane.activeDiffPayload).toBeNull();
    });

    it('opening diff sidebar closes plan sidebar and diff panel', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.setShowPlanSidebar(true);
      pane.setDiffPanelOpen(true);
      pane.openDiffSidebar({ payloadId: 'p1' });

      expect(pane.activeDiffPayload).toEqual({ payloadId: 'p1' });
      expect(pane.showPlanSidebar).toBe(false);
      expect(pane.diffPanel.open).toBe(false);
    });

    it('opening design preview closes other RHS panels on design threads', async () => {
      const pane = await buildPane(makeThread({ id: 't', mode: 'design' }));

      pane.setShowPlanSidebar(true);
      pane.setShowDesignPreviewPanel(true);

      expect(pane.showDesignPreviewPanel).toBe(true);
      expect(pane.showPlanSidebar).toBe(false);
      expect(pane.activeRhsPanel).toEqual({ kind: 'design-preview' });
    });

    it('diff panels do not open on design threads', async () => {
      const pane = await buildPane(makeThread({ id: 't', mode: 'design' }));

      pane.toggleDiffPanel();
      expect(pane.diffPanel.open).toBe(false);
      expect(pane.activeRhsPanel).toBeNull();

      pane.setDiffPanelOpen(true);
      expect(pane.diffPanel.open).toBe(false);
      expect(pane.activeRhsPanel).toBeNull();

      pane.openDiffSidebar({ payloadId: 'p1' });
      expect(pane.activeDiffPayload).toBeNull();
      expect(pane.activeRhsPanel).toBeNull();
    });

    it('closeRhsPanel closes whichever RHS panel kind is active', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.setShowPlanSidebar(true);
      expect(pane.activeRhsPanel?.kind).toBe('plan');
      pane.closeRhsPanel();
      expect(pane.activeRhsPanel).toBeNull();

      pane.setDiffPanelOpen(true);
      expect(pane.activeRhsPanel?.kind).toBe('diff-checkpoint');
      pane.closeRhsPanel();
      expect(pane.activeRhsPanel).toBeNull();
      expect(pane.diffPanel.open).toBe(false);

      pane.openDiffSidebar({ payloadId: 'p1' });
      expect(pane.activeRhsPanel?.kind).toBe('diff-payload');
      pane.closeRhsPanel();
      expect(pane.activeRhsPanel).toBeNull();

      const designPane = await buildPane(makeThread({ id: 'design-t', mode: 'design' }));
      designPane.setShowDesignPreviewPanel(true);
      expect(designPane.activeRhsPanel?.kind).toBe('design-preview');
      designPane.closeRhsPanel();
      expect(designPane.activeRhsPanel).toBeNull();
    });

    it('togglePlanSidebar respects mutex when opening', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.setDiffPanelOpen(true);
      pane.togglePlanSidebar();

      expect(pane.showPlanSidebar).toBe(true);
      expect(pane.diffPanel.open).toBe(false);
    });

    it('toggleDiffPanel respects mutex when opening', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.openDiffSidebar({ payloadId: 'p1' });
      pane.toggleDiffPanel();

      expect(pane.diffPanel.open).toBe(true);
      expect(pane.activeDiffPayload).toBeNull();
    });

    it('toggleDiffPanel opens on the workspace tab by default and honors an explicit tab', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      // No-arg toggle (header diff badge + diff.panel.toggle keybinding) lands
      // on the workspace tab.
      pane.toggleDiffPanel();
      expect(pane.diffPanel.open).toBe(true);
      expect(pane.diffPanel.tabMode).toBe('workspace');

      // Toggling again closes it.
      pane.toggleDiffPanel();
      expect(pane.diffPanel.open).toBe(false);

      // An explicit tab is respected on open.
      pane.toggleDiffPanel('messages');
      expect(pane.diffPanel.open).toBe(true);
      expect(pane.diffPanel.tabMode).toBe('messages');
    });

    it('closeActivePanel clears all three panel flags', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 't' }));

      pane.openDiffSidebar({ payloadId: 'p1' });
      pane.closeActivePanel();
      expect(pane.activeDiffPayload).toBeNull();

      pane.setShowPlanSidebar(true);
      pane.closeActivePanel();
      expect(pane.showPlanSidebar).toBe(false);

      pane.setDiffPanelOpen(true);
      pane.closeActivePanel();
      expect(pane.diffPanel.open).toBe(false);
    });

    it('closeActivePanel drops the diff-sidebar snapshot when the diff sidebar was the active panel', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      pane.recordDiffSidebarUI({
        viewMode: 'split',
        wordWrap: false,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 50,
      });

      // Close while the sidebar is the active panel — explicit close.
      pane.closeActivePanel();

      // Snapshot was dropped: switching away and back should not restore.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.activeDiffPayload).toBeNull();
      expect(pane.consumeDiffSidebarRestoreState()).toBeNull();
    });

    it('closeActivePanel keeps the thread width but clears the restore target', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      pane.setRhsSidebarWidthLive(620);
      pane.recordDiffSidebarUI({
        viewMode: 'split',
        wordWrap: false,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 50,
      });
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));

      pane.closeActivePanel();

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.activeDiffPayload).toBeNull();
      expect(pane.showPlanSidebar).toBe(false);
      expect(pane.rhsSidebarWidth).toBe(620);
    });
  });

  describe('right-side panel per-thread persistence', () => {
    it('restores the plan sidebar when switching back to its thread', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      expect(pane.showPlanSidebar).toBe(false);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.showPlanSidebar).toBe(true);
      expect(pane.activeRhsPanel).toEqual({ kind: 'plan' });
    });

    it('restores the checkpoint diff panel when switching back to its thread', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setDiffPanelOpen(true);

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      expect(pane.diffPanel.open).toBe(false);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.diffPanel.open).toBe(true);
      expect(pane.activeRhsPanel).toEqual({ kind: 'diff-checkpoint' });
    });

    it('does not auto-open design preview for a fresh design thread', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-a', mode: 'design' }));

      expect(pane.showDesignPreviewPanel).toBe(false);
      expect(pane.activeRhsPanel).toBeNull();
    });

    it('does not auto-open design preview when options hydrate while closed', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-a', mode: 'design' }));
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
      expect(pane.activeRhsPanel).toBeNull();

      pane.toggleDesignPreviewPanel();
      expect(pane.activeRhsPanel).toEqual({ kind: 'design-preview' });
    });

    it('restores design preview only after the user opened it for that thread', async () => {
      const threadA = makeThread({ id: 'thread-a', mode: 'design' });
      const threadB = makeThread({ id: 'thread-b', mode: 'design' });
      const pane = await buildPane(threadA);
      pane.setShowDesignPreviewPanel(true);

      setBindingMock('SwitchThread', async () => threadB);
      await pane.switchThread(threadB);
      expect(pane.showDesignPreviewPanel).toBe(false);

      setBindingMock('SwitchThread', async () => threadA);
      await pane.switchThread(threadA);
      expect(pane.showDesignPreviewPanel).toBe(true);
      expect(pane.activeRhsPanel).toEqual({ kind: 'design-preview' });
    });

    it('restores right-sidebar width per thread', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);
      pane.setRhsSidebarWidthLive(620);
      await pane.switchThread(makeThread({ id: 'thread-b' }));

      pane.setDiffPanelOpen(true);
      pane.setRhsSidebarWidthLive(590);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.rhsSidebarWidth).toBe(620);
      expect(pane.showPlanSidebar).toBe(true);

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      expect(pane.rhsSidebarWidth).toBe(590);
      expect(pane.diffPanel.open).toBe(true);
    });

    it('clamps right-sidebar width against the owning pane width', () => {
      setPaneWidth('left', 1000);
      setPaneWidth('right', 1400);
      const leftPane = createThreadPane({ paneId: 'left' });
      const rightPane = createThreadPane({ paneId: 'right' });

      leftPane.setRhsSidebarWidthLive(900);
      rightPane.setRhsSidebarWidthLive(900);

      expect(leftPane.getRhsSidebarMaxWidth()).toBe(500);
      expect(leftPane.rhsSidebarWidth).toBe(500);
      expect(rightPane.getRhsSidebarMaxWidth()).toBe(900);
      expect(rightPane.rhsSidebarWidth).toBe(900);
    });

    it('restores activeDiffPayload when switching back to a previously-open thread', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      pane.recordDiffSidebarUI({
        viewMode: 'split',
        wordWrap: true,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 120,
      });

      // Switch away — snapshot is captured.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      expect(pane.activeDiffPayload).toBeNull();

      // Switch back — sidebar re-arms with the saved payload + UI state.
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.activeDiffPayload).toEqual({ payloadId: 'pa', filePath: 'src/foo.ts' });

      const restored = pane.consumeDiffSidebarRestoreState();
      expect(restored).toEqual({
        viewMode: 'split',
        wordWrap: true,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 120,
      });
      // Consume is one-shot — second call returns null.
      expect(pane.consumeDiffSidebarRestoreState()).toBeNull();
    });

    it('reopening the active payload preserves recorded UI for switch-back restore', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      pane.recordDiffSidebarUI({
        viewMode: 'split',
        wordWrap: true,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 180,
      });

      pane.openDiffSidebar({ payloadId: 'pa', filePath: 'src/foo.ts' });
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));

      expect(pane.activeDiffPayload).toEqual({ payloadId: 'pa', filePath: 'src/foo.ts' });
      expect(pane.consumeDiffSidebarRestoreState()).toEqual({
        viewMode: 'split',
        wordWrap: true,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 180,
      });
    });

    it('does not restore on switch-back if user explicitly closed the sidebar', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa' });
      pane.recordDiffSidebarUI({
        viewMode: 'stacked',
        wordWrap: false,
        expandedFiles: [],
        scrollTop: 0,
      });
      pane.closeRhsPanel();

      await pane.switchThread(makeThread({ id: 'thread-b' }));
      await pane.switchThread(makeThread({ id: 'thread-a' }));

      expect(pane.activeDiffPayload).toBeNull();
      expect(pane.consumeDiffSidebarRestoreState()).toBeNull();
    });

    it('LRU-evicts oldest entries past the cap', async () => {
      // The pane's cap is 20. Open + switch 22 distinct threads, then
      // switch back to the first — its snapshot should have evicted.
      const pane = createThreadPane();
      const threadCount = 22;
      const threads = Array.from({ length: threadCount }, (_, i) => makeThread({ id: `t${i}` }));

      for (let i = 0; i < threadCount; i += 1) {
        const next = threads[i];
        if (next === undefined) continue;
        await pane.switchThread(next);
        pane.openDiffSidebar({ payloadId: `p${i}` });
        pane.recordDiffSidebarUI({
          viewMode: 'stacked',
          wordWrap: false,
          expandedFiles: [],
          scrollTop: i * 10,
        });
      }

      // Switch one more time to flush the last open into the map.
      await pane.switchThread(makeThread({ id: 'flush' }));

      // Switching away from the flush thread records it too, so t2 is evicted
      // before restore. t3 is still retained.
      await pane.switchThread(threads[3]!);
      expect(pane.activeDiffPayload).toEqual({ payloadId: 'p3' });

      await pane.switchThread(threads[0]!);
      expect(pane.activeDiffPayload).toBeNull();
    });

    it('clear() wipes the per-thread snapshot map', async () => {
      const pane = createThreadPane();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.openDiffSidebar({ payloadId: 'pa' });
      pane.recordDiffSidebarUI({
        viewMode: 'stacked',
        wordWrap: false,
        expandedFiles: [],
        scrollTop: 0,
      });

      pane.clear();
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.activeDiffPayload).toBeNull();
    });
  });

  it('ignores stale initial-load resolutions after a second thread switch', async () => {
    const pane = createThreadPane();
    type Paged = { items: Item[]; oldestTurnIndex: number; hasMore: boolean };
    let resolveA!: (paged: Paged) => void;
    let resolveB!: (paged: Paged) => void;
    const listA = new Promise<Paged>((resolve) => { resolveA = resolve; });
    const listB = new Promise<Paged>((resolve) => { resolveB = resolve; });

    setBindingMock('ListThreadSliceAround', (threadId: string) => (
      threadId === 'thread-a' ? listA : listB
    ));

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

    expect(pane.items.map((item) => item.id)).toEqual(['first', 'early', 'late']);

    pane.upsertItem(makeItem({ id: 'early', turnIndex: 0, itemIndex: 1, summary: 'updated' }));

    expect(pane.items.map((item) => item.id)).toEqual(['first', 'early', 'late']);
    expect(pane.items.find((item) => item.id === 'early')?.summary).toBe('updated');
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

    expect(pane.items.map((item) => item.id)).toEqual(['first', 'early', 'late']);
    expect(pane.timelineRevision).toBe(1);

    pane.upsertItems([
      makeItem({ id: 'late', turnIndex: 0, itemIndex: 2, summary: 'moved' }),
      makeItem({ id: 'early', turnIndex: 0, itemIndex: 1, summary: 'updated' }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual(['first', 'early', 'late']);
    expect(pane.items.find((item) => item.id === 'late')?.summary).toBe('moved');
    expect(pane.timelineRevision).toBe(2);
  });

  it('bumps timeline revision when switchThread installs the initial item window', async () => {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [makeItem({ id: 'loaded', threadId: 't', turnIndex: 0, itemIndex: 0 })],
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
        items: [makeItem({ id: `${id}-row`, threadId: id, turnIndex: 0, itemIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      };
    });

    await pane.switchThread(makeThread({ id: 't' }));
    await pane.switchThread(makeThread({ id: 'other' }));
    const revisionBeforeCacheRestore = pane.timelineRevision;

    await pane.switchThread(makeThread({ id: 't' }));

    expect(loadCalls).toEqual(['t', 'other']);
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
    pane.upsertItem(makeItem({
      id: 'bash',
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: sleep 1',
      meta: JSON.stringify({ input: { command: 'sleep 1' } }),
    }));
    const revision = pane.timelineRevision;

    pane.upsertItem(makeItem({
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
    }));

    expect(pane.items[0].status).toBe('completed');
    expect(pane.items[0].payloadKind).toBe('command_output');
    expect(pane.timelineRevision).toBe(revision);
  });

  it('does not bump timeline revision for collab-agent status-only chrome', () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'agent',
      kind: 'tool_call',
      status: 'running',
      toolName: 'collab_agent',
      meta: JSON.stringify({ input: { tool: 'spawn_agent', receiverThreadIds: ['child-1'] } }),
      payloadMeta: JSON.stringify({ input: { newAgentNickname: 'Reviewer' } }),
    }));
    const revision = pane.timelineRevision;

    pane.upsertItem(makeItem({
      id: 'agent',
      kind: 'tool_call',
      status: 'completed',
      toolName: 'collab_agent',
      meta: JSON.stringify({ input: { tool: 'spawn_agent', receiverThreadIds: ['child-1'] } }),
      payloadMeta: JSON.stringify({ input: { newAgentNickname: 'Reviewer' } }),
      updatedAt: 1,
    }));

    expect(pane.items[0].status).toBe('completed');
    expect(pane.timelineRevision).toBe(revision);
  });

  it('bumps timeline revision when an upsert changes timeline structure', () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'read',
      kind: 'tool_call',
      toolName: 'Read',
    }));
    const revision = pane.timelineRevision;

    pane.upsertItem(makeItem({
      id: 'read',
      kind: 'tool_call',
      toolName: 'Edit',
    }));

    expect(pane.timelineRevision).toBe(revision + 1);
  });

  it('preserves arrival order for rows with the same turn and item position', () => {
    const pane = createThreadPane();

    pane.upsertItems([
      makeItem({ id: 'later-position', turnIndex: 1, itemIndex: 0 }),
      makeItem({ id: 'first-arrived', turnIndex: 0, itemIndex: 0, createdAt: 200 }),
      makeItem({ id: 'second-arrived', turnIndex: 0, itemIndex: 0, createdAt: 100 }),
    ]);

    expect(pane.items.map((item) => item.id)).toEqual([
      'first-arrived',
      'second-arrived',
      'later-position',
    ]);
  });

  it('applies streaming deltas in place via replace-pattern', async () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'hello',
    }));
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
    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe('hello world!');
    expect(pane.items.find((item) => item.id === 'text:0:0')?.updatedAt).toBe(124);
  });

  it('thinking-row deltas trim to the 400-rune tail in place', async () => {
    // The frontend mirrors the server-side `thinkingPreviewRunes = 400`
    // cap so the completion upsert (which carries the same tail) does
    // not visibly shrink the row at settle. Full thinking content stays
    // on-demand via the expansion handle.
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'seed',
      payloadId: 'thinking-payload',
    }));

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

    const after = pane.items.find((item) => item.id === 'think:0:0')?.summary ?? '';
    expect([...after].length).toBe(400);
    expect(after.endsWith('a'.repeat(400))).toBe(true);
    expect(pane.items.find((item) => item.id === 'think:0:0')?.updatedAt).toBe(100);
  });

  it('replaces the streaming row on completion upsert', async () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
      summary: 'hello',
    }));
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
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'completed',
      summary: 'hello world',
    }));

    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe('hello world');
  });

  it('ignores stale deltas for an item that already settled', async () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'completed',
      summary: 'yield timeouts',
    }));

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: 'outs',
      updatedAt: 124,
    });

    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe('yield timeouts');
  });

  it('expansionStateFor returns the same handle across calls (survives row remount)', () => {
    // Why: virtua's overscan eviction unmounts a row component when it
    // scrolls past the buffer; remounting reconstructs the snippet's
    // closure-scoped $state from scratch. The pane registry returns
    // the SAME handle reference for the same itemId, so toggle state
    // and loaded chunks survive the round-trip.
    const pane = createThreadPane();
    const item = makeItem({ id: 'tool:5:0', kind: 'tool_call', payloadId: 'p-foo' });
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
    const first = pane.expansionStateForPayload('p-versioned', 'thread-1', version);
    await first.expand();
    expect(first.displayData).toBe('payload v1');

    version = 2;
    const second = pane.expansionStateForPayload('p-versioned', 'thread-1', version);
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
    // own onDestroy-revoking factory. virtua's overscan eviction would
    // unmount + remount the row on a back-scroll, refetching every
    // attachment from Go. The pane-owned cache survives remount; the
    // factory seeds from it and writes loaded previews back.
    const pane = createThreadPane();
    const cacheA = pane.attachmentCacheFor('item-1');
    cacheA.set('att-1', { id: 'att-1', filename: 'a.png', mimeType: 'image/png', size: 1, url: 'data:img' });
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
    pane.upsertItem(makeItem({ id: 'tool:0:0', kind: 'tool_call', payloadId: 'p-1', threadId: 'thread-a' }));
    expect(pane.items.length).toBe(1);
    const h1 = pane.expansionStateFor(pane.items[0]);
    pane.toggleSubagentGroupExpanded('parent-x');
    expect(pane.isSubagentGroupExpanded('parent-x')).toBe(true);

    await pane.switchThread(makeThread({ id: 'thread-b' }));
    pane.upsertItem(makeItem({ id: 'tool:0:0', kind: 'tool_call', payloadId: 'p-2', threadId: 'thread-b' }));
    const h2 = pane.expansionStateFor(pane.items[0]);
    // Different thread → different handle (the previous one was cleared).
    expect(h2).not.toBe(h1);
    // SubagentGroup state was cleared too.
    expect(pane.isSubagentGroupExpanded('parent-x')).toBe(false);
  });

  it('clears discussion channel state on switchThread', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));
    pane.mergeChannelMessages([{
      id: 'channel-message-1',
      channelId: 'channel-1',
      sequence: 1,
      fromType: 'agent',
      fromId: 'agent-1',
      fromRole: 'advocate',
      content: 'channel text',
      createdAt: 0,
    }]);
    pane.setChannelStatus('concluded');

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
      makeThread({ id: typeof threadId === 'string' ? threadId : 'design-a', mode: 'design' }));
    const clarification = (questionId: string) => designFence({
      kind: 'clarification_request',
      requestId: 'clarify-same-id',
      questions: [{
        id: questionId,
        prompt: 'Choose',
        choices: [{ id: 'yes', label: 'Yes' }],
      }],
    });

    await pane.switchThread(makeThread({ id: 'design-a', mode: 'design' }));
    pane.upsertItem(makeItem({
      id: 'assistant-a',
      threadId: 'design-a',
      kind: 'assistant_text',
      summary: clarification('first-thread'),
    }));
    expect(pane.pendingClarification?.questions[0]?.id).toBe('first-thread');

    await pane.switchThread(makeThread({ id: 'design-b', mode: 'design' }));
    pane.upsertItem(makeItem({
      id: 'assistant-b',
      threadId: 'design-b',
      kind: 'assistant_text',
      summary: clarification('second-thread'),
    }));

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
    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);

    // A running foreground tool_call alone doesn't flip the flag either.
    pane.upsertItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: false,
    }));
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
      activeTurn: {
        threadId,
        turnId: 'round-1',
        turnIndex: 4,
        startedAt: 1_700_000_000_000,
      },
      queueItems: [{
        id: 'queue-1',
        threadId,
        message: 'queued while working',
        attachmentIds: ['att-1'],
        enqueuedAt: 1_700_000_000_100,
      }],
      flushedItems: [{
        queueItemId: 'queue-flushed',
        userItemId: 'user:4:flush:1',
        message: 'already sent to provider',
      }],
      interactive: {
        approvals: [{
          requestId: 'approval-1',
          threadId,
          toolName: 'Edit',
          description: 'Allow edit?',
          input: null,
          title: 'Approve edit',
        }],
        userInputs: [],
      },
      todo: {
        threadId,
        steps: [{ step: 'keep working', status: 'inProgress' }],
        updatedAt: Date.now(),
      },
    }));

    await pane.switchThread(makeThread({ id: 'thread-live' }));

    expect(getActiveTurn('thread-live')).toEqual({
      turnId: 'round-1',
      turnIndex: 4,
      startedAt: 1_700_000_000_000,
    });
    expect(getQueueForThread('thread-live')).toEqual([{
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
    }]);
    expect(getFlushedForThread('thread-live').map((item) => ({
      queueItemId: item.queueItemId,
      userItemId: item.userItemId,
      message: item.message,
    }))).toEqual([{
      queueItemId: 'queue-flushed',
      userItemId: 'user:4:flush:1',
      message: 'already sent to provider',
    }]);
    expect(pane.pendingApprovals.map((approval) => approval.requestId)).toEqual(['approval-1']);
    expect(getThreadStatus('thread-live')).toBe('pending-approval');
    expect(pane.liveTodo?.steps).toEqual([{ step: 'keep working', status: 'inProgress' }]);
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
    setBindingMock('GetThreadLiveState', () => new Promise((resolve) => {
      releaseSnapshot = resolve;
    }));

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

  it('does not let an older live-state hydration apply after a newer one completed', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-hydration-order' }));

    const releases: Array<(value: unknown) => void> = [];
    setBindingMock('GetThreadLiveState', () => new Promise((resolve) => {
      releases.push(resolve);
    }));

    const older = pane.refreshFromBackend();
    for (let i = 0; i < 4 && releases.length < 1; i += 1) await Promise.resolve();
    const newer = pane.refreshFromBackend();
    for (let i = 0; i < 4 && releases.length < 2; i += 1) await Promise.resolve();
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
      queueItems: [{
        id: 'stale-queue',
        threadId: 'thread-hydration-order',
        message: 'stale',
        attachmentIds: [],
        enqueuedAt: 1,
      }],
      interactive: {
        approvals: [{
          requestId: 'stale-approval',
          threadId: 'thread-hydration-order',
          toolName: 'Edit',
          description: 'stale',
          input: null,
          title: 'Stale',
        }],
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
    setBindingMock('GetThreadLiveState', () => new Promise((resolve) => {
      releaseSnapshot = resolve;
    }));

    const switching = pane.switchThread(makeThread({ id: 'thread-queue-race' }));
    await Promise.resolve();
    replaceQueueForThread('thread-queue-race', [{
      id: 'queue-new',
      threadId: 'thread-queue-race',
      message: 'newer queue',
      attachmentIds: [],
      sourceProposedPlan: null,
      revisionSourceProposedPlan: null,
      enqueuedAt: 5,
    }]);
    releaseSnapshot({
      threadId: 'thread-queue-race',
      activeTurn: null,
      queueItems: [],
      interactive: { approvals: [], userInputs: [] },
      todo: null,
    });
    await switching;

    expect(getQueueForThread('thread-queue-race').map((item) => item.message)).toEqual(['newer queue']);
  });

  it('clear resets the pane completely', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread());
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    const item = makeItem({ id: 'x', kind: 'tool_call', payloadId: 'payload-x' });
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
    pane.mergeChannelMessages([{
      id: 'channel-message-1',
      channelId: 'channel-1',
      sequence: 1,
      fromType: 'agent',
      fromId: 'agent-1',
      fromRole: 'advocate',
      content: 'channel text',
      createdAt: 0,
    }]);
    pane.setChannelStatus('concluded');
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
    expect(pane.attachmentCacheFor(item.id).get('attachment-x')).toBeUndefined();
    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:pane-clear');
    revoke.mockRestore();
  });

  describe('windowed history', () => {
    it('upsertItem drops new items below the window floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({ id: 'at-floor', threadId: 'thread-windowed', turnIndex: 5, itemIndex: 0 }),
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
      pane.upsertItem(makeItem({ id: 'below', threadId: 'thread-windowed', turnIndex: 2, itemIndex: 0 }));
      expect(pane.items.map((it) => it.id)).toEqual(['at-floor']);
    });

    it('upsertItem still accepts replacements for known ids below the floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({ id: 'known', threadId: 't', turnIndex: 5, itemIndex: 0, summary: 'old' }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: seed,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      // Known id, turn below floor — cross-turn correction path. Must
      // still replace because the id is clearly in-window already.
      pane.upsertItem(makeItem({ id: 'known', threadId: 't', turnIndex: 2, itemIndex: 0, summary: 'new' }));
      expect(pane.items.find((it) => it.id === 'known')?.summary).toBe('new');
    });

    it('upsertItem rejects new streaming rows below the floor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'at-floor', threadId: 't', turnIndex: 5, itemIndex: 0 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      pane.upsertItem(makeItem({
        id: 'below-streaming',
        threadId: 't',
        turnIndex: 2,
        itemIndex: 0,
        status: 'streaming',
        summary: 'old output',
      }));

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
      setBindingMock('ListItemsBeforeTurn', async () => ({
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
      expect(result).toEqual({ status: 'loaded', insertedBeforeWindow: true, insertedRows: true });
    });

    it('loadOlder is a no-op when hasMoreHistory is false', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'a', turnIndex: 0, itemIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      let calls = 0;
      setBindingMock('ListItemsBeforeTurn', async () => {
        calls += 1;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();
      expect(calls).toBe(0);
      expect(result).toEqual({ status: 'noop', insertedBeforeWindow: false, insertedRows: false });
    });

    it('loadOlder guards against a thread swap mid-fetch', async () => {
      const pane = createThreadPane();
      let resolveOlder!: (v: {
        items: Item[]; oldestTurnIndex: number; hasMore: boolean;
      }) => void;
      const olderPromise = new Promise<{
        items: Item[]; oldestTurnIndex: number; hasMore: boolean;
      }>((r) => { resolveOlder = r; });
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', () => olderPromise);

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
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 't5', threadId: 't', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async (_threadId: string, itemId: string) =>
        itemId === 'target'
          ? makeItem({ id: 'target', threadId: 't', turnIndex: 1 })
          : null,
      );
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({ id: 'target', threadId: 't', turnIndex: 1 }),
          makeItem({ id: 't2', threadId: 't', turnIndex: 2 }),
          makeItem({ id: 't3', threadId: 't', turnIndex: 3 }),
          makeItem({ id: 't4', threadId: 't', turnIndex: 4 }),
        ],
        oldestTurnIndex: 1,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      const revisionBeforeLoadUntil = pane.timelineRevision;
      const ok = await pane.loadUntilItem('target');

      expect(ok).toBe(true);
      expect(pane.timelineRevision).toBeGreaterThan(revisionBeforeLoadUntil);
      expect(pane.oldestLoadedTurnIndex).toBe(1);
      expect(pane.items.map((it) => it.id)).toEqual(['target', 't2', 't3', 't4', 't5']);
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
      pane.requestScrollToItem('checkpoint-user-message', { flash: true });

      expect(pane.scrollToItemRequest.itemId).toBe('checkpoint-user-message');
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
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );
      let beforeTurnCalled: number | null = null;
      setBindingMock('ListItemsBeforeTurn', async (_id, beforeTurn) => {
        beforeTurnCalled = beforeTurn as number;
        return {
          items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
          oldestTurnIndex: 3,
          hasMore: false,
        };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();

      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(beforeTurnCalled).toBe(4);
      expect(pane.items.some((it) => it.id === 'deep')).toBe(true);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
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
      setBindingMock('ListItemsBeforeTurn', async () => {
        paged += 1;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));

      const ok = await pane.loadUntilItem('wrong');
      expect(ok).toBe(false);
      expect(paged).toBe(0);
    });

    it('loadOlder disables hasMoreHistory when the backend cannot advance the floor', async () => {
      // Pathological scenario: turns table claims more history exists
      // but the item range [newFloor, beforeTurn) is empty (a sparse
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
      setBindingMock('ListItemsBeforeTurn', async () => {
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
        items: ReturnType<typeof makeItem>[]; oldestTurnIndex: number; hasMore: boolean;
      }) => void;
      const olderPending = new Promise<{
        items: ReturnType<typeof makeItem>[]; oldestTurnIndex: number; hasMore: boolean;
      }>((r) => { releaseOlder = r; });
      setBindingMock('ListItemsBeforeTurn', () => olderPending);

      await pane.switchThread(makeThread({ id: 't' }));
      const olderPromise = pane.loadOlder();
      expect(pane.loadingOlder).toBe(true);

      // Kick off loadUntilItem, which increments pagingGeneration and
      // takes its own path. It must not deadlock loadOlder's cleanup.
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'tail', threadId: 't', turnIndex: 10 }),
      );
      await pane.loadUntilItem('tail');

      releaseOlder({ items: [], oldestTurnIndex: 10, hasMore: false });
      await olderPromise;

      expect(pane.loadingOlder).toBe(false);
    });

    it('loadUntilItem uses the bounded item budget when the pane floor is null', async () => {
      // Regression pin for the MAX_SAFE_INTEGER itemBudget bug: when
      // currentFloor is null (empty window), the request must pass a
      // bounded item budget rather than a sentinel number. Check that
      // the actual itemBudget argument is the LOAD_UNTIL_ITEM_HARD_CAP.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );
      let capturedBeforeTurn: number | null = null;
      let capturedBudget: number | null = null;
      setBindingMock('ListItemsBeforeTurn', async (_id, beforeTurn, budget) => {
        capturedBeforeTurn = beforeTurn as number;
        capturedBudget = budget as number;
        return {
          items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
          oldestTurnIndex: 3,
          hasMore: false,
        };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();
      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      // LOAD_UNTIL_ITEM_HARD_CAP (1000) bounds the per-call item
      // budget so a deep search hit doesn't request the entire history.
      expect(capturedBudget).toBeLessThanOrEqual(1000);
      expect(capturedBudget).toBeGreaterThan(0);
      expect(capturedBeforeTurn).toBe(4);
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
      setBindingMock('ListItemsBeforeTurn', async () => ({
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
      setBindingMock('ListItemsBeforeTurn', async () => {
        postSwitchCalls += 1;
        return { items: [], oldestTurnIndex: 2, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't3' }));
      await pane.loadOlder();
      expect(postSwitchCalls).toBe(1);
    });

    it('loadOlder dedupes by id when the backend re-returns an ancestor', async () => {
      // Backend contract: `ListItemsBeforeTurn` can legitimately
      // return an ancestor row that was already in the window (pulled
      // in by the initial load via `ListRecentItemsWithAncestors`'s
      // ancestor CTE). The store must not duplicate the row in
      // `items` — the dedup happens via `mergeItemsById`.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeTurn', async () => ({
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
      expect(pane.items.map((it) => it.id)).toEqual(['ancestor', 'between', 'child']);
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
      setBindingMock('ListItemsBeforeTurn', async () => ({
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
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [makeItem({ id: 'between', threadId: 't', turnIndex: 3 })],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual(['ancestor', 'between', 'child']);
      expect(result).toEqual({
        status: 'loaded',
        insertedBeforeWindow: false,
        insertedRows: true,
      });
    });

    it('loadUntilItem dedupes by id when pulling in a below-floor target', async () => {
      // Same contract as loadOlder's dedup, but via the
      // scroll-to-item entry point. If `ListItemsBeforeTurn` returns
      // a row already present by id (e.g. the subagent ancestor), no
      // duplicate should land in the window.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'tail', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 2 }),
      );
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'deep', threadId: 't', turnIndex: 2 }),
        ],
        oldestTurnIndex: 2,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(pane.items.filter((it) => it.id === 'ancestor').length).toBe(1);
      expect(pane.items.some((it) => it.id === 'deep')).toBe(true);
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
      pane.upsertItem(makeItem({ id: 'first', threadId: 't', turnIndex: 0, itemIndex: 0 }));
      expect(pane.items.map((it) => it.id)).toEqual(['first']);
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
        items, oldestTurnIndex: 0, hasMore: false,
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
        items: other, oldestTurnIndex: 0, hasMore: false,
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
      setBindingMock('ListThreadSliceAround', () => new Promise((resolve) => {
        releaseLoad = resolve;
      }));

      const switching = pane.switchThread(makeThread({ id: 't' }));
      // Drain microtasks so the switch sets up.
      await Promise.resolve();
      await Promise.resolve();

      // Streamed event arrives mid-load — upsert into the same items
      // array. mergeMissingItemsById in the load callback must keep it.
      pane.upsertItem(makeItem({
        id: 'streamed', threadId: 't', turnIndex: 1, itemIndex: 0,
      }));
      expect(pane.items.map((it) => it.id)).toEqual(['streamed']);

      // Load returns the canonical view. Triage's persist-then-emit
      // contract means the load SHOULD include 'streamed'; simulate
      // that.
      releaseLoad({
        items: [
          makeItem({ id: 'load', threadId: 't', turnIndex: 0, itemIndex: 0 }),
          makeItem({ id: 'streamed', threadId: 't', turnIndex: 1, itemIndex: 0 }),
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
      setBindingMock('ListThreadSliceAround', () => new Promise((resolve) => {
        releaseFirstLoad = resolve;
      }));

      const firstSwitch = pane.switchThread(makeThread({ id: 't' }));

      // Second switch comes in before the first resolves. Backend
      // returns a fresh canonical view.
      const secondItems = [
        makeItem({ id: 'second', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: secondItems, oldestTurnIndex: 0, hasMore: false,
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
        items: [
          makeItem({ id: 'stale-only', threadId: 't', turnIndex: 99 }),
        ],
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
        items: initialItems, oldestTurnIndex: 0, hasMore: false,
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
        items: revertedItems, oldestTurnIndex: 0, hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));

      // 'b' must never appear after the re-switch resolves. The
      // pre-revert items would be the cached snapshot if the
      // sameThreadReswitch guard were missing.
      expect(pane.items.map((it) => it.id)).toEqual(['a']);
    });

    it('bumps switchGeneration on every switchThread (including same-thread re-switch)', async () => {
      // The revert-to-checkpoint flow calls pane.switchThread(currentThread).
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
      setBindingMock('ListThreadSliceAround', () => new Promise((resolve) => {
        releaseLoad = resolve;
      }));

      const switching = pane.switchThread(makeThread({ id: 't' }));
      // Drain microtasks so the switch sets up.
      await Promise.resolve();
      await Promise.resolve();

      // Streamed upsert lands BEFORE the load resolves, seeding `a`.
      pane.upsertItem(makeItem({ id: 'a', threadId: 't', turnIndex: 0, itemIndex: 0 }));
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
        items: [], oldestTurnIndex: -1, hasMore: false,
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
      setBindingMock('ListThreadCheckpoints', stamp('ListThreadCheckpoints'));

      // Don't await — every mock hangs intentionally.
      void pane.switchThread(makeThread({ id: 't' }));

      // Yield enough microtasks for all five Promise constructors to
      // run (each one assigns its slot synchronously inside the
      // `() => new Promise(() => {})` body).
      for (let i = 0; i < 8; i++) await Promise.resolve();

      // All five must have started. The exact ordering between them
      // is non-deterministic by design; we only assert that no fetch
      // is missing — which it would be under serialisation.
      expect(Object.keys(startedAt).sort()).toEqual([
        'GetThreadLiveState',
        'ListRecentTurns',
        'ListThreadCheckpoints',
        'ListThreadSliceAround',
        'SwitchThread',
      ]);
    });

    it('does not call ListRecentThreadItems on switchThread (single-load contract)', async () => {
      // Pin the no-Phase-2 invariant: if the wider-window probe ever
      // creeps back into the switch path, the residual flicker
      // (Phase 2 prepend → applyJump fight with the controller's
      // sync-pin) returns. ListRecentThreadItems is reserved for
      // refreshFromBackend (transport-gap recovery), nothing else.
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
      setThreadScrollSnapshot('t', { kind: 'anchor', itemId: 'wanted', offsetTop: -42 });

      const pane = createThreadPane();
      let observedAnchor = '';
      setBindingMock('ListThreadSliceAround', async (
        threadID: unknown, anchorID: unknown, _count: unknown,
      ) => {
        observedAnchor = String(anchorID ?? '');
        void threadID;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
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
      setBindingMock('ListThreadSliceAround', async (
        threadID: unknown, anchorID: unknown, _count: unknown,
      ) => {
        observedAnchor = String(anchorID ?? '');
        void threadID;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));
      expect(observedAnchor).toBe('');
      clearThreadScrollSnapshotsForTest();
    });

    it('cache hit completes loading=false even when SwitchThread fails', async () => {
      const pane = createThreadPane();
      const items = [makeItem({ id: 'cached', threadId: 't', turnIndex: 0 })];
      setBindingMock('ListThreadSliceAround', async () => ({
        items, oldestTurnIndex: 0, hasMore: false,
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
      setBindingMock('ListThreadSliceAround', () => new Promise((_, reject) => {
        rejectFirstLoad = reject;
      }));
      const firstSwitch = pane.switchThread(makeThread({ id: 'first' }));

      // Second switch supersedes; populates with real data.
      const secondItems = [
        makeItem({ id: 'live', threadId: 'second', turnIndex: 0, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: secondItems, oldestTurnIndex: 0, hasMore: false,
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
          items, oldestTurnIndex: 0, hasMore: false,
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
          items, oldestTurnIndex: 0, hasMore: false,
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

    expect(getActiveTurn(pane.threadId)).toEqual({ turnId: 'turn-1', turnIndex: 0, startedAt: 1000 });
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
      pane.upsertItem(makeItem({ id: 'text:0:0', threadId: 'thread-a', status: 'streaming' }));

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
      pane.upsertItem(makeItem({
        id: 'tool:0:0',
        threadId: 'thread-patch',
        kind: 'tool_call',
        meta: '{"toolName":"Bash"}',
      }));

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
      pane.upsertItem(makeItem({
        id: 'text:0:0',
        threadId: 'thread-patch',
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'initial',
        updatedAt: 1,
      }));

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
      pane.upsertItem(makeItem({
        id: 'text:0:0',
        threadId: 'thread-patch',
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'initial received',
        updatedAt: 1,
      }));

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
      pane.upsertItem(makeItem({
        id: 'text:0:0',
        threadId: 'thread-patch',
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'partial reveal so far',
        updatedAt: 1,
      }));

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
      pane.upsertItem(makeItem({
        id: 'text:0:0',
        threadId: 'thread-patch',
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'seed',
        updatedAt: 1,
      }));

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
        pane.upsertItem(makeItem({
          id: 'think:0:0',
          threadId: 'thread-think',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: initial,
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        }));

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
          pane2.upsertItem(makeItem({
            id: 'think:0:0',
            threadId: 'thread-think-2',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: initial,
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }));
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

    it('does not produce wire-chunk-sized reveals when Claude bursts faster than the base rate', async () => {
      // Reproduces the user-reported regression: past ~400 chars, thinking
      // text appears in chunks "exactly like the old behavior before any
      // smoothing changes" — 5 words, pause, 15 words. The hypothesis is
      // that the adaptive catch-up math (`drain lag in 500ms`) scales the
      // per-tick reveal proportional to lag, so a wire that bursts faster
      // than the 160 cps base rate eventually settles at a steady-state
      // lag where per-tick = wire_rate * (16/500) — for a 2000 cps wire,
      // that's 64 chars (~10 words) per tick.
      //
      // Wire pattern is realistic: 50-char wire bursts arriving every
      // 25ms (= 2000 cps sustained, close to Claude's burst rate for
      // reasoning text). Streamed for ~1.5s so we walk well past the
      // 400-rune trim cap and reach steady-state lag.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-burst' }));
        pane.upsertItem(makeItem({
          id: 'think:0:0',
          threadId: 'thread-burst',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        }));

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
              itemId: 'think:0:0',
              kind: 'thinking',
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
        // A healthy smoother should stay under ~14 chars/tick (about 2
        // words) even under steady-state burst. The cap inside
        // `PerItemSmoother.tick()` is what enforces this; without it,
        // adaptive math at lag ~= wire_rate * (catchup_ms / 1000)
        // produces 60–100+ chars/tick under sustained 2000 cps bursts
        // and the user perceives those as chunks of 5–15 words.
        expect(maxLengthJump).toBeLessThanOrEqual(14);
        expect(maxContentJump).toBeLessThanOrEqual(14);
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
        for (let i = 0; i < 80; i++) seedWords.push(`word${String(i).padStart(2, '0')}`);
        const seed = seedWords.join(' ') + ' ';
        expect(seed.length).toBeGreaterThan(400);
        pane.upsertItem(makeItem({
          id: 'think:0:0',
          threadId: 'thread-think-burst',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: seed,
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        }));
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
        pane.upsertItem(makeItem({
          id: 'think:0:0',
          threadId: 'thread-drain',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        }));
        // Stream the first half (~150 chars) as deltas, then complete
        // with an extending summary that adds another ~150 chars on
        // top. This is the actual extending-summary path: smoother
        // received < patchSummary AND patchSummary.startsWith(received).
        const allWords: string[] = [];
        for (let i = 0; i < 50; i++) allWords.push(`item${String(i).padStart(2, '0')}`);
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
        pane.upsertItem(makeItem({
          id: 'think:0:0',
          threadId: 'thread-live-tail',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        }));

        // Stream enough text to push well past 400 runes.
        const words: string[] = [];
        for (let i = 0; i < 100; i++) words.push(`tok${String(i).padStart(2, '0')}`);
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
        pane2.upsertItem(makeItem({
          id: 'think:0:0',
          threadId: 'thread-live-tail-2',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        }));
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
              if (cur.length > 400 && cur.length > prev.length) growthPastTrimSamples++;
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

    it('clears the live thinking tail when the smoother disposes on completion', async () => {
      // Once the stream settles the smoother auto-disposes; the live
      // tail map entry must drop with it so ThinkingBlock falls back to
      // `item.summary` (the persisted trimmed tail) for the settled
      // row. A stranded live-tail entry would keep the collapsed view
      // showing the full pre-settle text indefinitely.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-tail-cleanup' }));
        pane.upsertItem(makeItem({
          id: 'think:0:0',
          threadId: 'thread-tail-cleanup',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        }));
        const words: string[] = [];
        for (let i = 0; i < 80; i++) words.push(`tok${String(i).padStart(2, '0')}`);
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
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
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
      // patch lands would never re-fire — the `itemSmoothers` and
      // `itemLiveThinkingTail` entries leaked until the next thread
      // switch, keeping the collapsed ThinkingBlock pinned to the
      // pre-settle live text instead of falling back to the persisted
      // summary.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-bare-status' }));
        pane.upsertItem(makeItem({
          id: 'think:0:0',
          threadId: 'thread-bare-status',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        }));
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
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        // Drain again to confirm no zombie rAF ticks (a fresh tick
        // after a leak would re-populate the live tail and re-fire
        // onReveal against the disposed slot).
        safety = 20;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
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
        pane.upsertItem(makeItem({
          id: 'text:0:0',
          threadId: 'thread-equal-text',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          updatedAt: 1,
        }));
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
        pane.upsertItem(makeItem({
          id: 'text:0:0',
          threadId: 'thread-snap-nosum',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'partial so far',
          updatedAt: 1,
        }));
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

  it('appendSubagentNotification records pass-through payloads, bounded', () => {
    const pane = createThreadPane();
    for (let i = 0; i < 40; i++) {
      pane.appendSubagentNotification({
        threadId: 'thread-1',
        meta: JSON.stringify({ agentId: `agent-${i}`, status: 'completed' }),
      });
    }
    // Bound should cap at 32 (subagentNotificationLimit). The newest
    // entry is at the tail; oldest entries have fallen off.
    expect(pane.subagentNotifications.length).toBe(32);
    expect(pane.subagentNotifications[pane.subagentNotifications.length - 1].meta)
      .toContain('agent-39');
    expect(pane.subagentNotifications[0].meta).toContain('agent-8');
  });

  describe('visibility-resume snap (snapSmoothersToReceived)', () => {
    // requestAnimationFrame is suspended while a tab is hidden, but the
    // WebSocket keeps delivering deltas into each smoother's `received`
    // buffer. The FakeSmoothingClock models this exactly: appending a delta
    // without calling `tickFrame` leaves `received` ahead of `revealed` with
    // a pending callback that never fires — the hidden-tab state. The
    // visibilitychange→visible entry point (App.svelte) calls
    // `snapSmoothersToReceived` so the backlog catches up to the wire in one
    // frame instead of crawling in at the ~840 cps per-tick cap on return.
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
        pane.upsertItem(makeItem({
          id: 'text:0:0',
          threadId: 'thread-vis-a',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          updatedAt: 1,
        }));
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
        pane.upsertItem(makeItem({
          id: 'text:0:0',
          threadId: 'thread-vis-b',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          updatedAt: 1,
        }));
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

        pane.upsertItem(makeItem({
          id: 'text:0:0',
          threadId: 'thread-vis-c',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          updatedAt: 1,
        }));
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
    function streamingThinking(id: string, itemIndex: number, threadId: string) {
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
        // It drains at the base cadence (no successor → no fast-drain).
        for (let i = 0; i < 80; i++) clock.tickFrame(16);
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
        pane.upsertItem(makeItem({
          id: 'tool:0:1',
          threadId: 't',
          kind: 'tool_call',
          status: 'running',
          turnIndex: 0,
          itemIndex: 1,
          toolName: 'Bash',
          summary: 'Bash',
          updatedAt: 3,
        }));
        // Gate stays at the thinking row — the tool call is withheld.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Fast-drain finishes the thinking within ~200ms (≈13 frames).
        for (let i = 0; i < 14; i++) clock.tickFrame(16);
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('fast-drains the frontier only when a successor is waiting', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        // Control: no successor — drains at the base cadence and is NOT done
        // after the ~200ms fast-drain window.
        const solo = await buildPane(makeThread({ id: 'solo' }));
        solo.upsertItem(streamingThinking('think:0:0', 0, 'solo'));
        solo.applyItemDelta({
          threadId: 'solo',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        for (let i = 0; i < 14; i++) clock.tickFrame(16);
        expect(solo.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        // With a successor, the same lag drains inside the window.
        const gated = await buildPane(makeThread({ id: 'gated' }));
        gated.upsertItem(streamingThinking('think:0:0', 0, 'gated'));
        gated.applyItemDelta({
          threadId: 'gated',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        gated.upsertItem(makeItem({
          id: 'tool:0:1',
          threadId: 'gated',
          kind: 'tool_call',
          status: 'running',
          turnIndex: 0,
          itemIndex: 1,
          toolName: 'Bash',
          summary: 'Bash',
          updatedAt: 3,
        }));
        for (let i = 0; i < 14; i++) clock.tickFrame(16);
        expect(gated.revealBoundary).toBeNull();
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
        pane.upsertItem(makeItem({
          id: 'text:0:1',
          threadId: 't',
          kind: 'assistant_text',
          status: 'streaming',
          turnIndex: 0,
          itemIndex: 1,
          summary: '',
          updatedAt: 3,
        }));
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

        // Thinking fast-drains → gate advances to the text row, which now
        // reveals from the start.
        for (let i = 0; i < 14; i++) clock.tickFrame(16);
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        for (let i = 0; i < 80; i++) clock.tickFrame(16);
        expect(pane.items[textIdx].summary).toBe('Hello world this is the answer');
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
        pane.upsertItem(makeItem({
          id: 'agent:0:0',
          threadId: 't',
          kind: 'tool_call',
          toolName: 'Agent',
          status: 'running',
          turnIndex: 0,
          itemIndex: 0,
          summary: 'Agent',
          updatedAt: 1,
        }));
        pane.upsertItem(makeItem({
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
        }));
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
        pane.upsertItem(makeItem({
          id: 'text:0:2',
          threadId: 't',
          kind: 'assistant_text',
          status: 'streaming',
          turnIndex: 0,
          itemIndex: 2,
          summary: '',
          updatedAt: 4,
        }));
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
        pane.upsertItem(makeItem({
          id: 'tool:0:1',
          threadId: 't',
          kind: 'tool_call',
          status: 'running',
          turnIndex: 0,
          itemIndex: 1,
          toolName: 'Bash',
          summary: 'Bash',
          updatedAt: 3,
        }));
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
        pane.upsertItem(makeItem({
          id: 'text:0:0', threadId: 't', kind: 'assistant_text', role: 'assistant',
          status: 'streaming', turnIndex: 0, itemIndex: 0, summary: '', updatedAt: 1,
        }));
        pane.applyItemDelta({
          threadId: 't', itemId: 'text:0:0', kind: 'assistant_text', delta: 'hello ', updatedAt: 2,
        });
        pane.upsertItem(makeItem({
          id: 'tool:0:1', threadId: 't', kind: 'tool_call', status: 'running',
          turnIndex: 0, itemIndex: 1, toolName: 'Bash', summary: 'Bash', updatedAt: 3,
        }));
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Turn-completion patch carries the final text, extending what streamed.
        pane.applyItemPatch({
          threadId: 't', itemId: 'text:0:0', kind: 'assistant_text',
          patch: { status: 'completed', summary: 'hello world done', updatedAt: 4 },
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
          threadId: 't', itemId: 'think:0:0', kind: 'thinking', delta: 'word '.repeat(40), updatedAt: 2,
        });
        pane.upsertItem(makeItem({
          id: 'text:0:1', threadId: 't', kind: 'assistant_text', role: 'assistant',
          status: 'streaming', turnIndex: 0, itemIndex: 1, summary: '', updatedAt: 3,
        }));
        pane.applyItemDelta({
          threadId: 't', itemId: 'text:0:1', kind: 'assistant_text', delta: 'the answer here', updatedAt: 4,
        });
        pane.upsertItem(makeItem({
          id: 'tool:0:2', threadId: 't', kind: 'tool_call', status: 'running',
          turnIndex: 0, itemIndex: 2, toolName: 'Bash', summary: 'Bash', updatedAt: 5,
        }));
        // Gate at thinking; text AND tool both withheld.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // thinking drains → gate steps to the text row (not straight to null).
        for (let i = 0; i < 14; i++) clock.tickFrame(16);
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        // text drains → gate drops (tool has no smoother, reveals immediately).
        for (let i = 0; i < 20; i++) clock.tickFrame(16);
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
          threadId: 't', itemId: 'think:0:0', kind: 'thinking', delta: 'word '.repeat(40), updatedAt: 2,
        });
        pane.upsertItem(makeItem({
          id: 'text:0:1', threadId: 't', kind: 'assistant_text', role: 'assistant',
          status: 'streaming', turnIndex: 0, itemIndex: 1, summary: '', updatedAt: 3,
        }));
        pane.applyItemDelta({
          threadId: 't', itemId: 'text:0:1', kind: 'assistant_text', delta: 'the answer', updatedAt: 4,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Optimistic revert removes the streaming frontier row.
        pane.removeItemById('think:0:0');
        // The withheld successor becomes the frontier and resumes from its start.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        for (let i = 0; i < 60; i++) clock.tickFrame(16);
        expect(pane.items.find((i) => i.id === 'text:0:1')?.summary).toBe('the answer');
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
          threadId: 't', itemId: 'think:0:0', kind: 'thinking', delta: 'word '.repeat(40), updatedAt: 2,
        });
        pane.upsertItem(makeItem({
          id: 'tool:0:1', threadId: 't', kind: 'tool_call', status: 'running',
          turnIndex: 0, itemIndex: 1, toolName: 'Bash', summary: 'Bash', updatedAt: 3,
        }));
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        await pane.switchThread(makeThread({ id: 'other-thread' }));
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('leaves the gate null for a settled thread (no streaming)', async () => {
      const pane = await buildPane(makeThread({ id: 't' }), [
        makeItem({ id: 'u:0', threadId: 't', kind: 'user_text', role: 'user', summary: 'hi', turnIndex: 0, itemIndex: 0 }),
        makeItem({ id: 'a:1', threadId: 't', kind: 'assistant_text', summary: 'done', turnIndex: 0, itemIndex: 1 }),
      ]);
      expect(pane.revealBoundary).toBeNull();
    });
  });

  // `pane.lastLiveContentAt` is the source the chat scroll controller
  // latches on to decide spring vs sync-pin (MessageTimeline's
  // animationModeForScroll → latchedSpringMode). It must advance ONLY on
  // genuine live timeline content arriving — text reveals, streaming
  // deltas, final-summary patches, new provider rows — and must NOT
  // advance on thread switch, bulk history loads, meta-only updates, or
  // the optimistic-send / rollback paths that drive `upsertItems`
  // directly. Each test ticks the fake clock to a nonzero base first so a
  // `=== 0` assertion genuinely means "never stamped" rather than
  // "stamped at time 0".
  describe('live-content stamp (scroll animation latch source)', () => {
    // Long backlog so the smoother reveals across many frames (never
    // caught up in 2-3 ticks). 120 words ≈ 840 chars; even at the
    // fast-drain cap that is ~60 frames, so frames 1-3 always reveal.
    const longText = (n: number) =>
      Array.from({ length: n }, (_, i) => `word${i}`).join(' ') + ' ';

    it('stamps on each smoother reveal frame, never on switch/upsert/delta-append', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(100); // base now()=100 so the `=== 0` checks are real
        const pane = await buildPane(makeThread({ id: 'stamp-reveal' }));
        // Switching into a thread (bulk slice load) is not live content.
        expect(pane.lastLiveContentAt).toBe(0);

        pane.upsertItem(makeItem({
          id: 'a:0:0',
          threadId: 'stamp-reveal',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'seed ',
          updatedAt: 1,
        }));
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
        pane.upsertItem(makeItem({
          id: 'a:0:0',
          threadId: 'stamp-drain',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'seed ',
          updatedAt: 1,
        }));
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

    it('stamps on a non-smoothed streaming delta (bypasses the smoother)', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        clock.tickFrame(16); // base now()=16
        const pane = await buildPane(makeThread({ id: 'stamp-nonsmooth' }));
        // tool_call is not a smoothable kind — applyItemDelta writes
        // summary directly and must stamp inline (no onReveal to do it).
        pane.upsertItem(makeItem({
          id: 'tool:0:0',
          threadId: 'stamp-nonsmooth',
          kind: 'tool_call',
          role: 'assistant',
          status: 'streaming',
          summary: 'out',
          updatedAt: 1,
        }));
        expect(pane.lastLiveContentAt).toBe(0);

        pane.applyItemDelta({
          threadId: 'stamp-nonsmooth',
          itemId: 'tool:0:0',
          kind: 'tool_call',
          delta: 'put',
          updatedAt: 2,
        });
        expect(pane.lastLiveContentAt).toBe(16);
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
        pane.upsertItem(makeItem({
          id: 'a:0:0',
          threadId: 'stamp-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'completed',
          summary: 'hello',
          updatedAt: 1,
        }));
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
        const pane = await buildPane(
          makeThread({ id: 'stamp-neg' }),
          [makeItem({ id: 'seed:0:0', threadId: 'stamp-neg', summary: 'pre' })],
        );
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
        pane.upsertItems([makeItem({
          id: 'new:1:0',
          threadId: 'stamp-neg',
          turnIndex: 1,
          kind: 'assistant_text',
          summary: 'fresh',
        })]);
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
        pane.upsertItem(makeItem({
          id: 'a:0:0',
          threadId: 'A',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'seed ',
          updatedAt: 1,
        }));
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

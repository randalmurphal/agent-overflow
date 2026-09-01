// stores/thread.svelte.test.ts
//
// The ThreadPane composition root: an empty pane's shape, what a thread
// switch loads and clears across every sub-store it composes, live-state
// hydration, and the identity cutoff the $derived getters give consumers.
// Each composed concern has its own sibling suite — draft placeholder,
// item window, streamed apply, timeline window, subagent fold, switch
// load, turns, reveal smoothing/sequencing, scroll, errors, companions —
// named after the module it covers.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { createThreadPane } from './thread.svelte';
import { LIVE_TODO_AUTOHIDE_MS } from './liveTodoState.svelte';
import { type Item } from '../types/models';
import {
  getActiveTurn,
  getThreadStatus,
  isThreadLiveStateHydrating,
} from './threadStatuses.svelte';
import {
  getFlushedForThread,
  getQueueForThread,
  replaceQueueForThread,
} from './sendQueue.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';

describe('createThreadPane', () => {
  beforeEach(installThreadPaneTestEnv);

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

  // The stale-binary banner has a snapshot leg for a webview that
  // (re)connects after the push: GetThreadLiveState carries the two
  // versions while the thread's live session runs an older CLI. The leg is
  // SET-ONLY — a snapshot without the versions says nothing, because it
  // could have been computed before a push that landed while the RPC was
  // in flight, and the backend only re-emits on transitions.
  it('hydrates the stale-binary banner from live state, set-only', async () => {
    const pane = createThreadPane();
    let versions: { sessionCliVersion?: string; installedCliVersion?: string } = {
      sessionCliVersion: '2.1.100',
      installedCliVersion: '2.1.200',
    };
    setBindingMock('GetThreadLiveState', async (threadId: string) => ({
      threadId,
      ...versions,
    }));

    await pane.switchThread(makeThread({ id: 'thread-stale', provider: 'claude' }));
    expect(pane.providerBanner?.status).toBe('binary_stale');
    expect(pane.providerBanner?.sessionVersion).toBe('2.1.100');
    expect(pane.providerBanner?.installedVersion).toBe('2.1.200');
    expect(pane.providerBanner?.threadId).toBe('thread-stale');

    versions = {};
    await pane.refreshFromBackend();
    expect(pane.providerBanner?.status).toBe('binary_stale');
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

  it('coalesces an overlapping refresh into one trailing run whose fresher snapshot wins', async () => {
    // Refreshes are single-flight (utils/refreshScheduler): a request
    // during one in flight never opens a second concurrent fetch — the
    // interleaving where an older snapshot resolves after a newer one is
    // structurally impossible. The second request is answered by exactly
    // one trailing run, and the surface converges to ITS snapshot.
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
    expect(releases).toHaveLength(1);
    const newer = pane.refreshFromBackend();
    for (let i = 0; i < 4; i += 1) await Promise.resolve();
    // Coalesced: no second fetch while the first is in flight.
    expect(releases).toHaveLength(1);

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

    // The queued request fires exactly one trailing run once the first
    // completes (after the scheduler's cooldown).
    await vi.waitFor(() => expect(releases).toHaveLength(2), {
      timeout: 2000,
    });
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

  describe('thread identity cutoff', () => {
    // thread:updated syncs (mode toggle, title regen, model change) replace
    // the pane's thread OBJECT wholesale. The primitive getters over it
    // (threadId, terminalThreadId, activeModel) are served from $deriveds so
    // that replacement does not wake consumers whose value never moved — the
    // 2026-08-19 incident was the nav rail's baseline effect keying on
    // pane.threadId through a plain getter: every shift+tab mode toggle
    // cleared and refetched the whole-thread tick list, blinking the rail.
    it('replacing the thread object with the same id does not wake identity consumers', () => {
      const pane = createThreadPane();
      pane.replaceThread(makeThread({ id: 't1', mode: 'chat' }));
      let idRuns = 0;
      let modelRuns = 0;
      const stop = $effect.root(() => {
        $effect(() => {
          void pane.threadId;
          void pane.terminalThreadId;
          idRuns += 1;
        });
        $effect(() => {
          void pane.activeModel;
          modelRuns += 1;
        });
      });

      try {
        flushSync();
        expect(idRuns).toBe(1);
        expect(modelRuns).toBe(1);

        // The mode-toggle sync: same id, new object. Neither consumer wakes.
        pane.replaceThread(makeThread({ id: 't1', mode: 'plan' }));
        flushSync();
        expect(idRuns).toBe(1);
        expect(modelRuns).toBe(1);

        // A real identity change still propagates; the model consumer stays
        // asleep because the model string did not move.
        pane.replaceThread(makeThread({ id: 't2', mode: 'chat' }));
        flushSync();
        expect(idRuns).toBe(2);
        expect(modelRuns).toBe(1);

        // And a model change wakes only the model consumer.
        pane.replaceThread(makeThread({ id: 't2', model: 'claude-opus-4-6' }));
        flushSync();
        expect(idRuns).toBe(2);
        expect(modelRuns).toBe(2);
      } finally {
        stop();
      }
    });
  });
});

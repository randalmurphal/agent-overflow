// The Stop: a plain interrupt that keeps the sent message, its same-tick
// clear of the working presentation, the background-agent confirmation, and
// the hold a Stop takes while this client's own send is unanswered.

import { afterEach, describe, expect, it, beforeEach, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import {
  isBenignInterruptError,
  reportNonBenignInterruptError,
  resetDirectSendsForTest,
  runInterrupt,
  trackDirectSend,
} from './threadInterrupt.svelte';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import { TransportError } from '../transport/wsClient';
import {
  getActiveTurn,
  getThreadStatus,
  isSendInFlight,
  isThreadWorking,
  projectSendStarted,
  projectTurnCompleted,
  projectTurnStarted,
} from './threadStatuses.svelte';
import {
  cancelBackgroundKillConfirmationForThread,
  pendingBackgroundKillConfirmation,
  resetForTest as resetBackgroundKillConfirmationForTest,
  resolveBackgroundKillConfirmation,
} from './backgroundKillConfirmation.svelte';
import { replaceSubagentRunStates, resetForTest as resetSubagentRunStatesForTest } from './subagentRunState.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { Item, Thread } from '../types/models';
import { isThreadInterruptPending, resetThreadInterruptStateForTest } from './threadInterruptState.svelte';

async function settle(): Promise<void> {
  for (let i = 0; i < 6; i++) await Promise.resolve();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function readyPane(threadId = 'thread-1'): ReturnType<typeof createThreadPane> {
  setBindingMock('SwitchThread', async (id: unknown) => ({
    id: typeof id === 'string' ? id : threadId,
  }));
  setBindingMock('ListPendingInteractiveRequests', async () => ({
    approvals: [],
    userInputs: [],
  }));
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  const pane = createThreadPane();
  const thread: Thread = {
    id: threadId,
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
  void pane.switchThread(thread);
  return pane;
}

function userItem(id: string, turnIndex: number, threadId = 'thread-1'): Item {
  return {
    id,
    threadId,
    turnIndex,
    kind: 'user_text',
    role: 'user',
    status: 'completed',
    summary: 'hello',
    payloadId: '',
    meta: '',
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}

function assistantItem(id: string, turnIndex: number, threadId = 'thread-1'): Item {
  return {
    id,
    threadId,
    turnIndex,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'streaming',
    summary: 'thinking aloud',
    payloadId: '',
    meta: '',
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}


describe('isBenignInterruptError', () => {
  // The transport redacts method-error text for every caller that is not on
  // loopback, so a remote client's error reads "method failed (id: ...)" and
  // only the code arm can recognize the race.
  it('recognizes a redacted already-handled from a remote client', () => {
    expect(isBenignInterruptError(new TransportError('already_handled', 'method failed (id: abc)')))
      .toBe(true);
  });

  it('still recognizes the loopback messages', () => {
    expect(isBenignInterruptError(new Error('no active turn for thread t1'))).toBe(true);
    expect(isBenignInterruptError(new Error('approval already resolved'))).toBe(true);
    expect(isBenignInterruptError(new Error('provider: stale interactive request'))).toBe(true);
  });

  it('lets a real failure through', () => {
    expect(isBenignInterruptError(new Error('provider session died'))).toBe(false);
    expect(isBenignInterruptError(new TransportError('method_error', 'method failed (id: abc)')))
      .toBe(false);
  });
});

describe('reportNonBenignInterruptError', () => {
  it('leaves the banner alone for a race another client won', () => {
    const setGeneralError = vi.fn();
    reportNonBenignInterruptError(
      { setGeneralError },
      new TransportError('already_handled', 'method failed (id: abc)'),
    );
    expect(setGeneralError).not.toHaveBeenCalled();
  });

  it('surfaces a real failure', () => {
    const setGeneralError = vi.fn();
    vi.spyOn(console, 'error').mockImplementation(() => {});
    reportNonBenignInterruptError({ setGeneralError }, new Error('provider session died'));
    expect(setGeneralError).toHaveBeenCalled();
  });
});

describe('runInterrupt on a running turn', () => {
  beforeEach(() => {
    resetThreadInterruptStateForTest();
    resetDirectSendsForTest();
    setBindingMock('GetThreadLiveState', async () => ({ threadId: 'thread-1', activeTurn: null }));
  });

  function runningPane() {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    return pane;
  }

  it('keeps the message, reads interrupted in the press\'s tick, and holds Send until the RPC answers', async () => {
    const pane = runningPane();
    pane.setSendInFlight(true);
    const interrupt = deferred<void>();
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', (...args: unknown[]) => { calls.push(args); return interrupt.promise; });

    runInterrupt(pane);
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(getThreadStatus('thread-1')).toBe('interrupted');
    expect(pane.sendInFlight).toBe(false);
    expect(isThreadInterruptPending('thread-1')).toBe(true);
    expect(calls).toEqual([['thread-1', []]]);

    interrupt.resolve();
    await settle();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.items.find((item) => item.id === 'u:0')).toBeDefined();
    expect(getThreadStatus('thread-1')).toBe('interrupted');
  });

  it('ignores a second Stop while the first is in flight', async () => {
    const pane = runningPane();
    const interrupt = deferred<void>();
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', (...args: unknown[]) => { calls.push(args); return interrupt.promise; });

    runInterrupt(pane);
    runInterrupt(pane);
    interrupt.resolve();
    await settle();
    expect(calls).toHaveLength(1);
  });

  it('is not held by a send that has already answered', async () => {
    const pane = runningPane();
    trackDirectSend('thread-1')('accepted');
    await settle();
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => { calls.push(args); });

    runInterrupt(pane);
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(calls).toEqual([['thread-1', []]]);
  });
});

// A Stop pressed while this client's direct send is unanswered waits for the
// send's answer, then interrupts unless the backend refused the send. The
// message is sent, never cancelled.
describe('runInterrupt during an unanswered send', () => {
  beforeEach(() => {
    resetThreadInterruptStateForTest();
    resetDirectSendsForTest();
    setBindingMock('GetThreadLiveState', async () => ({ threadId: 'thread-1', activeTurn: null }));
  });
  afterEach(() => __resetEntityIndexForTest());

  function sendingPane() {
    const pane = readyPane();
    pane.setSendInFlight(true);
    projectSendStarted('thread-1');
    const settleSend = trackDirectSend('thread-1');
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => { calls.push(args); });
    return { pane, settleSend, calls };
  }

  it('interrupts once when an accepted reply lands, and reads working until then', async () => {
    const { pane, settleSend, calls } = sendingPane();

    runInterrupt(pane);
    await settle();
    expect(calls).toEqual([]);
    expect(isThreadInterruptPending('thread-1')).toBe(true);
    expect(isThreadWorking('thread-1')).toBe(true);
    expect(isSendInFlight('thread-1', pane.sendInFlight)).toBe(true);

    settleSend('accepted');
    await settle();
    expect(calls).toEqual([['thread-1', []]]);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.sendInFlight).toBe(false);
    expect(pane.generalError ?? '').toBe('');
    // No turn opened, and none may (the Claude terminal UI discards the
    // prompt), so the delivered Stop ends the send's running presentation.
    expect(isThreadWorking('thread-1')).toBe(false);
    expect(isSendInFlight('thread-1', pane.sendInFlight)).toBe(false);
    expect(getThreadStatus('thread-1')).toBe('interrupted');
  });

  it('keeps the send reading as running when the interrupt fails', async () => {
    const { pane, settleSend } = sendingPane();
    setBindingMock('InterruptTurn', async () => { throw new Error('boom: provider crashed'); });
    await settle();

    runInterrupt(pane);
    settleSend('accepted');
    await settle();
    expect(pane.generalError ?? '').not.toBe('');
    expect(isThreadWorking('thread-1')).toBe(true);
  });

  it('waits for the answer through turns that start before or after the press', async () => {
    const { pane, settleSend, calls } = sendingPane();
    projectTurnStarted('thread-1', 'earlier', 0, 1);

    runInterrupt(pane);
    await settle();
    expect(calls).toEqual([]);
    expect(getActiveTurn('thread-1')?.turnId).toBe('earlier');
    projectTurnStarted('thread-1', 'turn-1', 1, 2);
    await settle();
    expect(calls).toEqual([]);
    expect(isThreadWorking('thread-1')).toBe(true);

    settleSend('accepted');
    await settle();
    expect(calls).toEqual([['thread-1', []]]);
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(getThreadStatus('thread-1')).toBe('interrupted');
  });

  it('attempts the interrupt when the outcome is unknown', async () => {
    const { pane, settleSend, calls } = sendingPane();

    runInterrupt(pane);
    settleSend('unknown');
    await settle();
    expect(calls).toEqual([['thread-1', []]]);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('sends no interrupt and reports nothing when the send was refused', async () => {
    const { pane, settleSend, calls } = sendingPane();

    runInterrupt(pane);
    settleSend('refused');
    await settle();
    expect(calls).toEqual([]);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.generalError ?? '').toBe('');
  });

  it('sends one interrupt when Stop is pressed twice', async () => {
    const { pane, settleSend, calls } = sendingPane();

    runInterrupt(pane);
    runInterrupt(pane);
    settleSend('accepted');
    await settle();
    expect(calls).toEqual([['thread-1', []]]);
  });

  it('still stops the thread it was pressed on after the pane switched away', async () => {
    const { pane, settleSend, calls } = sendingPane();

    runInterrupt(pane);
    await pane.switchThread({ ...pane.thread!, id: 'thread-2' });
    // thread-2's own send, which the Stop must not touch.
    pane.setSendInFlight(true);
    projectTurnStarted('thread-1', 'turn-1', 0, 1);
    settleSend('accepted');
    await settle();
    expect(calls).toEqual([['thread-1', []]]);
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(pane.sendInFlight).toBe(true);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('sends nothing once the thread\'s history is invalidated during the wait', async () => {
    const { pane, settleSend, calls } = sendingPane();
    noteThread('thread-1', 'computer-a', 1);

    runInterrupt(pane);
    noteThread('thread-1', 'computer-b', 2);
    expect(isThreadInterruptPending('thread-1')).toBe(false);

    projectTurnStarted('thread-1', 'turn-1', 0, 1);
    settleSend('accepted');
    await settle();
    expect(calls).toEqual([]);
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
  });

  it('leaves a later send alone when the thread\'s history is invalidated while the interrupt is unanswered', async () => {
    const { pane, settleSend } = sendingPane();
    noteThread('thread-1', 'computer-a', 1);
    const interrupt = deferred<void>();
    setBindingMock('InterruptTurn', () => interrupt.promise);

    runInterrupt(pane);
    settleSend('accepted');
    await settle();
    noteThread('thread-1', 'computer-b', 2);
    projectSendStarted('thread-1');

    interrupt.resolve();
    await settle();
    expect(isThreadWorking('thread-1')).toBe(true);
    expect(getThreadStatus('thread-1')).not.toBe('interrupted');
  });
});

// A Claude interrupt kills every live async agent, so the backend refuses a
// Stop with `background_agents_running` until the person confirms
// (transport/backgroundKillRefusal.ts). The Stop owns the optimistic clear:
// same tick when no listed agent is live, after the answer otherwise.
describe('runInterrupt with live background agents', () => {
  const agent = { launchItemId: 'tu-a', description: 'gate watcher', runState: 'parked', transcriptRootId: 'tu-a' };
  const refusal = () => new TransportError('background_agents_running', 'Stopping now would also stop 1 background agent.', {
    backgroundAgents: [agent],
  });

  beforeEach(() => {
    resetThreadInterruptStateForTest();
    resetDirectSendsForTest();
    resetBackgroundKillConfirmationForTest();
    resetSubagentRunStatesForTest();
    // The mounted pane hydrates live state; an unmocked read would land a
    // banner these tests assert stays empty.
    setBindingMock('GetThreadLiveState', async () => ({ threadId: 'thread-1', activeTurn: null }));
  });

  function stoppablePane() {
    const pane = readyPane();
    pane.upsertItem(userItem('u:0', 0));
    pane.upsertItem(assistantItem('a:0', 0));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    return pane;
  }

  it('clears the working presentation in the same tick when no listed agent is live, and sends an unconfirmed interrupt', async () => {
    const pane = stoppablePane();
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => { calls.push(args); });

    runInterrupt(pane);
    expect(getActiveTurn('thread-1')).toBeNull();
    await settle();
    expect(calls).toEqual([['thread-1', []]]);
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('keeps the turn running and asks when the backend refuses; "keep them" stops nothing', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'parked', waitingOn: 1, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if ((args[1] as string[]).length === 0) throw refusal();
    });

    runInterrupt(pane);
    // A listed live agent: no optimistic clear, the refusal is expected.
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    await settle();
    expect(calls).toEqual([['thread-1', []]]);
    expect(pendingBackgroundKillConfirmation()).toMatchObject({ threadId: 'thread-1', agents: [agent] });
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    // The question is not an interrupt in flight: the composer keeps its
    // Stop behind the dialog.
    expect(isThreadInterruptPending('thread-1')).toBe(false);

    resolveBackgroundKillConfirmation(false);
    await settle();
    expect(calls).toHaveLength(1);
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.generalError ?? '').toBe('');
  });

  it('"stop everything" clears the presentation and interrupts with the confirmation set', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if ((args[1] as string[]).length === 0) throw refusal();
    });

    runInterrupt(pane);
    await settle();
    expect(pendingBackgroundKillConfirmation()).not.toBeNull();
    resolveBackgroundKillConfirmation(true);
    await settle();
    expect(calls).toEqual([['thread-1', []], ['thread-1', ['tu-a']]]);
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.generalError ?? '').toBe('');
  });

  it('puts the turn back when a Stop cleared early on a stale registry and was refused', async () => {
    const pane = stoppablePane();
    await settle();
    pane.clearGeneralError();
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if ((args[1] as string[]).length === 0) throw refusal();
    });

    runInterrupt(pane);
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(getThreadStatus('thread-1')).toBe('interrupted');
    await settle();
    expect(getActiveTurn('thread-1')).toEqual({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    expect(getThreadStatus('thread-1')).toBe('running');
    expect(pane.generalError ?? '').toBe('');
    expect(pendingBackgroundKillConfirmation()?.threadId).toBe('thread-1');
    resolveBackgroundKillConfirmation(false);
    await settle();
    expect(calls).toEqual([['thread-1', []]]);
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
  });

  it('asks nothing when the turn ended before the refusal arrived: there is nothing left to stop', async () => {
    const pane = stoppablePane();
    let reject!: (err: unknown) => void;
    setBindingMock('InterruptTurn', () => new Promise((_resolve, rejectRPC) => { reject = rejectRPC; }));

    runInterrupt(pane);
    expect(getActiveTurn('thread-1')).toBeNull();
    projectTurnCompleted('thread-1', 'turn-1');
    reject(refusal());
    await settle();

    expect(getActiveTurn('thread-1')).toBeNull();
    expect(getThreadStatus('thread-1')).toBe('idle');
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('settles the question as "keep them" when the turn completes while it is open', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if ((args[1] as string[]).length === 0) throw refusal();
    });
    runInterrupt(pane);
    await settle();
    expect(pendingBackgroundKillConfirmation()).not.toBeNull();

    projectTurnCompleted('thread-1', 'turn-1');
    await settle();
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(calls).toHaveLength(1);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('any other failure keeps the optimistic stop and reports it', async () => {
    const pane = stoppablePane();
    await settle();
    pane.clearGeneralError();
    setBindingMock('InterruptTurn', async () => { throw new Error('boom: provider crashed'); });

    runInterrupt(pane);
    await settle();

    expect(getActiveTurn('thread-1')).toBeNull();
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(pane.generalError ?? '').not.toBe('');
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });

  it('asks again naming both when an agent launched while the question was open, and only that answer stops', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    // A resumed round: the confirmation names it by its transcript root.
    const later = { launchItemId: 'carrier-b', description: 'later worker', runState: 'running', transcriptRootId: 'tu-b' };
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      const confirmed = args[1] as string[];
      if (confirmed.length === 0) throw refusal();
      if (!confirmed.includes('tu-b')) {
        throw new TransportError('background_agents_running', 'Stopping now would also stop 2 background agents.', {
          backgroundAgents: [agent, later],
        });
      }
    });

    runInterrupt(pane);
    await settle();
    expect(pendingBackgroundKillConfirmation()?.agents).toEqual([agent]);
    resolveBackgroundKillConfirmation(true);
    await settle();
    // The confirmation named one agent; the backend refused it naming both,
    // and the turn is back on screen behind the new question.
    expect(calls).toEqual([['thread-1', []], ['thread-1', ['tu-a']]]);
    expect(pendingBackgroundKillConfirmation()?.agents).toEqual([agent, later]);
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    expect(isThreadInterruptPending('thread-1')).toBe(false);
    expect(pane.generalError ?? '').toBe('');

    resolveBackgroundKillConfirmation(true);
    await settle();
    expect(calls).toEqual([['thread-1', []], ['thread-1', ['tu-a']], ['thread-1', ['tu-a', 'tu-b']]]);
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(getActiveTurn('thread-1')).toBeNull();
    expect(pane.generalError ?? '').toBe('');
  });

  it('reports a refusal of a confirmed Stop whose agent list cannot be read, and keeps the turn', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if ((args[1] as string[]).length === 0) throw refusal();
      throw new TransportError('background_agents_running', 'Stopping now would also stop 1 background agent.', {
        backgroundAgents: 'unreadable',
      });
    });

    runInterrupt(pane);
    await settle();
    resolveBackgroundKillConfirmation(true);
    await settle();
    expect(calls).toHaveLength(2);
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    expect(getActiveTurn('thread-1')?.turnId).toBe('turn-1');
    expect(pane.generalError ?? '').not.toBe('');
  });

  it('a torn-down thread settles its open question as "keep them" and sends nothing more', async () => {
    const pane = stoppablePane();
    replaceSubagentRunStates('thread-1', new Map([['tu-a', { state: 'running', waitingOn: 0, report: null }]]));
    const calls: unknown[][] = [];
    setBindingMock('InterruptTurn', async (...args: unknown[]) => {
      calls.push(args);
      if ((args[1] as string[]).length === 0) throw refusal();
    });
    runInterrupt(pane);
    await settle();
    expect(pendingBackgroundKillConfirmation()).not.toBeNull();
    cancelBackgroundKillConfirmationForThread('thread-1');
    await settle();
    expect(calls).toHaveLength(1);
    expect(isThreadInterruptPending('thread-1')).toBe(false);
  });
});

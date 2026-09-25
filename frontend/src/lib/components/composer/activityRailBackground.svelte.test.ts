import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createBackgroundController } from './activityRailBackground.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { noteThread } from '../../transport/entityIndex';
import { __attachBackendForTest, detachBackend } from '../../transport/backends';
import { applyItemStreamEvent, flushItemEventQueue, resetItemEventQueue } from '../../stores/eventsItemStream';
import { liveSubagentRunState } from '../../stores/subagentRunState.svelte';
import { applyBackgroundTrayEvent } from '../../stores/eventsBackgroundTray';
import { wsClient } from '../../transport/wsClient';
import type { Item, Project, Thread } from '../../types/models';

const remote = 'tray-owner';
function attachOwner() {
  __attachBackendForTest({ id: remote, name: 'Nexus', backendId: remote, wsUrl: 'wss://nexus.invalid/ws', bootstrapUrl: 'https://nexus.invalid/bootstrap' }, {
    callByID: async () => null, callByName: async () => null, subscribe: () => () => {},
    setWatchedThreads() {}, setPresence() {}, setLease() {}, installStepUpProver() {}, close() {},
    getStatus: () => ({ status: 'connected', nextAttemptAt: null }),
    onStatusChange: (listener: (value: unknown) => void) => { listener({ status: 'connected', nextAttemptAt: null }); return () => {}; },
    getHello: () => null, onHelloChange: () => () => {}, onReplay: () => () => {},
  } as never);
}
function launch(id: string) {
  return makeItem({ id, status: 'running', isBackground: true, toolName: 'MCP/remote_run',
    meta: JSON.stringify({ remoteJob: { computerId: remote, requestId: id } }) });
}
async function flush() { await tick(); await Promise.resolve(); await tick(); }

describe('background tray recovery', () => {
  let release = () => {};
  afterEach(() => { release(); detachBackend(remote); vi.useRealTimers(); });

  it('reads nothing for a draft placeholder, whose synthetic id no computer owns', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    const read = setBindingMock('ListLiveBackgroundTasks', async () => []);
    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      pane.startDraftPlaceholder({ id: 'proj', name: 'proj', path: '/proj', createdAt: 0, updatedAt: 0 } as Project);
      expect(pane.hasDraftPlaceholder).toBe(true);
      release = $effect.root(() => createBackgroundController(() => pane, Date.now).mount());
      await flush();
      applyBackgroundTrayEvent({ threadId: pane.threadId!, refresh: true });
      await flush();
      expect(read).not.toHaveBeenCalled();
      expect(log).not.toHaveBeenCalled();
    } finally { log.mockRestore(); }
  });

  it('ignores other computers, fences an old reply, and rehydrates its owner without item events', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    noteThread(pane.threadId!, remote, 2);
    let finish!: (items: ReturnType<typeof launch>[]) => void;
    const oldRead = setBindingMock('ListLiveBackgroundTasks', () => new Promise((resolve) => { finish = resolve; }));
    let controller!: ReturnType<typeof createBackgroundController>;
    release = $effect.root(() => {
      controller = createBackgroundController(() => pane, Date.now);
      return controller.mount();
    });
    await flush();
    expect(oldRead).toHaveBeenCalledTimes(1);
    __setBackendStatusForTest('', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('', { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(oldRead).toHaveBeenCalledTimes(1);

    __setBackendStatusForTest(remote, { status: 'reconnecting', nextAttemptAt: null });
    const newRead = setBindingMock('ListLiveBackgroundTasks', async () => [launch('current')]);
    __setBackendStatusForTest(remote, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(controller.tasks.map((task) => task.rowId)).toEqual(['current']);
    finish([launch('stale')]);
    await flush();
    expect(controller.tasks.map((task) => task.rowId)).toEqual(['current']);
    expect(newRead).toHaveBeenCalledTimes(1);
  });

  it('keeps running rows through model edits and failed refreshes until a successful removal', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    noteThread(pane.threadId!, remote, 2);
    const read = setBindingMock('ListLiveBackgroundTasks', async () => [launch('running')]);
    let controller!: ReturnType<typeof createBackgroundController>;
    release = $effect.root(() => {
      controller = createBackgroundController(() => pane, Date.now);
      return controller.mount();
    });
    await flush();
    expect(controller.runningCount).toBe(1);
    pane.replaceThread({ ...pane.thread!, model: 'changed-model' });
    await flush();
    expect(controller.tasks.map((task) => task.rowId)).toEqual(['running']);
    expect(read).toHaveBeenCalledTimes(1);

    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      setBindingMock('ListLiveBackgroundTasks', async () => { throw new Error('read failed'); });
      emitWailsEvent('transport:gap', { channel: 'provider:background_tray' }, remote);
      await flush();
      expect(log).toHaveBeenCalled();
      expect(controller.tasks.map((task) => task.rowId)).toEqual(['running']);
      setBindingMock('ListLiveBackgroundTasks', async () => []);
      emitWailsEvent('transport:gap', { channel: 'provider:background_tray' }, remote);
      await flush();
      expect(controller.tasks).toEqual([]);
    } finally { log.mockRestore(); }
  });

  it('recovers only relevant owner gaps and removes all recovery listeners on unmount', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    noteThread(pane.threadId!, remote, 2);
    const read = setBindingMock('ListLiveBackgroundTasks', async () => [launch('running')]);
    release = $effect.root(() => createBackgroundController(() => pane, Date.now).mount());
    await flush();
    expect(read).toHaveBeenCalledTimes(1);
    emitWailsEvent('transport:gap', { channel: 'provider:background_tray' }, '');
    emitWailsEvent('transport:gap', { channel: 'git:status' }, remote);
    emitWailsEvent('transport:gap', { channel: 'provider:background_tasks_changed' }, remote);
    emitWailsEvent('transport:gap', { channel: 'provider:background_tray', threads: ['other-thread'] }, remote);
    await flush();
    expect(read).toHaveBeenCalledTimes(1);
    emitWailsEvent('transport:gap', { channel: 'provider:background_tray' }, remote);
    await flush();
    expect(read).toHaveBeenCalledTimes(2);
    release();
    release = () => {};
    emitWailsEvent('transport:gap', { channel: 'provider:background_tray' }, remote);
    __setBackendStatusForTest(remote, { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest(remote, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('reads after an item-event loss only when the loss may have touched its thread', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    noteThread(pane.threadId!, remote, 2);
    const read = setBindingMock('ListLiveBackgroundTasks', async () => [launch('running')]);
    release = $effect.root(() => createBackgroundController(() => pane, Date.now).mount());
    await flush();
    expect(read).toHaveBeenCalledTimes(1);
    emitWailsEvent('transport:gap', { channel: 'provider:item_event', seq: 1, threads: ['other-thread'] }, remote);
    await flush();
    expect(read).toHaveBeenCalledTimes(1);
    emitWailsEvent('transport:gap', { channel: 'provider:item_event', seq: 2, threads: ['other-thread', pane.threadId] }, remote);
    await flush();
    expect(read).toHaveBeenCalledTimes(2);
    emitWailsEvent('transport:gap', { channel: 'provider:item_event', seq: 3 }, remote);
    await flush();
    expect(read).toHaveBeenCalledTimes(3);
  });
});

describe('tray refresh reasons', () => {
  let release = () => {};
  afterEach(() => {
    release();
    release = () => {};
    resetItemEventQueue();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  async function mountTray(listed: Item[], thread?: Thread) {
    vi.useFakeTimers();
    const pane = await buildPane(thread);
    const read = setBindingMock('ListLiveBackgroundTasks', async () => listed);
    let controller!: ReturnType<typeof createBackgroundController>;
    release = $effect.root(() => {
      controller = createBackgroundController(() => pane, Date.now);
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(read).toHaveBeenCalledTimes(1);
    return { pane, read, controller };
  }

  function deliver(pane: { threadId: string | null }, items: Item[]): void {
    for (const item of items) applyItemStreamEvent({ action: 'upsert', threadId: pane.threadId!, item });
    flushItemEventQueue();
  }

  const agent = makeItem({ id: 'agent', kind: 'tool_call', toolName: 'Agent', status: 'running', isBackground: true });

  // Every writer replaces the snapshot, so the rows are held as read: a
  // deep proxy per row would only add cost to every tray derivation.
  it('holds the read rows themselves, not a reactive proxy per row', async () => {
    const { controller } = await mountTray([agent]);
    expect(controller.tasks[0].launch).toBe(agent);
  });

  it('reads nothing for 500 child completions streamed across many flushes', async () => {
    const { pane, read } = await mountTray([agent]);
    for (let batch = 0; batch < 50; batch += 1) {
      deliver(pane, Array.from({ length: 10 }, (_, i) => {
        const n = batch * 10 + i;
        return makeItem({
          id: `child-${n}:done`, kind: 'tool_completion', toolName: 'wait_agent', status: 'completed',
          parentId: 'agent', completionOf: `child-${n}`, itemIndex: n + 1,
        });
      }));
      await vi.advanceTimersByTimeAsync(60);
    }
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('reads nothing for a Claude thread\u2019s background launches and terminals, which its deltas carry', async () => {
    const { pane, read } = await mountTray([]);
    const launches = Array.from({ length: 5 }, (_, i) => makeItem({
      id: `bg-${i}`, kind: 'tool_call', toolName: 'Bash', status: 'running', isBackground: true, itemIndex: i + 1,
    }));
    deliver(pane, [...launches, ...launches.map((launch, i) => makeItem({
      id: `${launch.id}:done`, kind: 'tool_completion', toolName: 'Bash', status: 'completed',
      isBackground: true, completionOf: launch.id, itemIndex: i + 10,
    }))]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('reads once per coalesced burst of a Codex thread\u2019s background launches and terminals, once each when spaced', async () => {
    const { pane, read } = await mountTray([], makeThread({ provider: 'codex' }));
    const launches = Array.from({ length: 5 }, (_, i) => makeItem({
      id: `bg-${i}`, kind: 'tool_call', toolName: 'Bash', status: 'running', isBackground: true, itemIndex: i + 1,
    }));
    const terminals = launches.map((launch, i) => makeItem({
      id: `${launch.id}:done`, kind: 'tool_completion', toolName: 'Bash', status: 'completed',
      isBackground: true, completionOf: launch.id, itemIndex: i + 10,
    }));

    deliver(pane, [...launches, ...terminals]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(2);

    for (const item of [...launches, ...terminals]) {
      deliver(pane, [{ ...item, rev: item.rev + 1 }]);
      await vi.advanceTimersByTimeAsync(250);
    }
    expect(read).toHaveBeenCalledTimes(12);
  });

  it('reads when a listed Codex launch settles, and not when it is re-pushed still running', async () => {
    const nested = makeItem({ id: 'nested', kind: 'tool_call', toolName: 'Agent', status: 'running', parentId: 'agent' });
    const flagged = makeItem({ id: 'flagged', kind: 'tool_call', toolName: 'Bash', status: 'running', isBackground: true, itemIndex: 2 });
    const { pane, read } = await mountTray([agent, nested, flagged], makeThread({ provider: 'codex' }));

    deliver(pane, [{ ...nested, meta: JSON.stringify({ subagentDescendantCount: 3 }), rev: 2 }]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(1);

    // Settled in place: the result cleared the background flag.
    deliver(pane, [{ ...flagged, isBackground: false, status: 'completed', rev: 2 }]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(2);

    // Settled through a completion sibling that carries no background flag.
    deliver(pane, [makeItem({
      id: 'nested:done', kind: 'tool_completion', toolName: 'Agent', status: 'completed',
      parentId: 'agent', completionOf: 'nested', itemIndex: 3,
    })]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(3);
  });

  // A re-pushed launch the tray already lists is not a membership change:
  // it updates its row in place and never reads.
  function latestTool(summary: string, itemIndex: number, extra: Record<string, unknown> = {}): string {
    return JSON.stringify({
      ...extra,
      subagentLatestToolSummary: summary,
      subagentLatestToolTurnIndex: 0,
      subagentLatestToolItemIndex: itemIndex,
    });
  }

  function activity(controller: ReturnType<typeof createBackgroundController>, rowId: string): unknown {
    const task = controller.tasks.find((t) => t.rowId === rowId);
    return JSON.parse(task?.launch?.meta ?? '{}').subagentLatestToolSummary;
  }

  const claudeAgent = (id: string, meta?: string) => makeItem({
    id, kind: 'tool_call', toolName: 'Agent', status: 'running', isBackground: true, meta,
  });

  it('updates a listed Claude launch from a re-push with a new activity summary, with no read', async () => {
    const agent = claudeAgent('agent', latestTool('Read: a.ts', 3));
    const { pane, read, controller } = await mountTray([agent]);

    deliver(pane, [{ ...agent, rev: 2, meta: latestTool('Bash: pnpm test', 7, { subagentDescendantCount: 5 }) }]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(activity(controller, 'agent')).toBe('Bash: pnpm test');
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('keeps a listed launch\u2019s row for a re-push without the keys, with no read', async () => {
    const agent = claudeAgent('agent', latestTool('Read: a.ts', 3));
    const { pane, read, controller } = await mountTray([agent]);
    const before = controller.tasks[0].launch;

    deliver(pane, [{ ...agent, rev: 2, meta: JSON.stringify({ subagentDescendantCount: 5 }) }]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(controller.tasks[0].launch).toBe(before);
    expect(activity(controller, 'agent')).toBe('Read: a.ts');
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('leaves a Codex agent\u2019s runtime row alone when its settled spawn row is re-pushed', async () => {
    const runtime = makeItem({
      id: 'spawn', kind: 'tool_call', toolName: 'collab_agent', status: 'running', isBackground: true,
      meta: latestTool('Bash: ls', 2, { input: { tool: 'spawn_agent' } }),
    });
    const { pane, read, controller } = await mountTray([runtime], makeThread({ provider: 'codex' }));
    const before = controller.tasks[0].launch;

    deliver(pane, [{ ...runtime, status: 'completed', rev: 2, meta: latestTool('Read: stale.ts', 9, { input: { tool: 'spawn_agent' } }) }]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(controller.tasks[0].launch).toBe(before);
    expect(read).toHaveBeenCalledTimes(1);
  });

  // The tray shows anchor-level data only: it watches no agent scope and
  // takes no child row, open or closed. A Codex agent's latest tool comes
  // from the list read its tool calls ask for.
  it('watches no agent scope with its body open and takes a Codex agent\u2019s latest tool from the nudged read', async () => {
    const scopes: string[] = [];
    vi.spyOn(wsClient, 'setWatchedThreads').mockImplementation((_threads, watched) => {
      scopes.push(...watched.map((scope) => scope.scopeRootId));
    });
    const runtime = makeItem({
      id: 'spawn', kind: 'tool_call', toolName: 'collab_agent', status: 'running', isBackground: true,
      meta: latestTool('Bash: ls', 2),
    });
    const nested = makeItem({ id: 'nested', kind: 'tool_call', toolName: 'Bash', status: 'running', isBackground: true, parentId: 'outer' });
    const listed = [runtime, nested];
    const { pane, read, controller } = await mountTray(listed, makeThread({ provider: 'codex' }));
    pane.toggleActivityRailBackground();
    await vi.advanceTimersByTimeAsync(1_000);

    deliver(pane, [makeItem({ id: 'spawn:3', kind: 'tool_call', toolName: 'Read', status: 'running', parentId: 'spawn', summary: 'Read: child.ts', itemIndex: 3 })]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(activity(controller, 'spawn')).toBe('Bash: ls');
    expect(read).toHaveBeenCalledTimes(1);

    listed[0] = { ...runtime, meta: latestTool('Read: child.ts', 3) };
    emitWailsEvent('provider:background_tasks_changed', { threadId: pane.threadId }, '');
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(1);
    applyBackgroundTrayEvent({ threadId: pane.threadId!, refresh: true });
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(2);
    expect(activity(controller, 'spawn')).toBe('Read: child.ts');
    expect(scopes).toEqual([]);
  });

  // A Codex thread's runtime changes ask for a read while its agents
  // work; the scheduler holds a steady stream to one read per 400 ms.
  it('reads at most once per 400 ms under a steady refresh stream', async () => {
    const agents = Array.from({ length: 100 }, (_, i) => claudeAgent(`agent-${i}`, latestTool('start', 0)));
    const { pane, read } = await mountTray(agents);
    for (let elapsed = 0; elapsed < 2_000; elapsed += 50) {
      applyBackgroundTrayEvent({ threadId: pane.threadId!, refresh: true });
      await vi.advanceTimersByTimeAsync(50);
    }
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read.mock.calls.length - 1).toBeGreaterThanOrEqual(4);
    expect(read.mock.calls.length - 1).toBeLessThanOrEqual(6);
  });

  it('publishes each listed agent\u2019s run state by launch id from the reads, keeps it across re-pushes, and counts a parked agent as not running', async () => {
    const runState = (fields: Record<string, unknown>) => JSON.stringify({ ...JSON.parse(latestTool('Read: a.ts', 1)), ...fields });
    const parked = claudeAgent('parked', runState({
      subagentRunState: 'parked', subagentParkedCommands: 2,
      subagentParkedReportId: 'report-1', subagentParkedReportPreview: 'Found the race.',
    }));
    const working = claudeAgent('working', runState({ subagentRunState: 'running' }));
    const shell = makeItem({ id: 'shell', kind: 'tool_call', toolName: 'Bash', status: 'running', isBackground: true });
    const listed = [parked, working, shell];
    const { pane, read, controller } = await mountTray(listed);
    const threadId = pane.threadId!;
    expect(liveSubagentRunState(threadId, 'parked')).toEqual({ state: 'parked', waitingOn: 2, report: { id: 'report-1', preview: 'Found the race.' } });
    expect(liveSubagentRunState(threadId, 'working')).toEqual({ state: 'running', waitingOn: 0, report: null });
    expect(liveSubagentRunState(threadId, 'shell')).toBeNull();
    expect(liveSubagentRunState(threadId, 'unlisted')).toBeNull();
    expect(controller.count).toBe(3);
    expect(controller.runningCount).toBe(2);

    deliver(pane, [{ ...parked, rev: 2, meta: latestTool('Bash: ls', 4) }]);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(activity(controller, 'parked')).toBe('Bash: ls');
    expect(liveSubagentRunState(threadId, 'parked')?.state).toBe('parked');
    expect(read).toHaveBeenCalledTimes(1);

    applyBackgroundTrayEvent({
      threadId, launchIds: ['parked', 'working'], rows: [{ ...parked, meta: runState({ subagentRunState: 'running' }) }],
    });
    await vi.advanceTimersByTimeAsync(1_000);
    expect(liveSubagentRunState(threadId, 'parked')).toEqual({ state: 'running', waitingOn: 0, report: null });
    expect(liveSubagentRunState(threadId, 'working')).toBeNull();
    expect(controller.runningCount).toBe(2);
    expect(read).toHaveBeenCalledTimes(1);
  });

  // The scale this path exists for: every live agent's anchor is re-pushed
  // as its children are written. None of those pushes changes membership.
  it('reads nothing while 100 listed launches are re-pushed once a second for 10 s', async () => {
    const agents = Array.from({ length: 100 }, (_, i) => claudeAgent(`agent-${i}`, latestTool('start', 0)));
    const { pane, read, controller } = await mountTray(agents);
    for (let second = 1; second <= 10; second += 1) {
      deliver(pane, agents.map((agent) => ({ ...agent, rev: second, meta: latestTool(`step ${second}`, second) })));
      await vi.advanceTimersByTimeAsync(1_000);
    }
    expect(read).toHaveBeenCalledTimes(1);
    expect(activity(controller, 'agent-42')).toBe('step 10');
  });
});

describe('completion retention', () => {
  let release = () => {};
  afterEach(() => release());

  it('asks for the clock on a nested completion so the pair can prune', async () => {
    const pane = await buildPane();
    const threadId = pane.threadId!;
    const now = Date.now();
    const agent = makeItem({ id: 'agent', threadId, status: 'running', isBackground: true, toolName: 'Agent', createdAt: now - 60_000 });
    const bash = makeItem({ id: 'bash', threadId, status: 'running', isBackground: true, toolName: 'Bash', parentId: 'agent', createdAt: now - 5_000 });
    const done = makeItem({ id: 'bash-done', threadId, status: 'completed', completionOf: 'bash', parentId: 'agent', createdAt: now });
    setBindingMock('ListLiveBackgroundTasks', async () => [agent, bash, done]);
    // The shared clock last ticked before the completion landed; it only
    // advances again once the controller asks for it.
    let clock = $state(now - 30_000);
    let controller!: ReturnType<typeof createBackgroundController>;
    release = $effect.root(() => {
      controller = createBackgroundController(() => pane, () => clock);
      return controller.mount();
    });
    await flush();
    expect(controller.tasks.map((task) => [task.rowId, task.depth, task.status])).toEqual([
      ['agent', 0, 'running'],
      ['bash', 1, 'completed'],
    ]);
    expect(controller.hasPendingCompletion).toBe(true);
    clock = now + 1_000;
    await flush();
    expect(controller.tasks.map((task) => task.rowId)).toEqual(['agent']);
    expect(controller.count).toBe(1);
    expect(controller.hasPendingCompletion).toBe(false);
  });
});

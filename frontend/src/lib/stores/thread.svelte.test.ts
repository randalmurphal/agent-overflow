import { describe, expect, it, beforeEach, vi } from 'vitest';
import { createThreadPane, PAYLOAD_META_LIMIT } from './thread.svelte';
import type { Thread, Item, PayloadMeta } from '../types/models';
import type { ApprovalRequest, ContextWindow, RateLimitEntry, TokenUsage, ToolProgressMeta } from '../types/events';
import { setBindingMock } from '../../test/mocks/bindings-app';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp/workspace',
    projectPath: '/tmp/workspace',
    interactionMode: 'default',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function makeItem(overrides: Partial<Item> = {}): Item {
  return {
    id: 'item-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'message',
    role: 'assistant',
    summary: 'hello',
    createdAt: 0,
    ...overrides,
  };
}

describe('createThreadPane()', () => {
  let pane: ReturnType<typeof createThreadPane>;

  beforeEach(() => {
    pane = createThreadPane();
    // Default binding stubs used by switchThread/finalizeTurn.
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => [] as Item[]);
    setBindingMock('ListPayloadMetas', async () => [] as PayloadMeta[]);
  });

  describe('initial state', () => {
    it('starts with no thread selected', () => {
      expect(pane.thread).toBeNull();
      expect(pane.threadId).toBeNull();
    });

    it('starts with empty collections', () => {
      expect(pane.items).toEqual([]);
      expect(pane.payloadMetas.size).toBe(0);
      expect(pane.activeToolCalls.size).toBe(0);
      expect(pane.backgroundTasks.size).toBe(0);
      expect(pane.pendingApprovals).toEqual([]);
      expect(pane.rateLimits).toEqual([]);
    });

    it('starts with null streaming/session fields', () => {
      expect(pane.streamingContent).toBe('');
      expect(pane.sessionStatus).toBe('disconnected');
      expect(pane.tokenUsage).toBeNull();
      expect(pane.contextWindow).toBeNull();
      expect(pane.error).toBeNull();
      expect(pane.pendingMessage).toBeNull();
      expect(pane.loading).toBe(false);
    });
  });

  describe('switchThread()', () => {
    it('assigns thread, loads items, loads payload metas, and clears loading', async () => {
      const items = [makeItem({ id: 'a' }), makeItem({ id: 'b', itemIndex: 1 })];
      const metas: PayloadMeta[] = [
        { id: 'p1', kind: 'diff', meta: '{}', createdAt: 0 },
        { id: 'p2', kind: 'command_output', meta: '{}', createdAt: 0 },
      ];
      setBindingMock('ListItems', async () => items);
      setBindingMock('ListPayloadMetas', async () => metas);

      await pane.switchThread(makeThread());

      expect(pane.threadId).toBe('thread-1');
      expect(pane.items).toEqual(items);
      expect(pane.payloadMetas.size).toBe(2);
      expect(pane.payloadMetas.get('p1')).toEqual(metas[0]);
      expect(pane.loading).toBe(false);
    });

    it('resets all ephemeral state on switch', async () => {
      // Seed ephemeral state first.
      pane.appendTextDelta('hello');
      pane.addToolCall('tool-1', { toolName: 'bash' });
      pane.addBackgroundTask('bg-1', { name: 'build' });
      pane.setTokenUsage({ inputTokens: 10, outputTokens: 5 });
      pane.setContextWindow({ usedTokens: 100 });
      pane.setRateLimits([{ limitId: 'r1', limitName: 'm', usedPercent: 0.5, windowMins: 60, resetsAt: 0 }]);
      pane.addApproval({ requestId: 'a1', threadId: 'thread-1', toolName: 'bash', description: '', input: null, title: 't' });
      pane.setError('boom');
      pane.setPendingMessage('draft');
      pane.setSessionStatus('running');
      pane.addSessionApprovedTool('bash');

      await pane.switchThread(makeThread({ id: 'thread-2' }));

      expect(pane.streamingContent).toBe('');
      expect(pane.activeToolCalls.size).toBe(0);
      expect(pane.backgroundTasks.size).toBe(0);
      expect(pane.tokenUsage).toBeNull();
      expect(pane.contextWindow).toBeNull();
      expect(pane.rateLimits).toEqual([]);
      expect(pane.pendingApprovals).toEqual([]);
      expect(pane.error).toBeNull();
      expect(pane.pendingMessage).toBeNull();
      // sessionStatus resets to 'disconnected' until backend confirms otherwise.
      expect(pane.sessionStatus).toBe('disconnected');
      // sessionApprovedTools should reset too so a new thread starts fresh.
      expect(pane.isToolSessionApproved('bash')).toBe(false);
    });

    it('sets error state if ListItems fails', async () => {
      setBindingMock('ListItems', async () => { throw new Error('db gone'); });

      await pane.switchThread(makeThread());

      expect(pane.items).toEqual([]);
      expect(pane.error).toMatch(/Failed to load thread items/);
      expect(pane.loading).toBe(false);
    });

    it('falls back to empty payload metas if listing fails', async () => {
      setBindingMock('ListPayloadMetas', async () => { throw new Error('db gone'); });

      await pane.switchThread(makeThread());

      expect(pane.payloadMetas.size).toBe(0);
      // Payload failure is a warning, not a blocking error.
      expect(pane.error).toBeNull();
      expect(pane.loading).toBe(false);
    });

    it('logs a warning but does not throw if backend notify fails', async () => {
      setBindingMock('SwitchThread', async () => { throw new Error('rpc down'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      await pane.switchThread(makeThread());

      expect(pane.threadId).toBe('thread-1');
      expect(pane.loading).toBe(false);
      expect(consoleErr).toHaveBeenCalled();
      consoleErr.mockRestore();
    });

    // --- Bug D1 regression: drawer UI flags must not leak between threads ---
    it('resets showTerminal to false when switching threads and does not auto-restore', async () => {
      // Mount on thread A, open the terminal drawer.
      await pane.switchThread(makeThread({ id: 'thread-A' }));
      pane.setShowTerminal(true);
      expect(pane.showTerminal).toBe(true);

      // Switch to thread B: the drawer must be closed.
      await pane.switchThread(makeThread({ id: 'thread-B' }));
      expect(pane.showTerminal).toBe(false);

      // Switching back to A must not auto-restore the drawer — the user's
      // previous drawer state does not survive a thread switch.
      await pane.switchThread(makeThread({ id: 'thread-A' }));
      expect(pane.showTerminal).toBe(false);
    });

    it('resets diffPanel.open to false when switching threads', async () => {
      await pane.switchThread(makeThread({ id: 'thread-A' }));
      pane.setDiffPanelOpen(true);
      expect(pane.diffPanel.open).toBe(true);

      await pane.switchThread(makeThread({ id: 'thread-B' }));
      expect(pane.diffPanel.open).toBe(false);

      await pane.switchThread(makeThread({ id: 'thread-A' }));
      expect(pane.diffPanel.open).toBe(false);
    });

    // --- Bug D2 regression: slow ListItems from thread A must not clobber thread B ---
    it('ignores stale ListItems resolution when user switches threads during await', async () => {
      // Route each binding call to the thread-specific promise so switching
      // the mock between calls doesn't accidentally reroute A's in-flight
      // promise to B.
      let resolveA!: (items: Item[]) => void;
      const promiseA = new Promise<Item[]>((r) => { resolveA = r; });
      let resolveB!: (items: Item[]) => void;
      const promiseB = new Promise<Item[]>((r) => { resolveB = r; });

      setBindingMock('ListItems', (id: unknown) =>
        id === 'thread-A' ? promiseA : promiseB,
      );
      setBindingMock('ListPayloadMetas', async () => []);

      const switchA = pane.switchThread(makeThread({ id: 'thread-A' }));
      const switchB = pane.switchThread(makeThread({ id: 'thread-B' }));

      // Resolve B first so its awaiting code path writes `items = B`.
      resolveB([makeItem({ id: 'B-item', threadId: 'thread-B' })]);
      // Flush microtasks so switchB actually settles before A's late
      // resolution gets a chance to overwrite the pane.
      await switchB;

      expect(pane.items.map((i) => i.id)).toEqual(['B-item']);

      // Resolve A late — the older switch must not write its items back.
      resolveA([makeItem({ id: 'A-stale', threadId: 'thread-A' })]);
      await switchA;

      expect(pane.thread?.id).toBe('thread-B');
      expect(pane.items.map((i) => i.id)).toEqual(['B-item']);
    });

    it('ignores stale ListPayloadMetas resolution when thread switches mid-flight', async () => {
      let resolveMetasA!: (metas: PayloadMeta[]) => void;
      const metasPromiseA = new Promise<PayloadMeta[]>((r) => { resolveMetasA = r; });
      let resolveMetasB!: (metas: PayloadMeta[]) => void;
      const metasPromiseB = new Promise<PayloadMeta[]>((r) => { resolveMetasB = r; });

      setBindingMock('ListItems', async () => []);
      setBindingMock('ListPayloadMetas', (id: unknown) =>
        id === 'thread-A' ? metasPromiseA : metasPromiseB,
      );

      const switchA = pane.switchThread(makeThread({ id: 'thread-A' }));
      const switchB = pane.switchThread(makeThread({ id: 'thread-B' }));

      resolveMetasB([{ id: 'meta-B', kind: 'diff', meta: '{}', createdAt: 0 }]);
      await switchB;

      expect(Array.from(pane.payloadMetas.keys())).toEqual(['meta-B']);

      resolveMetasA([{ id: 'meta-A-stale', kind: 'diff', meta: '{}', createdAt: 0 }]);
      await switchA;

      expect(pane.thread?.id).toBe('thread-B');
      expect(Array.from(pane.payloadMetas.keys())).toEqual(['meta-B']);
    });

    it('late ListItems failure from prior switch does not wipe items on current thread', async () => {
      let rejectA!: (err: Error) => void;
      const promiseA = new Promise<Item[]>((_r, rej) => { rejectA = rej; });
      promiseA.catch(() => {});

      setBindingMock('ListItems', (id: unknown) => {
        if (id === 'thread-A') return promiseA;
        return Promise.resolve([makeItem({ id: 'B-item', threadId: 'thread-B' })]);
      });
      setBindingMock('ListPayloadMetas', async () => []);

      const switchA = pane.switchThread(makeThread({ id: 'thread-A' }));
      const switchB = pane.switchThread(makeThread({ id: 'thread-B' }));

      await switchB;
      expect(pane.items.map((i) => i.id)).toEqual(['B-item']);

      rejectA(new Error('late failure on A'));
      await switchA.catch(() => {});
      await Promise.resolve();

      expect(pane.thread?.id).toBe('thread-B');
      expect(pane.items.map((i) => i.id)).toEqual(['B-item']);
      expect(pane.error).toBeNull();
    });
  });

  describe('clear()', () => {
    it('empties all state and bumps turnGeneration', async () => {
      await pane.switchThread(makeThread());
      pane.appendTextDelta('hi');
      pane.addBackgroundTask('bg', {});
      pane.setError('bad');

      pane.clear();

      expect(pane.thread).toBeNull();
      expect(pane.items).toEqual([]);
      expect(pane.streamingContent).toBe('');
      expect(pane.backgroundTasks.size).toBe(0);
      expect(pane.error).toBeNull();
    });
  });

  describe('streaming content', () => {
    it('accumulates deltas in order', () => {
      pane.appendTextDelta('Hello, ');
      pane.appendTextDelta('world');
      pane.appendTextDelta('!');
      expect(pane.streamingContent).toBe('Hello, world!');
    });

    it('tolerates empty-string deltas', () => {
      pane.appendTextDelta('abc');
      pane.appendTextDelta('');
      pane.appendTextDelta('def');
      expect(pane.streamingContent).toBe('abcdef');
    });
  });

  describe('isTurnActive', () => {
    it('is false in the idle default state', () => {
      expect(pane.isTurnActive).toBe(false);
    });

    it('is true while streaming text is accumulating', () => {
      pane.appendTextDelta('hello');
      expect(pane.isTurnActive).toBe(true);
    });

    it('is true while at least one tool call is in flight', () => {
      pane.addToolCall('tool-1', { toolName: 'bash' });
      expect(pane.isTurnActive).toBe(true);
      pane.completeToolCall('tool-1');
      expect(pane.isTurnActive).toBe(false);
    });

    it('is true while a pendingMessage is set (optimistic turn)', () => {
      pane.setPendingMessage('hi');
      expect(pane.isTurnActive).toBe(true);
      pane.setPendingMessage(null);
      expect(pane.isTurnActive).toBe(false);
    });

    it('finalizeTurn clears every component so isTurnActive returns to false', async () => {
      await pane.switchThread(makeThread());
      pane.appendTextDelta('streaming');
      pane.addToolCall('tool-1', { toolName: 'bash' });
      pane.setPendingMessage('queued');
      expect(pane.isTurnActive).toBe(true);
      pane.finalizeTurn();
      expect(pane.isTurnActive).toBe(false);
    });
  });

  describe('finalizeTurn()', () => {
    it('clears streaming and active tool calls and reloads items', async () => {
      await pane.switchThread(makeThread());
      pane.appendTextDelta('streaming...');
      pane.addToolCall('tool-1', { toolName: 'bash' });
      pane.setPendingMessage('sent');

      const reloaded = [makeItem({ id: 'x' })];
      setBindingMock('ListItems', async () => reloaded);

      pane.finalizeTurn();
      // finalizeTurn schedules a promise; let it resolve.
      await Promise.resolve();
      await Promise.resolve();

      expect(pane.streamingContent).toBe('');
      expect(pane.activeToolCalls.size).toBe(0);
      expect(pane.pendingMessage).toBeNull();
      expect(pane.items).toEqual(reloaded);
    });

    it('ignores stale ListItems response if another turn starts first', async () => {
      await pane.switchThread(makeThread());

      // Set up a slow ListItems that we control manually.
      let resolveFirst!: (items: Item[]) => void;
      const firstPromise = new Promise<Item[]>((r) => { resolveFirst = r; });
      setBindingMock('ListItems', () => firstPromise);

      pane.finalizeTurn();
      // Snapshot what we'll later assert should not leak in.
      const staleItems = [makeItem({ id: 'stale' })];

      // Second finalizeTurn bumps turnGeneration and swaps to a fresh reload.
      const freshItems = [makeItem({ id: 'fresh' })];
      setBindingMock('ListItems', async () => freshItems);
      pane.finalizeTurn();
      await Promise.resolve();
      await Promise.resolve();

      expect(pane.items).toEqual(freshItems);

      // Now resolve the *first* promise — the stale reload should be ignored.
      resolveFirst(staleItems);
      await Promise.resolve();
      await Promise.resolve();
      expect(pane.items).toEqual(freshItems);
    });

    it('ignores stale ListItems if thread switches before it resolves', async () => {
      await pane.switchThread(makeThread({ id: 'thread-A' }));

      let resolveA!: (items: Item[]) => void;
      const firstPromise = new Promise<Item[]>((r) => { resolveA = r; });
      setBindingMock('ListItems', () => firstPromise);

      pane.finalizeTurn();

      // Switch to thread-B (bumps turnGeneration internally).
      setBindingMock('ListItems', async () => [makeItem({ id: 'B-item' })]);
      await pane.switchThread(makeThread({ id: 'thread-B' }));

      // Late resolution from thread-A must not overwrite thread-B's items.
      resolveA([makeItem({ id: 'A-late' })]);
      await Promise.resolve();
      await Promise.resolve();

      expect(pane.items.map((i) => i.id)).toEqual(['B-item']);
    });

    it('does nothing reload-wise when no thread is selected', async () => {
      pane.appendTextDelta('stray');
      pane.finalizeTurn();
      expect(pane.streamingContent).toBe('');
      // Should not have attempted to call ListItems.
      await Promise.resolve();
      expect(pane.items).toEqual([]);
    });
  });

  describe('tool call lifecycle', () => {
    it('adds and removes by id', () => {
      pane.addToolCall('t1', { toolName: 'bash' });
      pane.addToolCall('t2', { toolName: 'edit' });
      expect(pane.activeToolCalls.size).toBe(2);
      expect(pane.activeToolCalls.get('t1')).toEqual({ toolName: 'bash' });

      pane.completeToolCall('t1');
      expect(pane.activeToolCalls.size).toBe(1);
      expect(pane.activeToolCalls.has('t1')).toBe(false);
      expect(pane.activeToolCalls.has('t2')).toBe(true);
    });

    it('completeToolCall on unknown id is a no-op', () => {
      pane.addToolCall('t1', {});
      pane.completeToolCall('unknown');
      expect(pane.activeToolCalls.size).toBe(1);
    });

    it('updateToolProgress only updates known tool calls', () => {
      pane.addToolCall('t1', { toolName: 'bash' });
      const progress: ToolProgressMeta = { current: 2, total: 10, message: 'step 2' };
      pane.updateToolProgress('t1', progress);
      expect(pane.activeToolCalls.get('t1')).toEqual(progress);

      pane.updateToolProgress('unknown', progress);
      expect(pane.activeToolCalls.has('unknown')).toBe(false);
    });

    it('duplicate addToolCall overwrites data for the same id', () => {
      pane.addToolCall('t1', { toolName: 'old' });
      pane.addToolCall('t1', { toolName: 'new' });
      expect(pane.activeToolCalls.size).toBe(1);
      expect(pane.activeToolCalls.get('t1')).toEqual({ toolName: 'new' });
    });
  });

  describe('approvals', () => {
    const approval = (id: string, toolName = 'bash'): ApprovalRequest => ({
      requestId: id,
      threadId: 'thread-1',
      toolName,
      description: '',
      input: null,
      title: `approve ${toolName}`,
    });

    it('addApproval appends to pending list preserving order', () => {
      pane.addApproval(approval('a1'));
      pane.addApproval(approval('a2'));
      pane.addApproval(approval('a3'));
      expect(pane.pendingApprovals.map((a) => a.requestId)).toEqual(['a1', 'a2', 'a3']);
    });

    it('removeApproval removes by requestId', () => {
      pane.addApproval(approval('a1'));
      pane.addApproval(approval('a2'));
      pane.removeApproval('a1');
      expect(pane.pendingApprovals).toHaveLength(1);
      expect(pane.pendingApprovals[0].requestId).toBe('a2');
    });

    it('removeApproval on unknown id is a no-op', () => {
      pane.addApproval(approval('a1'));
      pane.removeApproval('missing');
      expect(pane.pendingApprovals).toHaveLength(1);
    });
  });

  describe('background tasks', () => {
    it('add and complete operate by id', () => {
      pane.addBackgroundTask('bg-1', { name: 'build' });
      pane.addBackgroundTask('bg-2', { name: 'test' });
      expect(pane.backgroundTasks.size).toBe(2);
      pane.completeBackgroundTask('bg-1');
      expect(pane.backgroundTasks.has('bg-1')).toBe(false);
      expect(pane.backgroundTasks.has('bg-2')).toBe(true);
    });

    it('completing an unknown id is a no-op', () => {
      pane.addBackgroundTask('bg-1', {});
      pane.completeBackgroundTask('missing');
      expect(pane.backgroundTasks.size).toBe(1);
    });
  });

  describe('session status transitions', () => {
    it('transitions through connected -> running -> ready -> error', () => {
      pane.setSessionStatus('connected');
      expect(pane.sessionStatus).toBe('connected');
      pane.setSessionStatus('running');
      expect(pane.sessionStatus).toBe('running');
      pane.setSessionStatus('ready');
      expect(pane.sessionStatus).toBe('ready');
      pane.setSessionStatus('error');
      expect(pane.sessionStatus).toBe('error');
    });
  });

  describe('token usage, context window, rate limits', () => {
    it('setTokenUsage stores the usage payload', () => {
      const usage: TokenUsage = {
        inputTokens: 100,
        outputTokens: 50,
        cacheReadInputTokens: 20,
        totalCostUsd: 0.002,
      };
      pane.setTokenUsage(usage);
      expect(pane.tokenUsage).toEqual(usage);
    });

    it('setContextWindow and setRateLimits store snapshots', () => {
      const cw: ContextWindow = { usedTokens: 1000, maxTokens: 8000, usedPercentage: 12.5 };
      pane.setContextWindow(cw);
      expect(pane.contextWindow).toEqual(cw);

      const limits: RateLimitEntry[] = [
        { limitId: 'l1', limitName: '1h', usedPercent: 0.3, windowMins: 60, resetsAt: 1000 },
      ];
      pane.setRateLimits(limits);
      expect(pane.rateLimits).toEqual(limits);
    });
  });

  describe('payload metas', () => {
    it('addPayloadMeta upserts by id', () => {
      const m1: PayloadMeta = { id: 'p1', kind: 'diff', meta: '{"v":1}', createdAt: 0 };
      const m1b: PayloadMeta = { id: 'p1', kind: 'diff', meta: '{"v":2}', createdAt: 0 };
      pane.addPayloadMeta(m1);
      pane.addPayloadMeta(m1b);
      expect(pane.payloadMetas.size).toBe(1);
      expect(pane.payloadMetas.get('p1')?.meta).toBe('{"v":2}');
    });

    // --- Bug D6 regression: LRU cap ---
    it('caps the map at PAYLOAD_META_LIMIT entries', () => {
      for (let i = 0; i < PAYLOAD_META_LIMIT + 100; i += 1) {
        pane.addPayloadMeta({
          id: `p-${i}`,
          kind: 'diff',
          meta: String(i),
          createdAt: i,
        });
      }
      expect(pane.payloadMetas.size).toBe(PAYLOAD_META_LIMIT);
      // First 100 inserts are the oldest and must have been evicted.
      expect(pane.payloadMetas.has('p-0')).toBe(false);
      expect(pane.payloadMetas.has('p-99')).toBe(false);
      // The newest are retained.
      expect(pane.payloadMetas.has(`p-${PAYLOAD_META_LIMIT + 99}`)).toBe(true);
    });

    it('touchPayloadMeta bumps the entry so it survives future evictions', () => {
      // Fill exactly to capacity; p-0 is the oldest.
      for (let i = 0; i < PAYLOAD_META_LIMIT; i += 1) {
        pane.addPayloadMeta({
          id: `p-${i}`,
          kind: 'diff',
          meta: '',
          createdAt: i,
        });
      }
      // Touch p-0 to move it to the tail.
      const meta = pane.touchPayloadMeta('p-0');
      expect(meta?.id).toBe('p-0');
      // Adding one more entry should now evict p-1, not p-0.
      pane.addPayloadMeta({
        id: 'p-new',
        kind: 'diff',
        meta: '',
        createdAt: 10_000,
      });
      expect(pane.payloadMetas.size).toBe(PAYLOAD_META_LIMIT);
      expect(pane.payloadMetas.has('p-0')).toBe(true);
      expect(pane.payloadMetas.has('p-1')).toBe(false);
      expect(pane.payloadMetas.has('p-new')).toBe(true);
    });

    it('re-adding an existing id refreshes its LRU position', () => {
      for (let i = 0; i < PAYLOAD_META_LIMIT; i += 1) {
        pane.addPayloadMeta({ id: `p-${i}`, kind: 'diff', meta: '', createdAt: i });
      }
      // Touch p-0 via re-add (e.g. event router updates meta).
      pane.addPayloadMeta({ id: 'p-0', kind: 'diff', meta: 'v2', createdAt: 0 });
      // Cause an eviction by adding one more.
      pane.addPayloadMeta({ id: 'p-new', kind: 'diff', meta: '', createdAt: 10_000 });
      expect(pane.payloadMetas.has('p-0')).toBe(true);
      expect(pane.payloadMetas.get('p-0')?.meta).toBe('v2');
      expect(pane.payloadMetas.has('p-1')).toBe(false);
    });

    it('switchThread truncates hydrated metas to the LRU cap', async () => {
      const huge: PayloadMeta[] = [];
      for (let i = 0; i < PAYLOAD_META_LIMIT + 50; i += 1) {
        huge.push({ id: `q-${i}`, kind: 'diff', meta: '', createdAt: i });
      }
      setBindingMock('ListPayloadMetas', async () => huge);

      await pane.switchThread(makeThread({ id: 'thread-huge' }));

      expect(pane.payloadMetas.size).toBe(PAYLOAD_META_LIMIT);
      // Newest PAYLOAD_META_LIMIT are kept; oldest 50 are dropped.
      expect(pane.payloadMetas.has('q-0')).toBe(false);
      expect(pane.payloadMetas.has(`q-${PAYLOAD_META_LIMIT + 49}`)).toBe(true);
    });

    it('touchPayloadMeta on an unknown id is a no-op', () => {
      pane.addPayloadMeta({ id: 'p-1', kind: 'diff', meta: '', createdAt: 0 });
      expect(pane.touchPayloadMeta('never-seen')).toBeUndefined();
      expect(pane.payloadMetas.size).toBe(1);
    });
  });

  describe('session-approved tools', () => {
    it('addSessionApprovedTool and isToolSessionApproved round-trip', () => {
      expect(pane.isToolSessionApproved('bash')).toBe(false);
      pane.addSessionApprovedTool('bash');
      expect(pane.isToolSessionApproved('bash')).toBe(true);
      expect(pane.isToolSessionApproved('edit')).toBe(false);
    });
  });

  describe('thread field mutations', () => {
    beforeEach(async () => {
      await pane.switchThread(makeThread({ title: 'original', model: 'model-a' }));
    });

    it('updateTitle updates only the title on the current thread', () => {
      pane.updateTitle('renamed');
      expect(pane.thread?.title).toBe('renamed');
      expect(pane.thread?.model).toBe('model-a');
    });

    it('updateModel updates only the model on the current thread', () => {
      pane.updateModel('model-b');
      expect(pane.thread?.model).toBe('model-b');
      expect(pane.thread?.title).toBe('original');
    });

    it('updateTitle is a no-op when no thread is selected', () => {
      pane.clear();
      pane.updateTitle('should not take');
      expect(pane.thread).toBeNull();
    });

    it('replaceThread swaps the entire thread object', () => {
      pane.replaceThread(makeThread({ id: 'thread-1', title: 'new', model: 'new-model' }));
      expect(pane.thread?.title).toBe('new');
      expect(pane.thread?.model).toBe('new-model');
    });
  });

  describe('error and pending message', () => {
    it('setError / clearError round-trip', () => {
      pane.setError('boom');
      expect(pane.error).toBe('boom');
      pane.clearError();
      expect(pane.error).toBeNull();
    });

    it('setPendingMessage stores and clears on null', () => {
      pane.setPendingMessage('draft');
      expect(pane.pendingMessage).toBe('draft');
      pane.setPendingMessage(null);
      expect(pane.pendingMessage).toBeNull();
    });
  });
});

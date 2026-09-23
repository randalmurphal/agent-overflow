// One item-event flush commits a pane's window once: row events that follow
// a pending upsert of their row apply after the batch, in arrival order, and
// a flush that reaches no window row moves none of the pane's structural
// revisions. What the flush needs from an event is derived once, at ingest.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Counts row keys without changing them.
const keyCalls = vi.hoisted(() => ({ count: 0 }));
vi.mock('../utils/compositeKey', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../utils/compositeKey')>();
  return {
    ...actual,
    compositeKey: (...parts: (string | number | boolean)[]) => {
      keyCalls.count += 1;
      return actual.compositeKey(...parts);
    },
  };
});
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { activityRunProse, activityRunRow, activityRunStub } from '../../test/helpers/activityRuns';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { applyItemStreamEvent, flushItemEventQueue, resetItemEventQueue } from './eventsItemStream';
import { resetPanesForTest } from './panes.svelte';
import { registerTimelineSurface, type TimelineMutation } from './timelineSurfaces';
import type { Item } from '../types/models';
import type { ItemStreamEvent } from '../types/events';

beforeEach(installThreadPaneTestEnv);
afterEach(() => {
  resetItemEventQueue();
  resetPanesForTest();
});

const threadId = 't';

function push(...events: ItemStreamEvent[]): void {
  for (const evt of events) applyItemStreamEvent(evt);
}

function upsert(item: Item): ItemStreamEvent {
  return { action: 'upsert', threadId: item.threadId, item };
}

function patch(itemId: string, fields: Omit<Extract<ItemStreamEvent, { action: 'patch' }>['patch'], 'rev'> & { rev?: number }): ItemStreamEvent {
  return { action: 'patch', threadId, itemId, kind: 'tool_call', patch: { rev: 0, ...fields } };
}

function tool(id: string, itemIndex: number, overrides: Partial<Item> = {}): Item {
  return makeItem({ id, threadId, turnIndex: 1, itemIndex, kind: 'tool_call', toolName: 'Bash', status: 'running', summary: `run ${id}`, ...overrides });
}

describe('item event flush', () => {
  it('commits a burst of interleaved upserts, patches, metas and deltas once', async () => {
    const pane = await buildPane(makeThread({ id: threadId }), [makeItem({ id: 'prompt', threadId, turnIndex: 1, itemIndex: 0, kind: 'user_text', role: 'user' })]);
    const apply = vi.spyOn(pane, 'applyProviderItemUpserts');
    const text = makeItem({ id: 'text', threadId, turnIndex: 1, itemIndex: 100, kind: 'assistant_text', status: 'streaming', summary: '' });
    for (let i = 1; i <= 50; i += 1) {
      push(
        upsert(tool(`tool-${i}`, i)),
        patch(`tool-${i}`, { status: 'completed', summary: `ran ${i}`, updatedAt: 10 + i }),
        { action: 'meta', threadId, itemId: `tool-${i}`, kind: 'tool_call', meta: `{"n":${i}}`, updatedAt: 10 + i },
      );
    }
    push(
      upsert(text),
      { action: 'delta', threadId, itemId: 'text', kind: 'assistant_text', delta: 'hello', updatedAt: 200 },
      { action: 'delta', threadId, itemId: 'text', kind: 'assistant_text', delta: ' world', updatedAt: 201 },
    );

    flushItemEventQueue();
    pane.__flushItemSmoothersForTest();

    expect(apply).toHaveBeenCalledOnce();
    expect(pane.items).toHaveLength(52);
    for (let i = 1; i <= 50; i += 1) {
      expect(pane.getItemById(`tool-${i}`)).toMatchObject({ status: 'completed', summary: `ran ${i}`, meta: `{"n":${i}}` });
    }
    expect(pane.getItemById('text')?.summary).toBe('hello world');
  });

  it('keeps each row\'s order when it is upserted again after a deferred event', async () => {
    const pane = await buildPane(makeThread({ id: threadId }));
    const apply = vi.spyOn(pane, 'applyProviderItemUpserts');
    push(
      upsert(tool('a', 1, { summary: 'first' })),
      patch('a', { summary: 'patched', updatedAt: 2 }),
      upsert(tool('b', 2, { summary: 'other' })),
      upsert(tool('a', 1, { summary: 'second', updatedAt: 3 })),
      { action: 'meta', threadId, itemId: 'a', kind: 'tool_call', meta: '{"m":1}', updatedAt: 3 },
    );

    flushItemEventQueue();

    // The second upsert of `a` must land after the patch that preceded it,
    // so the batch splits there and nowhere else.
    expect(apply).toHaveBeenCalledTimes(2);
    expect(pane.getItemById('a')).toMatchObject({ summary: 'second', meta: '{"m":1}' });
    expect(pane.getItemById('b')?.summary).toBe('other');
  });

  it('does not coalesce a row\'s deferred deltas across its deferred correction', async () => {
    const pane = await buildPane(makeThread({ id: threadId }));
    const apply = vi.spyOn(pane, 'applyProviderItemUpserts');
    const text = makeItem({ id: 'text', threadId, turnIndex: 1, itemIndex: 1, kind: 'assistant_text', status: 'streaming', summary: '' });
    const delta = (value: string, updatedAt: number): ItemStreamEvent =>
      ({ action: 'delta', threadId, itemId: 'text', kind: 'assistant_text', delta: value, updatedAt });
    push(
      upsert(text),
      delta('draft', 2),
      { action: 'patch', threadId, itemId: 'text', kind: 'assistant_text', patch: { rev: 0, summary: 'corrected ', updatedAt: 3 } },
      delta('tail', 4),
    );

    flushItemEventQueue();
    pane.__flushItemSmoothersForTest();

    expect(apply).toHaveBeenCalledOnce();
    expect(pane.getItemById('text')?.summary).toBe('corrected tail');
  });

  it('lands a row\'s queued delta before a later upsert of that row replaces its text', async () => {
    const text = makeItem({ id: 'text', threadId, turnIndex: 1, itemIndex: 1, kind: 'assistant_text', status: 'streaming', summary: 'a' });
    const pane = await buildPane(makeThread({ id: threadId }), [text]);
    push(
      { action: 'delta', threadId, itemId: 'text', kind: 'assistant_text', delta: 'b', updatedAt: 2 },
      upsert({ ...text, summary: 'ab', updatedAt: 3 }),
      // Another row's removal applies the pending upserts ahead of the tail.
      upsert(tool('other', 2)),
      { action: 'remove', threadId, itemId: 'other' },
    );

    flushItemEventQueue();
    pane.__flushItemSmoothersForTest();

    expect(pane.getItemById('text')?.summary).toBe('ab');
  });

  it('applies a deferred patch before a removal of its row', async () => {
    const pane = await buildPane(makeThread({ id: threadId }));
    push(
      upsert(tool('a', 1)),
      patch('a', { status: 'completed', updatedAt: 2 }),
      { action: 'remove', threadId, itemId: 'a' },
      upsert(tool('b', 2)),
    );

    flushItemEventQueue();

    expect(pane.items.map(item => item.id)).toEqual(['b']);
  });

  it('moves no structural revision for a flush of subagent children only', async () => {
    const rows = [
      activityRunProse('p0', 0),
      activityRunRow('b', 2),
      activityRunRow('c', 3),
      activityRunRow('d', 4),
      activityRunRow('agent', 7, { toolName: 'Agent', status: 'running', summary: 'Agent: work' }),
    ];
    const pane = await buildPane(makeThread({ id: threadId }), rows, 'main', [activityRunStub()]);
    const items = pane.items;
    const timelineRevision = pane.timelineRevision;
    const runsRevision = pane.activityRuns.revision;
    const windowRevision = pane.activityRuns.windowRevision;
    const memory = pane.debugMemoryStats();

    for (let i = 0; i < 40; i += 1) {
      push(
        upsert(activityRunRow(`child-${i}`, 8 + i, { parentId: 'agent', status: 'running' })),
        {
          action: 'patch', threadId, itemId: `child-${i}`, parentId: 'agent', kind: 'tool_call',
          patch: { rev: 1, status: 'completed', updatedAt: 9 },
        },
      );
    }
    flushItemEventQueue();

    expect(pane.items).toBe(items);
    expect(pane.timelineRevision).toBe(timelineRevision);
    expect(pane.activityRuns.revision).toBe(runsRevision);
    expect(pane.activityRuns.windowRevision).toBe(windowRevision);
    expect(pane.debugMemoryStats()).toEqual(memory);
  });

  it('drops a delta or patch whose parentId is malformed', async () => {
    const text = makeItem({ id: 'text', threadId, turnIndex: 1, itemIndex: 1, kind: 'assistant_text', status: 'streaming', summary: 'a' });
    const pane = await buildPane(makeThread({ id: threadId }), [text]);
    const malformed = 42 as unknown as string;
    push(
      { action: 'delta', threadId, itemId: 'text', parentId: malformed, kind: 'assistant_text', delta: 'b', updatedAt: 2 },
      { action: 'patch', threadId, itemId: 'text', parentId: malformed, kind: 'assistant_text', patch: { rev: 1, status: 'completed', updatedAt: 3 } },
    );
    flushItemEventQueue();
    pane.__flushItemSmoothersForTest();

    expect(pane.getItemById('text')).toMatchObject({ summary: 'a', status: 'streaming' });
  });

  it('coalesces each of a row\'s delta kinds on its own, in the order it arrived', () => {
    const deltas: Array<[string, string, string]> = [];
    const surface = (id: string) => registerTimelineSurface({
      threadId: id, backend: () => undefined, refresh: async () => {},
      apply: (mutation: TimelineMutation) => {
        if (mutation.kind === 'delta') deltas.push([mutation.event.itemId, mutation.event.kind, mutation.event.delta]);
      },
    });
    const release = surface(threadId);
    const delta = (itemId: string, kind: string, value: string): ItemStreamEvent =>
      ({ action: 'delta', threadId, itemId, kind, delta: value, updatedAt: 1 });
    push(
      delta('a', 'assistant_text', 'one '),
      delta('b', 'assistant_text', 'other'),
      delta('a', 'thinking', 'think'),
      delta('a', 'assistant_text', 'two'),
    );

    flushItemEventQueue();
    release();

    // Rows are independent, so only each row's own order is asserted.
    expect(deltas.filter(([id]) => id === 'a')).toEqual([
      ['a', 'assistant_text', 'one two'],
      ['a', 'thinking', 'think'],
    ]);
    expect(deltas.filter(([id]) => id === 'b')).toEqual([['b', 'assistant_text', 'other']]);
  });
});

describe('item event ingest', () => {
  // Validation and sizing each read the item; a Proxy counts them apart.
  // `inputPayloadId` is absent from these items, so only validation reads
  // it (sizing walks own keys), and only sizing enumerates the keys.
  function counted(item: Item, tally: { validations: number; sizings: number }): Item {
    return new Proxy(item, {
      get(target, key, receiver) {
        if (key === 'inputPayloadId') tally.validations += 1;
        return Reflect.get(target, key, receiver);
      },
      ownKeys(target) {
        tally.sizings += 1;
        return Reflect.ownKeys(target);
      },
    });
  }

  it('validates, sizes and keys each event once, and the flush repeats none of it', () => {
    // No pane shows the thread, so nothing but this module touches the rows.
    const tally = { validations: 0, sizings: 0 };
    keyCalls.count = 0;
    for (let i = 0; i < 20; i += 1) push(upsert(counted(tool(`row-${i}`, i), tally)));
    for (let i = 0; i < 20; i += 1) {
      push({ action: 'delta', threadId, itemId: 'stream', kind: 'assistant_text', delta: 'ab', updatedAt: i });
    }
    for (let i = 0; i < 20; i += 1) {
      push({ action: 'delta', threadId, itemId: `row-${i}`, kind: 'assistant_text', delta: 'ab', updatedAt: i });
    }
    for (let i = 0; i < 20; i += 1) push(patch(`row-${i}`, { status: 'completed', updatedAt: i }));
    const ingested = { ...tally, keys: keyCalls.count };

    flushItemEventQueue();

    expect(ingested).toEqual({ validations: 20, sizings: 20, keys: 80 });
    expect({ ...tally, keys: keyCalls.count }).toEqual(ingested);
  });

  it('fails a frame whose id cannot key its row, and the rest of its batch applies', async () => {
    const pane = await buildPane(makeThread({ id: threadId }));
    push(upsert(tool('before', 1)));
    expect(() => push(upsert(tool('bad\u0000id', 2)))).toThrow(/NUL separator/);
    push(upsert(tool('after', 3)));

    flushItemEventQueue();

    expect(pane.items.map(item => item.id)).toEqual(['before', 'after']);
  });
});

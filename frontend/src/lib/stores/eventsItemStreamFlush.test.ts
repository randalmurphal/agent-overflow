// One item-event flush commits a pane's window once: row events that follow
// a pending upsert of their row apply after the batch, in arrival order, and
// a flush that reaches no window row moves none of the pane's structural
// revisions.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { activityRunProse, activityRunRow, activityRunStub } from '../../test/helpers/activityRuns';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { applyItemStreamEvent, flushItemEventQueue, resetItemEventQueue } from './eventsItemStream';
import { resetPanesForTest } from './panes.svelte';
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

    for (let i = 0; i < 40; i += 1) {
      push(
        upsert(activityRunRow(`child-${i}`, 8 + i, { parentId: 'agent', status: 'running' })),
        { action: 'patch', threadId, itemId: `child-${i}`, kind: 'tool_call', patch: { rev: 1, status: 'completed', updatedAt: 9 } },
      );
    }
    flushItemEventQueue();

    expect(pane.items).toBe(items);
    expect(pane.timelineRevision).toBe(timelineRevision);
    expect(pane.activityRuns.revision).toBe(runsRevision);
    expect(pane.activityRuns.windowRevision).toBe(windowRevision);
    expect(pane.subagentLiveAggregate('agent')).toMatchObject({ count: 40, activePreview: '' });
  });
});

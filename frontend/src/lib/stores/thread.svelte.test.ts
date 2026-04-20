import { beforeEach, describe, expect, it } from 'vitest';
import { createThreadPane } from './thread.svelte';
import type { Item } from '../types/models';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem, makeThread } from '../../test/helpers/chat';

describe('createThreadPane', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => [] as Item[]);
  });

  it('starts empty', () => {
    const pane = createThreadPane();

    expect(pane.thread).toBeNull();
    expect(pane.threadId).toBeNull();
    expect(pane.items).toEqual([]);
    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.contextWindow).toBeNull();
    expect(pane.error).toBeNull();
    expect(pane.isTurnActive).toBe(false);
  });

  it('loads items and seeds the context window from thread.lastTokenUsage', async () => {
    const pane = createThreadPane();
    const items = [
      makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
      makeItem({ id: 'text:0:0', itemIndex: 1, summary: 'hello back' }),
    ];
    setBindingMock('ListItems', async () => items);

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
    });
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
    pane.setError('boom');
    pane.setShowTerminal(true);
    pane.setShowPlanSidebar(true);

    await pane.switchThread(makeThread({ id: 'thread-b' }));

    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.error).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.showPlanSidebar).toBe(false);
  });

  it('ignores stale ListItems resolutions after a second thread switch', async () => {
    const pane = createThreadPane();
    let resolveA!: (items: Item[]) => void;
    let resolveB!: (items: Item[]) => void;
    const listA = new Promise<Item[]>((resolve) => { resolveA = resolve; });
    const listB = new Promise<Item[]>((resolve) => { resolveB = resolve; });

    setBindingMock('ListItems', (threadId: string) => (
      threadId === 'thread-a' ? listA : listB
    ));

    const switchA = pane.switchThread(makeThread({ id: 'thread-a' }));
    const switchB = pane.switchThread(makeThread({ id: 'thread-b' }));

    resolveB([makeItem({ id: 'b', threadId: 'thread-b', summary: 'from b' })]);
    await switchB;
    resolveA([makeItem({ id: 'a', threadId: 'thread-a', summary: 'from a' })]);
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

  it('derives turn activity from streaming text, running tools, and approvals', () => {
    const pane = createThreadPane();

    expect(pane.isTurnActive).toBe(false);

    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    expect(pane.isTurnActive).toBe(true);

    pane.upsertItem(makeItem({
      id: 'text:0:0',
      kind: 'assistant_text',
      status: 'completed',
    }));
    expect(pane.isTurnActive).toBe(false);

    pane.upsertItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: false,
    }));
    expect(pane.isTurnActive).toBe(true);

    pane.upsertItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
    }));
    expect(pane.isTurnActive).toBe(false);

    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      toolName: 'edit',
      description: 'Allow edit?',
      input: null,
      title: 'Approve edit',
    });
    expect(pane.isTurnActive).toBe(true);

    pane.removeApproval('req-1');
    expect(pane.isTurnActive).toBe(false);
  });

  it('clear resets the pane completely', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread());
    pane.upsertItem(makeItem({ id: 'x' }));
    pane.setError('boom');
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      toolName: 'bash',
      description: 'Allow bash?',
      input: null,
      title: 'Approve bash',
    });

    pane.clear();

    expect(pane.thread).toBeNull();
    expect(pane.items).toEqual([]);
    expect(pane.pendingApprovals).toEqual([]);
    expect(pane.contextWindow).toBeNull();
    expect(pane.error).toBeNull();
  });
});

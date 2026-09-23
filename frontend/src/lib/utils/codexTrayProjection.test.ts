import { describe, expect, it, vi } from 'vitest';
import type { Item } from '../types/models';
import { CODEX_LATEST_TOOL_META, createCodexLatestToolProjection } from './codexTrayProjection';

function item(overrides: Partial<Item>): Item {
  return {
    id: 'item',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'running',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    rev: 0,
    ...overrides,
  };
}

function agent(id: string, overrides: Partial<Item> = {}): Item {
  return item({ id, toolName: 'collab_agent', summary: `spawn ${id}`, ...overrides });
}

function tool(parentId: string, itemIndex: number, summary: string, overrides: Partial<Item> = {}): Item {
  return item({ id: `${parentId}:${itemIndex}`, parentId, itemIndex, toolName: 'Bash', summary, ...overrides });
}

function latest(row: Item): unknown {
  return JSON.parse(row.meta ?? '{}')[CODEX_LATEST_TOOL_META.summary];
}

describe('createCodexLatestToolProjection', () => {
  it('moves the latest tool forward and ignores an older or unchanged one', () => {
    const projection = createCodexLatestToolProjection();
    const snapshot = [agent('a', { meta: JSON.stringify({ input: { tool: 'spawn_agent' } }) })];
    projection.reset(snapshot);

    const first = projection.apply(snapshot, tool('a', 1, ' Bash: pnpm test '));
    expect(latest(first[0])).toBe('Bash: pnpm test');
    expect(JSON.parse(first[0].meta!).input).toEqual({ tool: 'spawn_agent' });

    const newer = projection.apply(first, tool('a', 2, 'Read: newest.ts'));
    expect(latest(newer[0])).toBe('Read: newest.ts');

    expect(projection.apply(newer, tool('a', 1, 'Bash: pnpm test (done)'))).toBe(newer);
    expect(projection.apply(newer, tool('a', 2, 'Read: newest.ts'))).toBe(newer);
    const grown = projection.apply(newer, tool('a', 2, 'Read: newest.ts (2 files)'));
    expect(latest(grown[0])).toBe('Read: newest.ts (2 files)');
  });

  it('ignores tools that do not belong to a running agent row of the snapshot', () => {
    const projection = createCodexLatestToolProjection();
    const snapshot = [
      agent('done', { status: 'completed' }),
      item({ id: 'bash', toolName: 'Bash' }),
      agent('a'),
    ];
    projection.reset(snapshot);
    expect(projection.apply(snapshot, tool('done', 1, 'x'))).toBe(snapshot);
    expect(projection.apply(snapshot, tool('bash', 1, 'x'))).toBe(snapshot);
    expect(projection.apply(snapshot, tool('missing', 1, 'x'))).toBe(snapshot);
    expect(projection.apply(snapshot, tool('a', 1, '   '))).toBe(snapshot);
    expect(projection.apply(snapshot, tool('a', 1, 'spawn nested', { toolName: 'collab_agent' }))).toBe(snapshot);
  });

  it('logs malformed agent meta and leaves the row alone', () => {
    const projection = createCodexLatestToolProjection();
    const snapshot = [agent('a', { meta: '{not json' })];
    projection.reset(snapshot);
    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      expect(projection.apply(snapshot, tool('a', 1, 'Bash: ls'))).toBe(snapshot);
      expect(log).toHaveBeenCalledWith('ActivityRail: malformed Codex launch meta for a');
    } finally {
      log.mockRestore();
    }
  });

  it('follows each reset snapshot, not the positions of an older one', () => {
    const projection = createCodexLatestToolProjection();
    projection.reset([agent('a'), agent('b')]);
    const reordered = [agent('b'), item({ id: 'x', toolName: 'Bash' }), agent('a')];
    projection.reset(reordered);
    const next = projection.apply(reordered, tool('a', 1, 'Read: a.ts'));
    expect(latest(next[2])).toBe('Read: a.ts');
    expect(next[0]).toBe(reordered[0]);
    expect(next[1]).toBe(reordered[1]);
  });

  // The projection runs once per streamed child tool call of every Codex
  // agent, so its cost must not grow with the tray. A scan reads every row
  // per call; the index reads the parent's row and, on a change, copies the
  // snapshot once.
  it('reads only the parent row per tool call, whatever the tray size', () => {
    const projection = createCodexLatestToolProjection();
    const rows = Array.from({ length: 1_000 }, (_, i) => agent(`a${i}`));
    projection.reset(rows);
    let reads = 0;
    const counted = new Proxy(rows, {
      get(target, key, receiver) {
        if (typeof key === 'string' && /^\d+$/.test(key)) reads += 1;
        return Reflect.get(target, key, receiver);
      },
    });
    for (let i = 0; i < 1_000; i += 1) {
      expect(projection.apply(counted, tool('elsewhere', i, 'Bash: ls'))).toBe(counted);
      expect(projection.apply(counted, tool(`a${i}`, 0, '   '))).toBe(counted);
    }
    expect(reads).toBe(0);

    const next = projection.apply(counted, tool('a500', 1, 'Read: a.ts'));
    expect(latest(next[500])).toBe('Read: a.ts');
    reads = 0;
    const current = new Proxy(next, {
      get(target, key, receiver) {
        if (typeof key === 'string' && /^\d+$/.test(key)) reads += 1;
        return Reflect.get(target, key, receiver);
      },
    });
    expect(projection.apply(current, tool('a500', 0, 'Bash: older'))).toBe(current);
    expect(reads).toBe(1);
  });
});

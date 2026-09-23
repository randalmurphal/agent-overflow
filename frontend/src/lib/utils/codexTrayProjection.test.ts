import { describe, expect, it, vi } from 'vitest';
import type { Item } from '../types/models';
import { TRAY_LATEST_TOOL_META, createTrayLatestToolProjection } from './codexTrayProjection';

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
  return JSON.parse(row.meta ?? '{}')[TRAY_LATEST_TOOL_META.summary];
}

function decorated(summary: unknown, turnIndex: unknown, itemIndex: unknown, extra: Record<string, unknown> = {}): string {
  return JSON.stringify({
    ...extra,
    [TRAY_LATEST_TOOL_META.summary]: summary,
    [TRAY_LATEST_TOOL_META.turnIndex]: turnIndex,
    [TRAY_LATEST_TOOL_META.itemIndex]: itemIndex,
  });
}

describe('createTrayLatestToolProjection: Codex child tools', () => {
  it('moves the latest tool forward and ignores an older or unchanged one', () => {
    const projection = createTrayLatestToolProjection();
    const snapshot = [agent('a', { meta: JSON.stringify({ input: { tool: 'spawn_agent' } }) })];
    projection.reset(snapshot);

    const first = projection.applyChildTool(snapshot, tool('a', 1, ' Bash: pnpm test '));
    expect(latest(first[0])).toBe('Bash: pnpm test');
    expect(JSON.parse(first[0].meta!).input).toEqual({ tool: 'spawn_agent' });

    const newer = projection.applyChildTool(first, tool('a', 2, 'Read: newest.ts'));
    expect(latest(newer[0])).toBe('Read: newest.ts');

    expect(projection.applyChildTool(newer, tool('a', 1, 'Bash: pnpm test (done)'))).toBe(newer);
    expect(projection.applyChildTool(newer, tool('a', 2, 'Read: newest.ts'))).toBe(newer);
    const grown = projection.applyChildTool(newer, tool('a', 2, 'Read: newest.ts (2 files)'));
    expect(latest(grown[0])).toBe('Read: newest.ts (2 files)');
  });

  it('ignores tools that do not belong to a running agent row of the snapshot', () => {
    const projection = createTrayLatestToolProjection();
    const snapshot = [
      agent('done', { status: 'completed' }),
      item({ id: 'bash', toolName: 'Bash' }),
      agent('a'),
    ];
    projection.reset(snapshot);
    expect(projection.applyChildTool(snapshot, tool('done', 1, 'x'))).toBe(snapshot);
    // A running row, but not an agent: plain rows have no child tools.
    expect(projection.applyChildTool(snapshot, tool('bash', 1, 'x'))).toBe(snapshot);
    expect(projection.applyChildTool(snapshot, tool('missing', 1, 'x'))).toBe(snapshot);
    expect(projection.applyChildTool(snapshot, tool('a', 1, '   '))).toBe(snapshot);
    expect(projection.applyChildTool(snapshot, tool('a', 1, 'spawn nested', { toolName: 'collab_agent' }))).toBe(snapshot);
  });

  it('logs malformed agent meta and leaves the row alone', () => {
    const projection = createTrayLatestToolProjection();
    const snapshot = [agent('a', { meta: '{not json' })];
    projection.reset(snapshot);
    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      expect(projection.applyChildTool(snapshot, tool('a', 1, 'Bash: ls'))).toBe(snapshot);
      expect(log).toHaveBeenCalledWith('ActivityRail: malformed tray row meta for a');
    } finally {
      log.mockRestore();
    }
  });

  it('follows each reset snapshot, not the positions of an older one', () => {
    const projection = createTrayLatestToolProjection();
    projection.reset([agent('a'), agent('b')]);
    const reordered = [agent('b'), item({ id: 'x', toolName: 'Bash' }), agent('a')];
    projection.reset(reordered);
    const next = projection.applyChildTool(reordered, tool('a', 1, 'Read: a.ts'));
    expect(latest(next[2])).toBe('Read: a.ts');
    expect(next[0]).toBe(reordered[0]);
    expect(next[1]).toBe(reordered[1]);
  });

  // The projection runs once per streamed child tool call of every Codex
  // agent, so its cost must not grow with the tray. A scan reads every row
  // per call; the index reads the parent's row and, on a change, copies the
  // snapshot once.
  it('reads only the parent row per tool call, whatever the tray size', () => {
    const projection = createTrayLatestToolProjection();
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
      expect(projection.applyChildTool(counted, tool('elsewhere', i, 'Bash: ls'))).toBe(counted);
      expect(projection.applyChildTool(counted, tool(`a${i}`, 0, '   '))).toBe(counted);
    }
    expect(reads).toBe(0);

    const next = projection.applyChildTool(counted, tool('a500', 1, 'Read: a.ts'));
    expect(latest(next[500])).toBe('Read: a.ts');
    reads = 0;
    const current = new Proxy(next, {
      get(target, key, receiver) {
        if (typeof key === 'string' && /^\d+$/.test(key)) reads += 1;
        return Reflect.get(target, key, receiver);
      },
    });
    expect(projection.applyChildTool(current, tool('a500', 0, 'Bash: older'))).toBe(current);
    expect(reads).toBe(1);
  });
});

describe('createTrayLatestToolProjection: re-pushed launches', () => {
  it('carries a newer decoration onto the listed row and keeps everything else', () => {
    const projection = createTrayLatestToolProjection();
    const row = item({ id: 'L', toolName: 'Agent', meta: decorated('Read: a.ts', 0, 3, { subagentProgress: { toolUses: 2 } }) });
    const other = item({ id: 'M', toolName: 'Agent' });
    const snapshot = [row, other];
    projection.reset(snapshot);

    const pushed = { ...row, summary: 'renamed', meta: decorated(' Bash: ls ', 0, 5, { subagentDescendantCount: 9 }) };
    const next = projection.applyPushedLaunch(snapshot, pushed);
    expect(latest(next[0])).toBe('Bash: ls');
    expect(JSON.parse(next[0].meta!).subagentProgress).toEqual({ toolUses: 2 });
    expect(JSON.parse(next[0].meta!).subagentDescendantCount).toBeUndefined();
    expect(next[0].summary).toBe(row.summary);
    expect(next[1]).toBe(other);
  });

  it('keeps the row for a push without the decoration, an older one, or an unchanged one', () => {
    const projection = createTrayLatestToolProjection();
    const snapshot = [item({ id: 'L', toolName: 'Agent', meta: decorated('Bash: ls', 1, 5) })];
    projection.reset(snapshot);
    for (const meta of [
      undefined,
      JSON.stringify({ subagentDescendantCount: 4 }),
      decorated('   ', 2, 0),
      decorated('Bash: ls', '1', 6),
      decorated('Read: older.ts', 1, 4),
      decorated('Read: older.ts', 0, 9),
      decorated('Bash: ls', 1, 5),
    ]) {
      expect(projection.applyPushedLaunch(snapshot, { ...snapshot[0], meta })).toBe(snapshot);
    }
    expect(projection.applyPushedLaunch(snapshot, { ...snapshot[0], id: 'unlisted', meta: decorated('x', 9, 9) })).toBe(snapshot);
  });

  it('never decorates a row that is not a running launch of the snapshot', () => {
    const projection = createTrayLatestToolProjection();
    const snapshot = [
      item({ id: 'done', toolName: 'Agent', status: 'completed' }),
      item({ id: 'L:done', toolName: 'Agent', status: 'completed', completionOf: 'L', kind: 'tool_completion' }),
    ];
    projection.reset(snapshot);
    for (const target of snapshot) {
      expect(projection.applyPushedLaunch(snapshot, { ...target, meta: decorated('x', 9, 9) })).toBe(snapshot);
    }
  });
});

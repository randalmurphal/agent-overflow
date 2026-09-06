import { beforeEach, expect, it } from 'vitest';
import { __resetEntityIndexForTest, captureThreadMetadataRead, currentThreadRow, forgetBackendEntities, noteRowsFromCall, noteThread, onThreadOwnershipChanged, resolveThreadBackend, threadBackend } from './entityIndex';

beforeEach(__resetEntityIndexForTest);

it.each([false, true])('keeps the new owner regardless of catalog order (reverse %s)', (reverse) => {
  const catalogs = [{ backend: 'old', epoch: 0 }, { backend: 'new', epoch: 1 }];
  if (reverse) catalogs.reverse();
  for (const { backend, epoch } of catalogs) noteRowsFromCall(1090132042, [{ id: 'thread', ownershipEpoch: epoch }], backend);
  expect(resolveThreadBackend('thread')).toBe('new');
  expect(currentThreadRow({ id: 'thread', ownershipEpoch: 0 }, 'old')).toBe(false);
  expect(currentThreadRow({ id: 'thread', ownershipEpoch: 1 }, 'new')).toBe(true);
  expect(noteThread('thread', 'old')).toBe(false);
  expect(noteThread('thread', 'old', 0)).toBe(false);
  expect(forgetBackendEntities('old').threadIds).toEqual([]);
  expect(threadBackend('thread')).toBe('new');
});

it('accepts a returning conversation and rejects late frames from each former owner', () => {
  noteThread('thread', 'mac', 0);
  noteThread('thread', 'gpu', 1);
  noteThread('thread', 'mac', 2);
  expect(noteThread('thread', 'gpu', 1)).toBe(false);
  expect(noteThread('thread', 'mac', 0)).toBe(false);
  expect(resolveThreadBackend('thread')).toBe('mac');
});

it('refuses conflicting claims instead of choosing an execution host by arrival order', () => {
  noteThread('thread', 'mac', 3);
  expect(noteThread('thread', 'gpu', 3)).toBe(false);
  expect(() => resolveThreadBackend('thread')).toThrow('Two computers claim');
  expect(currentThreadRow({ id: 'thread', ownershipEpoch: 3 }, 'mac')).toBe(false);
  expect(currentThreadRow({ id: 'thread', ownershipEpoch: 3 }, 'gpu')).toBe(false);
  noteThread('thread', 'mac', 3);
  expect(() => resolveThreadBackend('thread')).toThrow('Two computers claim');
  noteThread('thread', 'gpu', 4);
  expect(resolveThreadBackend('thread')).toBe('gpu');
});

it('publishes the former computer before callers replace a moved row', () => {
  const origins: string[] = [];
  const stop = onThreadOwnershipChanged((id, previous) => origins.push(`${id}:${previous}:${threadBackend(id)}`));
  try {
    noteThread('thread', 'mac', 0);
    noteThread('thread', 'gpu', 1);
    noteThread('thread', 'mac', 0);
    noteThread('thread', 'mac', 2);
    expect(origins).toEqual(['thread:mac:gpu', 'thread:gpu:mac']);
  } finally { stop(); }
});

it.each([-1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, '2', null])('ignores invalid ownership epoch %s', (epoch) => {
  expect(noteThread('thread', 'gpu', epoch)).toBe(false);
  expect(threadBackend('thread')).toBeUndefined();
});

it('a search hint cannot undo a move; a stamped search result can teach its new owner', () => {
  noteThread('thread', 'mac', 0);
  noteRowsFromCall(3644945077, [{ threadId: 'thread', ownershipEpoch: 1 }], 'gpu');
  noteRowsFromCall(3644945077, [{ threadId: 'thread' }], 'mac');
  expect(resolveThreadBackend('thread')).toBe('gpu');
});

it('learns hidden workflow threads only from declared metadata, preserving a newer move', () => {
  noteThread('moved', 'gpu', 1);
  noteRowsFromCall(70120675, {
    item: { id: 'run', projectId: 'project', triageThreadId: 'triage' },
    phases: [{ threadId: 'phase' }], units: [{ threadId: 'moved' }],
    outputs: { threadId: 'output-is-not-an-owner' },
  }, 'mac');
  expect(threadBackend('triage')).toBe('mac');
  expect(threadBackend('phase')).toBe('mac');
  expect(threadBackend('moved')).toBe('gpu');
  expect(threadBackend('output-is-not-an-owner')).toBeUndefined();
  expect(forgetBackendEntities('mac').workflowItemIds).toEqual(['run']);
});

it.each([1090132042, 2451527188, 3644945077, 1098302047])('guards pending metadata %s including an unindexed archived conversation after forgetting its new owner', (method) => {
  const old = captureThreadMetadataRead(method, 'mac')!;
  const fresh = captureThreadMetadataRead(method, 'gpu')!;
  const shape = (epoch: number) => {
    const row = { id: 'thread', threadId: 'thread', ownershipEpoch: epoch };
    return method === 1098302047 ? row : [row];
  };
  try {
    noteThread('thread', 'gpu', 1);
    forgetBackendEntities('gpu');
    expect(() => old.verify(shape(0))).toThrow('ownership changed');
    expect(() => old.verify(shape(2))).not.toThrow();
    expect(() => old.verify(method === 1098302047 ? null : [])).not.toThrow();
    expect(() => fresh.verify(shape(1))).toThrow('removed');
    expect(captureThreadMetadataRead(42, 'mac')).toBeUndefined();
  } finally { old.release(); fresh.release(); }
});

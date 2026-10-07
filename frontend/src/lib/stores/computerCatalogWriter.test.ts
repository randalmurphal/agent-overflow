import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { computerCatalogWriter } from './computerCatalogWriter';
import * as replicaSession from '../replica/session';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { detachBackend } from '../transport/backends';
import { observeCatalogStamp, resetCatalogStampsForTest } from '../replica/catalogStamp';
import type { ThreadGroup } from '../types/models';

const cache = { token: 1, write: vi.fn<typeof replicaSession.putReplicaCatalog>() };
beforeEach(() => {
  cache.write.mockResolvedValue(undefined);
  vi.spyOn(replicaSession, 'putReplicaCatalog').mockImplementation(cache.write);
  vi.spyOn(replicaSession, 'replicaToken').mockImplementation(() => cache.token);
});
afterEach(() => {
  resetCatalogStampsForTest();
  resetStagedBackends();
  vi.restoreAllMocks();
  cache.write.mockReset();
  cache.token = 1;
});
const group = (id: string): ThreadGroup => ({ id, name: id, projectId: 'p', createdAt: 0, updatedAt: 0 });

it('coalesces a burst, writes only its computer, and records an empty catalog after deletion', async () => {
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  let rows = [group('gpu'), group('local')];
  const writer = computerCatalogWriter('groups', () => rows, (next) => { rows = next; }, (row) => row.id === 'local' ? '' : 'gpu');
  writer.persist('gpu');
  writer.persist('gpu');
  await vi.waitFor(() => expect(cache.write).toHaveBeenCalledTimes(1));
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', [group('gpu')], 'stamp', 1);
  rows = [group('local')];
  writer.persist('gpu');
  await vi.waitFor(() => expect(cache.write).toHaveBeenCalledTimes(2));
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', [], 'stamp', 1);
});

it('does not write queued rows to a removed computer or a new generation', async () => {
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  const writer = computerCatalogWriter('groups', () => [group('gpu')], () => {}, () => 'gpu');
  writer.persist('gpu');
  ++cache.token;
  await Promise.resolve();
  expect(cache.write).not.toHaveBeenCalled();
  writer.persist('gpu');
  detachBackend('gpu');
  await Promise.resolve();
  expect(cache.write).not.toHaveBeenCalled();
});

it('keeps at most one write running and saves the newest rows after it finishes', async () => {
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  let done!: () => void;
  cache.write.mockImplementationOnce(() => new Promise<void>((resolve) => { done = resolve; }));
  let rows = [group('first')];
  const writer = computerCatalogWriter('groups', () => rows, (next) => { rows = next; }, () => 'gpu');
  writer.persist('gpu');
  await Promise.resolve();
  rows = [group('last')];
  writer.persist('gpu');
  writer.persist('gpu');
  await Promise.resolve();
  expect(cache.write).toHaveBeenCalledTimes(1);
  done();
  await vi.waitFor(() => expect(cache.write).toHaveBeenCalledTimes(2));
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', [group('last')], 'stamp', 1);
});

it('cancels queued writes on reset and accepts new changes', async () => {
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  let rows = [group('before-reset')];
  const writer = computerCatalogWriter('groups', () => rows, (next) => { rows = next; }, () => 'gpu');
  writer.persist('gpu');
  writer.reset();
  await Promise.resolve();
  expect(cache.write).not.toHaveBeenCalled();

  rows = [group('after-reset')];
  writer.persist('gpu');
  await vi.waitFor(() => expect(cache.write).toHaveBeenCalledTimes(1));
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', rows, 'stamp', 1);
});

it('commits a mutation, persists its computer, and skips a commit that changes nothing', async () => {
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  let rows = [group('a')];
  const commit = vi.fn((next: ThreadGroup[]) => { rows = next; });
  const writer = computerCatalogWriter('groups', () => rows, commit, () => 'gpu');
  writer.mutate('gpu', (current) => current.map((row) => row.id === 'missing' ? group('x') : row));
  expect(commit).not.toHaveBeenCalled();
  writer.mutate('gpu', (current) => [...current, group('b')]);
  expect(commit).toHaveBeenCalledTimes(1);
  expect(rows.map((row) => row.id)).toEqual(['a', 'b']);
  await vi.waitFor(() => expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', rows, 'stamp', 1));
});

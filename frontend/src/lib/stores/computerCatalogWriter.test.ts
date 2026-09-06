import { afterEach, expect, it, vi } from 'vitest';
import { computerCatalogWriter } from './computerCatalogWriter';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { detachBackend } from '../transport/backends';
import { observeCatalogStamp, resetCatalogStampsForTest } from '../replica/catalogStamp';
import type { ThreadGroup } from '../types/models';

const cache = vi.hoisted(() => ({ token: 1, write: vi.fn(async () => {}) }));
vi.mock('../replica/session', () => ({
  putReplicaCatalog: (...args: unknown[]) => cache.write(...args as []),
  replicaToken: () => cache.token,
}));
afterEach(() => { resetCatalogStampsForTest(); resetStagedBackends(); cache.write.mockReset(); cache.token = 1; });
const group = (id: string): ThreadGroup => ({ id, name: id, projectId: 'p', createdAt: 0, updatedAt: 0 });

it('coalesces a burst, writes only its computer, and records an empty catalog after deletion', async () => {
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  let rows = [group('gpu'), group('local')];
  const writer = computerCatalogWriter('groups', () => rows, (row) => row.id === 'local' ? '' : 'gpu');
  writer.changed('gpu');
  writer.changed('gpu');
  await vi.waitFor(() => expect(cache.write).toHaveBeenCalledTimes(1));
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', [group('gpu')], 'stamp', 1);
  rows = [group('local')];
  writer.changed('gpu');
  await vi.waitFor(() => expect(cache.write).toHaveBeenCalledTimes(2));
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', [], 'stamp', 1);
});

it('does not write queued rows to a removed computer or a new generation', async () => {
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  const writer = computerCatalogWriter('groups', () => [group('gpu')], () => 'gpu');
  writer.changed('gpu');
  ++cache.token;
  await Promise.resolve();
  expect(cache.write).not.toHaveBeenCalled();
  writer.changed('gpu');
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
  const writer = computerCatalogWriter('groups', () => rows, () => 'gpu');
  writer.changed('gpu');
  await Promise.resolve();
  rows = [group('last')];
  writer.changed('gpu');
  writer.changed('gpu');
  await Promise.resolve();
  expect(cache.write).toHaveBeenCalledTimes(1);
  done();
  await vi.waitFor(() => expect(cache.write).toHaveBeenCalledTimes(2));
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', [group('last')], 'stamp', 1);
});

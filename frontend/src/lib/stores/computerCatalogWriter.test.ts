import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { CATALOG_WRITE_SPACING_MS, computerCatalogWriter as createWriter } from './computerCatalogWriter';
import * as replicaSession from '../replica/session';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { detachBackend } from '../transport/backends';
import { observeCatalogStamp, resetCatalogStampsForTest } from '../replica/catalogStamp';
import type { ThreadGroup } from '../types/models';

const cache = { token: 1, write: vi.fn<typeof replicaSession.putReplicaCatalog>() };
// A writer outlives its test in the page-hidden flush, so each is reset.
const writers: Array<{ reset(): void }> = [];
const computerCatalogWriter: typeof createWriter = (...args) => {
  const writer = createWriter(...args);
  writers.push(writer);
  return writer;
};
beforeEach(() => {
  cache.write.mockResolvedValue(undefined);
  vi.spyOn(replicaSession, 'putReplicaCatalog').mockImplementation(cache.write);
  vi.spyOn(replicaSession, 'replicaToken').mockImplementation(() => cache.token);
});
afterEach(() => {
  for (const writer of writers.splice(0)) writer.reset();
  vi.useRealTimers();
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
  writer.reset();
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
  vi.useFakeTimers();
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
  // The spacing ends while the first write still runs: the next waits for it.
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(cache.write).toHaveBeenCalledTimes(1);
  done();
  await vi.advanceTimersByTimeAsync(0);
  expect(cache.write).toHaveBeenCalledTimes(2);
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', [group('last')], 'stamp', 1);
});

it('starts a computer\'s writes a spacing apart and writes the newest rows when it ends', async () => {
  vi.useFakeTimers();
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  let rows = [group('first')];
  const writer = computerCatalogWriter('groups', () => rows, (next) => { rows = next; }, () => 'gpu');
  writer.persist('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(cache.write).toHaveBeenCalledTimes(1);

  // A change every 100 ms for five spacings: one write per spacing.
  for (let i = 0; i < (5 * CATALOG_WRITE_SPACING_MS) / 100; i++) {
    rows = [group(`change-${i}`)];
    writer.persist('gpu');
    await vi.advanceTimersByTimeAsync(100);
  }
  expect(cache.write).toHaveBeenCalledTimes(6);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', rows, 'stamp', 1);

  // After a quiet spell the next change is written at once.
  const settled = cache.write.mock.calls.length;
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  rows = [group('after-quiet')];
  writer.persist('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(cache.write).toHaveBeenCalledTimes(settled + 1);
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', rows, 'stamp', 1);
});

it('spaces each computer apart from the others', async () => {
  vi.useFakeTimers();
  stageBackend({ id: 'gpu' });
  stageBackend({ id: 'lab' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  observeCatalogStamp('lab', 'groups', 'stamp');
  const rows = [group('gpu'), group('lab')];
  const writer = computerCatalogWriter('groups', () => rows, () => {}, (row) => row.id);
  writer.persist('gpu');
  await vi.advanceTimersByTimeAsync(0);
  writer.persist('gpu');
  writer.persist('lab');
  await vi.advanceTimersByTimeAsync(0);
  expect(cache.write.mock.calls.map((call) => call[0])).toEqual(['gpu', 'lab']);
});

it('does not write rows waiting out a spacing to a new generation', async () => {
  vi.useFakeTimers();
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  const writer = computerCatalogWriter('groups', () => [group('gpu')], () => {}, () => 'gpu');
  writer.persist('gpu');
  await vi.advanceTimersByTimeAsync(0);
  writer.persist('gpu');
  ++cache.token;
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(cache.write).toHaveBeenCalledTimes(1);
});

it('writes what is waiting when the page goes hidden', async () => {
  vi.useFakeTimers();
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  let rows = [group('first')];
  const writer = computerCatalogWriter('groups', () => rows, (next) => { rows = next; }, () => 'gpu');
  writer.persist('gpu');
  await vi.advanceTimersByTimeAsync(0);
  rows = [group('closing')];
  writer.persist('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(cache.write).toHaveBeenCalledTimes(1);

  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
  document.dispatchEvent(new Event('visibilitychange'));
  await vi.advanceTimersByTimeAsync(0);
  expect(cache.write).toHaveBeenCalledTimes(2);
  expect(cache.write).toHaveBeenLastCalledWith('gpu', 'groups', rows, 'stamp', 1);

  // Becoming visible again does not cut a spacing short.
  rows = [group('back')];
  writer.persist('gpu');
  visibility.mockReturnValue('visible');
  document.dispatchEvent(new Event('visibilitychange'));
  await vi.advanceTimersByTimeAsync(0);
  expect(cache.write).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(cache.write).toHaveBeenCalledTimes(3);
});

it('forgets a spacing on reset', async () => {
  vi.useFakeTimers();
  stageBackend({ id: 'gpu' });
  observeCatalogStamp('gpu', 'groups', 'stamp');
  const writer = computerCatalogWriter('groups', () => [group('gpu')], () => {}, () => 'gpu');
  writer.persist('gpu');
  await vi.advanceTimersByTimeAsync(0);
  writer.reset();
  writer.persist('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(cache.write).toHaveBeenCalledTimes(2);
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

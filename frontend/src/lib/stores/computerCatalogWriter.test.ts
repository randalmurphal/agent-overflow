import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  CATALOG_WRITE_SPACING_MS, computerCatalogWriter, persistCatalogTree, resetCatalogWritesForTest,
} from './computerCatalogWriter';
import * as replicaSession from '../replica/session';
import { stageBackend } from '../../test/helpers/backends';
import { detachBackend } from '../transport/backends';
import { invalidateCatalogStamp, observeCatalogStamp } from '../replica/catalogStamp';
import {
  catalogProvenance,
  catalogTreeOf,
  markCatalogTree,
  PARTIAL_PROVENANCE,
  settleCatalogAnswers,
  settleCatalogRead,
} from './catalogLoad.svelte';
import type { CatalogKind } from '../replica/catalog';
import type { ProjectWithCounts, Thread, ThreadGroup } from '../types/models';
import type { BackendKey } from '../transport/backendKey';

const replica = { token: 1, tree: vi.fn<typeof replicaSession.putReplicaTree>() };
beforeEach(() => {
  replica.tree.mockResolvedValue('written');
  vi.spyOn(replicaSession, 'putReplicaTree').mockImplementation(replica.tree);
  vi.spyOn(replicaSession, 'replicaToken').mockImplementation(() => replica.token);
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  replica.tree.mockReset();
  replica.token = 1;
});

const KINDS: readonly CatalogKind[] = ['projects', 'threads', 'groups'];
const group = (id: string): ThreadGroup => ({ id, name: id, projectId: 'p', createdAt: 0, updatedAt: 0 });
const project = (id: string): ProjectWithCounts => ({
  project: { id, name: id, path: `/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 0,
});
const thread = (id: string): Thread => ({
  id, title: id, provider: 'claude', workspacePath: '/w', projectPath: '/w', mode: 'chat', model: 'm', createdAt: 0, updatedAt: 0, archived: false,
});

/**
 * The three catalogs' writers, as the stores make them, over rows a test
 * sets. Each row's id names the computer it belongs to, before a colon.
 */
function stores(options: { valueThreads?: boolean } = {}) {
  const ownerOf = (id: string) => id.split(':')[0];
  const rows = { projects: [] as ProjectWithCounts[], threads: [] as Thread[], groups: [] as ThreadGroup[] };
  return {
    rows,
    projects: computerCatalogWriter('projects', () => rows.projects, (next) => { rows.projects = next; }, (row) => ownerOf(row.project.id)),
    threads: computerCatalogWriter('threads', () => rows.threads, (next) => { rows.threads = next; }, (row) => ownerOf(row.id), { rowsAreValues: options.valueThreads }),
    groups: computerCatalogWriter('groups', () => rows.groups, (next) => { rows.groups = next; }, (row) => ownerOf(row.id)),
  };
}

function attach(backend: BackendKey): void {
  stageBackend({ id: backend });
  for (const kind of KINDS) observeCatalogStamp(backend, kind, `${kind}-stamp`);
}

/** Make `backend`'s three catalogs one tree, as a round that answered all three does. */
function answerTree(backend: BackendKey): void {
  for (const kind of KINDS) settleCatalogAnswers(kind, [backend]);
  markCatalogTree(backend);
}

const writes = () => replica.tree.mock.calls.map(([backend, catalogs]) => ({
  backend,
  projects: catalogs.projects.rows, threads: catalogs.threads.rows, groups: catalogs.groups.rows,
}));

it('writes a change as the whole tree of its computer, with only that computer\'s rows', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.projects = [project('gpu:p'), project('home:p')];
  rows.groups = [group('gpu:a'), group('home:b')];
  answerTree('gpu');
  groups.changed('gpu');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(writes()).toEqual([{ backend: 'gpu', projects: [project('gpu:p')], threads: [], groups: [group('gpu:a')] }]);
  const [, catalogs, token] = replica.tree.mock.calls[0]!;
  expect([catalogs.projects.stamp, catalogs.threads.stamp, catalogs.groups.stamp, token]).toEqual(['projects-stamp', 'threads-stamp', 'groups-stamp', 1]);
});

it('keeps a request made while the catalogs are not one tree, and writes it when they become one', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  groups.mutate('gpu', (current) => [...current, group('gpu:b')]);
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).not.toHaveBeenCalled();

  // Kept rows adopted for all three make them one tree.
  const kept = { kind: 'tree', tree: 'read-1' } as const;
  for (const kind of KINDS) settleCatalogRead(kind, 'gpu', { answered: false, error: new Error('offline'), kept, changed: kind === 'groups' });
  expect(catalogTreeOf('gpu')).toEqual(kept);
  await vi.advanceTimersByTimeAsync(0);
  expect(writes()).toEqual([{ backend: 'gpu', projects: [], threads: [], groups: [group('gpu:a'), group('gpu:b')] }]);
});

it('writes nothing for kept rows a read adopted when nothing asked for a write', async () => {
  vi.useFakeTimers();
  attach('gpu');
  stores();
  const kept = { kind: 'tree', tree: 'read-1' } as const;
  for (const kind of KINDS) settleCatalogRead(kind, 'gpu', { answered: false, error: new Error('offline'), kept, changed: false });
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).not.toHaveBeenCalled();
});

it('writes nothing while one catalog is partial', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { groups } = stores();
  const kept = { kind: 'tree', tree: 'read-1' } as const;
  settleCatalogRead('projects', 'gpu', { answered: false, error: new Error('offline'), kept, changed: false });
  settleCatalogRead('threads', 'gpu', { answered: false, error: new Error('offline'), kept, changed: false });
  settleCatalogRead('groups', 'gpu', { answered: false, error: new Error('offline'), kept: PARTIAL_PROVENANCE, changed: false });
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  expect(catalogProvenance('gpu', 'groups')).toEqual(PARTIAL_PROVENANCE);
  expect(replica.tree).not.toHaveBeenCalled();
});

it('writes after the write that runs, with the rows of then, and keeps a request that came during it', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups, threads } = stores();
  rows.groups = [group('gpu:a')];
  answerTree('gpu');
  let done!: (outcome: 'written') => void;
  replica.tree.mockImplementationOnce(() => new Promise((resolve) => { done = resolve; }));
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  threads.mutate('gpu', (current) => [...current, thread('gpu:t')]);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).toHaveBeenCalledTimes(1);
  done('written');
  await vi.advanceTimersByTimeAsync(0);
  expect(writes().map((write) => write.threads)).toEqual([[], [thread('gpu:t')]]);
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).toHaveBeenCalledTimes(2);
});

it('tries a write IndexedDB failed again after the spacing, with the rows of then', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  answerTree('gpu');
  replica.tree.mockResolvedValueOnce('failed');
  persistCatalogTree('gpu');
  await vi.advanceTimersByTimeAsync(0);
  groups.mutate('gpu', (current) => [...current, group('gpu:b')]);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS - 1);
  expect(replica.tree).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(1);
  expect(writes().map((write) => write.groups)).toEqual([[group('gpu:a')], [group('gpu:a'), group('gpu:b')]]);
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).toHaveBeenCalledTimes(2);
});

it('holds a write that met a stale stamp until this window holds another stamp, then writes it', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  answerTree('gpu');
  replica.tree.mockResolvedValueOnce('stale-stamp');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(5 * CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).toHaveBeenCalledTimes(1);
  // A change of this window's own replaces the stamp, which it then holds.
  invalidateCatalogStamp('gpu', 'groups');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(2);
  expect(replica.tree.mock.calls[1]![1].groups.stamp).not.toBe('groups-stamp');
});

it('tries a stale write again at once when this window came to hold a stamp while it ran', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { groups } = stores();
  answerTree('gpu');
  let done!: (outcome: 'stale-stamp') => void;
  replica.tree.mockImplementationOnce(() => new Promise((resolve) => { done = resolve; }));
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  invalidateCatalogStamp('gpu', 'groups');
  done('stale-stamp');
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).toHaveBeenCalledTimes(2);
});

it('drops the requests of a session that is gone', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { groups } = stores();
  answerTree('gpu');
  replica.tree.mockResolvedValueOnce('gone');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(5 * CATALOG_WRITE_SPACING_MS);
  invalidateCatalogStamp('gpu', 'groups');
  markCatalogTree('gpu');
  await vi.advanceTimersByTimeAsync(5 * CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).toHaveBeenCalledTimes(1);
});

it('does not write queued rows to a removed computer or a new generation', async () => {
  attach('gpu');
  const { groups } = stores();
  answerTree('gpu');
  groups.changed('gpu');
  ++replica.token;
  await Promise.resolve();
  expect(replica.tree).not.toHaveBeenCalled();
  groups.changed('gpu');
  detachBackend('gpu');
  await Promise.resolve();
  expect(replica.tree).not.toHaveBeenCalled();
});

it("starts a computer's writes a spacing apart and writes the newest rows when it ends", async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups, projects } = stores();
  rows.groups = [group('gpu:first')];
  answerTree('gpu');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(1);

  // A change to one catalog or another every 100 ms for five spacings: one write per spacing.
  for (let i = 0; i < (5 * CATALOG_WRITE_SPACING_MS) / 100; i++) {
    rows.groups = [group(`gpu:change-${i}`)];
    (i % 2 === 0 ? groups : projects).changed('gpu');
    await vi.advanceTimersByTimeAsync(100);
  }
  expect(replica.tree).toHaveBeenCalledTimes(6);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(writes().at(-1)!.groups).toEqual(rows.groups);

  // After a quiet spell the next change is written at once.
  const settled = replica.tree.mock.calls.length;
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  rows.groups = [group('gpu:after-quiet')];
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(settled + 1);
});

it('spaces each computer apart from the others', async () => {
  vi.useFakeTimers();
  attach('gpu');
  attach('lab');
  const { groups } = stores();
  answerTree('gpu');
  answerTree('lab');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  groups.changed('gpu');
  groups.changed('lab');
  await vi.advanceTimersByTimeAsync(0);
  expect(writes().map(({ backend }) => backend)).toEqual(['gpu', 'lab']);
});

it('does not write rows waiting out a spacing to a new generation', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { groups } = stores();
  answerTree('gpu');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  groups.changed('gpu');
  ++replica.token;
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).toHaveBeenCalledTimes(1);
});

it('writes what is waiting when the page goes hidden', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:first')];
  answerTree('gpu');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  rows.groups = [group('gpu:closing')];
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(1);

  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
  document.dispatchEvent(new Event('visibilitychange'));
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(2);
  expect(writes()[1]!.groups).toEqual([group('gpu:closing')]);

  // Becoming visible again does not cut a spacing short.
  groups.changed('gpu');
  visibility.mockReturnValue('visible');
  document.dispatchEvent(new Event('visibilitychange'));
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(replica.tree).toHaveBeenCalledTimes(3);
});

it('hands every write of a values writer the same row cache', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, threads } = stores({ valueThreads: true });
  rows.threads = [thread('gpu:t')];
  answerTree('gpu');
  threads.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  persistCatalogTree('gpu');
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  const caches = replica.tree.mock.calls.map((call) => call[1].threads.cache);
  expect(caches).toHaveLength(2);
  expect(caches[0]).toBeInstanceOf(WeakMap);
  expect(caches[1]).toBe(caches[0]);
  expect(replica.tree.mock.calls[0]![1].projects.cache).toBeUndefined();
});

it('cancels queued writes and spacings on reset and accepts new changes', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { groups } = stores();
  answerTree('gpu');
  groups.changed('gpu');
  resetCatalogWritesForTest();
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).not.toHaveBeenCalled();
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  resetCatalogWritesForTest();
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(2);
});

it('commits a mutation, writes its computer, and skips a commit that changes nothing', async () => {
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  answerTree('gpu');
  const before = rows.groups;
  groups.mutate('gpu', (current) => current.map((row) => row.id === 'missing' ? group('gpu:x') : row));
  expect(rows.groups).toBe(before);
  groups.mutate('gpu', (current) => [...current, group('gpu:b')]);
  expect(rows.groups.map((row) => row.id)).toEqual(['gpu:a', 'gpu:b']);
  expect(catalogTreeOf('gpu')).not.toBeNull();
  await vi.waitFor(() => expect(writes().at(-1)?.groups).toEqual(rows.groups));
});

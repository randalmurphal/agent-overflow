import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  CATALOG_WRITE_SPACING_MS, computerCatalogWriter, persistCatalogTree, resetCatalogWritesForTest,
} from './computerCatalogWriter';
import * as replicaSession from '../replica/session';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { detachBackend } from '../transport/backends';
import { observeCatalogStamp, resetCatalogStampsForTest } from '../replica/catalogStamp';
import {
  catalogProvenance, catalogTreeOf, markCatalogTree, resetCatalogLoadForTest, settleCatalogAnswers, settleCatalogRead,
} from './catalogLoad.svelte';
import type { CatalogKind } from '../replica/catalog';
import type { ProjectWithCounts, Thread, ThreadGroup } from '../types/models';
import type { BackendKey } from '../transport/backendKey';

const replica = {
  token: 1,
  single: vi.fn<typeof replicaSession.putReplicaCatalog>(),
  tree: vi.fn<typeof replicaSession.putReplicaTree>(),
};
beforeEach(() => {
  replica.single.mockResolvedValue('written');
  replica.tree.mockResolvedValue('written');
  vi.spyOn(replicaSession, 'putReplicaCatalog').mockImplementation(replica.single);
  vi.spyOn(replicaSession, 'putReplicaTree').mockImplementation(replica.tree);
  vi.spyOn(replicaSession, 'replicaToken').mockImplementation(() => replica.token);
});
afterEach(() => {
  resetCatalogWritesForTest();
  vi.useRealTimers();
  resetCatalogStampsForTest();
  resetStagedBackends();
  resetCatalogLoadForTest();
  vi.restoreAllMocks();
  replica.single.mockReset();
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
function answerTree(backend: BackendKey): string {
  for (const kind of KINDS) settleCatalogAnswers(kind, [backend]);
  markCatalogTree(backend);
  return catalogTreeOf(backend)!.tree;
}

const singles = () => replica.single.mock.calls.map(([backend, kind, rows, tree]) => ({ backend, kind, rows, tree: tree.tree }));
const trees = () => replica.tree.mock.calls.map(([backend, tree, catalogs]) => ({ backend, tree: tree.tree, catalogs }));

it('writes a change to a tree catalog as that one record, with only its computer\'s rows', async () => {
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a'), group('home:b')];
  const tree = answerTree('gpu');
  groups.changed('gpu');
  groups.changed('gpu');
  await vi.waitFor(() => expect(replica.single).toHaveBeenCalledTimes(1));
  expect(replica.single).toHaveBeenLastCalledWith('gpu', 'groups', [group('gpu:a')], { kind: 'tree', tree }, 'groups-stamp', 1, undefined);
  expect(replica.tree).not.toHaveBeenCalled();
});

it('writes nothing for a catalog of no known source, a partial one or an unrecorded one', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  // No known source: the change makes it partial.
  groups.mutate('gpu', (current) => [...current, group('gpu:b')]);
  expect(catalogProvenance('gpu', 'groups')).toEqual({ kind: 'partial' });
  // An answer alone is partial.
  settleCatalogAnswers('groups', ['gpu']);
  groups.changed('gpu');
  // Unrecorded kept rows, and a change applied to them.
  settleCatalogRead('groups', 'gpu', { answered: false, error: new Error('offline'), kept: { kind: 'unrecorded' }, changed: false });
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  expect(replica.single).not.toHaveBeenCalled();
  expect(replica.tree).not.toHaveBeenCalled();
});

it('writes a tree as the three catalogs of its computer in one write, without waiting out a spacing', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores({ valueThreads: true });
  rows.projects = [project('gpu:p'), project('home:p')];
  rows.threads = [thread('gpu:t')];
  rows.groups = [group('gpu:g')];
  const first = answerTree('gpu');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).toHaveBeenCalledTimes(1);

  const tree = answerTree('gpu');
  persistCatalogTree('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(tree).not.toBe(first);
  expect(trees()).toEqual([{
    backend: 'gpu', tree,
    catalogs: {
      projects: { rows: [project('gpu:p')], stamp: 'projects-stamp', cache: undefined },
      threads: { rows: [thread('gpu:t')], stamp: 'threads-stamp', cache: expect.any(WeakMap) },
      groups: { rows: [group('gpu:g')], stamp: 'groups-stamp', cache: undefined },
    },
  }]);
  expect(replica.tree.mock.calls[0][3]).toBe(1);
});

it('writes a tree after the write that runs, in place of the changes that wait, and never an older tree\'s record after it', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups, threads } = stores();
  rows.groups = [group('gpu:a')];
  const older = answerTree('gpu');
  let done!: (outcome: 'written') => void;
  replica.single.mockImplementationOnce(() => new Promise((resolve) => { done = resolve; }));
  groups.mutate('gpu', (current) => [...current, group('gpu:b')]);
  await vi.advanceTimersByTimeAsync(0);
  expect(singles()).toEqual([{ backend: 'gpu', kind: 'groups', rows: [group('gpu:a'), group('gpu:b')], tree: older }]);

  // A round completes a new tree while that write runs, and a change waits.
  const newer = answerTree('gpu');
  persistCatalogTree('gpu');
  threads.mutate('gpu', (current) => [...current, thread('gpu:t')]);
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).not.toHaveBeenCalled();

  done('written');
  await vi.advanceTimersByTimeAsync(0);
  expect(trees().map(({ tree }) => tree)).toEqual([newer]);
  expect(trees()[0].catalogs.threads.rows).toEqual([thread('gpu:t')]);
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  // The waiting change went with the tree; nothing of the older tree follows.
  expect(singles()).toHaveLength(1);
});

it('tries a tree IndexedDB failed again after the spacing, with the rows of then, and writes no change before it', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  const tree = answerTree('gpu');
  replica.tree.mockResolvedValueOnce('failed');
  persistCatalogTree('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(1);

  groups.mutate('gpu', (current) => [...current, group('gpu:b')]);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS - 1);
  expect(replica.tree).toHaveBeenCalledTimes(1);
  expect(replica.single).not.toHaveBeenCalled();

  await vi.advanceTimersByTimeAsync(1);
  expect(trees().map(({ tree: id }) => id)).toEqual([tree, tree]);
  expect(trees()[1].catalogs.groups.rows).toEqual([group('gpu:a'), group('gpu:b')]);
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  expect(replica.single).not.toHaveBeenCalled();
});

it('tries a change IndexedDB failed again after the spacing, and drops one that was refused', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  answerTree('gpu');
  replica.single.mockResolvedValueOnce('failed');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(replica.single).toHaveBeenCalledTimes(2);

  replica.single.mockResolvedValueOnce('refused');
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).toHaveBeenCalledTimes(3);
  await vi.advanceTimersByTimeAsync(3 * CATALOG_WRITE_SPACING_MS);
  expect(replica.single).toHaveBeenCalledTimes(3);
});

it('does not write queued rows to a removed computer or a new generation', async () => {
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  answerTree('gpu');
  groups.changed('gpu');
  persistCatalogTree('gpu');
  ++replica.token;
  await Promise.resolve();
  expect(replica.single).not.toHaveBeenCalled();
  expect(replica.tree).not.toHaveBeenCalled();
  groups.changed('gpu');
  persistCatalogTree('gpu');
  detachBackend('gpu');
  await Promise.resolve();
  expect(replica.single).not.toHaveBeenCalled();
  expect(replica.tree).not.toHaveBeenCalled();
});

it('keeps at most one write running per computer and saves the newest rows after it finishes', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups, projects } = stores();
  rows.groups = [group('gpu:first')];
  answerTree('gpu');
  let done!: (outcome: 'written') => void;
  replica.single.mockImplementationOnce(() => new Promise((resolve) => { done = resolve; }));
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  rows.groups = [group('gpu:last')];
  groups.changed('gpu');
  projects.changed('gpu');
  // The spacing ends while the first write still runs: the next waits for it.
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(replica.single).toHaveBeenCalledTimes(1);
  done('written');
  await vi.advanceTimersByTimeAsync(0);
  expect(singles().map(({ kind }) => kind)).toEqual(['groups', 'groups', 'projects']);
  expect(singles()[1].rows).toEqual([group('gpu:last')]);
});

it('starts a catalog\'s writes a spacing apart and writes the newest rows when it ends', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:first')];
  answerTree('gpu');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).toHaveBeenCalledTimes(1);

  // A change every 100 ms for five spacings: one write per spacing.
  for (let i = 0; i < (5 * CATALOG_WRITE_SPACING_MS) / 100; i++) {
    rows.groups = [group(`gpu:change-${i}`)];
    groups.changed('gpu');
    await vi.advanceTimersByTimeAsync(100);
  }
  expect(replica.single).toHaveBeenCalledTimes(6);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(singles().at(-1)!.rows).toEqual(rows.groups);

  // After a quiet spell the next change is written at once.
  const settled = replica.single.mock.calls.length;
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  rows.groups = [group('gpu:after-quiet')];
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).toHaveBeenCalledTimes(settled + 1);
  expect(singles().at(-1)!.rows).toEqual(rows.groups);
});

it('spaces each computer apart from the others', async () => {
  vi.useFakeTimers();
  attach('gpu');
  attach('lab');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a'), group('lab:a')];
  answerTree('gpu');
  answerTree('lab');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  groups.changed('gpu');
  groups.changed('lab');
  await vi.advanceTimersByTimeAsync(0);
  expect(singles().map(({ backend }) => backend)).toEqual(['gpu', 'lab']);
});

it('does not write rows waiting out a spacing to a new generation', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  answerTree('gpu');
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  groups.changed('gpu');
  ++replica.token;
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(replica.single).toHaveBeenCalledTimes(1);
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
  expect(replica.single).toHaveBeenCalledTimes(1);

  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
  document.dispatchEvent(new Event('visibilitychange'));
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).toHaveBeenCalledTimes(2);
  expect(singles()[1].rows).toEqual([group('gpu:closing')]);

  // Becoming visible again does not cut a spacing short.
  groups.changed('gpu');
  visibility.mockReturnValue('visible');
  document.dispatchEvent(new Event('visibilitychange'));
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  expect(replica.single).toHaveBeenCalledTimes(3);
});

it('tries a failed tree again at once when the page goes hidden', async () => {
  vi.useFakeTimers();
  attach('gpu');
  stores();
  answerTree('gpu');
  replica.tree.mockResolvedValueOnce('failed');
  persistCatalogTree('gpu');
  await vi.advanceTimersByTimeAsync(0);
  vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
  document.dispatchEvent(new Event('visibilitychange'));
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.tree).toHaveBeenCalledTimes(2);
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
  await vi.advanceTimersByTimeAsync(0);
  threads.changed('gpu');
  await vi.advanceTimersByTimeAsync(CATALOG_WRITE_SPACING_MS);
  const caches = [
    replica.single.mock.calls[0][6], replica.tree.mock.calls[0][2].threads.cache, replica.single.mock.calls[1][6],
  ];
  expect(caches[0]).toBeInstanceOf(WeakMap);
  expect(new Set(caches).size).toBe(1);
});

it('cancels queued writes and spacings on reset and accepts new changes', async () => {
  vi.useFakeTimers();
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  answerTree('gpu');
  groups.changed('gpu');
  resetCatalogWritesForTest();
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).not.toHaveBeenCalled();
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  resetCatalogWritesForTest();
  groups.changed('gpu');
  await vi.advanceTimersByTimeAsync(0);
  expect(replica.single).toHaveBeenCalledTimes(2);
});

it('commits a mutation, writes its computer, and skips a commit that changes nothing', async () => {
  attach('gpu');
  const { rows, groups } = stores();
  rows.groups = [group('gpu:a')];
  const tree = answerTree('gpu');
  const before = rows.groups;
  groups.mutate('gpu', (current) => current.map((row) => row.id === 'missing' ? group('gpu:x') : row));
  expect(rows.groups).toBe(before);
  groups.mutate('gpu', (current) => [...current, group('gpu:b')]);
  expect(rows.groups.map((row) => row.id)).toEqual(['gpu:a', 'gpu:b']);
  expect(catalogProvenance('gpu', 'groups')).toEqual({ kind: 'tree', tree });
  await vi.waitFor(() => expect(singles().at(-1)).toEqual({ backend: 'gpu', kind: 'groups', rows: rows.groups, tree }));
});

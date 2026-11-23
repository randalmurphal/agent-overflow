// A deletion frame from a computer removes the row from that computer's
// kept catalogs, through the subscription the app installs.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setupEventListeners } from './events';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { stageBackend } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { noteProject, noteThread, noteThreadGroup, projectBackend, threadGroupBackend } from '../transport/entityIndex';
import * as replicaSession from '../replica/session';
import { currentCatalogStamp } from '../replica/catalogStamp';
import type { CatalogKind } from '../replica/catalog';
import { markCatalogTree } from './catalogLoad.svelte';
import { CATALOG_WRITE_SPACING_MS } from './computerCatalogWriter';
import { addProjectLocal, getProjects } from './projects.svelte';
import { getThreadGroups, upsertThreadGroup } from './threadGroups.svelte';
import { getThreads, prependThread } from './threads.svelte';
import type { Project, ProjectWithCounts, ThreadGroup } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../test/helpers/sidebarCatalogs';
import { takePinnedBackend } from '../transport/backends';
import { renameThreadGroupAction } from '../components/sidebar/threadGroupActions';

const LAPTOP = 'laptop';
const project = (id: string): Project => ({ id, name: id, path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false });
const group = (id: string): ThreadGroup => ({ id, projectId: 'p', name: id, createdAt: 0, updatedAt: 0 });

let cleanup: () => void = () => {};
let writes: { backend: BackendKey; projects: string[]; groups: string[]; threads: string[] }[] = [];

beforeEach(() => {
  stageBackend({ id: LAPTOP, backendId: 'laptop-id', name: 'Laptop' });
  writes = [];
  vi.spyOn(replicaSession, 'putReplicaTree').mockImplementation(async (backend, catalogs) => {
    writes.push({
      backend,
      projects: (catalogs.projects.rows as ProjectWithCounts[]).map((row) => row.project.id),
      groups: (catalogs.groups.rows as ThreadGroup[]).map((row) => row.id),
      threads: catalogs.threads.rows.map((row) => row.id),
    });
    return 'written';
  });
  cleanup = setupEventListeners();
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
});

afterEach(() => {
  vi.useRealTimers();
  cleanup();
  vi.restoreAllMocks();
});

/** The laptop holds project P with group G and thread T; home holds project H. */
function holdTrees(): void {
  noteProject('p', LAPTOP);
  addProjectLocal(project('p'));
  noteThreadGroup('g', LAPTOP);
  upsertThreadGroup(group('g'));
  noteThread('t', LAPTOP, 0);
  prependThread(makeThread({ id: 't', projectId: 'p', groupId: 'g', placement: 'grouped' }));
  noteProject('h', HOME_BACKEND);
  addProjectLocal(project('h'));
  markCatalogTree(LAPTOP);
  markCatalogTree(HOME_BACKEND);
}

function stamps(kind: CatalogKind): Record<string, string> {
  return { home: currentCatalogStamp(HOME_BACKEND, kind), laptop: currentCatalogStamp(LAPTOP, kind) };
}

// A computer's writes start CATALOG_WRITE_SPACING_MS apart: run the fake
// clock until the laptop writes, within two spacings.
async function laptopWrite(): Promise<(typeof writes)[number]> {
  const wrote = () => writes.some((write) => write.backend === LAPTOP);
  await vi.advanceTimersByTimeAsync(0);
  for (let ran = 0; !wrote() && ran < CATALOG_WRITE_SPACING_MS * 2; ran += 50) await vi.advanceTimersByTimeAsync(50);
  expect(wrote()).toBe(true);
  return writes.filter((write) => write.backend === LAPTOP).at(-1)!;
}

describe('a deletion frame from another computer', () => {
  it("takes a deleted project out of that computer's kept tree and replaces that computer's stamp", async () => {
    holdTrees();
    await laptopWrite();
    writes = [];
    const before = stamps('projects');

    emitWailsEvent('project:updated', { action: 'deleted', id: 'p' }, 'laptop-id');

    expect(getProjects().map((row) => row.project.id)).toEqual(['h']);
    const after = stamps('projects');
    expect(after.laptop).not.toBe(before.laptop);
    expect(after.home).toBe(before.home);
    expect((await laptopWrite()).projects).toEqual([]);
    expect(writes.filter((write) => write.backend === HOME_BACKEND)).toEqual([]);
    expect(projectBackend('p')).toBeUndefined();
  });

  it("takes a deleted group and its members' membership out of that computer's kept tree", async () => {
    holdTrees();
    await laptopWrite();
    writes = [];
    const before = stamps('groups');

    emitWailsEvent('thread-group:updated', { action: 'delete', group: group('g') }, 'laptop-id');

    expect(getThreadGroups()).toEqual([]);
    expect(getThreads().find((row) => row.id === 't')?.groupId).toBeUndefined();
    const after = stamps('groups');
    expect(after.laptop).not.toBe(before.laptop);
    expect(after.home).toBe(before.home);
    expect((await laptopWrite()).groups).toEqual([]);
    expect(writes.filter((write) => write.backend === HOME_BACKEND)).toEqual([]);
    expect(threadGroupBackend('g')).toBeUndefined();
  });

  it("takes a deleted thread out of that computer's kept tree", async () => {
    holdTrees();
    await laptopWrite();
    writes = [];
    const before = stamps('threads');

    emitWailsEvent('thread:updated', { action: 'deleted', id: 't' }, 'laptop-id');

    expect(getThreads()).toEqual([]);
    const after = stamps('threads');
    expect(after.laptop).not.toBe(before.laptop);
    expect(after.home).toBe(before.home);
    expect((await laptopWrite()).threads).toEqual([]);
    expect(writes.filter((write) => write.backend === HOME_BACKEND)).toEqual([]);
  });
});

describe("a deletion frame while that computer's read is out", () => {
  /** The laptop answers `answer` only once `release` resolves; home answers nothing. */
  function laptopAnswers<T>(name: 'ListProjects' | 'ListThreadGroups', answer: T[]): () => void {
    let release!: () => void;
    const held = new Promise<T[]>((resolve) => { release = () => resolve(answer); });
    setBindingMock(name, () => takePinnedBackend() === LAPTOP ? held : Promise.resolve([]));
    return release;
  }

  it('keeps a project deleted before the answer that still holds it out of the list', async () => {
    const release = laptopAnswers('ListProjects', [{ project: project('p'), threadCount: 0, lastActive: 0 }]);
    const read = readSidebarCatalogs();
    emitWailsEvent('project:updated', { action: 'deleted', id: 'p' }, 'laptop-id');
    release();
    await read;
    expect(getProjects()).toEqual([]);
  });

  it('keeps a group deleted before the answer that still holds it out of the list', async () => {
    const release = laptopAnswers('ListThreadGroups', [group('g')]);
    const read = readSidebarCatalogs();
    emitWailsEvent('thread-group:updated', { action: 'delete', group: group('g') }, 'laptop-id');
    release();
    await read;
    expect(getThreadGroups()).toEqual([]);
  });
});

describe("a call's answer for a row deleted while the call was out", () => {
  it('does not bring back a group renamed on one screen and deleted on another before the rename answered', async () => {
    holdTrees();
    await laptopWrite();
    writes = [];
    let answer!: (group: ThreadGroup) => void;
    setBindingMock('RenameThreadGroup', () => new Promise<ThreadGroup>((resolve) => { answer = resolve; }));
    const rename = renameThreadGroupAction(group('g'), 'Renamed');
    emitWailsEvent('thread-group:updated', { action: 'delete', group: group('g') }, 'laptop-id');
    answer({ ...group('g'), name: 'Renamed' });
    await rename;

    expect(getThreadGroups()).toEqual([]);
    expect((await laptopWrite()).groups).toEqual([]);
    expect(writes.filter((write) => write.backend === HOME_BACKEND && write.groups.length > 0)).toEqual([]);
  });
});

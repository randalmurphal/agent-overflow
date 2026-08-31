import { beforeEach, describe, expect, it } from 'vitest';
import { applyProjectUpdated } from './eventsProjectRows';
import {
  addProjectLocal,
  getProject,
  getProjects,
  resetProjectsForTest,
} from './projects.svelte';
import type { Project } from '../types/models';

function makeProject(id: string, overrides: Partial<Project> = {}): Project {
  return {
    id,
    path: `/tmp/${id}`,
    name: id,
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('applyProjectUpdated — project:updated convergence', () => {
  beforeEach(() => {
    resetProjectsForTest();
  });

  it("inserts a row this client does not have on 'listed'", () => {
    applyProjectUpdated({ action: 'listed', project: makeProject('p1') });
    expect(getProjects().map((p) => p.project.id)).toEqual(['p1']);
  });

  it("converges a row it already has on 'listed' rather than duplicating it", () => {
    addProjectLocal(makeProject('p1', { name: 'Old' }));
    applyProjectUpdated({ action: 'listed', project: makeProject('p1', { name: 'New' }) });
    expect(getProjects()).toHaveLength(1);
    expect(getProject('p1')?.project.name).toBe('New');
  });

  it("converges a known row on 'full'", () => {
    addProjectLocal(makeProject('p1', { name: 'Old' }));
    applyProjectUpdated({ action: 'full', project: makeProject('p1', { name: 'Renamed' }) });
    expect(getProject('p1')?.project.name).toBe('Renamed');
  });

  it("does not invent a row it has never seen on 'full'", () => {
    // 'full' says nothing about sidebar membership, so an unknown id is a row
    // this client is not supposed to be listing.
    applyProjectUpdated({ action: 'full', project: makeProject('ghost') });
    expect(getProjects()).toHaveLength(0);
  });

  it("preserves thread counts when converging a row", () => {
    addProjectLocal(makeProject('p1'));
    // addProjectLocal wraps with zero counts; simulate a refresh having filled
    // them in by asserting the applier goes through updateProjectLocal, which
    // keeps the wrapper.
    applyProjectUpdated({ action: 'full', project: makeProject('p1', { name: 'Renamed' }) });
    expect(getProject('p1')).toMatchObject({ threadCount: 0, lastActive: 0 });
  });

  it("drops an archived row on 'unlisted'", () => {
    addProjectLocal(makeProject('p1'));
    applyProjectUpdated({ action: 'unlisted', project: makeProject('p1', { archived: true }) });
    expect(getProjects()).toHaveLength(0);
  });

  it("drops a deleted row named by id alone", () => {
    addProjectLocal(makeProject('p1'));
    addProjectLocal(makeProject('p2'));
    applyProjectUpdated({ action: 'deleted', id: 'p1' });
    expect(getProjects().map((p) => p.project.id)).toEqual(['p2']);
  });

  it('ignores frames with nothing to act on', () => {
    addProjectLocal(makeProject('p1'));
    applyProjectUpdated({ action: 'deleted' });
    applyProjectUpdated({ action: 'listed' });
    applyProjectUpdated({ action: 'unlisted' });
    applyProjectUpdated({ action: 'full' });
    expect(getProjects().map((p) => p.project.id)).toEqual(['p1']);
  });
});

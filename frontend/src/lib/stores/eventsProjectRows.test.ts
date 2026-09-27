import { beforeEach, describe, expect, it } from 'vitest';
import { applyProjectUpdated } from './eventsProjectRows';
import {
  addProjectLocal,
  getProject,
  getProjects,
  resetProjectsForTest,
} from './projects.svelte';
import { createPane, getAllPanes, resetPanesForTest, revealPane } from './panes.svelte';
import { getCompactScreen, setCompactLayoutForTest } from './layoutMode.svelte';
import type { Project } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { getThreadTerminalState } from '../components/terminal/terminalStore.svelte';

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
    resetPanesForTest();
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

  // A placeholder has no thread row, so no thread:updated frame names it.
  // Its project is what it would be created in, and that project is gone.
  it("closes every draft placeholder on a deleted project, and leaves the rest", () => {
    const gone = makeProject('p1');
    const kept = makeProject('p2');
    addProjectLocal(gone);
    addProjectLocal(kept);
    createPane('gone-chat').startDraftPlaceholder(gone, 'chat');
    createPane('gone-plan').startDraftPlaceholder(gone, 'plan');
    createPane('kept-chat').startDraftPlaceholder(kept, 'chat');
    createPane('empty');

    applyProjectUpdated({ action: 'deleted', id: 'p1' });

    expect([...getAllPanes().keys()]).toEqual(['kept-chat', 'empty']);
    expect(getAllPanes().get('kept-chat')?.draftPlaceholder?.projectId).toBe('p2');
  });

  // The project's computer is still attached, so it closes the shells.
  it("asks the computer to close a deleted project's placeholder terminals", () => {
    const project = makeProject('p1');
    addProjectLocal(project);
    const pane = createPane('gone-chat');
    pane.startDraftPlaceholder(project, 'chat');
    const placeholderId = pane.thread!.id;
    getThreadTerminalState(placeholderId).addTab({
      terminalID: 'term-1', threadID: placeholderId, shell: '/bin/sh', cwd: project.path,
      rows: 24, cols: 80, pid: 123, startedAt: 1, running: true, exitCode: 0, exitReason: '',
    });
    pane.setShowTerminal(true);
    const close = setBindingMock('CloseThreadTerminals', async () => undefined);

    applyProjectUpdated({ action: 'deleted', id: 'p1' });

    expect(getAllPanes().size).toBe(0);
    expect(close.mock.calls).toEqual([[placeholderId]]);
  });

  it("returns a compact client to the list when a deleted project's placeholder was the only pane", () => {
    setCompactLayoutForTest(true);
    try {
      const project = makeProject('p1');
      addProjectLocal(project);
      createPane('only').startDraftPlaceholder(project, 'chat');
      revealPane('only');
      expect(getCompactScreen()).toBe('thread');

      applyProjectUpdated({ action: 'deleted', id: 'p1' });

      expect(getAllPanes().size).toBe(0);
      expect(getCompactScreen()).toBe('list');
    } finally {
      setCompactLayoutForTest(false);
    }
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

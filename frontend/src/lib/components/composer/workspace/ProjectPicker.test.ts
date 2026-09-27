import { afterEach, beforeEach, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';

import ProjectPicker from './ProjectPicker.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import { addProjectLocal, resetProjectsForTest } from '../../../stores/projects.svelte';
import { getToasts } from '../../../stores/toast.svelte';
import { resetPanesForTest } from '../../../stores/panes.svelte';
import { __resetEntityIndexForTest, noteProject } from '../../../transport/entityIndex';
import { HOME_BACKEND } from '../../../transport/backendKey';
import type { Project } from '../../../types/models';
import { resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../../../test/helpers/backends';

function makeProject(id: string, name: string): Project {
  return { id, name, path: `/repos/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false };
}

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetProjectsForTest();
});

afterEach(() => {
  resetStagedBackends();
  __resetEntityIndexForTest();
  resetProjectsForTest();
});

// With several computers attached, a project whose owner is unknown has no
// computer to stage the draft on; the picker says so and moves nothing.
it('refuses a project no computer is known to own and keeps the draft where it is', async () => {
  stageBackend();
  const owned = makeProject('project-1', 'Owned');
  const unowned = makeProject('project-2', 'Unowned');
  addProjectLocal(owned);
  addProjectLocal(unowned);
  noteProject(owned.id, HOME_BACKEND);
  const defaults = setBindingMock('GetThreadDefaults', async () => ({}));
  const pane = createThreadPane();
  pane.startDraftPlaceholder(owned, 'chat');
  const anchor = document.createElement('button');
  document.body.append(anchor);

  const { component } = render(ProjectPicker, { props: { pane, anchor } });
  component.openPicker();
  await fireEvent.click(await screen.findByRole('menuitem', { name: /Unowned/ }));

  await waitFor(() => expect(getToasts().at(-1)?.message).toContain('computer that owns'));
  expect(pane.thread?.projectId).toBe(owned.id);
  expect(defaults).not.toHaveBeenCalled();
  pane.clear();
  anchor.remove();
});

// The Start work form's branch box: the name the project's pattern gives
// the ticket, or empty for a project that does not name the branch after
// the ticket (Settings > Issues > Starting work), which Start work then
// waits on until the person names it.

import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import type { DetailView, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { Project } from '../../types/models';
import StartWorkForm from './StartWorkForm.svelte';
import { addProjectLocal } from '../../stores/projects.svelte';
import { noteProject } from '../../transport/entityIndex';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const SITE = 'https://example.atlassian.net';

const DETAIL = {
  card: { id: '10101', key: 'WEB-1', title: 'Retry cursor', status: { id: 's1', name: 'To Do' }, created: 0, updated: 0 },
  url: `${SITE}/browse/WEB-1`, description: '', fields: [], comments: [], links: [], attachments: [], history: [], rows: [], sprints: [],
  moves: [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress' } }],
} as unknown as DetailView;

async function openForm(nameBranch: boolean | undefined) {
  setBindingMock('ProjectTrackerSetup', async () => ({
    revision: 'r1',
    setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', branch: 'feature/{key}', name_branch: nameBranch }] },
  } as SetupView));
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', path: '/tmp/widgets', name: 'widgets', archived: false } as never);
  const anchor = document.createElement('button');
  document.body.append(anchor);
  const onStart = vi.fn();
  const view = render(StartWorkForm, {
    props: {
      anchor, open: true, detail: DETAIL, projects: [{ id: 'p1', name: 'widgets' } as Project], project: 'p1',
      place: { tracker: 'jira', site: SITE, project: 'WEB' }, me: 'acc-1', branches: [], started: null, branch: null,
      onStart, onOpenThread: () => {}, onClose: () => {},
    },
  });
  const go = () => document.querySelector<HTMLButtonElement>('[data-testid="start-work-go"]')!;
  // The saved defaults have answered once Save as defaults is offered.
  await waitFor(() => expect(document.querySelector<HTMLInputElement>('[data-testid="start-work-save"]')?.disabled).toBe(false));
  expect(go().disabled).toBe(nameBranch === false);
  const box = document.querySelector<HTMLInputElement>('[data-testid="start-work-branch"]')!;
  return { view, box, go, onStart };
}

describe('<StartWorkForm> branch', () => {
  it('offers the name the project\'s pattern gives the ticket', async () => {
    const { box, view } = await openForm(undefined);
    expect(box.value).toBe('feature/WEB-1');
    view.unmount();
  });

  it('opens the box empty for a project that does not name the branch, and starts once it is named', async () => {
    const { box, go, onStart, view } = await openForm(false);
    expect(box.value).toBe('');
    await fireEvent.input(box, { target: { value: 'spike/cursor' } });
    await waitFor(() => expect(go().disabled).toBe(false));
    await fireEvent.click(go());
    expect(onStart).toHaveBeenCalledWith(expect.objectContaining({ branch: 'spike/cursor' }));
    view.unmount();
  });
});

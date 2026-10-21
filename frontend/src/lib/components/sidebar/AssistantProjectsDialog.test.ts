import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import AssistantProjectsDialog from './AssistantProjectsDialog.svelte';
import { closeAssistantProjectsDialog, getAssistantProjectsAgentId, openAssistantProjectsDialog } from '../../stores/newAssistant.svelte';
import { refreshProjects, resetProjectsForTest } from '../../stores/projects.svelte';
import { refreshThreads, replaceAllThreads } from '../../stores/threads.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { __resetEntityIndexForTest, noteThread, threadBackend } from '../../transport/entityIndex';
import { TransportError } from '../../transport/wsClient';
import type { Project, Thread } from '../../types/models';

function project(id: string, name: string, archived = false): Project {
  return { id, path: `/repo/${id}`, name, sortPosition: 0, createdAt: 0, updatedAt: 0, archived };
}

// An assistant's Projects dialog (docs/specs/standing-agents.md,
// "Projects"): it opens on what the assistant serves, lists the computer's
// unarchived projects, and saves the choice through SetAssistantProjects to
// the assistant's current thread.
describe('<AssistantProjectsDialog>', () => {
  const agent = makeThread({ id: 'agent', title: 'Ops', projectId: '', agentId: 'ops', agentProjectIds: ['p2', 'p3'] });

  beforeEach(async () => {
    resetBindingMocks();
    resetProjectsForTest();
    __resetEntityIndexForTest();
    setBindingMock('ListProjects', async () => [
      { project: project('p1', 'Alpha'), threadCount: 0 },
      { project: project('p2', 'Beta'), threadCount: 0 },
      // Archived, but served: not listed, and kept in the choice.
      { project: project('p3', 'Gamma', true), threadCount: 0 },
      { project: project('p4', 'Delta', true), threadCount: 0 },
    ]);
    await refreshProjects();
    setBindingMock('ListThreads', async () => [agent]);
    await refreshThreads();
  });
  afterEach(() => closeAssistantProjectsDialog());

  const items = (view: { getAllByTestId: (id: string) => HTMLElement[] }) => view.getAllByTestId('assistant-projects-item') as HTMLInputElement[];
  const item = (view: { getAllByTestId: (id: string) => HTMLElement[] }, id: string) => items(view).find((el) => el.dataset.projectId === id)!;

  async function open(thread: Thread = agent) {
    if (thread !== agent) replaceAllThreads([thread]);
    const view = render(AssistantProjectsDialog);
    openAssistantProjectsDialog(thread);
    await tick();
    return view;
  }

  it('opens on the served projects and saves the new choice', async () => {
    const save = setBindingMock('SetAssistantProjects', vi.fn(async () => agent));
    const view = await open();
    expect(view.getByRole('dialog')).toHaveAccessibleName("Ops's projects");
    expect(items(view).map((el) => el.dataset.projectId)).toEqual(['p1', 'p2']);
    expect(items(view).map((el) => el.checked)).toEqual([false, true]);
    await fireEvent.click(item(view, 'p2'));
    await fireEvent.click(item(view, 'p1'));
    await fireEvent.click(view.getByTestId('assistant-projects-save'));
    await waitFor(() => expect(save).toHaveBeenCalledWith('agent', false, ['p3', 'p1']));
    await waitFor(() => expect(view.queryByTestId('assistant-projects-dialog')).toBeNull());
  });

  it('follows the assistant across a handoff while open: the successor\'s title, the choice kept, saved to the successor', async () => {
    const save = setBindingMock('SetAssistantProjects', vi.fn(async () => agent));
    const view = await open();
    await fireEvent.click(item(view, 'p1'));
    // The successor lists first, then the old thread becomes a past one.
    const successor = makeThread({ id: 'next', title: 'Ops renamed', projectId: '', agentId: 'ops', agentProjectIds: ['p2', 'p3'] });
    noteThread('next', threadBackend('agent')!);
    replaceAllThreads([successor, agent]);
    replaceAllThreads([successor, { ...agent, agentId: undefined, pastAgentId: 'ops' }]);
    await tick();
    expect(view.getByRole('dialog')).toHaveAccessibleName("Ops renamed's projects");
    expect(items(view).map((el) => el.checked)).toEqual([true, true]);
    await fireEvent.click(view.getByTestId('assistant-projects-save'));
    await waitFor(() => expect(save).toHaveBeenCalledWith('next', false, ['p2', 'p3', 'p1']));
  });

  it('closes when the assistant leaves the list', async () => {
    const view = await open();
    expect(view.getByTestId('assistant-projects-dialog')).toBeInTheDocument();
    replaceAllThreads([]);
    await waitFor(() => expect(view.queryByTestId('assistant-projects-dialog')).toBeNull());
    expect(getAssistantProjectsAgentId()).toBeNull();
  });

  it('saves All projects with no list, and refuses to save nothing', async () => {
    const save = setBindingMock('SetAssistantProjects', vi.fn(async () => agent));
    const view = await open({ ...agent, agentProjectIds: ['p2'] });
    const button = view.getByTestId('assistant-projects-save') as HTMLButtonElement;
    await fireEvent.click(item(view, 'p2'));
    expect(button.disabled).toBe(true);
    await fireEvent.click(view.getByTestId('assistant-projects-all'));
    expect(button.disabled).toBe(false);
    await fireEvent.click(button);
    await waitFor(() => expect(save).toHaveBeenCalledWith('agent', true, []));
  });

  it('opens an assistant serving all projects with All checked', async () => {
    const view = await open({ ...agent, agentAllProjects: true, agentProjectIds: [] });
    expect((view.getByTestId('assistant-projects-all') as HTMLInputElement).checked).toBe(true);
    expect(items(view).every((el) => el.checked && el.disabled)).toBe(true);
  });

  it('shows a refusal inline and stays open', async () => {
    setBindingMock('SetAssistantProjects', async () => {
      throw new TransportError('invalid_argument', 'Project p9 is not on this computer.');
    });
    const view = await open();
    await fireEvent.click(view.getByTestId('assistant-projects-save'));
    await waitFor(() => expect(view.getByTestId('assistant-projects-error')).toHaveTextContent('Project p9 is not on this computer.'));
    expect(view.getByTestId('assistant-projects-dialog')).toBeInTheDocument();
  });
});

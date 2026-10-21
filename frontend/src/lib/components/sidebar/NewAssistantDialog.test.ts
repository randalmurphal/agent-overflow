import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import NewAssistantDialog from './NewAssistantDialog.svelte';
import { closeNewAssistantDialog, openNewAssistantDialog } from '../../stores/newAssistant.svelte';
import { refreshProjects, resetProjectsForTest } from '../../stores/projects.svelte';
import { getThreads } from '../../stores/threads.svelte';
import { applyThreadUpdated } from '../../stores/eventsThreadRows';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { __resetEntityIndexForTest } from '../../transport/entityIndex';
import { TransportError } from '../../transport/wsClient';
import type { Project } from '../../types/models';

function project(id: string, name: string): Project {
  return { id, path: `/repo/${id}`, name, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false };
}

describe('<NewAssistantDialog>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    resetProjectsForTest();
    resetPanesForTest();
    __resetEntityIndexForTest();
    setBindingMock('ListProjects', async () => [
      { project: project('p1', 'Alpha'), threadCount: 0 },
      { project: project('p2', 'Beta'), threadCount: 0 },
    ]);
    await refreshProjects();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
  });
  afterEach(() => closeNewAssistantDialog());

  const item = (view: { getAllByTestId: (id: string) => HTMLElement[] }, id: string) =>
    view.getAllByTestId('assistant-projects-item').find((el) => el.getAttribute('data-project-id') === id) as HTMLInputElement;

  it('opens with the project it came from checked and creates the assistant serving the chosen projects', async () => {
    const created = makeThread({ id: 'assistant-thread', title: 'Ops', projectId: '', agentId: 'agent-1' });
    const create = setBindingMock('CreateAssistant', vi.fn(async () => created));
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p2' });
    await tick();

    expect(view.getAllByTestId('assistant-projects-item').map((el) => el.getAttribute('data-project-id'))).toEqual(['p1', 'p2']);
    expect(item(view, 'p2').checked).toBe(true);
    expect(item(view, 'p1').checked).toBe(false);
    await fireEvent.click(item(view, 'p1'));
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: '  Ops  ' } });
    await fireEvent.click(view.getByTestId('new-assistant-create'));

    await waitFor(() => expect(create).toHaveBeenCalledWith({ name: 'Ops', projectIds: ['p2', 'p1'], allProjects: false }));
    await waitFor(() => expect(view.queryByTestId('new-assistant-dialog')).toBeNull());
    expect(getThreads().some((thread) => thread.id === 'assistant-thread')).toBe(true);
  });

  it('serves every project with All projects, and returns to the checked ones without it', async () => {
    const create = setBindingMock('CreateAssistant', vi.fn(async () => makeThread({ id: 't', projectId: '', agentId: 'a' })));
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p1' });
    await tick();
    const all = view.getByTestId('assistant-projects-all') as HTMLInputElement;
    await fireEvent.click(all);
    expect(item(view, 'p1').disabled && item(view, 'p2').disabled && item(view, 'p2').checked).toBe(true);
    await fireEvent.click(all);
    expect([item(view, 'p1').checked, item(view, 'p2').checked]).toEqual([true, false]);
    await fireEvent.click(all);
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'Scout' } });
    await fireEvent.click(view.getByTestId('new-assistant-create'));
    await waitFor(() => expect(create).toHaveBeenCalledWith({ name: 'Scout', projectIds: [], allProjects: true }));
  });

  it('keeps the row a frame brought during the creation over the older reply', async () => {
    const created = makeThread({ id: 'assistant-raced', title: 'Ops', projectId: 'p1', agentId: 'agent-1' });
    setBindingMock('CreateAssistant', async () => {
      applyThreadUpdated({ action: 'listed', thread: { ...created, title: 'Ops renamed meanwhile' } });
      return created;
    });
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p1' });
    await tick();
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'Ops' } });
    await fireEvent.click(view.getByTestId('new-assistant-create'));
    await waitFor(() => expect(view.queryByTestId('new-assistant-dialog')).toBeNull());
    expect(getThreads().find((thread) => thread.id === 'assistant-raced')?.title).toBe('Ops renamed meanwhile');
  });

  it('refuses a name in use inline, naming the assistant that holds it, and stays open', async () => {
    setBindingMock('CreateAssistant', async () => {
      throw new TransportError('assistant_name_taken', 'The assistant "Ops" already has that name; names are unique on this computer.');
    });
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p1' });
    await tick();
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'ops' } });
    await fireEvent.click(view.getByTestId('new-assistant-create'));
    await waitFor(() => expect(view.getByTestId('new-assistant-error')).toHaveTextContent('The assistant "Ops" already has that name'));
    expect(view.getByTestId('new-assistant-dialog')).toBeInTheDocument();
  });

  it('keeps Create inert until a name is typed and a project is chosen', async () => {
    const view = render(NewAssistantDialog);
    // From the Assistants header: no project comes preselected.
    openNewAssistantDialog({});
    await tick();
    const create = view.getByTestId('new-assistant-create') as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: '   ' } });
    expect(create.disabled).toBe(true);
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'Ops' } });
    expect(create.disabled).toBe(true);
    await fireEvent.click(item(view, 'p1'));
    expect(create.disabled).toBe(false);
    await fireEvent.click(item(view, 'p1'));
    expect(create.disabled).toBe(true);
  });

  it('starts each opening clean', async () => {
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p1' });
    await tick();
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'Ops' } });
    await fireEvent.click(view.getByTestId('assistant-projects-all'));
    closeNewAssistantDialog();
    await tick();
    openNewAssistantDialog({ projectId: 'p2' });
    await tick();
    expect((view.getByTestId('new-assistant-name') as HTMLInputElement).value).toBe('');
    expect((view.getByTestId('assistant-projects-all') as HTMLInputElement).checked).toBe(false);
    expect([item(view, 'p1').checked, item(view, 'p2').checked]).toEqual([false, true]);
  });
});

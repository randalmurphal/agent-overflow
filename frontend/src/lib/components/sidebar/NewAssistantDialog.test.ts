import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import NewAssistantDialog from './NewAssistantDialog.svelte';
import { closeNewAssistantDialog, getNewAssistantRequest, openNewAssistantDialog } from '../../stores/newAssistant.svelte';
import { setSelectedBackend } from '../../stores/selectedBackend.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { refreshProjects, resetProjectsForTest } from '../../stores/projects.svelte';
import { getThreads } from '../../stores/threads.svelte';
import { applyThreadUpdated } from '../../stores/eventsThreadRows';
import { createPane, destroyPane, ensureMainPane, findPaneShowingThread, getPane, resetPanesForTest } from '../../stores/panes.svelte';
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

  /** Starts a creation whose answer the test settles, then cancels and reopens the dialog on p2. */
  async function cancelAndReopenDuringCreate() {
    let settle!: { resolve: (thread: unknown) => void; reject: (err: unknown) => void };
    const create = setBindingMock('CreateAssistant', vi.fn(() => new Promise((resolve, reject) => { settle = { resolve, reject }; })));
    const open = setBindingMock('SwitchThread', vi.fn(async () => {}));
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p1' });
    await tick();
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'Ops' } });
    await fireEvent.click(view.getByTestId('new-assistant-create'));
    await waitFor(() => expect(create).toHaveBeenCalled());
    await fireEvent.click(view.getByTestId('new-assistant-cancel'));
    openNewAssistantDialog({ projectId: 'p2' });
    await tick();
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'Second' } });
    return { view, settle, open };
  }

  it('lists an assistant created after Cancel, and leaves the newer opening and the panes alone', async () => {
    const { view, settle, open } = await cancelAndReopenDuringCreate();
    settle.resolve(makeThread({ id: 'late-assistant', title: 'Ops', projectId: '', agentId: 'agent-late' }));
    await waitFor(() => expect(getThreads().some((thread) => thread.id === 'late-assistant')).toBe(true));
    await tick();
    expect(view.getByTestId('new-assistant-dialog')).toBeInTheDocument();
    expect((view.getByTestId('new-assistant-name') as HTMLInputElement).value).toBe('Second');
    expect((view.getByTestId('new-assistant-create') as HTMLButtonElement).disabled).toBe(false);
    expect(open).not.toHaveBeenCalled();
  });

  it('keeps a creation that failed after Cancel off the newer opening', async () => {
    const { view, settle } = await cancelAndReopenDuringCreate();
    vi.spyOn(console, 'error').mockImplementation(() => {});
    settle.reject(new TransportError('assistant_name_taken', 'Taken.'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    await tick();
    expect(view.queryByTestId('new-assistant-error')).toBeNull();
    expect((view.getByTestId('new-assistant-create') as HTMLButtonElement).disabled).toBe(false);
  });

  it('opens the created thread in the focused pane when the pane it came from closed meanwhile', async () => {
    const main = ensureMainPane();
    const side = createPane('side');
    let resolve!: (thread: unknown) => void;
    setBindingMock('CreateAssistant', () => new Promise((settle) => { resolve = settle; }));
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p1', targetPane: side });
    await tick();
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'Ops' } });
    await fireEvent.click(view.getByTestId('new-assistant-create'));
    destroyPane('side');
    resolve(makeThread({ id: 'made', title: 'Ops', projectId: '', agentId: 'a' }));
    await waitFor(() => expect(findPaneShowingThread('made')).toBe(main));
    expect(getPane('side')).toBeUndefined();
  });

  it('keeps the computer selected when it opened from the Assistants header', () => {
    setSelectedBackend('gpu');
    try {
      openNewAssistantDialog({});
      setSelectedBackend(HOME_BACKEND);
      expect(getNewAssistantRequest()?.selectedBackend).toBe('gpu');
      // An opening from a project creates on the project's computer.
      openNewAssistantDialog({ projectId: 'p1' });
      expect(getNewAssistantRequest()?.selectedBackend).toBeUndefined();
    } finally {
      setSelectedBackend(HOME_BACKEND);
    }
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

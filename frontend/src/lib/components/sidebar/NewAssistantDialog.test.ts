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
    setBindingMock('GitListWorktrees', async () => [
      { path: '/repo/p2/.worktrees/feature', branch: 'feature', head: 'abc', deleteBlocked: false, missing: false },
    ]);
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
  });
  afterEach(() => closeNewAssistantDialog());

  it('opens on the project it came from and creates the assistant with the name and workspace', async () => {
    const created = makeThread({ id: 'assistant-thread', title: 'Ops', projectId: 'p2', agentId: 'agent-1' });
    const create = setBindingMock('CreateAssistant', vi.fn(async () => created));
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p2' });
    await tick();

    expect((view.getByTestId('new-assistant-project') as HTMLSelectElement).value).toBe('p2');
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: '  Ops  ' } });
    await waitFor(() => expect(view.getByRole('option', { name: 'feature' })).toBeInTheDocument());
    await fireEvent.change(view.getByTestId('new-assistant-workspace'), { target: { value: '/repo/p2/.worktrees/feature' } });
    await fireEvent.click(view.getByTestId('new-assistant-create'));

    await waitFor(() => expect(create).toHaveBeenCalledWith({
      projectId: 'p2', name: 'Ops', worktreePath: '/repo/p2/.worktrees/feature', branch: 'feature',
    }));
    await waitFor(() => expect(view.queryByTestId('new-assistant-dialog')).toBeNull());
    expect(getThreads().some((thread) => thread.id === 'assistant-thread')).toBe(true);
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

  it('asks for a new worktree by its branch name', async () => {
    const create = setBindingMock('CreateAssistant', vi.fn(async () => makeThread({ id: 't', projectId: 'p1', agentId: 'a' })));
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p1' });
    await tick();
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: 'Scout' } });
    await fireEvent.change(view.getByTestId('new-assistant-workspace'), { target: { value: 'new' } });
    await fireEvent.input(view.getByTestId('new-assistant-branch'), { target: { value: 'scout-home' } });
    await fireEvent.click(view.getByTestId('new-assistant-create'));
    await waitFor(() => expect(create).toHaveBeenCalledWith({ projectId: 'p1', name: 'Scout', worktreeBranch: 'scout-home' }));
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

  it('keeps Create inert until a name is typed', async () => {
    const view = render(NewAssistantDialog);
    openNewAssistantDialog({ projectId: 'p1' });
    await tick();
    expect((view.getByTestId('new-assistant-create') as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.input(view.getByTestId('new-assistant-name'), { target: { value: '   ' } });
    expect((view.getByTestId('new-assistant-create') as HTMLButtonElement).disabled).toBe(true);
  });
});

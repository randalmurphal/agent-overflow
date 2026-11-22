import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import AssistantProjectsDialog from './AssistantProjectsDialog.svelte';
import { closeAssistantProjectsDialog, getAssistantProjectsAgentId, openAssistantProjectsDialog } from '../../stores/newAssistant.svelte';
import { resetProjectsForTest } from '../../stores/projects.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { loadSidebarCatalogs } from '../../stores/sidebarCatalogs';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { __resetEntityIndexForTest, noteThread, threadBackend } from '../../transport/entityIndex';
import { TransportError } from '../../transport/wsClient';
import type { Project, Thread } from '../../types/models';
import { applySettingsSnapshot } from '../../stores/settings.svelte';
import { makeSettings } from '../../../test/helpers/settings';

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
    await readSidebarCatalogs();
    setBindingMock('ListThreads', async () => [agent]);
    await loadSidebarCatalogs();
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

  it('leaves a newer opening alone when a save from before Cancel answers, for the same assistant or another', async () => {
    const other = makeThread({ id: 'other', title: 'Scout', projectId: '', agentId: 'scout', agentProjectIds: ['p1'] });
    for (const [reopen, outcome] of [[agent, 'resolve'], [agent, 'reject'], [other, 'resolve'], [other, 'reject']] as const) {
      replaceAllThreads([agent, other]);
      let settle!: { resolve: (thread: unknown) => void; reject: (err: unknown) => void };
      setBindingMock('SetAssistantProjects', () => new Promise((resolve, reject) => { settle = { resolve, reject }; }));
      vi.spyOn(console, 'error').mockImplementation(() => {});
      const view = render(AssistantProjectsDialog);
      openAssistantProjectsDialog(agent);
      await tick();
      await fireEvent.click(view.getByTestId('assistant-projects-save'));
      await fireEvent.click(view.getByTestId('assistant-projects-cancel'));
      openAssistantProjectsDialog(reopen);
      await tick();
      if (outcome === 'resolve') settle.resolve(agent);
      else settle.reject(new TransportError('invalid_argument', 'Refused.'));
      await new Promise((resolve) => setTimeout(resolve, 0));
      await tick();
      const label = `${reopen.id} ${outcome}`;
      expect(view.queryByTestId('assistant-projects-dialog'), label).toBeInTheDocument();
      expect(view.queryByTestId('assistant-projects-error'), label).toBeNull();
      expect((view.getByTestId('assistant-projects-save') as HTMLButtonElement).disabled, label).toBe(false);
      view.unmount();
      closeAssistantProjectsDialog();
    }
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

  it('closes when assistants are turned off and opens for nothing while they are', async () => {
    const view = await open();
    applySettingsSnapshot(makeSettings({ assistantsEnabled: false }));
    await tick();
    expect(getAssistantProjectsAgentId()).toBeNull();
    await waitFor(() => expect(view.queryByTestId('assistant-projects-dialog')).toBeNull());
    applySettingsSnapshot(makeSettings({ assistantsEnabled: true }));
    await tick();
    expect(view.queryByTestId('assistant-projects-dialog')).toBeNull();
    view.unmount();

    // The opener refuses on its own, with no dialog mounted to close it.
    applySettingsSnapshot(makeSettings({ assistantsEnabled: false }));
    openAssistantProjectsDialog(agent);
    expect(getAssistantProjectsAgentId()).toBeNull();
  });
});

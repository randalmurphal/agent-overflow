import { takePinnedBackend } from '../../../transport/backends';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import MachinePicker from './MachinePicker.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Project, Thread } from '../../../types/models';
import { resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../../test/helpers/chat';
import { idleWorkspaceActivity } from '../../../../test/helpers/workspaceLock';
import { resetPanesForTest } from '../../../stores/panes.svelte';
import { refreshProjects, resetProjectsForTest } from '../../../stores/projects.svelte';
import { __resetSelectedBackendForTest, selectedBackend } from '../../../stores/selectedBackend.svelte';
import { __resetEntityIndexForTest, noteProject, noteThread } from '../../../transport/entityIndex';
import { __resetBackendIdentityForTest, setBackendIdentityFromBootstrap } from '../../../transport/backendIdentity';
import { HOME_BACKEND } from '../../../transport/backendKey';
import { grantBackendScopes, revokeBackendScopes } from '../../../../test/helpers/scopes';
import {
  REMOTE_BACKEND_UUID,
  resetStagedBackends,
  stageBackend,
} from '../../../../test/helpers/backends';

const HOME_UUID = '11111111-2222-4333-8444-555555555555';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({ workspacePath: '/repo', projectPath: '/repo', projectId: 'project-1', ...overrides });
}

function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'project-1',
    path: '/repo',
    name: 'Repo',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread) {
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  return buildRegisteredPane(thread);
}

function buildPlaceholderPane(project = makeProject()) {
  const pane = createThreadPane();
  pane.startDraftPlaceholder(project, 'chat', {
    provider: 'claude',
    model: 'm',
    workspacePath: project.path,
    branch: 'main',
  });
  return pane;
}

async function seedProjects(projects: Project[]): Promise<void> {
  setBindingMock('ListProjects', async () => {
    const backend = takePinnedBackend();
    return projects.filter((_project, index) => backend === 'laptop' ? index > 0 : index === 0)
      .map((project) => ({ project, threadCount: 0 }));
  });
  await refreshProjects();
}

describe('<MachinePicker>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    resetPanesForTest();
    resetProjectsForTest();
    resetStagedBackends();
    __resetEntityIndexForTest();
    __resetSelectedBackendForTest();
    __resetBackendIdentityForTest();
    setBackendIdentityFromBootstrap(HOME_UUID, 1, 'Desk');
    await grantBackendScopes('laptop', ['threads:read', 'threads:operate', 'settings:read']);
  });

  afterEach(() => {
    revokeBackendScopes('laptop');
    resetStagedBackends();
    __resetEntityIndexForTest();
    __resetSelectedBackendForTest();
    __resetBackendIdentityForTest();
  });

  it('names the machine the pane’s project lives on, and locks once the thread has messages', async () => {
    stageBackend();
    noteProject('project-1', 'laptop');
    noteThread('thread-1', 'laptop');
    const pane = await buildPane(makeThread());
    const { getByTestId } = render(MachinePicker, { props: { pane } });
    const trigger = getByTestId('machine-picker-trigger');
    expect(trigger.textContent ?? '').toMatch(/Laptop/);
    expect(trigger).toHaveAttribute('data-locked', 'true');
    expect(trigger).toBeDisabled();
  });

  it('lists every attached machine on a draft, home by its own name, and dims an unreachable one', async () => {
    stageBackend({ status: 'reconnecting' });
    await seedProjects([makeProject()]);
    const pane = buildPlaceholderPane();
    const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });
    const trigger = getByTestId('machine-picker-trigger');
    expect(trigger.textContent ?? '').toMatch(/Desk/);
    expect(trigger).not.toHaveAttribute('data-locked');

    await fireEvent.click(trigger);
    const home = await findByRole('menuitem', { name: /Desk/ });
    const laptop = await findByRole('menuitem', { name: /Laptop/ });
    expect(home.textContent ?? '').toMatch(/\u2713/);
    expect(laptop.textContent ?? '').not.toMatch(/\u2713/);
    expect(laptop).toHaveAttribute('aria-disabled', 'true');
    expect(laptop.textContent ?? '').toMatch(/Offline/);
    expect(laptop.getAttribute('title')).toBe('This computer is offline right now.');
    expect(laptop.textContent ?? '').not.toMatch(/No browser/);
  });

  it('asks for the checkout rather than choosing an unrelated project', async () => {
    stageBackend();
    const remoteProject = makeProject({ id: 'project-2', path: '/home/me/other', name: 'Other' });
    await seedProjects([makeProject(), remoteProject]);
    noteProject('project-2', 'laptop');
    const pane = buildPlaceholderPane();
    const defaults = setBindingMock('GetThreadDefaults', async () => ({}));
    const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });
    await fireEvent.click(getByTestId('machine-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /Laptop/ }));
    expect(await findByRole('dialog', { name: 'Choose Repo on Laptop' })).toBeInTheDocument();
    expect(defaults).not.toHaveBeenCalled();
    expect(selectedBackend()).toBe(HOME_BACKEND);
  });

  it('flips to the SAME repository on the chosen machine when the entry spans it', async () => {
    stageBackend();
    const homeRepo = makeProject({ id: 'p-home', path: '/home/me/app', name: 'app', remoteURL: 'git@github.com:me/app.git' });
    const laptopRepo = makeProject({ id: 'p-laptop', path: '/Users/me/app', name: 'app', remoteURL: 'https://github.com/me/app' });
    const laptopOther = makeProject({ id: 'p-other', path: '/Users/me/other', name: 'other', remoteURL: 'https://github.com/me/other' });
    // The other project sorts first on the laptop; the sibling must still win.
    await seedProjects([homeRepo, laptopOther, laptopRepo]);
    noteProject('p-laptop', 'laptop');
    noteProject('p-other', 'laptop');
    const pane = buildPlaceholderPane(homeRepo);
    const defaults = setBindingMock('GetThreadDefaults', async () => ({ provider: 'claude', model: 'm' }));

    const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });
    await fireEvent.click(getByTestId('machine-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /Laptop/ }));

    await waitFor(() => {
      expect(defaults).toHaveBeenCalledTimes(1);
      expect(pane.thread?.projectId).toBe('p-laptop');
    });
  });

  it('opens the folder picker when the chosen machine has no project yet', async () => {
    stageBackend();
    await seedProjects([makeProject()]);
    const pane = buildPlaceholderPane();
    const defaults = setBindingMock('GetThreadDefaults', async () => ({ provider: 'claude', model: 'm' }));

    const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });
    await fireEvent.click(getByTestId('machine-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /Laptop/ }));

    expect(await findByRole('dialog', { name: 'Choose Repo on Laptop' })).toBeInTheDocument();
    expect(defaults).not.toHaveBeenCalled();
    expect(pane.hasDraftPlaceholder).toBe(true);
    expect(selectedBackend()).toBe(HOME_BACKEND);
  });

  describe('choosing the checkout on a computer with none', () => {
    const appRemote = 'git@github.com:me/app.git';

    async function openFolderPicker(source: Project) {
      stageBackend();
      await grantBackendScopes('laptop', ['threads:read', 'threads:operate', 'git:operate', 'files:read', 'settings:read']);
      await seedProjects([source]);
      setBindingMock('BrowseDirectory', async () => ({
        path: '/home/me/app', parent: '/home/me', separator: '/', entries: [], truncated: false, exists: true,
      }));
      const pane = buildPlaceholderPane(source);
      const view = render(MachinePicker, { props: { pane } });
      await fireEvent.click(view.getByTestId('machine-picker-trigger'));
      await fireEvent.click(await view.findByRole('menuitem', { name: /Laptop/ }));
      const dialog = await view.findByRole('dialog', { name: `Choose ${source.name} on Laptop` });
      await waitFor(() => expect(view.getByTestId('add-project-submit')).not.toBeDisabled());
      return { pane, dialog, ...view };
    }

    it('refuses a folder that is a checkout of another repository and keeps the computer fixed', async () => {
      const create = setBindingMock('CreateProject', async () => { throw new Error('must not create'); });
      setBindingMock('InspectProjectFolder', async (path: string) => {
        expect(takePinnedBackend()).toBe('laptop');
        expect(path).toBe('/home/me/app');
        return { repository: true, remoteURL: 'https://github.com/me/other', rootCommit: 'def' };
      });
      const { pane, dialog, getByTestId, findByTestId } = await openFolderPicker(
        makeProject({ name: 'app', remoteURL: appRemote, rootCommit: 'abc' }));
      expect(dialog).toHaveTextContent('Laptop has no checkout of app yet.');
      expect(dialog.querySelector('select')).toBeDisabled();

      await fireEvent.click(getByTestId('add-project-submit'));
      expect((await findByTestId('add-project-error')).textContent)
        .toBe('That folder is a checkout of github.com/me/other, not github.com/me/app.');
      expect(create).not.toHaveBeenCalled();
      expect(pane.thread?.projectId).toBe('project-1');
    });

    it('adopts a checkout of the same repository and moves the draft onto it', async () => {
      setBindingMock('InspectProjectFolder', async () => ({ repository: true, rootCommit: 'abc' }));
      const created = makeProject({ id: 'p-laptop', path: '/home/me/app', name: 'app', rootCommit: 'abc', createdAt: 5 });
      const create = setBindingMock('CreateProject', async (path: string) => {
        expect(takePinnedBackend()).toBe('laptop');
        expect(path).toBe('/home/me/app');
        return created;
      });
      setBindingMock('GetThreadDefaults', async () => ({ provider: 'claude', model: 'm' }));
      const { pane, getByTestId } = await openFolderPicker(makeProject({ name: 'app', remoteURL: appRemote, rootCommit: 'abc' }));

      await fireEvent.click(getByTestId('add-project-submit'));
      await waitFor(() => {
        expect(pane.thread?.projectId).toBe('p-laptop');
        expect(selectedBackend()).toBe('laptop');
      });
      expect(create).toHaveBeenCalledTimes(1);
    });

    it('lets a plain-directory project pick any folder, as its own project there', async () => {
      const inspect = setBindingMock('InspectProjectFolder', async () => ({ repository: false }));
      setBindingMock('CreateProject', async () => makeProject({ id: 'p-laptop', path: '/home/me/app', name: 'app' }));
      setBindingMock('GetThreadDefaults', async () => ({ provider: 'claude', model: 'm' }));
      const { pane, dialog, getByTestId } = await openFolderPicker(makeProject({ name: 'notes' }));
      expect(dialog).toHaveTextContent('It becomes its own project there.');

      await fireEvent.click(getByTestId('add-project-submit'));
      await waitFor(() => expect(pane.thread?.projectId).toBe('p-laptop'));
      expect(inspect).not.toHaveBeenCalled();
    });
  });

  describe('execution access', () => {
    it('keeps a reachable execution host selectable without unrelated browser details', async () => {
      stageBackend();
      await seedProjects([makeProject()]);
      const pane = buildPlaceholderPane();
      const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });

      await fireEvent.click(getByTestId('machine-picker-trigger'));
      const laptop = await findByRole('menuitem', { name: /Laptop/ });
      expect(laptop.textContent ?? '').not.toMatch(/No browser/);
      expect(laptop).not.toHaveAttribute('aria-disabled', 'true');
      expect(laptop.getAttribute('title')).toBeNull();
    });

    it('explains read-only access and prevents creating a thread on that host', async () => {
      stageBackend();
      await grantBackendScopes('laptop', ['threads:read', 'settings:read']);
      await seedProjects([makeProject()]);
      const pane = buildPlaceholderPane();
      const defaults = setBindingMock('GetThreadDefaults', async () => ({}));
      const { getByTestId, findByRole, queryByRole } = render(MachinePicker, { props: { pane } });
      await fireEvent.click(getByTestId('machine-picker-trigger'));
      const laptop = await findByRole('menuitem', { name: /Laptop/ });
      expect(laptop).toHaveAttribute('aria-disabled', 'true');
      expect(laptop.textContent).toContain('View only');
      await fireEvent.click(laptop);
      expect(defaults).not.toHaveBeenCalled();
      expect(queryByRole('dialog', { name: /Choose Repo/ })).toBeNull();
      expect(pane.thread?.projectId).toBe('project-1');
      // A grant arriving while the picker is open enables the same row.
      await grantBackendScopes('laptop', ['threads:read', 'threads:operate']);
      await waitFor(() => expect(laptop).not.toHaveAttribute('aria-disabled', 'true'));
      expect(laptop.textContent).not.toContain('View only');
    });

    it('says nothing about a machine that has them', async () => {
      stageBackend({
        hello: {
          protocolVersion: 1,
          capabilities: ['browser'],
          backendId: REMOTE_BACKEND_UUID,
          backendName: 'Laptop',
          serverTimeMs: 0,
          clockSkewMs: 0,
          bundleId: '',
        } as never,
      });
      await seedProjects([makeProject()]);
      const pane = buildPlaceholderPane();
      const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });

      await fireEvent.click(getByTestId('machine-picker-trigger'));
      const item = await findByRole('menuitem', { name: /Laptop/ });
      expect(item.textContent ?? '').not.toMatch(/No browser/);
      expect(item.getAttribute('title')).toBeNull();
    });
  });
});

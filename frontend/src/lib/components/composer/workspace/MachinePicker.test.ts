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
import { __resetEntityIndexForTest, noteProject } from '../../../transport/entityIndex';
import { __resetBackendIdentityForTest, setBackendIdentityFromBootstrap } from '../../../transport/backendIdentity';
import { HOME_BACKEND } from '../../../transport/backendKey';
import {
  REMOTE_BACKEND_UUID,
  resetStagedBackends,
  stageBackend,
} from '../../../../test/helpers/backends';
import { getToasts } from '../../../stores/toast.svelte';

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
  setBindingMock('ListProjects', async () => projects.map((project) => ({ project, threadCount: 0 })));
  await refreshProjects();
}

describe('<MachinePicker>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetProjectsForTest();
    resetStagedBackends();
    __resetEntityIndexForTest();
    __resetSelectedBackendForTest();
    __resetBackendIdentityForTest();
    setBackendIdentityFromBootstrap(HOME_UUID, 1, 'Desk');
  });

  afterEach(() => {
    resetStagedBackends();
    __resetEntityIndexForTest();
    __resetSelectedBackendForTest();
    __resetBackendIdentityForTest();
  });

  it('names the machine the pane’s project lives on, and locks once the thread has messages', async () => {
    stageBackend();
    noteProject('project-1', 'laptop');
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
    expect(laptop.textContent ?? '').toMatch(/Unreachable/);
    // Unreachable is the louder of the two and owns the one description
    // line: a machine this client cannot talk to has no browser here either
    // way, so saying both would be saying one thing twice.
    expect(laptop.textContent ?? '').not.toMatch(/No browser/);
  });

  it('flips the draft onto the chosen machine’s first project and stages the route', async () => {
    stageBackend();
    const remoteProject = makeProject({ id: 'project-2', path: '/home/me/app', name: 'App' });
    await seedProjects([makeProject(), remoteProject]);
    noteProject('project-2', 'laptop');
    const pane = buildPlaceholderPane();
    // The flip re-stages the placeholder on the far project; its defaults
    // read is the first RPC that has to reach the chosen machine.
    const defaults = setBindingMock('GetThreadDefaults', async () => ({ provider: 'claude', model: 'm' }));

    const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });
    await fireEvent.click(getByTestId('machine-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /Laptop/ }));

    await waitFor(() => {
      expect(defaults).toHaveBeenCalledTimes(1);
      expect(pane.thread?.projectId).toBe('project-2');
      expect(getByTestId('machine-picker-trigger').textContent ?? '').toMatch(/Laptop/);
    });
    expect(pane.hasDraftPlaceholder).toBe(true);
    expect(selectedBackend()).toBe('laptop');
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

  it('says so, and moves nothing, when the chosen machine has no project yet', async () => {
    stageBackend();
    await seedProjects([makeProject()]);
    const pane = buildPlaceholderPane();
    const defaults = setBindingMock('GetThreadDefaults', async () => ({ provider: 'claude', model: 'm' }));

    const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });
    await fireEvent.click(getByTestId('machine-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /Laptop/ }));

    await waitFor(() => {
      expect(getToasts().some((t) => /no projects yet/.test(t.message))).toBe(true);
    });
    expect(defaults).not.toHaveBeenCalled();
    expect(pane.hasDraftPlaceholder).toBe(true);
    expect(selectedBackend()).toBe(HOME_BACKEND);
  });

  // A machine that cannot run a headless browser is still a machine you can
  // send work to, so it stays selectable and only says so. Absence of the
  // capability is the answer: a backend too old to advertise anything reads
  // as having no browser rather than as unknown.
  describe('browser capability', () => {
    it('marks a reachable machine with no browser tools, without disabling it', async () => {
      stageBackend();
      await seedProjects([makeProject()]);
      const pane = buildPlaceholderPane();
      const { getByTestId, findByRole } = render(MachinePicker, { props: { pane } });

      await fireEvent.click(getByTestId('machine-picker-trigger'));
      const laptop = await findByRole('menuitem', { name: /Laptop/ });
      expect(laptop.textContent ?? '').toMatch(/No browser/);
      expect(laptop).not.toHaveAttribute('aria-disabled', 'true');
      expect(laptop.getAttribute('title')).toBe(
        'An agent on this machine cannot open a browser',
      );
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

// The sidebar's import entry point (F5). The catalogue surface itself is
// covered by SessionImportModal.test.ts — what matters here is that the
// header exposes the trigger, that it is inert in a view-only session (the
// visible half of the store's RPC refusal), and that it raises the store's
// `open` rather than owning a second copy of that state.
//
// The modal is deliberately NOT mounted from this section: Sidebar renders
// it only while expanded, so a mod+b collapse mid-run would unmount the
// surface. It hangs off App.svelte with the other store-gated overlays, and
// the assertion below pins that this section raises `open` and nothing else.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ProjectsSection from './ProjectsSection.svelte';
import { makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { refreshProjects, resetProjectsForTest } from '../../stores/projects.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { setThreadFilterQuery } from '../../stores/threadFilter.svelte';
import { isSidebarSectionExpanded, resetSidebarForTest } from '../../stores/sidebar.svelte';
import { closeNewAssistantDialog, getNewAssistantRequest } from '../../stores/newAssistant.svelte';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import {
  isSessionImportOpen,
  resetSessionImportForTest,
} from '../../stores/sessionImport.svelte';

function importButton(container: HTMLElement): HTMLButtonElement {
  const icon = container.querySelector('[data-testid="sidebar-import-sessions-icon"]');
  const button = icon?.closest('button');
  if (!button) throw new Error('import trigger not rendered');
  return button as HTMLButtonElement;
}

describe('ProjectsSection import trigger', () => {
  beforeEach(() => {
    resetSessionImportForTest();
    setPageGrantsFromBootstrap(false);
  });

  afterEach(() => {
    setPageGrantsFromBootstrap(false);
    resetSessionImportForTest();
  });

  it('renders an enabled trigger between the sort menu and Add Project', () => {
    const view = render(ProjectsSection, { props: { pane: null } });
    const button = importButton(view.container);

    expect(button).toBeEnabled();
    expect(button).toHaveAttribute('aria-label', 'Import Sessions');
    // No override in a local session: the tooltip mirrors the aria-label.
    expect(button).toHaveAttribute('title', 'Import Sessions');

    const header = button.parentElement;
    const buttons = [...(header?.querySelectorAll('button') ?? [])];
    const position = buttons.indexOf(button);
    expect(buttons[position - 1]).toHaveAttribute('aria-label', expect.stringContaining('Sort Projects'));
    expect(buttons[position + 1]).toHaveAttribute('aria-label', 'Add Project');
  });

  it('raises the store flag and mounts nothing of its own', async () => {
    const view = render(ProjectsSection, { props: { pane: null } });
    expect(isSessionImportOpen()).toBe(false);

    await fireEvent.click(importButton(view.container));

    expect(isSessionImportOpen()).toBe(true);
    // The surface lives in App.svelte; a section that mounted its own copy
    // would take it down with the sidebar on collapse.
    expect(view.queryByTestId('session-import-body')).not.toBeInTheDocument();
  });

  it('is disabled with an ungranted tooltip in a view-only session', async () => {
    setPageGrantsFromBootstrap(true);
    const view = render(ProjectsSection, { props: { pane: null } });
    const button = importButton(view.container);

    expect(button).toBeDisabled();
    expect(button).toHaveAttribute('title', 'Not granted to this device');
    // The action still has a name for assistive tech; only the hover text
    // explains why it is unavailable.
    expect(button).toHaveAttribute('aria-label', 'Import Sessions');

    await fireEvent.click(button);
    expect(isSessionImportOpen()).toBe(false);
  });
});

// The two sections (docs/specs/standing-agents.md, "Sidebar"): an
// assistant's card above the projects, holding its helpers; its project
// work in that project under its row there; each section folds on its own
// and a search shows its matches through a fold.
describe('ProjectsSection assistants and projects', () => {
  const agent = makeThread({ id: 'agent', title: 'Ops', projectId: '', agentId: 'ops', agentProjectIds: ['p1'] });
  const helper = makeThread({ id: 'helper', title: 'Research notes', projectId: '', helperOfAgentId: 'ops', spawnedByThreadId: 'agent', pinnedAt: 1 });
  const work = makeThread({ id: 'work', title: 'Fix the build', projectId: 'p1', spawnedByThreadId: 'agent', pinnedAt: 1 });
  const plain = makeThread({ id: 'plain', title: 'Plain thread', projectId: 'p1', pinnedAt: 1 });

  beforeEach(async () => {
    resetBindingMocks();
    resetSidebarForTest();
    resetProjectsForTest();
    setThreadFilterQuery('');
    closeNewAssistantDialog();
    setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: false }));
    setBindingMock('ListProjects', async () => [
      { project: { id: 'p1', path: '/repo/p1', name: 'Alpha', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 2 },
    ]);
    await refreshProjects();
    replaceAllThreads([agent, helper, work, plain]);
  });

  afterEach(() => {
    setThreadFilterQuery('');
    closeNewAssistantDialog();
  });

  const idsIn = (el: Element | null) =>
    Array.from(el?.querySelectorAll<HTMLElement>('[data-sidebar-thread-id]') ?? [], (row) => row.dataset.sidebarThreadId);

  it('lists the card with its helper above the projects, and the project work under the assistant\'s row in its project', () => {
    const view = render(ProjectsSection, { props: { pane: null } });
    expect(idsIn(view.getByTestId('sidebar-assistants-section'))).toEqual(['agent', 'helper']);
    expect(idsIn(view.getByTestId('sidebar-projects-section'))).toEqual(['agent', 'work', 'plain']);
    const projectRow = view.getByTestId('sidebar-projects-section').querySelector('[data-sidebar-thread-id="agent"]');
    expect(projectRow?.querySelector('[data-testid="thread-row-agent-projects"]')).toBeNull();
    expect(view.getByTestId('sidebar-assistants-section').querySelector('[data-testid="thread-row-agent-projects"]')?.textContent?.trim()).toBe('Alpha');
  });

  it('folds each section on its own and shows a search\'s matches through a fold', async () => {
    const view = render(ProjectsSection, { props: { pane: null } });
    await fireEvent.click(view.getByTestId('sidebar-section-toggle-assistants'));
    expect(idsIn(view.getByTestId('sidebar-assistants-section'))).toEqual([]);
    expect(idsIn(view.getByTestId('sidebar-projects-section'))).toEqual(['agent', 'work', 'plain']);
    await fireEvent.click(view.getByTestId('sidebar-section-toggle-projects'));
    expect(idsIn(view.getByTestId('sidebar-projects-section'))).toEqual([]);
    expect(view.getByTestId('sidebar-section-toggle-projects')).toHaveAttribute('aria-expanded', 'false');

    setThreadFilterQuery('research');
    await tick();
    expect(idsIn(view.getByTestId('sidebar-assistants-section'))).toEqual(['agent', 'helper']);
    setThreadFilterQuery('build');
    await tick();
    expect(idsIn(view.getByTestId('sidebar-assistants-section'))).toEqual([]);
    expect(idsIn(view.getByTestId('sidebar-projects-section'))).toEqual(['agent', 'work']);

    setThreadFilterQuery('');
    await tick();
    expect(idsIn(view.getByTestId('sidebar-projects-section'))).toEqual([]);
    await fireEvent.click(view.getByTestId('sidebar-section-toggle-assistants'));
    expect(idsIn(view.getByTestId('sidebar-assistants-section'))).toEqual(['agent', 'helper']);
  });

  it('marks a folded section\'s chevron expanded while a search shows its matches', async () => {
    const view = render(ProjectsSection, { props: { pane: null } });
    const toggles = () => (['assistants', 'projects'] as const).map((section) => view.getByTestId(`sidebar-section-toggle-${section}`));
    await fireEvent.click(toggles()[0]);
    await fireEvent.click(toggles()[1]);
    for (const toggle of toggles()) {
      expect(toggle).toHaveAttribute('aria-expanded', 'false');
      expect(toggle.getAttribute('aria-label')).toMatch(/^Expand /);
    }

    setThreadFilterQuery('plain');
    await tick();
    expect(idsIn(view.getByTestId('sidebar-projects-section'))).toEqual(['plain']);
    for (const toggle of toggles()) {
      expect(toggle).toHaveAttribute('aria-expanded', 'true');
      expect(toggle.getAttribute('aria-label')).toMatch(/^Collapse /);
    }

    setThreadFilterQuery('');
    await tick();
    for (const toggle of toggles()) expect(toggle).toHaveAttribute('aria-expanded', 'false');
  });

  it('unfolds a folded Projects section to show a project just added', async () => {
    const created = { id: 'p2', path: '/Users/me', name: 'me', sortPosition: 1, createdAt: 1, updatedAt: 1, archived: false };
    setBindingMock('BrowseDirectory', async () => ({ path: '/Users/me', parent: '/Users', separator: '/', entries: [], truncated: false, exists: true }));
    setBindingMock('CreateProject', async () => created);
    const view = render(ProjectsSection, { props: { pane: null } });
    await fireEvent.click(view.getByTestId('sidebar-section-toggle-projects'));
    expect(isSidebarSectionExpanded('projects')).toBe(false);

    await fireEvent.click(view.getByRole('button', { name: 'Add Project' }));
    const submit = await view.findByTestId('add-project-submit');
    await waitFor(() => expect(submit).toBeEnabled());
    await fireEvent.click(submit);
    await waitFor(() => expect(isSidebarSectionExpanded('projects')).toBe(true));
    expect(view.getByTestId('sidebar-section-toggle-projects')).toHaveAttribute('aria-expanded', 'true');
  });

  it('opens New Assistant from the section header with no project chosen', async () => {
    const view = render(ProjectsSection, { props: { pane: null } });
    const button = view.getByTestId('sidebar-new-assistant-icon').closest('button')!;
    expect(button).toHaveAttribute('aria-label', 'New Assistant');
    await fireEvent.click(button);
    expect(getNewAssistantRequest()).toMatchObject({ openInNewPane: false });
    expect(getNewAssistantRequest()?.projectId).toBeUndefined();
  });
});

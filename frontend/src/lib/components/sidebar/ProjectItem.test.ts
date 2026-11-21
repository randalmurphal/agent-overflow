// Duplicate project names are legal (paths are the unique key). The list
// disambiguates its rows: a unique name renders bare (with the classic
// truncate span), a duplicate gains a dim parent-dir prefix that must never
// ellipsize.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import ProjectItem from './ProjectItem.svelte';
import ProjectList from './ProjectList.svelte';
import { addProjectLocal, resetProjectsForTest, updateProjectLocal } from '../../stores/projects.svelte';
import { storeEntries, storeEntryFor } from '../../../test/helpers/sidebarEntry';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import type { Project } from '../../types/models';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { getToasts, resetToastsForTest } from '../../stores/toast.svelte';
import { collapseProject, resetSidebarForTest } from '../../stores/sidebar.svelte';
import { projectTurnStarted, resetForTest as resetThreadStatuses } from '../../stores/threadStatuses.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { __resetEntityIndexForTest, noteProject, noteThread } from '../../transport/entityIndex';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { Thread } from '../../types/models';

function makeProject(id: string, name: string, path: string): Project {
  return {
    id,
    name,
    path,
    slug: id,
    color: '',
    sortPosition: 0,
    createdAt: 1,
    updatedAt: 1,
    archived: false,
  } as Project;
}

// The row takes the entry the sidebar would render for it, through a getter
// so a row the test changes reaches the component the way the projection's
// own derivation does.
function renderItem(p: Project) {
  return render(ProjectItem, {
    props: {
      get entry() { return storeEntryFor(p.id); },
      displayLabel: { prefix: '', name: p.name },
      threads: [],
      pane: null,
    } as never,
  });
}

afterEach(() => {
  cleanup();
  resetProjectsForTest();
  resetToLocalPage();
});

// The row's new-thread control stays in place for a session that cannot use
// it — a project whose row lost an affordance reads as a broken sidebar
// rather than a read-only one — and goes inert instead.
describe('ProjectItem create controls', () => {
  it('offers new-thread on the local page and no per-project terminal button', () => {
    const p = makeProject('a', 'web', '/work/web');
    addProjectLocal(p);
    const { getByTestId, queryByTestId } = renderItem(p);
    expect((getByTestId('project-item-new-thread') as HTMLButtonElement).disabled).toBe(false);
    // Terminals open from the palette, the chat header or the compact menu;
    // the header carries no terminal control of its own.
    expect(queryByTestId('project-item-new-terminal')).toBeNull();
  });

  it('renders it inert for a view-only session', async () => {
    const p = makeProject('a', 'web', '/work/web');
    addProjectLocal(p);
    await pairViewOnly();
    const { getByTestId } = renderItem(p);
    const newThread = getByTestId('project-item-new-thread') as HTMLButtonElement;
    expect(newThread.disabled).toBe(true);
    expect(newThread.title).toBe('Not granted to this device');
    // New Group writes under the same grant (CreateThreadGroup).
    const newGroup = getByTestId('project-item-new-group') as HTMLButtonElement;
    expect(newGroup.disabled).toBe(true);
    expect(newGroup.title).toBe('Not granted to this device');
  });
});

// The label is disambiguated by the list over the rows it renders, so these
// render the list: a row shows what it was handed.
describe('ProjectList label', () => {
  function renderList() {
    return render(ProjectList, {
      props: {
        get entries() { return storeEntries(); },
        threadsByProject: new Map(),
        pane: null,
      } as never,
    });
  }

  function labelOf(view: ReturnType<typeof renderList>, path: string): HTMLElement {
    const row = [...view.container.querySelectorAll('[data-testid="project-item"]')]
      .find((el) => el.querySelector(`[title="${path}"]`));
    expect(row).toBeTruthy();
    return (row as HTMLElement).querySelector('[data-testid="project-item-label"]') as HTMLElement;
  }

  it('renders a unique name bare', () => {
    addProjectLocal(makeProject('a', 'web', '/work/web'));
    const view = renderList();
    const label = labelOf(view, '/work/web');
    expect(label).toHaveTextContent('web');
    expect(label.textContent).not.toContain('/');
  });

  it('renders a parent-dir prefix when another project shares the name', () => {
    addProjectLocal(makeProject('a', 'web', '/work/clients/web'));
    addProjectLocal(makeProject('b', 'web', '/work/personal/web'));
    const view = renderList();
    const label = labelOf(view, '/work/clients/web');
    expect(label.textContent?.replace(/\s+/g, '')).toBe('clients/web');
    // No ellipsis on the conflict layout: the prefix exists to be read.
    expect(label.className).not.toContain('truncate');
    expect(label.querySelector('.truncate')).toBeNull();
  });

  it('drops the prefix again once the conflicting project is gone', async () => {
    const a = makeProject('a', 'web', '/work/clients/web');
    addProjectLocal(a);
    addProjectLocal(makeProject('b', 'web', '/work/personal/web'));
    const view = renderList();
    resetProjectsForTest();
    addProjectLocal(a);
    await tick();
    expect(labelOf(view, '/work/clients/web').textContent?.replace(/\s+/g, '')).toBe('web');
  });

  it('renders a checkout at the filesystem root with a single leading slash', () => {
    addProjectLocal(makeProject('root', 'web', '/web'));
    addProjectLocal(makeProject('nested', 'web', '/work/web'));
    const view = renderList();
    expect(labelOf(view, '/web').textContent?.replace(/\s+/g, '')).toBe('/web');
  });
});

// The phone has no hover: the header's create controls cannot be revealed,
// so the row carries a visible menu button whose menu lists them, and the
// header does not drag.
describe('ProjectItem compact layout', () => {
  afterEach(() => {
    setCompactLayoutForTest(false);
  });

  it('opens the project menu, carrying New Terminal, from its menu button', async () => {
    setCompactLayoutForTest(true);
    const p = makeProject('a', 'web', '/work/web');
    addProjectLocal(p);
    const onNewTerminal = vi.fn();
    const { getByTestId, getByRole } = render(ProjectItem, {
      props: {
        get entry() { return storeEntryFor(p.id); },
        displayLabel: { prefix: '', name: p.name },
        threads: [],
        pane: null,
        onNewTerminal,
      } as never,
    });
    expect(getByTestId('project-item').querySelector('[draggable]')?.getAttribute('draggable')).toBe('false');
    await fireEvent.click(getByTestId('project-item-menu'));
    await tick();
    expect(document.querySelector('[data-popover] [role="menu"]')).not.toBeNull();
    await fireEvent.click(getByRole('menuitem', { name: 'New Terminal' }));
    expect(onNewTerminal).toHaveBeenCalledWith('a');
  });
});

describe('ProjectItem identity read failure', () => {
  afterEach(() => {
    resetToastsForTest();
    resetBindingMocks();
  });

  it('flags the entry with git\'s reason and clears once the read succeeds', async () => {
    const failed = { ...makeProject('a', 'web', '/work/web'), identityError: 'detected dubious ownership' };
    addProjectLocal(failed);
    const { getByTestId, queryByTestId, getByRole } = renderItem(failed);
    const flag = getByTestId('project-item-identity-error');
    expect(flag.getAttribute('title')).toBe("Could not fully verify /work/web: detected dubious ownership");

    updateProjectLocal({ ...failed, identityError: undefined });
    await tick();
    expect(queryByTestId('project-item-identity-error')).toBeNull();
    expect(getByRole('button', { name: /^(Expand|Collapse) Project$/ })).toBeTruthy();
  });

  // A tap rechecks on the owning computer, then shows a reason that remains
  // where hover cannot, without toggling the row.
  it('rechecks on a tap and shows the reason that remains', async () => {
    const failed = { ...makeProject('a', 'web', '/work/web'), identityError: 'detected dubious ownership' };
    addProjectLocal(failed);
    const refresh = setBindingMock('RefreshProjectIdentity', async () => failed);
    const { getByTestId, getByRole } = renderItem(failed);
    const toggle = () => getByRole('button', { name: /^(Expand|Collapse) Project$/ }).getAttribute('aria-label');
    const before = toggle();
    await fireEvent.click(getByTestId('project-item-identity-error'));
    await vi.waitFor(() => expect(getToasts().map((toast) => [toast.type, toast.message]))
      .toEqual([['warning', "Could not fully verify /work/web: detected dubious ownership"]]));
    expect(refresh).toHaveBeenCalledWith('a');
    expect(toggle()).toBe(before);
  });

  it('clears the flag without a toast when the recheck verifies', async () => {
    const failed = { ...makeProject('a', 'web', '/work/web'), identityError: 'Could not verify repository identity with glab on gitlab.com.' };
    addProjectLocal(failed);
    let answer!: (row: Project) => void;
    setBindingMock('RefreshProjectIdentity', () => new Promise<Project>((resolve) => { answer = resolve; }));
    const { getByTestId, queryByTestId } = renderItem(failed);
    const flag = getByTestId('project-item-identity-error');
    await fireEvent.click(flag);
    // One recheck at a time.
    expect(flag.hasAttribute('disabled')).toBe(true);
    answer({ ...failed, identityError: undefined, repositoryID: 'gitlab:gitlab.com:7' });
    await vi.waitFor(() => expect(queryByTestId('project-item-identity-error')).toBeNull());
    expect(getToasts()).toEqual([]);
  });

  it('reports a recheck that could not run', async () => {
    const failed = { ...makeProject('a', 'web', '/work/web'), identityError: 'detected dubious ownership' };
    addProjectLocal(failed);
    setBindingMock('RefreshProjectIdentity', async () => { throw new Error('computer unreachable'); });
    const { getByTestId } = renderItem(failed);
    await fireEvent.click(getByTestId('project-item-identity-error'));
    await vi.waitFor(() => expect(getToasts().map((toast) => toast.type)).toEqual(['error', 'warning']));
    expect(getToasts()[0].message).toBe('Computer unreachable.');
  });
});

// An action takes its project when the gesture happens. A repository checked
// out on two computers is one row, and which checkout wears it changes when a
// computer lists or a checkout is archived; an action already under way must
// not move onto the other one.
describe('ProjectItem action target', () => {
  afterEach(() => {
    resetStagedBackends();
    __resetEntityIndexForTest();
    resetBindingMocks();
    resetToastsForTest();
  });

  it('archives the checkout the menu opened on after the entry changes hands', async () => {
    stageBackend({ id: 'laptop', backendId: 'laptop-id', name: 'Laptop' });
    const home = { ...makeProject('ck-home', 'app', '/home/app'), repositoryID: 'github:github.com:1' };
    const laptop = { ...makeProject('ck-laptop', 'app-laptop', '/laptop/app'), repositoryID: 'github:github.com:1' };
    noteProject(home.id, '');
    noteProject(laptop.id, 'laptop');
    addProjectLocal(home);
    addProjectLocal(laptop);
    const archive = setBindingMock('ArchiveProject', vi.fn(async () => {}));
    // The entry as the list would hand it over, asked for by the member that
    // stays, and kept once the archive takes its last row out of the store:
    // the list unmounts the row there, which a single render cannot.
    let latest = storeEntryFor('ck-laptop');
    const entryNow = () => {
      latest = storeEntries().find(
        (candidate) => candidate.members.some((member) => member.project.id === 'ck-laptop'),
      ) ?? latest;
      return latest;
    };
    const { getByTestId, getByRole } = render(ProjectItem, {
      props: {
        get entry() { return entryNow(); },
        get displayLabel() { return { prefix: '', name: entryNow().name }; },
        threads: [],
        pane: null,
      } as never,
    });
    const row = getByTestId('project-item');
    expect(row.getAttribute('data-project-id')).toBe('ck-home');

    await fireEvent.contextMenu(row.querySelector('[role="button"][aria-expanded]') as HTMLElement);
    await tick();
    await fireEvent.click(getByRole('menuitem', { name: 'Archive Project' }));
    await tick();

    // Home's checkout goes archived while the confirmation is open, so the
    // live one wears the entry from here on and the row renames itself.
    updateProjectLocal({ ...home, archived: true });
    await tick();
    expect(getByTestId('project-item').getAttribute('data-project-id')).toBe('ck-laptop');
    expect(getByTestId('project-item-label')).toHaveTextContent('app-laptop');
    // The confirmation still names what the user opened it on.
    expect(getByRole('dialog')).toHaveTextContent('Hide "app" from the sidebar.');

    await fireEvent.click(getByRole('button', { name: 'Archive' }));
    await vi.waitFor(() => expect(archive).toHaveBeenCalledWith('ck-home'));
  });
});

// A collapsed project's rollup dot carries its threads' status; one from an
// unreachable computer is its last report, shown still.
describe('ProjectItem status rollup', () => {
  afterEach(() => {
    resetStagedBackends();
    __resetEntityIndexForTest();
    resetThreadStatuses();
    resetSidebarForTest();
  });

  it("stops the motion while the thread's computer is unreachable", async () => {
    const p = makeProject('a', 'web', '/work/web');
    addProjectLocal(p);
    collapseProject(p.id);
    const thread = { id: 'remote', projectId: p.id, title: 'remote', provider: 'claude', workspacePath: '/work/web', projectPath: '/work/web', createdAt: 0, updatedAt: 0, archived: false } as Thread;
    const staged = stageBackend({ status: 'reconnecting' });
    noteThread('remote', 'laptop');
    projectTurnStarted('remote', 'turn-1', 0, 0);
    const { getByTestId } = render(ProjectItem, {
      props: {
        get entry() { return storeEntryFor(p.id); },
        displayLabel: { prefix: '', name: p.name },
        threads: [thread],
        pane: null,
      } as never,
    });
    const dot = () => getByTestId('project-item-status-dot').firstElementChild;
    expect(getByTestId('project-item-status-dot')).toHaveAttribute('data-status', 'running');
    expect(dot()).not.toHaveClass('animate-pulse');

    staged.setStatus('connected');
    await tick();
    expect(dot()).toHaveClass('animate-pulse');
  });
});

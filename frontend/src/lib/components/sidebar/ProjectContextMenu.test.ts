// Project deletion from the sidebar (D25). Delete asks the backend what the
// deletion involves before it offers anything, and the three answers — nothing
// to say, something to say, or "I could not tell" — each get their own path.

import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ProjectContextMenu from './ProjectContextMenu.svelte';
import {
  resetBindingMocks,
  setBindingMock,
  getBindingMock,
} from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import type { ProjectWithCounts } from '../../types/models';
import type { ProjectDeletionPreview, ProjectDeletionResult } from '../../types/workflow';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import { isProjectExpanded, resetSidebarForTest } from '../../stores/sidebar.svelte';
import {
  consumePendingGroupRename,
  getThreadGroupById,
  resetThreadGroupsForTest,
} from '../../stores/threadGroups.svelte';
import {
  getThreadFilterQuery,
  setThreadFilterQuery,
} from '../../stores/threadFilter.svelte';

function makeProject(): ProjectWithCounts {
  return {
    project: {
      id: 'project-1',
      path: '/tmp/repo',
      name: 'Repo',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    },
    threadCount: 2,
  };
}

function emptyPreview(): ProjectDeletionPreview {
  return {
    projectId: 'project-1',
    runCount: 0,
    liveRunIds: [],
    automationCount: 0,
    worktrees: [],
    hasWork: false,
  };
}

function workPreview(): ProjectDeletionPreview {
  return {
    projectId: 'project-1',
    runCount: 3,
    liveRunIds: ['run-1'],
    automationCount: 1,
    worktrees: [
      {
        path: '/tmp/worktrees/run-1',
        branch: 'workflow-run-1',
        dirtyFileCount: 2,
        retained: true,
        reason: '2 uncommitted or untracked files',
      },
      {
        path: '/tmp/worktrees/run-2',
        branch: 'workflow-run-2',
        dirtyFileCount: 0,
        retained: false,
      },
    ],
    hasWork: true,
  };
}

function emptyResult(): ProjectDeletionResult {
  return { threadIds: [], retainedWorktrees: [] };
}

function renderMenu() {
  const anchor = document.createElement('div');
  document.body.appendChild(anchor);
  return render(ProjectContextMenu, {
    props: {
      project: makeProject(),
      anchor,
      open: true,
      onClose: () => {},
      onRename: () => {},
    },
  });
}

async function selectDelete(baseElement: HTMLElement): Promise<void> {
  const item = Array.from(baseElement.querySelectorAll('[role="menuitem"]')).find(
    (el) => el.textContent?.trim() === 'Delete Project',
  );
  expect(item).toBeTruthy();
  await fireEvent.click(item as Element);
}

function toastMessages(): string[] {
  return getToasts().map((toast) => toast.message);
}

describe('<ProjectContextMenu> delete flow', () => {
  beforeEach(() => {
    resetBindingMocks();
    setPageGrantsFromBootstrap(false);
    for (const toast of [...getToasts()]) removeToast(toast.id);
    setBindingMock('DeleteProject', async () => emptyResult());
  });

  afterEach(() => {
    setPageGrantsFromBootstrap(false);
  });

  it('omits Open in Editor in a view-only session', () => {
    setPageGrantsFromBootstrap(true);
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { baseElement } = renderMenu();

    expect(baseElement.textContent).not.toContain('Open in Editor');
    expect(open).not.toHaveBeenCalled();
  });

  it('takes the plain confirm when the project owns no workflow work', async () => {
    setBindingMock('ProjectDeletionPreview', async () => emptyPreview());
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.textContent).toContain('Permanently delete "Repo"');
    });
    // The richer dialog is for projects that own work; this one must not appear.
    expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).toBeNull();

    const confirm = Array.from(baseElement.querySelectorAll('button')).find(
      (el) => el.textContent?.trim() === 'Delete',
    );
    await fireEvent.click(confirm as Element);

    await waitFor(() => {
      expect(getBindingMock('DeleteProject')).toHaveBeenCalledWith('project-1');
    });
  });

  it('says what the cleanup does, that branches are kept, and which checkouts stay', async () => {
    setBindingMock('ProjectDeletionPreview', async () => workPreview());
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).not.toBeNull();
    });
    const work = baseElement.querySelector('[data-testid="project-delete-work"]');
    expect(work?.textContent).toContain('3 workflow runs and 1 automation will be deleted with it');
    expect(work?.textContent).toContain('1 still working');
    expect(
      baseElement.querySelector('[data-testid="project-delete-branches"]')?.textContent,
    ).toContain('branches are kept');

    // Only the checkout that will survive is listed — the clean one costs the
    // user nothing and needs no row.
    const rows = baseElement.querySelectorAll('[data-testid="project-delete-worktree"]');
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain('/tmp/worktrees/run-1');
    expect(rows[0].textContent).toContain('workflow-run-1');
    expect(rows[0].textContent).toContain('2 uncommitted or untracked files');
    expect(baseElement.textContent).not.toContain('/tmp/worktrees/run-2');

    const confirm = baseElement.querySelector('[data-testid="project-delete-confirm"]');
    expect(confirm?.textContent?.trim()).toBe('Delete Project');
    await fireEvent.click(confirm as Element);

    await waitFor(() => {
      expect(getBindingMock('DeleteProject')).toHaveBeenCalledWith('project-1');
    });
  });

  it('warns about the checkouts the deletion could not remove', async () => {
    setBindingMock('ProjectDeletionPreview', async () => emptyPreview());
    setBindingMock('DeleteProject', async () => ({
      threadIds: ['thread-1'],
      retainedWorktrees: [
        {
          path: '/tmp/worktrees/run-1',
          branch: 'workflow-run-1',
          reason: '2 uncommitted or untracked files',
        },
      ],
    }));
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.textContent).toContain('Permanently delete "Repo"');
    });
    const confirm = Array.from(baseElement.querySelectorAll('button')).find(
      (el) => el.textContent?.trim() === 'Delete',
    );
    await fireEvent.click(confirm as Element);

    await waitFor(() => {
      expect(toastMessages().join(' ')).toContain('Deleted project "Repo"');
    });
    const warning = getToasts().find((toast) => toast.type === 'warning');
    expect(warning?.message).toContain('1 checkout left in place');
    expect(warning?.message).toContain('/tmp/worktrees/run-1');
    expect(warning?.message).toContain('2 uncommitted or untracked files');
  });

  it('stops at the preview failure instead of deleting anything', async () => {
    setBindingMock('ProjectDeletionPreview', async () => {
      throw new Error('git is unavailable');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(toastMessages().join(' ')).toContain('Git is unavailable');
    });
    expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).toBeNull();
    expect(baseElement.textContent).not.toContain('Permanently delete "Repo"');
    expect(getBindingMock('DeleteProject')).not.toHaveBeenCalled();
    consoleError.mockRestore();
  });
});

describe('<ProjectContextMenu> New Group…', () => {
  beforeEach(() => {
    resetBindingMocks();
    setPageGrantsFromBootstrap(false);
    resetSidebarForTest();
    resetThreadGroupsForTest();
    setThreadFilterQuery('');
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  function newGroupItem(baseElement: HTMLElement): Element {
    const item = Array.from(baseElement.querySelectorAll('[role="menuitem"]')).find(
      (el) => el.textContent?.trim() === 'New Group…',
    );
    if (!item) throw new Error('New Group… not rendered');
    return item;
  }

  it('creates the group in this project, expands it, and asks for the rename', async () => {
    const create = setBindingMock('CreateThreadGroup', vi.fn(
      async (projectId: string, name: string) => ({
        id: 'g-new',
        projectId,
        name,
        createdAt: 0,
        updatedAt: 0,
      }),
    ));
    const { baseElement } = renderMenu();

    const item = Array.from(baseElement.querySelectorAll('[role="menuitem"]')).find(
      (el) => el.textContent?.trim() === 'New Group…',
    );
    await fireEvent.click(item as Element);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(create).toHaveBeenCalledWith('project-1', 'New Group');
    expect(getThreadGroupById('g-new')).toBeTruthy();
    expect(isProjectExpanded('project-1')).toBe(true);
    // The row that has not mounted yet owes the inline rename.
    expect(consumePendingGroupRename('g-new')).toBe(true);
  });

  it('clears the sidebar search first — the filter would hide the new row', async () => {
    // A brand-new group is empty and named "New Group", so an active query
    // drops it from the project's bucket: it would be created, never render,
    // and never open the rename the user is about to type into.
    setBindingMock('CreateThreadGroup', vi.fn(async (projectId: string, name: string) => ({
      id: 'g-new',
      projectId,
      name,
      createdAt: 0,
      updatedAt: 0,
    })));
    setThreadFilterQuery('spike');
    const { baseElement } = renderMenu();

    await fireEvent.click(newGroupItem(baseElement));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(getThreadFilterQuery()).toBe('');
  });
});
